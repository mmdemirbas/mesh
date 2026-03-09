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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/time/rate"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/netutil"
	"github.com/mmdemirbas/mesh/internal/proxy"
	"github.com/mmdemirbas/mesh/internal/state"
)

// --- SSH Server (accepts incoming connections) ---

// SSHServer listens for incoming SSH connections and handles forwarding requests.
type SSHServer struct {
	cfg config.Server
	log *slog.Logger
}

func NewSSHServer(cfg config.Server, log *slog.Logger) *SSHServer {
	return &SSHServer{cfg: cfg, log: log.With("component", "sshd", "listen", cfg.Listen)}
}

func (s *SSHServer) Run(ctx context.Context) error {
	state.Global.Update("server", s.cfg.Listen, state.Starting, "")
	hostKey, err := loadSigner(s.cfg.HostKey)
	if err != nil {
		state.Global.Update("server", s.cfg.Listen, state.Failed, err.Error())
		return fmt.Errorf("load host key %s: %w", s.cfg.HostKey, err)
	}

	authorizedKeys, err := loadAuthorizedKeys(s.cfg.AuthorizedKeys)
	if err != nil {
		state.Global.Update("server", s.cfg.Listen, state.Failed, err.Error())
		return fmt.Errorf("load authorized keys %s: %w", s.cfg.AuthorizedKeys, err)
	}

	type limiterEntry struct {
		limiter  *rate.Limiter
		lastSeen time.Time
	}
	var (
		limitersMu sync.Mutex
		limiters   = make(map[string]*limiterEntry)
	)

	// Periodically evict stale rate limiter entries to prevent unbounded memory growth.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				limitersMu.Lock()
				for ip, entry := range limiters {
					if time.Since(entry.lastSeen) > 10*time.Minute {
						delete(limiters, ip)
					}
				}
				limitersMu.Unlock()
			}
		}
	}()

	sshCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			ip := conn.RemoteAddr().(*net.TCPAddr).IP.String()
			limitersMu.Lock()
			entry, exists := limiters[ip]
			if !exists {
				if len(limiters) > 10000 {
					// Aggressive eviction: remove entries older than 2 minutes under pressure
					for eIP, e := range limiters {
						if time.Since(e.lastSeen) > 2*time.Minute {
							delete(limiters, eIP)
						}
					}
					if len(limiters) > 10000 {
						limitersMu.Unlock()
						s.log.Warn("Rate limiter map at capacity after eviction, rejecting new IP", "ip", ip, "size", len(limiters))
						return nil, fmt.Errorf("server under heavy load, connection rejected")
					}
				}
				entry = &limiterEntry{limiter: rate.NewLimiter(5, 5)}
				limiters[ip] = entry
			}
			entry.lastSeen = time.Now()
			limiter := entry.limiter
			limitersMu.Unlock()

			for _, ak := range authorizedKeys {
				if bytes.Equal(key.Marshal(), ak.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}

			if err := limiter.Wait(context.Background()); err != nil {
				return nil, err
			}

			return nil, fmt.Errorf("unknown public key for %q", conn.User())
		},
	}
	sshCfg.AddHostKey(hostKey)
	applySSHConfigOptions(&sshCfg.Config, s.cfg.Options)

	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		state.Global.Update("server", s.cfg.Listen, state.Failed, err.Error())
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	defer listener.Close()
	state.Global.Update("server", s.cfg.Listen, state.Listening, "")
	s.log.Info("SSH server listening")

	stop := context.AfterFunc(ctx, func() { listener.Close() })
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
		go s.handleConn(ctx, conn, sshCfg)
	}
}

