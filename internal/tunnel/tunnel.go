// Package tunnel implements SSH client and server functionality.
package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"os/exec"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/time/rate"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/netutil"
	"github.com/mmdemirbas/mesh/internal/proxy"
	"github.com/mmdemirbas/mesh/internal/state"
)

// SSH timing defaults.
const (
	defaultTCPKeepAlive       = 30 * time.Second
	defaultHandshakeTimeout   = 30 * time.Second
	defaultSSHClientTimeout   = 15 * time.Second
	defaultServerAliveInterval = 15 * time.Second
)

// keepaliveForwardSet is the name assigned to the synthetic forward set
// created when a connection has no forwards (keeps the SSH connection alive).
const keepaliveForwardSet = "keepalive"

// SSH server rate limiting and eviction constants.
const (
	// rateLimitPerSec / rateLimitBurst cap auth attempts per remote IP.
	rateLimitPerSec = 5
	rateLimitBurst  = 5

	// limiterEvictInterval is how often the eviction goroutine runs.
	limiterEvictInterval = 5 * time.Minute
	// limiterStaleAfter is the idle duration before normal eviction.
	limiterStaleAfter = 10 * time.Minute
	// limiterPressureAfter is the idle duration used when the map is at capacity.
	limiterPressureAfter = 2 * time.Minute
	// limiterMaxEntries is the maximum number of per-IP limiters before aggressive eviction.
	limiterMaxEntries = 10000
)

// authFailureEntry tracks cumulative SSH auth rejections and last activity for an IP.
type authFailureEntry struct {
	count    atomic.Int64
	lastSeen atomic.Int64 // unix nanoseconds
}

// authFailuresByIP tracks cumulative SSH auth rejection counts per remote IP.
// Values are *authFailureEntry. Evicted periodically alongside rate limiters.
var authFailuresByIP sync.Map

// SnapshotAuthFailures returns a point-in-time copy of auth failure counts keyed by remote IP.
func SnapshotAuthFailures() map[string]int64 {
	out := make(map[string]int64)
	authFailuresByIP.Range(func(k, v any) bool {
		out[k.(string)] = v.(*authFailureEntry).count.Load()
		return true
	})
	return out
}

// evictOldAuthFailures removes authFailuresByIP entries not seen since cutoff.
func evictOldAuthFailures(cutoff time.Time) {
	cutoffNano := cutoff.UnixNano()
	authFailuresByIP.Range(func(k, v any) bool {
		if v.(*authFailureEntry).lastSeen.Load() < cutoffNano {
			authFailuresByIP.Delete(k)
		}
		return true
	})
}

// limiterEntry holds a per-IP rate limiter and the time it was last used.
type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// evictOldLimiters deletes entries from limiters whose lastSeen is older than maxAge,
// measured from now. The caller must hold the map lock.
func evictOldLimiters(limiters map[string]*limiterEntry, maxAge time.Duration, now time.Time) {
	for ip, e := range limiters {
		if now.Sub(e.lastSeen) > maxAge {
			delete(limiters, ip)
		}
	}
}

// matchesAnyAuthorizedKey reports whether incoming matches any pre-marshaled authorized key.
func matchesAnyAuthorizedKey(incoming ssh.PublicKey, authorizedBytes [][]byte) bool {
	inBytes := incoming.Marshal()
	for _, akBytes := range authorizedBytes {
		if bytes.Equal(inBytes, akBytes) {
			return true
		}
	}
	return false
}

// recordAuthFailure increments the failure counter for ip in m and returns the new total.
func recordAuthFailure(m *sync.Map, ip string) int64 {
	entry, _ := m.LoadOrStore(ip, &authFailureEntry{})
	e := entry.(*authFailureEntry)
	e.lastSeen.Store(time.Now().UnixNano())
	return e.count.Add(1)
}

// --- SSH Server (accepts incoming connections) ---

// SSHServer listens for incoming SSH connections and handles forwarding requests.
// permitOpenPolicy is the pre-parsed result of the PermitOpen SSH option.
// Matching against a map is O(1) per request instead of re-parsing the
// comma-separated string on every direct-tcpip channel open.
type permitOpenPolicy struct {
	allowAll bool              // "any" or empty
	denyAll  bool              // "none"
	exact    map[string]bool   // host:port exact matches
	wildHost map[uint32]bool   // *:port — port is the key
	wildPort map[string]bool   // host:* — host is the key
}

func parsePermitOpen(options map[string]string) permitOpenPolicy {
	raw := config.GetOption(options, "PermitOpen")
	if raw == "" || raw == "any" {
		return permitOpenPolicy{allowAll: true}
	}
	pol := permitOpenPolicy{
		exact:    make(map[string]bool),
		wildHost: make(map[uint32]bool),
		wildPort: make(map[string]bool),
	}
	for _, p := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p == "none" {
			return permitOpenPolicy{denyAll: true}
		}
		if strings.HasPrefix(p, "*:") {
			if port, err := strconv.Atoi(p[2:]); err == nil {
				pol.wildHost[uint32(port)] = true
			}
			continue
		}
		if strings.HasSuffix(p, ":*") {
			pol.wildPort[p[:len(p)-2]] = true
			continue
		}
		pol.exact[p] = true
	}
	return pol
}

func (p *permitOpenPolicy) allows(host string, port uint32) bool {
	if p.allowAll {
		return true
	}
	if p.denyAll {
		return false
	}
	target := net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10))
	return p.exact[target] || p.wildHost[port] || p.wildPort[host]
}

type SSHServer struct {
	cfg       config.Listener
	log       *slog.Logger
	permitOpen permitOpenPolicy
}

func NewSSHServer(cfg config.Listener, log *slog.Logger) *SSHServer {
	return &SSHServer{
		cfg:        cfg,
		log:        log.With("component", "sshd", "listen", cfg.Bind),
		permitOpen: parsePermitOpen(cfg.Options),
	}
}