func (s *SSHServer) handleConn(ctx context.Context, conn net.Conn, cfg *ssh.ServerConfig) {
	netutil.ApplyTCPKeepAlive(conn)
	// Set a generous handshake deadline for high-latency links; always clear it afterward.
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetDeadline(time.Time{})

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || strings.Contains(err.Error(), "connection reset by peer") {
			s.log.Debug("Handshake failed (likely health check/scanner)", "remote", conn.RemoteAddr(), "error", err)
		} else {
			s.log.Error("Handshake failed", "remote", conn.RemoteAddr(), "error", err)
		}
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{})
	defer sshConn.Close()

	s.log.Info("Client connected", "remote", sshConn.RemoteAddr(), "user", sshConn.User())

	var mu sync.Mutex
	listeners := make(map[string]net.Listener)
	defer func() {
		mu.Lock()
		for _, l := range listeners {
			l.Close()
		}
		mu.Unlock()
	}()

	// Handle global requests (tcpip-forward)
	go func() {
		for req := range reqs {
			switch req.Type {
			case "tcpip-forward":
				go handleTCPIPForward(ctx, req, sshConn, &mu, listeners, s.log)
			case "cancel-tcpip-forward":
				go handleCancelTCPIPForward(req, &mu, listeners, s.log)
			default:
				if req.WantReply {
					req.Reply(false, nil)
				}
			}
		}
	}()

	// Handle channel requests
	for newChan := range chans {
		switch newChan.ChannelType() {
		case "direct-tcpip":
			go handleDirectTCPIP(newChan, s.log)
		case "session":
			go handleSession(ctx, newChan, s.cfg.Shell, s.log)
		default:
			newChan.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}

	s.log.Info("Client disconnected", "remote", sshConn.RemoteAddr())
}

// --- SSH Client (connects to a peer) ---

// SSHClient connects to a remote SSH server and manages forwarding + proxies.
type SSHClient struct {
	cfg config.Connection
	log *slog.Logger
}

func NewSSHClient(cfg config.Connection, log *slog.Logger) *SSHClient {
	return &SSHClient{cfg: cfg, log: log.With("component", "ssh", "name", cfg.Name)}
}

func (c *SSHClient) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, fset := range c.cfg.Forwards {
		fset := fset // capture loop variable
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runForwardSet(ctx, &fset)
		}()
	}
	wg.Wait()
	return nil
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

	signer, err := loadSigner(c.cfg.Auth.Key)
	if err != nil {
		state.Global.Update("connection", id, state.Failed, err.Error())
		c.log.Error("load key failed", "key", c.cfg.Auth.Key, "error", err)
		return
	}

	var hostKeyCallback ssh.HostKeyCallback
	if c.cfg.Auth.KnownHosts != "" {
		hkc, err := knownhosts.New(c.cfg.Auth.KnownHosts)
		if err != nil {
			state.Global.Update("connection", id, state.Failed, err.Error())
			c.log.Error("load known_hosts failed", "file", c.cfg.Auth.KnownHosts, "error", err)
			return
		}
		hostKeyCallback = hkc
	} else {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
		c.log.Warn("known_hosts is not configured. Vulnerable to MITM attacks.")
	}

	sshCfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}

	opts := mergeOptions(c.cfg.Options, fset.Options)

	if timeoutStr := config.GetOption(opts, "ConnectTimeout"); timeoutStr != "" {
		if t, err := strconv.Atoi(timeoutStr); err == nil {
			sshCfg.Timeout = time.Duration(t) * time.Second
		}
	}

	applySSHConfigOptions(&sshCfg.Config, opts)

	// Parse IPQoS for this forward set's connection
	tosValue, err := ParseIPQoS(config.GetOption(opts, "IPQoS"))
	if err != nil {
		state.Global.Update("connection", id, state.Failed, err.Error())
		c.log.Error("invalid ipqos", "set", fset.Name, "ipqos", config.GetOption(opts, "IPQoS"), "error", err)
		return
	}

	log := c.log.With("set", fset.Name)

	for {
		if ctx.Err() != nil {
			return
		}

		state.Global.Update("connection", id, state.Connecting, "")
		target := c.discoverTarget(ctx)
		if target == "" {
			state.Global.Update("connection", id, state.Retrying, "no reachable target")
			log.Warn("No reachable target", "retry_in", retryInterval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay(retryInterval)):
				continue
			}
		}

		user, host := parseTarget(target)
		if user != "" {
			sshCfg.User = user
		}

		log.Info("Connecting", "target", target)

		dialer := net.Dialer{Timeout: sshCfg.Timeout, Control: dialerControlIPQoS(tosValue)}
		conn, err := dialer.DialContext(ctx, "tcp", host)
		if err != nil {
			state.Global.Update("connection", id, state.Retrying, err.Error())
			log.Error("Dial failed", "target", target, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay(retryInterval)):
				continue
			}
		}
		netutil.ApplyTCPKeepAlive(conn)

		sshConn, chans, reqs, err := ssh.NewClientConn(conn, host, sshCfg)
		if err != nil {
			state.Global.Update("connection", id, state.Retrying, err.Error())
			log.Error("SSH Handshake failed", "target", target, "error", err)
			conn.Close()
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay(retryInterval)):
				continue
			}
		}
		client := ssh.NewClient(sshConn, chans, reqs)

		state.Global.Update("connection", id, state.Connected, target)
		log.Info("Connected", "target", target)

		c.runSession(ctx, client, fset, log)
		client.Close()

		state.Global.Update("connection", id, state.Retrying, "session ended")
		log.Warn("Session ended, reconnecting", "retry_in", retryInterval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay(retryInterval)):
		}
	}
}