func (s *SSHServer) Run(ctx context.Context) error {
	state.Global.Update("server", s.cfg.Bind, state.Starting, "")
	hostKey, err := loadSigner(s.cfg.HostKey)
	if err != nil {
		state.Global.Update("server", s.cfg.Bind, state.Failed, err.Error())
		return fmt.Errorf("load host key %s: %w", s.cfg.HostKey, err)
	}

	authorizedKeys, err := loadAuthorizedKeys(s.cfg.AuthorizedKeys)
	if err != nil {
		state.Global.Update("server", s.cfg.Bind, state.Failed, err.Error())
		return fmt.Errorf("load authorized keys %s: %w", s.cfg.AuthorizedKeys, err)
	}
	// Pre-marshal authorized keys once to avoid repeated marshaling on every auth attempt.
	authorizedKeyBytes := make([][]byte, len(authorizedKeys))
	for i, ak := range authorizedKeys {
		authorizedKeyBytes[i] = ak.Marshal()
	}

	var (
		limitersMu sync.Mutex
		limiters   = make(map[string]*limiterEntry)
	)

	// Periodically evict stale rate limiter entries to prevent unbounded memory growth.
	go func() {
		ticker := time.NewTicker(limiterEvictInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				limitersMu.Lock()
				evictOldLimiters(limiters, limiterStaleAfter, now)
				limitersMu.Unlock()
				evictOldAuthFailures(now.Add(-limiterStaleAfter))
			}
		}
	}()

	sshCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) { //nolint:gosec // G408: rate-limiter state is shared by design; key verification happens inside this callback
			tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
			if !ok {
				return nil, fmt.Errorf("unsupported address type: %T", conn.RemoteAddr())
			}
			ip := tcpAddr.IP.String()
			limitersMu.Lock()
			entry, exists := limiters[ip]
			if !exists {
				if len(limiters) > limiterMaxEntries {
					// Aggressive eviction: remove entries older than limiterPressureAfter under capacity pressure.
					evictOldLimiters(limiters, limiterPressureAfter, time.Now())
					if len(limiters) > limiterMaxEntries {
						limitersMu.Unlock()
						s.log.Warn("Rate limiter map at capacity after eviction, rejecting new IP", "ip", ip, "size", len(limiters))
						return nil, fmt.Errorf("server under heavy load, connection rejected")
					}
				}
				entry = &limiterEntry{limiter: rate.NewLimiter(rateLimitPerSec, rateLimitBurst)}
				limiters[ip] = entry
			}
			entry.lastSeen = time.Now()
			limiter := entry.limiter
			limitersMu.Unlock()

			// Rate-limit all auth attempts (not just failures) to bound CPU from key comparison.
			// Use server ctx so goroutines unblock on shutdown instead of leaking.
			if err := limiter.Wait(ctx); err != nil {
				return nil, err
			}

			if matchesAnyAuthorizedKey(key, authorizedKeyBytes) {
				s.log.Debug("Auth accepted", "remote", conn.RemoteAddr(), "user", conn.User())
				return &ssh.Permissions{}, nil
			}

			s.log.Debug("Auth rejected", "remote", conn.RemoteAddr(), "user", conn.User(), "fingerprint", ssh.FingerprintSHA256(key))
			recordAuthFailure(&authFailuresByIP, ip)
			return nil, fmt.Errorf("unknown public key for %q", conn.User())
		},
	}
	sshCfg.AddHostKey(hostKey)

	// Pre-auth banner (RFC 4252 section 5.4)
	if s.cfg.Banner != "" {
		bannerData, err := os.ReadFile(s.cfg.Banner) //nolint:gosec // G304: path from user config, validated at load time
		if err != nil {
			s.log.Warn("Failed to read banner file", "path", s.cfg.Banner, "error", err)
		} else {
			bannerText := string(bannerData)
			sshCfg.BannerCallback = func(conn ssh.ConnMetadata) string {
				return bannerText
			}
		}
	}

	// Read MOTD file at startup for post-auth display
	var motdText []byte
	if s.cfg.MOTD != "" {
		motdText, err = os.ReadFile(s.cfg.MOTD) //nolint:gosec // G304: path from user config, validated at load time
		if err != nil {
			s.log.Warn("Failed to read motd file", "path", s.cfg.MOTD, "error", err)
			motdText = nil
		}
	}

	applySSHConfigOptions(&sshCfg.Config, s.cfg.Options)

	listener, err := net.Listen("tcp", s.cfg.Bind)
	if err != nil {
		state.Global.Update("server", s.cfg.Bind, state.Failed, err.Error())
		return fmt.Errorf("listen %s: %w", s.cfg.Bind, err)
	}
	var lnOnce sync.Once
	closeLn := func() { lnOnce.Do(func() { _ = listener.Close() }) }
	defer closeLn()
	state.Global.Update("server", s.cfg.Bind, state.Listening, "")
	serverMetrics := state.Global.GetMetrics("server", s.cfg.Bind)
	serverMetrics.StartTime.Store(time.Now().UnixNano())
	s.log.Info("SSH server listening")

	stop := context.AfterFunc(ctx, closeLn)
	defer stop()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log.Error("Accept failed", "error", err)
			continue
		}
		go s.handleConn(ctx, conn, sshCfg, serverMetrics, motdText)
	}
}

func (s *SSHServer) handleConn(ctx context.Context, conn net.Conn, cfg *ssh.ServerConfig, serverMetrics *state.Metrics, motd []byte) {
	tcpKeepAlive := defaultTCPKeepAlive
	if val := config.GetOption(s.cfg.Options, "TCPKeepAlive"); val != "" {
		if seconds, err := strconv.Atoi(val); err == nil && seconds > 0 {
			tcpKeepAlive = time.Duration(seconds) * time.Second
		}
	}
	netutil.ApplyTCPKeepAlive(conn, tcpKeepAlive)

	// Set a handshake deadline to prevent slowloris-style attacks where a client
	// connects but never completes the SSH handshake, holding resources indefinitely.
	_ = conn.SetDeadline(time.Now().Add(defaultHandshakeTimeout))

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || strings.Contains(err.Error(), "connection reset by peer") {
			s.log.Debug("Handshake failed (likely health check/scanner)", "remote", conn.RemoteAddr(), "error", err)
		} else {
			s.log.Error("Handshake failed", "remote", conn.RemoteAddr(), "error", err)
		}
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{}) // clear deadline; data flows indefinitely
	defer func() { _ = sshConn.Close() }()

	// Per-connection context so background goroutines (keep-alive, forwarding)
	// stop when this connection ends rather than running until server shutdown.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	s.log.Info("Client connected", "remote", sshConn.RemoteAddr(), "user", sshConn.User())

	// Per-connection identity announced by mesh peers via a standard SSH
	// global request. Non-mesh clients simply won't send it — the fallback
	// is the TCP address as before.
	var clientNodeName atomic.Value

	var mu sync.Mutex
	listeners := make(map[string]net.Listener)
	defer func() {
		mu.Lock()
		for _, l := range listeners {
			_ = l.Close()
		}
		mu.Unlock()
	}()

	// Handle global requests (tcpip-forward, mesh identity, keepalive)
	go func() {
		for req := range reqs {
			switch req.Type {
			case "mesh-node-name@mesh":
				if len(req.Payload) > 0 {
					clientNodeName.Store(string(req.Payload))
					s.log.Info("Client identified", "remote", sshConn.RemoteAddr(), "node", string(req.Payload))
				}
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
			case "tcpip-forward":
				go handleTCPIPForward(connCtx, req, sshConn, &mu, listeners, s.log, s.cfg.Bind, s.cfg.Options, &clientNodeName)
			case "cancel-tcpip-forward":
				go handleCancelTCPIPForward(req, &mu, listeners, s.log)
			case "keepalive@openssh.com":
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
			default:
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}()

	// Handle keep-alives (server-side) -- must start before blocking on chans
	go startKeepAlive(connCtx, sshConn, s.cfg.Options, true, s.log)

	// Handle channel requests with per-connection limit to prevent goroutine exhaustion.
	const maxChannelsPerConn = 1000
	var activeChannels atomic.Int64
	for newChan := range chans {
		if activeChannels.Load() >= maxChannelsPerConn {
			_ = newChan.Reject(ssh.ResourceShortage, "too many channels")
			s.log.Warn("Channel limit reached, rejecting", "remote", sshConn.RemoteAddr(), "limit", maxChannelsPerConn)
			continue
		}
		switch newChan.ChannelType() {
		case "direct-tcpip":
			activeChannels.Add(1)
			go func() {
				defer activeChannels.Add(-1)
				handleDirectTCPIP(newChan, s.log, &s.permitOpen, serverMetrics)
			}()
		case "session":
			activeChannels.Add(1)
			go func() {
				defer activeChannels.Add(-1)
				handleSession(connCtx, newChan, s.cfg.Shell, s.cfg.AcceptEnv, motd, s.log)
			}()
		default:
			_ = newChan.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}

	s.log.Info("Client disconnected", "remote", sshConn.RemoteAddr())
}

// --- SSH Client (connects to a peer) ---

// SSHClient connects to a remote SSH server and manages forwarding + proxies.
type SSHClient struct {
	cfg      config.Connection
	nodeName string // node name from config (e.g. "client", "server")
	log      *slog.Logger
}

func NewSSHClient(cfg config.Connection, nodeName string, log *slog.Logger) *SSHClient {
	return &SSHClient{cfg: cfg, nodeName: nodeName, log: log.With("component", "ssh", "name", cfg.Name)}
}

func (c *SSHClient) Run(ctx context.Context) error {
	if c.cfg.Mode == "multiplex" {
		return c.runMultiplex(ctx)
	}
	var wg sync.WaitGroup
	for _, fset := range c.cfg.Forwards {
		fset := fset
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runForwardSet(ctx, &fset)
		}()
	}
	wg.Wait()
	return nil
}

// runMultiplex connects to ALL targets simultaneously (one connection per target).
// Each target gets its own set of forwards.
func (c *SSHClient) runMultiplex(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, target := range c.cfg.Targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runMultiplexTarget(ctx, target)
		}()
	}
	wg.Wait()
	return nil
}

func (c *SSHClient) runMultiplexTarget(ctx context.Context, target string) {
	// Each multiplex target acts like its own connection with a single-target failover.
	// If there are no forwards, create a dummy forward set to keep the connection alive.
	fsets := c.cfg.Forwards
	if len(fsets) == 0 {
		fsets = []config.ForwardSet{{Name: keepaliveForwardSet}}
	}

	var wg sync.WaitGroup
	for _, fset := range fsets {
		fset := fset
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runForwardSetForTarget(ctx, &fset, target)
		}()
	}
	wg.Wait()
}

// buildAuthMethods constructs SSH auth methods from the config.
// Methods are tried in order: agent → key → password_command.
// The returned cleanup function closes resources (e.g. agent socket) and must
// be called when the auth methods are no longer needed.
func (c *SSHClient) buildAuthMethods(id string) ([]ssh.AuthMethod, func(), error) {
	var methods []ssh.AuthMethod
	var agentConn net.Conn

	// 1. SSH Agent (most secure — keys never leave the agent process)
	if c.cfg.Auth.Agent {
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			c.log.Warn("auth.agent=true but SSH_AUTH_SOCK not set")
		} else {
			conn, err := net.Dial("unix", sock) //nolint:gosec // G704: sock is SSH_AUTH_SOCK env var, not untrusted network input
			if err != nil {
				c.log.Warn("Could not connect to SSH agent", "error", err)
			} else {
				agentConn = conn
				agentClient := agent.NewClient(conn)
				methods = append(methods, ssh.PublicKeysCallback(agentClient.Signers))
			}
		}
	}

	// 2. Private key file
	if c.cfg.Auth.Key != "" {
		signer, err := loadSigner(c.cfg.Auth.Key)
		if err != nil {
			if agentConn != nil {
				_ = agentConn.Close()
			}
			return nil, nil, fmt.Errorf("load key %s: %w", c.cfg.Auth.Key, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	// 3. Password command (least privileged — password obtained from external tool)
	if c.cfg.Auth.PasswordCommand != "" {
		password, err := runPasswordCommand(c.cfg.Auth.PasswordCommand)
		if err != nil {
			if agentConn != nil {
				_ = agentConn.Close()
			}
			return nil, nil, fmt.Errorf("password_command failed: %w", err)
		}
		methods = append(methods, ssh.Password(password))
		methods = append(methods, ssh.KeyboardInteractive(
			func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}))
	}

	if len(methods) == 0 {
		if agentConn != nil {
			_ = agentConn.Close()
		}
		return nil, nil, errors.New("no auth methods configured (set agent, key, or password_command)")
	}
	cleanup := func() {
		if agentConn != nil {
			_ = agentConn.Close()
		}
	}
	return methods, cleanup, nil
}

// runPasswordCommand executes a shell command and returns its trimmed stdout as a password.
func runPasswordCommand(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := shellCommand(command)
	cmd := exec.CommandContext(ctx, base.Path, base.Args[1:]...) //nolint:gosec // G204: args from shellCommand (user-configured password_command)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *SSHClient) buildSSHConfig(fset *config.ForwardSet, id string) (*ssh.ClientConfig, func(), map[string]string, int, error) {
	authMethods, authCleanup, err := c.buildAuthMethods(id)
	if err != nil {
		return nil, nil, nil, 0, err
	}

	opts := mergeOptions(c.cfg.Options, fset.Options)

	var hostKeyCallback ssh.HostKeyCallback
	switch {
	case c.cfg.Auth.KnownHosts != "":
		hkc, err := knownhosts.New(c.cfg.Auth.KnownHosts)
		if err != nil {
			authCleanup()
			return nil, nil, nil, 0, fmt.Errorf("load known_hosts %s: %w", c.cfg.Auth.KnownHosts, err)
		}
		hostKeyCallback = hkc
	case strings.ToLower(config.GetOption(opts, "StrictHostKeyChecking")) == "no":
		hostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec // G106: explicit user opt-out via StrictHostKeyChecking=no
		c.log.Warn("StrictHostKeyChecking=no is configured. Vulnerable to MITM attacks.")
	default:
		authCleanup()
		return nil, nil, nil, 0, errors.New("SSH server identity cannot be verified: auth.known_hosts is not configured and StrictHostKeyChecking is not set to 'no'. " +
			"Set auth.known_hosts to a known_hosts file, or add StrictHostKeyChecking: 'no' to options (insecure, allows MITM attacks)")
	}

	defaultUser := "root"
	if u, err := user.Current(); err == nil && u.Username != "" {
		defaultUser = u.Username
	}

	sshCfg := &ssh.ClientConfig{
		User:            defaultUser,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         defaultSSHClientTimeout,
	}

	if timeoutStr := config.GetOption(opts, "ConnectTimeout"); timeoutStr != "" {
		if t, err := strconv.Atoi(timeoutStr); err == nil {
			sshCfg.Timeout = time.Duration(t) * time.Second
		}
	}

	applySSHConfigOptions(&sshCfg.Config, opts)

	if val := config.GetOption(opts, "HostKeyAlgorithms"); val != "" {
		sshCfg.HostKeyAlgorithms = strings.Split(val, ",")
	}

	interactiveTos, _, err := ParseIPQoS(config.GetOption(opts, "IPQoS"))
	if err != nil {
		authCleanup()
		return nil, nil, nil, 0, fmt.Errorf("invalid ipqos: %w", err)
	}

	return sshCfg, authCleanup, opts, interactiveTos, nil
}

// runForwardSetForTarget runs a forward set against a specific target (used by multiplex mode).
func (c *SSHClient) runForwardSetForTarget(ctx context.Context, fset *config.ForwardSet, target string) {
	_, host := parseTarget(target)
	id := c.cfg.Name + " " + host
	if fset.Name != "" && fset.Name != keepaliveForwardSet {
		id += " [" + fset.Name + "]"
	}
	state.Global.Update("connection", id, state.Starting, "")

	retryInterval := 10 * time.Second
	if c.cfg.Retry != "" {
		if d, err := time.ParseDuration(c.cfg.Retry); err == nil {
			retryInterval = d
		}
	}

	sshCfg, authCleanup, opts, interactiveTos, err := c.buildSSHConfig(fset, id)
	if err != nil {
		state.Global.Update("connection", id, state.Failed, err.Error())
		c.log.Error("SSH config failed", "target", target, "error", err)
		return
	}
	defer authCleanup()

	log := c.log.With("target", target)

	for {
		if ctx.Err() != nil {
			return
		}

		state.Global.Update("connection", id, state.Connecting, "")
		user, hostPort := parseTarget(target)
		if user != "" {
			sshCfg.User = user
		}

		log.Info("Connecting")

		dialer := net.Dialer{Timeout: sshCfg.Timeout, Control: dialerControlIPQoS(interactiveTos)}
		t0 := time.Now()
		conn, err := dialer.DialContext(ctx, "tcp", hostPort) //nolint:gosec // G704: host is user-configured SSH target, not untrusted input
		if err != nil {
			state.Global.Update("connection", id, state.Retrying, err.Error())
			log.Warn("Target unreachable", "error", err)
			t := time.NewTimer(retryDelay(retryInterval))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				continue
			}
		}

		tcpKeepAlive := defaultTCPKeepAlive
		if val := config.GetOption(opts, "TCPKeepAlive"); val != "" {
			if seconds, err := strconv.Atoi(val); err == nil && seconds > 0 {
				tcpKeepAlive = time.Duration(seconds) * time.Second
			}
		}
		netutil.ApplyTCPKeepAlive(conn, tcpKeepAlive)

		t1 := time.Now()
		if sshCfg.Timeout > 0 {
			_ = conn.SetDeadline(time.Now().Add(sshCfg.Timeout))
		}
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, hostPort, sshCfg)
		if err != nil {
			_ = conn.Close()
			state.Global.Update("connection", id, state.Retrying, err.Error())
			log.Error("SSH handshake failed", "elapsed", time.Since(t1).Round(time.Millisecond), "error", err)
			t := time.NewTimer(retryDelay(retryInterval))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				continue
			}
		}
		_ = conn.SetDeadline(time.Time{})
		client := ssh.NewClient(sshConn, chans, reqs)

		state.Global.Update("connection", id, state.Connected, target)
		state.Global.UpdatePeer("connection", id, conn.RemoteAddr().String())

		log.Info("Connected", "tcp", t1.Sub(t0).Round(time.Millisecond), "ssh", time.Since(t1).Round(time.Millisecond))

		err = c.runSession(ctx, client, fset, opts, log)

		if err != nil && config.GetOption(opts, "ExitOnForwardFailure") == "yes" {
			state.Global.Update("connection", id, state.Failed, "ExitOnForwardFailure")
			log.Error("Fatal forward failure, stopping reconnection", "error", err)
			return
		}

		state.Global.Update("connection", id, state.Retrying, "session ended")
		log.Warn("Session ended, reconnecting", "retry_in", retryInterval)
		t := time.NewTimer(retryDelay(retryInterval))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (c *SSHClient) runForwardSet(ctx context.Context, fset *config.ForwardSet) {
	id := c.cfg.Name + " [" + fset.Name + "]"
	state.Global.Update("connection", id, state.Starting, "")

	retryInterval := 10 * time.Second
	if c.cfg.Retry != "" {
		if d, err := time.ParseDuration(c.cfg.Retry); err == nil {
			retryInterval = d
		}
	}

	sshCfg, authCleanup, opts, interactiveTos, err := c.buildSSHConfig(fset, id)
	if err != nil {
		state.Global.Update("connection", id, state.Failed, err.Error())
		c.log.Error("SSH config failed", "error", err)
		return
	}
	defer authCleanup()

	log := c.log.With("set", fset.Name)

	for {
		if ctx.Err() != nil {
			return
		}

		state.Global.Update("connection", id, state.Connecting, "")
		t0 := time.Now()
		target, conn := c.probeTarget(ctx, sshCfg.Timeout, interactiveTos)
		if target == "" {
			state.Global.Update("connection", id, state.Retrying, "no reachable target")
			log.Warn("No reachable target", "retry_in", retryInterval)
			t := time.NewTimer(retryDelay(retryInterval))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				continue
			}
		}

		user, host := parseTarget(target)
		if user != "" {
			sshCfg.User = user
		}

		log.Info("Connecting", "target", target)

		tcpKeepAlive := defaultTCPKeepAlive
		if val := config.GetOption(opts, "TCPKeepAlive"); val != "" {
			if seconds, err := strconv.Atoi(val); err == nil && seconds > 0 {
				tcpKeepAlive = time.Duration(seconds) * time.Second
			}
		}
		netutil.ApplyTCPKeepAlive(conn, tcpKeepAlive)

		t1 := time.Now()
		if sshCfg.Timeout > 0 {
			_ = conn.SetDeadline(time.Now().Add(sshCfg.Timeout))
		}
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, host, sshCfg)
		if err != nil {
			_ = conn.Close()
			state.Global.Update("connection", id, state.Retrying, err.Error())
			log.Error("SSH handshake failed", "target", target, "elapsed", time.Since(t1).Round(time.Millisecond), "error", err)
			t := time.NewTimer(retryDelay(retryInterval))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				continue
			}
		}
		_ = conn.SetDeadline(time.Time{}) // clear deadline; data flows indefinitely
		client := ssh.NewClient(sshConn, chans, reqs)

		state.Global.Update("connection", id, state.Connected, target)
		state.Global.UpdatePeer("connection", id, conn.RemoteAddr().String())

		log.Info("Connected", "target", target,
			"tcp", t1.Sub(t0).Round(time.Millisecond),
			"ssh", time.Since(t1).Round(time.Millisecond))

		err = c.runSession(ctx, client, fset, opts, log)

		if err != nil && config.GetOption(opts, "ExitOnForwardFailure") == "yes" {
			state.Global.Update("connection", id, state.Failed, "ExitOnForwardFailure")
			log.Error("Fatal forward failure, stopping reconnection", "error", err)
			return
		}

		state.Global.Update("connection", id, state.Retrying, "session ended")
		log.Warn("Session ended, reconnecting", "retry_in", retryInterval)
		t := time.NewTimer(retryDelay(retryInterval))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (c *SSHClient) runSession(ctx context.Context, client *ssh.Client, fset *config.ForwardSet, opts map[string]string, log *slog.Logger) error {
	// Announce our identity so the peer can display it in its dashboard.
	// Format: "nodeName/connectionName/forwardSetName"
	// This is a standard SSH global request (RFC 4254 §4): implementations
	// that don't recognise it simply ignore it, so this is safe with any sshd.
	identity := c.nodeName + "/" + c.cfg.Name + "/" + fset.Name
	_, _, _ = client.SendRequest("mesh-node-name@mesh", false, []byte(identity))

	var wg sync.WaitGroup
	sCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()

	var closeOnce sync.Once
	closeClient := func() { closeOnce.Do(func() { _ = client.Close() }) }
	defer closeClient()

	var fatalErr error
	var errMu sync.Mutex
	setFatal := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if fatalErr == nil {
			fatalErr = err
		}
		sCancel()
	}

	// Start keep-alives tied to the session context.
	// Uses ServerAliveInterval/ServerAliveCountMax from config; defaults to 15s / 3 failures.
	aliveInterval := defaultServerAliveInterval
	if val := config.GetOption(opts, "ServerAliveInterval"); val != "" {
		if s, err := strconv.Atoi(val); err == nil && s > 0 {
			aliveInterval = time.Duration(s) * time.Second
		}
	}
	aliveCountMax := 3
	if val := config.GetOption(opts, "ServerAliveCountMax"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			aliveCountMax = n
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(aliveInterval)
		defer ticker.Stop()
		failCount := 0
		for {
			select {
			case <-sCtx.Done():
				return
			case <-ticker.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					failCount++
					if failCount > aliveCountMax || isHardConnError(err) {
						log.Warn("Keep-alive failed, closing connection", "error", err)
						closeClient()
						return
					}
				} else {
					failCount = 0
				}
			}
		}
	}()

	// Monitor connection
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = client.Wait()
		sCancel()
	}()

	// Force connection close on context shutdown
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-sCtx.Done()
		closeClient()
	}()

	// Outbound rules: Remote (-R or remote proxy)
	for _, fwd := range fset.Remote {
		fwd := fwd
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if fwd.Type == "forward" {
				err = c.runRemoteForward(sCtx, client, fset.Name, fwd, log)
			} else {
				err = c.runRemoteProxy(sCtx, client, fset.Name, fwd, log)
			}
			if err != nil && config.GetOption(opts, "ExitOnForwardFailure") == "yes" {
				log.Error("Remote forward failed", "error", err)
				setFatal(err)
			}
		}()
	}

	// Inbound rules: Local (-L or local proxy)
	for _, fwd := range fset.Local {
		fwd := fwd
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if fwd.Type == "forward" {
				err = c.runLocalForward(sCtx, client, fset.Name, fwd, log)
			} else {
				err = c.runLocalProxy(sCtx, client, fset.Name, fwd, log)
			}
			if err != nil && config.GetOption(opts, "ExitOnForwardFailure") == "yes" {
				log.Error("Local forward failed", "error", err)
				setFatal(err)
			}
		}()
	}

	wg.Wait()
	return fatalErr
}