func (c *SSHClient) runSession(ctx context.Context, client *ssh.Client, fset *config.ForwardSet, log *slog.Logger) {
	var wg sync.WaitGroup
	sCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()

	// Start keep-alives tied to the session context
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sCtx.Done():
				return
			case <-ticker.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					client.Close()
					return
				}
			}
		}
	}()

	// Monitor connection
	wg.Add(1)
	go func() {
		defer wg.Done()
		client.Wait()
		sCancel()
	}()

	// Force connection close on context shutdown
	go func() {
		<-sCtx.Done()
		client.Close()
	}()

	// Port forwarding: Remote (-R)
	for _, fwd := range fset.Remote {
		fwd := fwd
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runRemoteForward(sCtx, client, fwd, log)
		}()
	}

	// Port forwarding: Local (-L)
	for _, fwd := range fset.Local {
		fwd := fwd
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runLocalForward(sCtx, client, fwd, log)
		}()
	}

	// Connection-scoped proxies: Remote (-R dynamic)
	for _, pxy := range fset.Proxies.Remote {
		pxy := pxy
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runRemoteProxy(sCtx, client, pxy, log)
		}()
	}

	// Connection-scoped proxies: Local (-D)
	for _, pxy := range fset.Proxies.Local {
		pxy := pxy
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runLocalProxy(sCtx, client, pxy, log)
		}()
	}

	wg.Wait()
}