// runRemoteForward (-R equivalent): bind on peer, forward here
func (c *SSHClient) runRemoteForward(ctx context.Context, client *ssh.Client, fsetName string, fwd config.Forward, log *slog.Logger) error {
	log.Info("Forward -R", "bind", fwd.Bind, "target", fwd.Target)
	compID := fmt.Sprintf("%s [%s] %s", c.cfg.Name, fsetName, fwd.Bind)
	metrics := state.Global.GetMetrics("forward", compID)
	metrics.BytesTx.Store(0)
	metrics.BytesRx.Store(0)
	metrics.Streams.Store(0)
	metrics.StartTime.Store(time.Now().UnixNano())
	state.Global.Update("forward", compID, state.Starting, "")

	var listener net.Listener
	var err error
	backoff := 100 * time.Millisecond
	for range 6 {
		listener, err = client.Listen("tcp", fwd.Bind)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C: // wait out TIME_WAIT locking
		}
		backoff *= 2
	}
	if err != nil {
		log.Error("Remote listen failed", "bind", fwd.Bind, "error", err)
		state.Global.Update("forward", compID, state.Failed, err.Error())
		return err
	}
	var lnOnce sync.Once
	closeLn := func() { lnOnce.Do(func() { _ = listener.Close() }) }
	defer closeLn()

	state.Global.Update("forward", compID, state.Listening, "")
	state.Global.UpdateBind("forward", compID, listener.Addr().String())

	// Break accept loop immediately on context cancel
	stop := context.AfterFunc(ctx, closeLn)
	defer stop()
	// Clean up state on exit regardless of reason (context cancel or accept error)
	defer state.Global.Delete("forward", compID)
	defer state.Global.DeleteMetrics("forward", compID)

	acceptAndForward(ctx, listener, func() (net.Conn, error) {
		return net.DialTimeout("tcp", fwd.Target, 10*time.Second)
	}, log, metrics)
	return nil
}

// runLocalForward (-L equivalent): bind here, forward to peer
func (c *SSHClient) runLocalForward(ctx context.Context, client *ssh.Client, fsetName string, fwd config.Forward, log *slog.Logger) error {
	log.Info("Forward -L", "bind", fwd.Bind, "target", fwd.Target)
	compID := fmt.Sprintf("%s [%s] %s", c.cfg.Name, fsetName, fwd.Bind)
	metrics := state.Global.GetMetrics("forward", compID)
	metrics.BytesTx.Store(0)
	metrics.BytesRx.Store(0)
	metrics.Streams.Store(0)
	metrics.StartTime.Store(time.Now().UnixNano())
	state.Global.Update("forward", compID, state.Starting, "")

	listener, err := netutil.ListenReusable(ctx, "tcp", fwd.Bind)
	if err != nil {
		log.Error("Local listen failed", "bind", fwd.Bind, "error", err)
		state.Global.Update("forward", compID, state.Failed, err.Error())
		return err
	}
	var lnOnce sync.Once
	closeLn := func() { lnOnce.Do(func() { _ = listener.Close() }) }
	defer closeLn()

	state.Global.Update("forward", compID, state.Listening, "")
	state.Global.UpdateBind("forward", compID, listener.Addr().String())

	// Break accept loop immediately on context cancel
	stop := context.AfterFunc(ctx, closeLn)
	defer stop()
	// Clean up state on exit regardless of reason (context cancel or accept error)
	defer state.Global.Delete("forward", compID)
	defer state.Global.DeleteMetrics("forward", compID)

	acceptAndForward(ctx, listener, func() (net.Conn, error) {
		return client.Dial("tcp", fwd.Target)
	}, log, metrics)
	return nil
}