// runRemoteForward (-R equivalent): bind on peer, forward here
func (c *SSHClient) runRemoteForward(ctx context.Context, client *ssh.Client, fwd config.FwdRule, log *slog.Logger) {
	log.Info("Forward -R", "bind", fwd.Bind, "target", fwd.Target)
	listener, err := client.Listen("tcp", fwd.Bind)
	if err != nil {
		log.Error("Remote listen failed", "bind", fwd.Bind, "error", err)
		return
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()

	acceptAndForward(ctx, listener, func() (net.Conn, error) {
		return net.DialTimeout("tcp", fwd.Target, 10*time.Second)
	}, log)
}

// runLocalForward (-L equivalent): bind here, forward to peer
func (c *SSHClient) runLocalForward(ctx context.Context, client *ssh.Client, fwd config.FwdRule, log *slog.Logger) {
	log.Info("Forward -L", "bind", fwd.Bind, "target", fwd.Target)
	listener, err := net.Listen("tcp", fwd.Bind)
	if err != nil {
		log.Error("Local listen failed", "bind", fwd.Bind, "error", err)
		return
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()

	acceptAndForward(ctx, listener, func() (net.Conn, error) {
		return client.Dial("tcp", fwd.Target)
	}, log)
}

// runRemoteProxy binds proxy on peer, traffic exits HERE.
func (c *SSHClient) runRemoteProxy(ctx context.Context, client *ssh.Client, pxy config.Proxy, log *slog.Logger) {
	log.Info("Proxy remote bind", "type", pxy.Type, "bind", pxy.Bind, "upstream", pxy.Upstream)
	listener, err := client.Listen("tcp", pxy.Bind)
	if err != nil {
		log.Error("Remote proxy listen failed", "bind", pxy.Bind, "error", err)
		return
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()

	switch pxy.Type {
	case "socks":
		proxy.ServeSocks(ctx, listener, nil, log) // nil dialer = exit locally
	case "http":
		proxy.ServeHTTPProxy(ctx, listener, pxy.Upstream, log) // Upstream dialed locally
	}
}

// runLocalProxy binds proxy here, traffic exits PEER.
func (c *SSHClient) runLocalProxy(ctx context.Context, client *ssh.Client, pxy config.Proxy, log *slog.Logger) {
	log.Info("Proxy local bind", "type", pxy.Type, "bind", pxy.Bind, "upstream", pxy.Upstream)
	listener, err := net.Listen("tcp", pxy.Bind)
	if err != nil {
		log.Error("Local proxy listen failed", "bind", pxy.Bind, "error", err)
		return
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()

	// For SOCKS, direct traffic through the SSH tunnel
	sshDialer := func(network, addr string) (net.Conn, error) {
		return client.Dial(network, addr)
	}

	switch pxy.Type {
	case "socks":
		proxy.ServeSocks(ctx, listener, sshDialer, log)
	case "http":
		// Wrap the upstream SOCKS or target destination in the SSH dialer
		httpDialer := func(addr string) (net.Conn, error) {
			if pxy.Upstream != "" {
				return proxy.DialViaSocks5(sshDialer, pxy.Upstream, addr)
			}
			return sshDialer("tcp", addr)
		}
		proxy.ServeHTTPProxyWithDialer(ctx, listener, httpDialer, log)
	}
}

// discoverTarget probes each target in order to find the first reachable one.
// TODO: Avoid double-dial by passing the probed connection through to runForwardSet
// instead of closing it and re-dialing. This would save a round-trip on high-latency links.
func (c *SSHClient) discoverTarget(ctx context.Context) string {
	for _, target := range c.cfg.Targets {
		_, hostPort := parseTarget(target)
		host, _, err := net.SplitHostPort(hostPort)
		if err != nil {
			host = hostPort
		}

		dialer := net.Dialer{Timeout: 3 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", hostPort)

		if err != nil && strings.HasSuffix(host, ".local") {
			c.log.Debug("mDNS Dial failed, retrying for .local", "target", target)

			select {
			case <-ctx.Done():
				return ""
			case <-time.After(1 * time.Second):
			}

			conn, err = dialer.DialContext(ctx, "tcp", hostPort)
		}

		if err != nil {
			c.log.Debug("Target unreachable", "target", target)
			continue
		}
		conn.Close()
		return target
	}
	return ""
}

// --- Shared forwarding helpers ---

// handleTCPIPForward handles tcpip-forward global requests on the server side.
func handleTCPIPForward(ctx context.Context, req *ssh.Request, sshConn *ssh.ServerConn, mu *sync.Mutex, listeners map[string]net.Listener, log *slog.Logger) {
	var fwdReq struct {
		BindAddr string
		BindPort uint32
	}
	if err := ssh.Unmarshal(req.Payload, &fwdReq); err != nil {
		if req.WantReply {
			req.Reply(false, nil)
		}
		return
	}

	addr := net.JoinHostPort(fwdReq.BindAddr, strconv.FormatUint(uint64(fwdReq.BindPort), 10))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("tcpip-forward listen failed", "addr", addr, "error", err)
		if req.WantReply {
			req.Reply(false, nil)
		}
		return
	}

	actualPort := uint32(ln.Addr().(*net.TCPAddr).Port)
	mu.Lock()
	listeners[addr] = ln
	mu.Unlock()

	log.Info("tcpip-forward active", "addr", addr)
	if req.WantReply {
		req.Reply(true, ssh.Marshal(struct{ Port uint32 }{actualPort}))
	}

	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		netutil.ApplyTCPKeepAlive(conn)
		go func() {
			defer conn.Close()
			origin := conn.RemoteAddr().(*net.TCPAddr)
			payload := ssh.Marshal(struct {
				DestAddr   string
				DestPort   uint32
				OriginAddr string
				OriginPort uint32
			}{fwdReq.BindAddr, fwdReq.BindPort, origin.IP.String(), uint32(origin.Port)})

			ch, reqs, err := sshConn.OpenChannel("forwarded-tcpip", payload)
			if err != nil {
				return
			}
			go ssh.DiscardRequests(reqs)
			netutil.BiCopy(conn, ch)
			ch.Close()
		}()
	}
}

func handleCancelTCPIPForward(req *ssh.Request, mu *sync.Mutex, listeners map[string]net.Listener, log *slog.Logger) {
	var r struct {
		BindAddr string
		BindPort uint32
	}
	if err := ssh.Unmarshal(req.Payload, &r); err != nil {
		if req.WantReply {
			req.Reply(false, nil)
		}
		return
	}
	addr := net.JoinHostPort(r.BindAddr, strconv.FormatUint(uint64(r.BindPort), 10))
	mu.Lock()
	if l, ok := listeners[addr]; ok {
		l.Close()
		delete(listeners, addr)
	}
	mu.Unlock()
	if req.WantReply {
		req.Reply(true, nil)
	}
	log.Info("tcpip-forward cancelled", "addr", addr)
}

func handleDirectTCPIP(newChan ssh.NewChannel, log *slog.Logger) {
	var req struct {
		DestAddr string
		DestPort uint32
		SrcAddr  string
		SrcPort  uint32
	}
	if err := ssh.Unmarshal(newChan.ExtraData(), &req); err != nil {
		newChan.Reject(ssh.ConnectionFailed, "parse error")
		return
	}

	target := net.JoinHostPort(req.DestAddr, strconv.FormatUint(uint64(req.DestPort), 10))
	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		newChan.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	netutil.ApplyTCPKeepAlive(conn)

	ch, chReqs, err := newChan.Accept()
	if err != nil {
		conn.Close()
		return
	}
	go ssh.DiscardRequests(chReqs)
	netutil.BiCopy(ch, conn)
	ch.Close()
	conn.Close()
}

// acceptAndForward accepts connections and forwards each to a target.
func acceptAndForward(ctx context.Context, listener net.Listener, dialer func() (net.Conn, error), log *slog.Logger) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			return
		}
		netutil.ApplyTCPKeepAlive(conn)
		go func() {
			defer conn.Close()
			target, err := dialer()
			if err != nil {
				log.Debug("Forward dial failed", "error", err)
				return
			}
			defer target.Close()
			netutil.BiCopy(conn, target)
		}()
	}
}

// --- Utility functions ---

func loadSigner(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

func loadAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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
			break
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
	jitter := time.Duration(rand.Int63n(int64(base / 4)))
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
	if val := config.GetOption(options, "Ciphers"); val != "" {
		cfg.Ciphers = strings.Split(val, ",")
	} else if len(cfg.Ciphers) == 0 {
		cfg.Ciphers = []string{"chacha20-poly1305@openssh.com", "aes128-gcm@openssh.com"}
	}

	if val := config.GetOption(options, "KexAlgorithms"); val != "" {
		cfg.KeyExchanges = strings.Split(val, ",")
	} else if len(cfg.KeyExchanges) == 0 {
		cfg.KeyExchanges = []string{"curve25519-sha256@libssh.org", "curve25519-sha256"}
	}

	if val := config.GetOption(options, "MACs"); val != "" {
		cfg.MACs = strings.Split(val, ",")
	} else if len(cfg.MACs) == 0 {
		cfg.MACs = []string{"umac-64-etm@openssh.com", "hmac-sha2-256-etm@openssh.com"}
	}
}