// runRemoteProxy binds proxy on peer, traffic exits HERE.
func (c *SSHClient) runRemoteProxy(ctx context.Context, client *ssh.Client, fsetName string, pxy config.Forward, log *slog.Logger) error {
	log.Info("Proxy remote bind", "type", pxy.Type, "bind", pxy.Bind, "target", pxy.Target)
	compID := fmt.Sprintf("%s [%s] %s", c.cfg.Name, fsetName, pxy.Bind)
	metrics := state.Global.GetMetrics("forward", compID)
	metrics.BytesTx.Store(0)
	metrics.BytesRx.Store(0)
	metrics.Streams.Store(0)
	metrics.StartTime.Store(time.Now().UnixNano())
	state.Global.Update("forward", compID, state.Starting, "")

	var listener net.Listener
	var err error
	backoff := 100 * time.Millisecond
	for range 6 {
		listener, err = client.Listen("tcp", pxy.Bind)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C: // wait out TIME_WAIT locking
		}
		backoff *= 2
	}
	if err != nil {
		log.Error("Remote proxy listen failed", "bind", pxy.Bind, "error", err)
		state.Global.Update("forward", compID, state.Failed, err.Error())
		return err
	}
	var lnOnce sync.Once
	closeLn := func() { lnOnce.Do(func() { _ = listener.Close() }) }
	defer closeLn()

	state.Global.Update("forward", compID, state.Listening, "")
	state.Global.UpdateBind("forward", compID, listener.Addr().String())

	// Guarantee immediate closure when session aborts, breaking accept loops
	stop := context.AfterFunc(ctx, closeLn)
	defer stop()
	defer state.Global.Delete("forward", compID)
	defer state.Global.DeleteMetrics("forward", compID)

	switch pxy.Type {
	case "socks":
		proxy.ServeSocks(ctx, listener, nil, log, metrics) // nil dialer = exit locally
	case "http":
		httpDialer := func(addr string) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 10*time.Second) // exits HERE
		}
		proxy.ServeHTTPProxyWithDialer(ctx, listener, httpDialer, log, metrics)
	}
	return nil
}

// runLocalProxy binds proxy here, traffic exits PEER.
func (c *SSHClient) runLocalProxy(ctx context.Context, client *ssh.Client, fsetName string, pxy config.Forward, log *slog.Logger) error {
	log.Info("Proxy local bind", "type", pxy.Type, "bind", pxy.Bind, "target", pxy.Target)
	compID := fmt.Sprintf("%s [%s] %s", c.cfg.Name, fsetName, pxy.Bind)
	metrics := state.Global.GetMetrics("forward", compID)
	metrics.BytesTx.Store(0)
	metrics.BytesRx.Store(0)
	metrics.Streams.Store(0)
	metrics.StartTime.Store(time.Now().UnixNano())
	state.Global.Update("forward", compID, state.Starting, "")

	listener, err := netutil.ListenReusable(ctx, "tcp", pxy.Bind)
	if err != nil {
		log.Error("Local proxy listen failed", "bind", pxy.Bind, "error", err)
		state.Global.Update("forward", compID, state.Failed, err.Error())
		return err
	}
	var lnOnce sync.Once
	closeLn := func() { lnOnce.Do(func() { _ = listener.Close() }) }
	defer closeLn()

	state.Global.Update("forward", compID, state.Listening, "")
	state.Global.UpdateBind("forward", compID, listener.Addr().String())

	// Guarantee immediate closure when session aborts, breaking accept loops
	stop := context.AfterFunc(ctx, closeLn)
	defer stop()
	defer state.Global.Delete("forward", compID)
	defer state.Global.DeleteMetrics("forward", compID)

	// For SOCKS, direct traffic through the SSH tunnel
	sshDialer := func(network, addr string) (net.Conn, error) {
		return client.Dial(network, addr)
	}

	switch pxy.Type {
	case "socks":
		proxy.ServeSocks(ctx, listener, sshDialer, log, metrics)
	case "http":
		// Wrap the upstream SOCKS or target destination in the SSH dialer
		httpDialer := func(addr string) (net.Conn, error) {
			if pxy.Target != "" {
				return proxy.DialViaSocks5(sshDialer, pxy.Target, addr)
			}
			return sshDialer("tcp", addr)
		}
		proxy.ServeHTTPProxyWithDialer(ctx, listener, httpDialer, log, metrics)
	}
	return nil
}

// probeTarget finds the first reachable target and returns both the target string and
// the open TCP connection, eliminating the double-dial that would otherwise waste a
// full round-trip on every reconnect. The caller is responsible for closing the conn.
//
// A short probe timeout (capped at 3s) is used for reachability checks, separate from
// the SSH handshake timeout, so unreachable targets fail fast and fallback targets
// (e.g., a public IP after a dead .local) are tried without long waits.
func (c *SSHClient) probeTarget(ctx context.Context, handshakeTimeout time.Duration, ipQoS int) (target string, conn net.Conn) {
	probeTimeout := 3 * time.Second
	if handshakeTimeout > 0 && handshakeTimeout/2 < probeTimeout {
		probeTimeout = handshakeTimeout / 2
	}

	for _, t := range c.cfg.Targets {
		_, hostPort := parseTarget(t)
		host, _, err := net.SplitHostPort(hostPort)
		if err != nil {
			host = hostPort
		}

		dialer := net.Dialer{Timeout: probeTimeout, Control: dialerControlIPQoS(ipQoS)}
		t0 := time.Now()
		conn, err = dialer.DialContext(ctx, "tcp", hostPort)
		if err != nil && strings.HasSuffix(host, ".local") {
			mdnsRetry := time.NewTimer(150 * time.Millisecond) // mDNS usually resolves within 50-100ms
			select {
			case <-ctx.Done():
				mdnsRetry.Stop()
				return "", nil
			case <-mdnsRetry.C:
			}
			conn, err = dialer.DialContext(ctx, "tcp", hostPort)
		}

		elapsed := time.Since(t0).Round(time.Millisecond)
		if err != nil {
			c.log.Debug("Target unreachable", "target", t, "elapsed", elapsed, "error", err)
			continue
		}
		c.log.Debug("Target reachable", "target", t, "elapsed", elapsed)
		return t, conn
	}
	return "", nil
}

// --- Shared forwarding helpers ---

// onceCloseListener wraps net.Listener with sync.Once-guarded Close to prevent
// double-close when both context cancellation and cancel-tcpip-forward race.
type onceCloseListener struct {
	net.Listener
	once sync.Once
	err  error
}

func (l *onceCloseListener) Close() error {
	l.once.Do(func() { l.err = l.Listener.Close() })
	return l.err
}

// handleTCPIPForward handles tcpip-forward global requests on the server side.
func handleTCPIPForward(ctx context.Context, req *ssh.Request, sshConn *ssh.ServerConn, mu *sync.Mutex, listeners map[string]net.Listener, log *slog.Logger, parentBind string, options map[string]string, clientNodeName *atomic.Value) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic recovered in tcpip-forward handler", "panic", r)
		}
	}()
	var fwdReq struct {
		BindAddr string
		BindPort uint32
	}
	if err := ssh.Unmarshal(req.Payload, &fwdReq); err != nil {
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
		return
	}

	gatewayPorts := config.GetOption(options, "GatewayPorts")
	bindAddr := fwdReq.BindAddr

	// GatewayPorts implementation
	switch strings.ToLower(gatewayPorts) {
	case "yes":
		// all remote forwards bind to all interfaces
		bindAddr = "0.0.0.0"
	case "no":
		// all remote forwards bind to localhost only (ignore requested bind addr)
		bindAddr = "127.0.0.1"
	case "clientspecified":
		// Honor client request, but reject wildcard binds (0.0.0.0, ::) to
		// prevent accidental network exposure. Use GatewayPorts=yes to allow.
		if ip := net.ParseIP(bindAddr); ip != nil && ip.IsUnspecified() {
			log.Warn("tcpip-forward rejected: wildcard bind requires GatewayPorts=yes", "addr", bindAddr)
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			return
		}
	default:
		// Default to mesh's previous behavior: use what's requested, or localhost if it looks like loopback
		if bindAddr == "" || bindAddr == "localhost" {
			bindAddr = "127.0.0.1"
		}
	}

	addr := net.JoinHostPort(bindAddr, strconv.FormatUint(uint64(fwdReq.BindPort), 10))

	// Hold mu across listen+insert so cancel-tcpip-forward cannot miss
	// a listener that has been created but not yet registered.
	mu.Lock()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		mu.Unlock()
		log.Error("tcpip-forward listen failed", "addr", addr, "error", err)
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
		return
	}
	ln = &onceCloseListener{Listener: ln}
	listeners[addr] = ln
	mu.Unlock()

	actualPort := uint32(ln.Addr().(*net.TCPAddr).Port)
	actualAddr := ln.Addr().String()

	var peerIP string
	if tcpAddr, ok := sshConn.RemoteAddr().(*net.TCPAddr); ok {
		ip := tcpAddr.IP
		if ip4 := ip.To4(); ip4 != nil {
			ip = ip4
		}
		// Strip zone ID (e.g. %en0) — it's interface-specific and clutters display
		ipStr := ip.String()
		if idx := strings.Index(ipStr, "%"); idx != -1 {
			ipStr = ipStr[:idx]
		}
		peerIP = net.JoinHostPort(ipStr, strconv.Itoa(tcpAddr.Port))
	} else {
		peerIP = sshConn.RemoteAddr().String()
	}

	peerAddr := peerIP
	if sshConn.User() != "" {
		peerAddr = sshConn.User() + "@" + peerAddr
	}

	compID := actualAddr + "|" + parentBind
	state.Global.Update("dynamic", compID, state.Listening, peerAddr)
	// Store mesh node name (if announced) so the dashboard can show it
	if v := clientNodeName.Load(); v != nil {
		state.Global.UpdatePeer("dynamic", compID, v.(string))
	}
	dm := state.Global.GetMetrics("dynamic", compID)
	dm.StartTime.Store(time.Now().UnixNano())
	defer func() {
		state.Global.Delete("dynamic", compID)
		state.Global.DeleteMetrics("dynamic", compID)
		log.Info("tcpip-forward closed", "addr", addr)
	}()

	log.Info("tcpip-forward active", "addr", addr)
	if req.WantReply {
		_ = req.Reply(true, ssh.Marshal(struct{ Port uint32 }{actualPort}))
	}

	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		netutil.ApplyTCPKeepAlive(conn, 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			origin, ok := conn.RemoteAddr().(*net.TCPAddr)
			if !ok {
				return
			}
			payload := ssh.Marshal(struct {
				DestAddr   string
				DestPort   uint32
				OriginAddr string
				OriginPort uint32
			}{fwdReq.BindAddr, fwdReq.BindPort, origin.IP.String(), uint32(origin.Port)})

			ch, reqs, err := sshConn.OpenChannel("forwarded-tcpip", payload)
			if err != nil {
				log.Debug("forwarded-tcpip channel open failed", "addr", actualAddr, "origin", origin, "error", err)
				return
			}
			go ssh.DiscardRequests(reqs)
			dm := state.Global.GetMetrics("dynamic", compID)
			dm.Streams.Add(1)
			defer dm.Streams.Add(-1)
			netutil.CountedBiCopy(conn, ch, &dm.BytesTx, &dm.BytesRx)
			_ = ch.Close()
		}()
	}
}

func handleCancelTCPIPForward(req *ssh.Request, mu *sync.Mutex, listeners map[string]net.Listener, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic recovered in cancel-tcpip-forward handler", "panic", r)
		}
	}()
	var r struct {
		BindAddr string
		BindPort uint32
	}
	if err := ssh.Unmarshal(req.Payload, &r); err != nil {
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
		return
	}
	addr := net.JoinHostPort(r.BindAddr, strconv.FormatUint(uint64(r.BindPort), 10))
	mu.Lock()
	if l, ok := listeners[addr]; ok {
		_ = l.Close()
		delete(listeners, addr)
	}
	mu.Unlock()
	if req.WantReply {
		_ = req.Reply(true, nil)
	}
	log.Info("tcpip-forward cancelled", "addr", addr)
}

func handleDirectTCPIP(newChan ssh.NewChannel, log *slog.Logger, policy *permitOpenPolicy, metrics *state.Metrics) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic recovered in direct-tcpip handler", "panic", r)
		}
	}()
	var req struct {
		DestAddr string
		DestPort uint32
		SrcAddr  string
		SrcPort  uint32
	}
	if err := ssh.Unmarshal(newChan.ExtraData(), &req); err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "parse error")
		return
	}

	target := net.JoinHostPort(req.DestAddr, strconv.FormatUint(uint64(req.DestPort), 10))

	if !policy.allows(req.DestAddr, req.DestPort) {
		log.Warn("direct-tcpip rejected by PermitOpen", "target", target)
		_ = newChan.Reject(ssh.ConnectionFailed, "prohibited by PermitOpen")
		return
	}

	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Debug("direct-tcpip dial failed", "target", target, "error", err)
		_ = newChan.Reject(ssh.ConnectionFailed, "connection refused")
		return
	}
	netutil.ApplyTCPKeepAlive(conn, 0)

	ch, chReqs, err := newChan.Accept()
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(chReqs)
	if metrics != nil {
		metrics.Streams.Add(1)
		defer metrics.Streams.Add(-1)
		netutil.CountedBiCopy(ch, conn, &metrics.BytesTx, &metrics.BytesRx)
	} else {
		netutil.BiCopy(ch, conn)
	}
	_ = ch.Close()
	_ = conn.Close()
}

// acceptAndForward accepts connections and forwards each to a target.
// If metrics is non-nil, bytes and active stream counts are tracked.
// Waits for all in-flight forwarding goroutines before returning.
func acceptAndForward(ctx context.Context, listener net.Listener, dialer func() (net.Conn, error), log *slog.Logger, metrics *state.Metrics) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			return
		}
		netutil.ApplyTCPKeepAlive(conn, 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			target, err := dialer()
			if err != nil {
				log.Debug("Forward dial failed", "error", err)
				return
			}
			defer func() { _ = target.Close() }()
			if metrics != nil {
				metrics.Streams.Add(1)
				defer metrics.Streams.Add(-1)
				netutil.CountedBiCopy(conn, target, &metrics.BytesTx, &metrics.BytesRx)
			} else {
				netutil.BiCopy(conn, target)
			}
		}()
	}
}

// --- Utility functions ---

func loadSigner(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the user-configured SSH private key path
	if err != nil {
		return nil, fmt.Errorf("read private key %q: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse private key %q: %w", path, err)
	}
	return signer, nil
}

func loadAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the user-configured authorized_keys file path
	if err != nil {
		return nil, fmt.Errorf("read authorized_keys %q: %w", path, err)
	}
	var keys []ssh.PublicKey
	rest := data
	lineNum := 0
	for len(rest) > 0 {
		lineNum++
		key, _, _, r, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			// Skip blank/comment lines silently; warn on actual parse failures
			if len(bytes.TrimSpace(rest)) > 0 && !bytes.HasPrefix(bytes.TrimSpace(rest), []byte("#")) {
				slog.Warn("Skipping unparsable authorized_keys entry", "file", path, "line", lineNum, "error", err)
			}
			// Advance past the current line instead of stopping, so subsequent valid keys are still loaded
			if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
				rest = rest[idx+1:]
			} else {
				break // no more lines
			}
			continue
		}
		keys = append(keys, key)
		rest = r
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no keys in %s", path)
	}
	return keys, nil
}

func parseTarget(target string) (user, host string) {
	if i := strings.Index(target, "@"); i >= 0 {
		user = target[:i]
		host = target[i+1:]
	} else {
		host = target
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host += ":22"
	}
	return
}

// retryDelay returns the base duration plus random jitter up to 25% of base,
// preventing thundering herd reconnection storms.
func retryDelay(base time.Duration) time.Duration {
	jitter := time.Duration(rand.Int63n(int64(base / 4))) //nolint:gosec // G404: non-cryptographic jitter for retry backoff
	return base + jitter
}

// mergeOptions merges two maps, with the child overriding the parent.
func mergeOptions(parent, child map[string]string) map[string]string {
	merged := make(map[string]string)
	for k, v := range parent {
		merged[k] = v
	}
	for k, v := range child {
		merged[k] = v
	}
	return merged
}

// applySSHConfigOptions applies supported SSH options to the base ssh.Config.
func applySSHConfigOptions(cfg *ssh.Config, options map[string]string) {
	// The defaults in golang.org/x/crypto/ssh are extensive and secure.
	// We only override them explicitly if the user defines them.
	if val := config.GetOption(options, "Ciphers"); val != "" {
		cfg.Ciphers = strings.Split(val, ",")
	}

	if val := config.GetOption(options, "KexAlgorithms"); val != "" {
		cfg.KeyExchanges = strings.Split(val, ",")
	}

	if val := config.GetOption(options, "MACs"); val != "" {
		cfg.MACs = strings.Split(val, ",")
	}

	if val := config.GetOption(options, "RekeyLimit"); val != "" {
		if n := parseByteSize(val); n > 0 {
			cfg.RekeyThreshold = n
		}
	}
}

// parseByteSize parses a human-readable byte size (e.g., "1G", "500M", "64K") to bytes.
func parseByteSize(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	multiplier := uint64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		multiplier = 1024
		s = s[:len(s)-1]
	case 'M', 'm':
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	case 'G', 'g':
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n * multiplier
}

// startKeepAlive handles both ClientAlive* (server-side) and ServerAlive* (client-side) options.
func startKeepAlive(ctx context.Context, conn ssh.Conn, options map[string]string, isServer bool, log *slog.Logger) {
	intervalKey := "ServerAliveInterval"
	countMaxKey := "ServerAliveCountMax"
	reqType := "keepalive@openssh.com" // Client-to-Server

	if isServer {
		intervalKey = "ClientAliveInterval"
		countMaxKey = "ClientAliveCountMax"
		reqType = "keepalive@golang.org" // Server-to-Client
	}

	interval := 0
	if val := config.GetOption(options, intervalKey); val != "" {
		var err error
		interval, err = strconv.Atoi(val)
		if err != nil {
			slog.Warn("Invalid keepalive interval, must be integer seconds", "key", intervalKey, "value", val, "error", err)
		}
	}
	if interval <= 0 {
		return
	}

	countMax := 3
	if val := config.GetOption(options, countMaxKey); val != "" {
		if c, err := strconv.Atoi(val); err == nil && c >= 0 {
			countMax = c
		}
	}

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	keepAliveLoop(ctx, ticker.C, conn, reqType, countMax, log)
}

// keepAliveLoop sends periodic heartbeat requests on conn until ctx is cancelled,
// the connection is closed, or consecutive failures exceed countMax.
// Extracted for testability: callers inject the tick channel so tests run without real timers.
func keepAliveLoop(ctx context.Context, tick <-chan time.Time, conn ssh.Conn, reqType string, countMax int, log *slog.Logger) {
	failCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			_, _, err := conn.SendRequest(reqType, true, nil)
			if err != nil {
				failCount++
				if failCount > countMax || isHardConnError(err) {
					log.Warn("Keep-alive failed, closing connection", "remote", conn.RemoteAddr(), "error", err, "fail_count", failCount)
					_ = conn.Close()
					return
				}
			} else {
				failCount = 0
			}
		}
	}
}

// isHardConnError reports whether err is a fatal connection error (RST, EOF,
// broken pipe, closed connection) that will not recover on retry.
func isHardConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection")
}
