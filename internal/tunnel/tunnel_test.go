package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/state"
	"golang.org/x/crypto/ssh"
)

func TestParseIPQoS(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantInter    int
		wantNonInter int
		wantErr      bool
	}{
		{"empty returns -1", "", -1, -1, false},
		{"lowdelay", "lowdelay", 0x10, 0x10, false},
		{"throughput", "throughput", 0x08, 0x08, false},
		{"reliability", "reliability", 0x04, 0x04, false},
		{"none", "none", 0x00, 0x00, false},
		{"ef", "ef", 0xb8, 0xb8, false},
		{"af11", "af11", 0x28, 0x28, false},
		{"af43", "af43", 0x98, 0x98, false},
		{"cs0", "cs0", 0x00, 0x00, false},
		{"cs7", "cs7", 0xe0, 0xe0, false},
		{"two values", "lowdelay throughput", 0x10, 0x08, false},
		{"two DSCP", "af11 ef", 0x28, 0xb8, false},
		{"case insensitive", "LowDelay", 0x10, 0x10, false},
		{"unknown value", "invalid", 0, 0, true},
		{"three values", "a b c", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inter, nonInter, err := ParseIPQoS(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseIPQoS(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr {
				if inter != tt.wantInter {
					t.Errorf("interactive = %#x, want %#x", inter, tt.wantInter)
				}
				if nonInter != tt.wantNonInter {
					t.Errorf("nonInteractive = %#x, want %#x", nonInter, tt.wantNonInter)
				}
			}
		})
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		input    string
		wantUser string
		wantHost string
	}{
		{"root@192.168.1.1:22", "root", "192.168.1.1:22"},
		{"admin@10.0.0.1:2222", "admin", "10.0.0.1:2222"},
		{"user@host.local", "user", "host.local:22"}, // no port, appends :22
		{"192.168.1.1:22", "", "192.168.1.1:22"},     // no user
		{"192.168.1.1", "", "192.168.1.1:22"},        // no user, no port
		{"host.local", "", "host.local:22"},          // hostname only
		{"root@[::1]:22", "root", "[::1]:22"},        // IPv6 with user
		{"user@host:22", "user", "host:22"},          // simple user@host:port
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			user, host := parseTarget(tt.input)
			if user != tt.wantUser {
				t.Errorf("user = %q, want %q", user, tt.wantUser)
			}
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
		})
	}
}

func TestMergeOptions(t *testing.T) {
	tests := []struct {
		name   string
		parent map[string]string
		child  map[string]string
		want   map[string]string
	}{
		{
			"both nil",
			nil, nil,
			map[string]string{},
		},
		{
			"parent only",
			map[string]string{"a": "1", "b": "2"},
			nil,
			map[string]string{"a": "1", "b": "2"},
		},
		{
			"child only",
			nil,
			map[string]string{"c": "3"},
			map[string]string{"c": "3"},
		},
		{
			"child overrides parent",
			map[string]string{"a": "1", "b": "2"},
			map[string]string{"b": "override", "c": "3"},
			map[string]string{"a": "1", "b": "override", "c": "3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeOptions(tt.parent, tt.child)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d; got %v", len(got), len(tt.want), got)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestMergeOptionsDoesNotMutateInputs(t *testing.T) {
	parent := map[string]string{"a": "1"}
	child := map[string]string{"b": "2"}
	mergeOptions(parent, child)

	if _, ok := parent["b"]; ok {
		t.Error("mergeOptions mutated parent map")
	}
	if _, ok := child["a"]; ok {
		t.Error("mergeOptions mutated child map")
	}
}

func TestApplySSHConfigOptions(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]string
		check   func(t *testing.T, cfg *ssh.Config)
	}{
		{
			"ciphers",
			map[string]string{"Ciphers": "aes256-ctr,aes128-ctr"},
			func(t *testing.T, cfg *ssh.Config) {
				if len(cfg.Ciphers) != 2 || cfg.Ciphers[0] != "aes256-ctr" || cfg.Ciphers[1] != "aes128-ctr" {
					t.Errorf("Ciphers = %v", cfg.Ciphers)
				}
			},
		},
		{
			"kex algorithms",
			map[string]string{"KexAlgorithms": "curve25519-sha256"},
			func(t *testing.T, cfg *ssh.Config) {
				if len(cfg.KeyExchanges) != 1 || cfg.KeyExchanges[0] != "curve25519-sha256" {
					t.Errorf("KeyExchanges = %v", cfg.KeyExchanges)
				}
			},
		},
		{
			"MACs",
			map[string]string{"MACs": "hmac-sha2-256,hmac-sha2-512"},
			func(t *testing.T, cfg *ssh.Config) {
				if len(cfg.MACs) != 2 || cfg.MACs[0] != "hmac-sha2-256" {
					t.Errorf("MACs = %v", cfg.MACs)
				}
			},
		},
		{
			"empty options no change",
			map[string]string{},
			func(t *testing.T, cfg *ssh.Config) {
				if cfg.Ciphers != nil || cfg.KeyExchanges != nil || cfg.MACs != nil {
					t.Error("empty options should not set any fields")
				}
			},
		},
		{
			"nil options",
			nil,
			func(t *testing.T, cfg *ssh.Config) {
				// should not panic
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ssh.Config{}
			applySSHConfigOptions(cfg, tt.options)
			tt.check(t, cfg)
		})
	}
}

func TestIsHardConnError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"EOF", io.EOF, true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"OpError", &net.OpError{Op: "read", Err: errors.New("reset")}, true},
		{"connection reset string", errors.New("connection reset by peer"), true},
		{"broken pipe string", errors.New("broken pipe"), true},
		{"closed connection string", errors.New("use of closed network connection"), true},
		{"random error", errors.New("timeout"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isHardConnError(tt.err)
			if got != tt.want {
				t.Errorf("isHardConnError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	base := 10 * time.Second
	for range 100 {
		d := retryDelay(base)
		if d < base {
			t.Errorf("retryDelay returned %v < base %v", d, base)
		}
		if d >= base+base/4 {
			t.Errorf("retryDelay returned %v >= base+25%% (%v)", d, base+base/4)
		}
	}
}

func TestRetryDelayDistribution(t *testing.T) {
	base := 4 * time.Second
	var hasJitter bool
	first := retryDelay(base)
	for range 50 {
		if retryDelay(base) != first {
			hasJitter = true
			break
		}
	}
	if !hasJitter {
		t.Error("retryDelay returned the same value 50 times; expected jitter")
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		input string
		want  uint64
	}{
		{"1G", 1024 * 1024 * 1024},
		{"1g", 1024 * 1024 * 1024},
		{"500M", 500 * 1024 * 1024},
		{"500m", 500 * 1024 * 1024},
		{"64K", 64 * 1024},
		{"64k", 64 * 1024},
		{"1024", 1024},
		{"0", 0},
		{"", 0},
		{"abc", 0},
		{"  2G  ", 2 * 1024 * 1024 * 1024},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseByteSize(tt.input)
			if got != tt.want {
				t.Errorf("parseByteSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestApplySSHConfigOptions_RekeyLimit(t *testing.T) {
	cfg := &ssh.Config{}
	applySSHConfigOptions(cfg, map[string]string{"RekeyLimit": "1G"})
	if cfg.RekeyThreshold != 1024*1024*1024 {
		t.Errorf("RekeyThreshold = %d, want %d", cfg.RekeyThreshold, 1024*1024*1024)
	}
}

func TestRunPasswordCommand(t *testing.T) {
	password, err := runPasswordCommand("echo hunter2")
	if err != nil {
		t.Fatalf("runPasswordCommand failed: %v", err)
	}
	if password != "hunter2" {
		t.Errorf("password = %q, want %q", password, "hunter2")
	}
}

func TestRunPasswordCommand_TrimsWhitespace(t *testing.T) {
	password, err := runPasswordCommand("echo '  spaced  '")
	if err != nil {
		t.Fatalf("runPasswordCommand failed: %v", err)
	}
	if password != "spaced" {
		t.Errorf("password = %q, want %q", password, "spaced")
	}
}

func TestRunPasswordCommand_Failure(t *testing.T) {
	_, err := runPasswordCommand("false")
	if err == nil {
		t.Error("expected error for failing command")
	}
}

func TestBuildAuthMethods_KeyOnly(t *testing.T) {
	// Create a temporary SSH key for testing
	key := generateTestKey(t)
	client := &SSHClient{
		cfg: config.Connection{
			Auth: config.AuthCfg{Key: key},
		},
		log: slog.Default(),
	}
	methods, cleanup, err := client.buildAuthMethods("test")
	if err != nil {
		t.Fatalf("buildAuthMethods failed: %v", err)
	}
	defer cleanup()
	if len(methods) != 1 {
		t.Errorf("expected 1 auth method, got %d", len(methods))
	}
}

func TestBuildAuthMethods_PasswordCommand(t *testing.T) {
	client := &SSHClient{
		cfg: config.Connection{
			Auth: config.AuthCfg{PasswordCommand: "echo testpass"},
		},
		log: slog.Default(),
	}
	methods, cleanup, err := client.buildAuthMethods("test")
	if err != nil {
		t.Fatalf("buildAuthMethods failed: %v", err)
	}
	defer cleanup()
	// Should have password + keyboard-interactive
	if len(methods) != 2 {
		t.Errorf("expected 2 auth methods (password + keyboard-interactive), got %d", len(methods))
	}
}

func TestBuildAuthMethods_NoAuth(t *testing.T) {
	client := &SSHClient{
		cfg: config.Connection{Auth: config.AuthCfg{}},
		log: slog.Default(),
	}
	_, _, err := client.buildAuthMethods("test")
	if err == nil {
		t.Error("expected error when no auth methods configured")
	}
}

func TestBuildAuthMethods_AgentWithoutSocket(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	client := &SSHClient{
		cfg: config.Connection{
			Auth: config.AuthCfg{Agent: true, PasswordCommand: "echo fallback"},
		},
		log: slog.Default(),
	}
	methods, cleanup, err := client.buildAuthMethods("test")
	if err != nil {
		t.Fatalf("buildAuthMethods failed: %v", err)
	}
	defer cleanup()
	// Agent fails but password_command should still provide methods
	if len(methods) < 1 {
		t.Error("expected at least 1 auth method from password fallback")
	}
}

// generateTestKey creates a temporary ed25519 SSH private key file for testing.
func generateTestKey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	keyPath := dir + "/test_key"
	// Generate a minimal OpenSSH ed25519 key
	key, err := ssh.ParseRawPrivateKey([]byte(testKeyPEM))
	if err != nil {
		t.Fatalf("failed to parse test key: %v", err)
	}
	_ = key
	if err := os.WriteFile(keyPath, []byte(testKeyPEM), 0600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

// Real ed25519 private key for testing (unencrypted, OpenSSH format)
var testKeyPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n" +
	"QyNTUxOQAAACDnp69QUZbotg+ywW7wDj22PysFU0yjNQNFEAcBkQVhJgAAAJDFh3iTxYd4\n" +
	"kwAAAAtzc2gtZWQyNTUxOQAAACDnp69QUZbotg+ywW7wDj22PysFU0yjNQNFEAcBkQVhJg\n" +
	"AAAEB901nbqWqieuIsr77JNYvv652WVn0qRTX2g1+e2JP38Oenr1BRlui2D7LBbvAOPbY/\n" +
	"KwVTTKM1A0UQBwGRBWEmAAAACXRlc3RAdGVzdAECAwQ=\n" +
	"-----END OPENSSH PRIVATE KEY-----\n"

// newTestPublicKey generates a fresh in-process ed25519 public key for testing.
func newTestPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer from key: %v", err)
	}
	return signer.PublicKey()
}

// --- evictOldLimiters ---

func TestEvictOldLimiters(t *testing.T) {
	now := time.Now()
	maxAge := 10 * time.Minute

	tests := []struct {
		name        string
		ages        map[string]time.Duration // ip -> age at call time
		wantRemoved []string
		wantKept    []string
	}{
		{
			name:        "removes entries older than maxAge",
			ages:        map[string]time.Duration{"stale": 11 * time.Minute, "fresh": 5 * time.Minute},
			wantRemoved: []string{"stale"},
			wantKept:    []string{"fresh"},
		},
		{
			name:     "keeps all fresh entries",
			ages:     map[string]time.Duration{"a": 1 * time.Minute, "b": 9 * time.Minute},
			wantKept: []string{"a", "b"},
		},
		{
			name:        "removes all stale entries",
			ages:        map[string]time.Duration{"x": 20 * time.Minute, "y": 15 * time.Minute},
			wantRemoved: []string{"x", "y"},
		},
		{
			name: "empty map does not panic",
			ages: map[string]time.Duration{},
		},
		{
			name:     "entry exactly at maxAge is kept (condition is strictly greater)",
			ages:     map[string]time.Duration{"edge": 10 * time.Minute},
			wantKept: []string{"edge"},
		},
		{
			name:        "one nanosecond over maxAge is removed",
			ages:        map[string]time.Duration{"over": 10*time.Minute + time.Nanosecond},
			wantRemoved: []string{"over"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lims := make(map[string]*limiterEntry)
			for ip, age := range tt.ages {
				lims[ip] = &limiterEntry{lastSeen: now.Add(-age)}
			}
			evictOldLimiters(lims, maxAge, now)
			for _, ip := range tt.wantRemoved {
				if _, ok := lims[ip]; ok {
					t.Errorf("%q should have been evicted but remains", ip)
				}
			}
			for _, ip := range tt.wantKept {
				if _, ok := lims[ip]; !ok {
					t.Errorf("%q should remain but was evicted", ip)
				}
			}
			wantLen := len(tt.wantKept)
			if len(lims) != wantLen {
				t.Errorf("map len = %d, want %d", len(lims), wantLen)
			}
		})
	}
}

func TestEvictOldLimiters_DoesNotTouchFreshUnderPressure(t *testing.T) {
	// Simulate the pressure-eviction scenario: map is over limiterMaxEntries.
	// Only entries older than pressureAfter should be removed; fresh ones survive.
	now := time.Now()
	lims := make(map[string]*limiterEntry)
	lims["old"] = &limiterEntry{lastSeen: now.Add(-3 * time.Minute)}
	lims["fresh"] = &limiterEntry{lastSeen: now.Add(-30 * time.Second)}

	evictOldLimiters(lims, limiterPressureAfter, now) // pressureAfter = 2 minutes

	if _, ok := lims["old"]; ok {
		t.Error("entry older than pressureAfter should be evicted")
	}
	if _, ok := lims["fresh"]; !ok {
		t.Error("entry younger than pressureAfter should remain")
	}
}

// --- matchesAnyAuthorizedKey ---

func TestMatchesAnyAuthorizedKey(t *testing.T) {
	keyA := newTestPublicKey(t)
	keyB := newTestPublicKey(t)
	keyC := newTestPublicKey(t)

	tests := []struct {
		name       string
		incoming   ssh.PublicKey
		authorized []ssh.PublicKey
		want       bool
	}{
		{"match first", keyA, []ssh.PublicKey{keyA, keyB}, true},
		{"match last", keyB, []ssh.PublicKey{keyA, keyB}, true},
		{"match middle", keyB, []ssh.PublicKey{keyA, keyB, keyC}, true},
		{"no match", keyC, []ssh.PublicKey{keyA, keyB}, false},
		{"empty authorized list", keyA, []ssh.PublicKey{}, false},
		{"single key matches", keyA, []ssh.PublicKey{keyA}, true},
		{"single key no match", keyB, []ssh.PublicKey{keyA}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesAnyAuthorizedKey(tt.incoming, tt.authorized)
			if got != tt.want {
				t.Errorf("matchesAnyAuthorizedKey = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchesAnyAuthorizedKey_DifferentKeyTypesDoNotMatch(t *testing.T) {
	// Generate two independent ed25519 keys; their public bytes must differ.
	keyA := newTestPublicKey(t)
	keyB := newTestPublicKey(t)
	if matchesAnyAuthorizedKey(keyA, []ssh.PublicKey{keyB}) {
		t.Error("two distinct keys should not match")
	}
}

// --- recordAuthFailure ---

func TestRecordAuthFailure(t *testing.T) {
	var m sync.Map

	if got := recordAuthFailure(&m, "10.0.0.1"); got != 1 {
		t.Errorf("first failure = %d, want 1", got)
	}
	if got := recordAuthFailure(&m, "10.0.0.1"); got != 2 {
		t.Errorf("second failure = %d, want 2", got)
	}
	if got := recordAuthFailure(&m, "10.0.0.1"); got != 3 {
		t.Errorf("third failure = %d, want 3", got)
	}
}

func TestRecordAuthFailure_IndependentPerIP(t *testing.T) {
	var m sync.Map

	recordAuthFailure(&m, "10.0.0.1")
	recordAuthFailure(&m, "10.0.0.1")

	// A different IP starts at 1, not inheriting the count of 10.0.0.1.
	if got := recordAuthFailure(&m, "10.0.0.2"); got != 1 {
		t.Errorf("first failure for new IP = %d, want 1", got)
	}
}

func TestRecordAuthFailure_Concurrent(t *testing.T) {
	var m sync.Map
	const n = 100

	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			recordAuthFailure(&m, "192.168.1.1")
		}()
	}
	wg.Wait()

	v, ok := m.Load("192.168.1.1")
	if !ok {
		t.Fatal("key not found after concurrent writes")
	}
	if got := v.(*authFailureEntry).count.Load(); got != n {
		t.Errorf("concurrent count = %d, want %d", got, n)
	}
}

// --- stubSSHConn: minimal ssh.Conn implementation for keepAlive tests ---

type stubSSHConn struct {
	sendFn func(name string, wantReply bool, payload []byte) (bool, []byte, error)
	closed chan struct{}
	once   sync.Once
}

func newStubConn(sendFn func(string, bool, []byte) (bool, []byte, error)) *stubSSHConn {
	return &stubSSHConn{sendFn: sendFn, closed: make(chan struct{})}
}

func (s *stubSSHConn) SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error) {
	return s.sendFn(name, wantReply, payload)
}
func (s *stubSSHConn) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}
func (s *stubSSHConn) RemoteAddr() net.Addr                                               { return &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22} }
func (s *stubSSHConn) LocalAddr() net.Addr                                                { return &net.TCPAddr{} }
func (s *stubSSHConn) User() string                                                       { return "" }
func (s *stubSSHConn) SessionID() []byte                                                  { return nil }
func (s *stubSSHConn) ClientVersion() []byte                                              { return nil }
func (s *stubSSHConn) ServerVersion() []byte                                              { return nil }
func (s *stubSSHConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return nil, nil, errors.New("not implemented")
}
func (s *stubSSHConn) Wait() error { return nil }

// --- startKeepAlive (options parsing) ---

func TestStartKeepAlive_ZeroIntervalReturnsImmediately(t *testing.T) {
	conn := newStubConn(func(string, bool, []byte) (bool, []byte, error) {
		t.Error("SendRequest must not be called when interval is 0")
		return true, nil, nil
	})
	done := make(chan struct{})
	go func() {
		startKeepAlive(context.Background(), conn, nil, false, slog.Default())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("startKeepAlive did not return immediately for zero interval")
	}
}

func TestStartKeepAlive_NegativeIntervalReturnsImmediately(t *testing.T) {
	conn := newStubConn(func(string, bool, []byte) (bool, []byte, error) {
		t.Error("SendRequest must not be called when interval is negative")
		return true, nil, nil
	})
	done := make(chan struct{})
	go func() {
		opts := map[string]string{"ServerAliveInterval": "-1"}
		startKeepAlive(context.Background(), conn, opts, false, slog.Default())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("startKeepAlive did not return immediately for negative interval")
	}
}

// --- keepAliveLoop ---

func TestKeepAliveLoop_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	conn := newStubConn(func(string, bool, []byte) (bool, []byte, error) {
		return true, nil, nil
	})
	done := make(chan struct{})
	go func() {
		keepAliveLoop(ctx, tick, conn, "keepalive@openssh.com", 3, slog.Default())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keepAliveLoop did not stop after context cancellation")
	}
	select {
	case <-conn.closed:
		t.Error("connection should not be closed on clean context cancellation")
	default:
	}
}

func TestKeepAliveLoop_SendsRequestType(t *testing.T) {
	tick := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var gotType string
	conn := newStubConn(func(name string, _ bool, _ []byte) (bool, []byte, error) {
		gotType = name
		cancel() // stop after first send
		return true, nil, nil
	})
	tick <- time.Time{}
	keepAliveLoop(ctx, tick, conn, "keepalive@golang.org", 3, slog.Default())
	if gotType != "keepalive@golang.org" {
		t.Errorf("request type = %q, want %q", gotType, "keepalive@golang.org")
	}
}

func TestKeepAliveLoop_SoftErrorsCloseAfterCountMax(t *testing.T) {
	// countMax=2: failures 1 and 2 are tolerated; failure 3 closes connection.
	tick := make(chan time.Time, 10)
	conn := newStubConn(func(string, bool, []byte) (bool, []byte, error) {
		return false, nil, errors.New("soft error")
	})
	go keepAliveLoop(context.Background(), tick, conn, "keepalive", 2, slog.Default())
	tick <- time.Time{}
	tick <- time.Time{}
	tick <- time.Time{}
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("connection not closed after exceeding countMax")
	}
}

func TestKeepAliveLoop_HardErrorClosesImmediately(t *testing.T) {
	tick := make(chan time.Time, 1)
	conn := newStubConn(func(string, bool, []byte) (bool, []byte, error) {
		return false, nil, io.EOF // io.EOF is a hard error
	})
	done := make(chan struct{})
	go func() {
		keepAliveLoop(context.Background(), tick, conn, "keepalive", 100, slog.Default())
		close(done)
	}()
	tick <- time.Time{}
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("connection not closed immediately after hard error")
	}
	<-done
}

func TestKeepAliveLoop_SuccessResetsfailCount(t *testing.T) {
	// 2 soft failures, then 1 success, then 1 more soft failure.
	// With countMax=2, the connection must NOT be closed: after the reset, only 1
	// failure has accumulated, which is below the threshold.
	tick := make(chan time.Time, 4)
	processed := make(chan struct{}, 4)
	callNum := 0
	conn := newStubConn(func(string, bool, []byte) (bool, []byte, error) {
		callNum++
		n := callNum
		defer func() { processed <- struct{}{} }()
		if n == 3 {
			return true, nil, nil // success on 3rd call resets failCount
		}
		return false, nil, errors.New("soft")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go keepAliveLoop(ctx, tick, conn, "keepalive", 2, slog.Default())

	for range 4 {
		tick <- time.Time{}
	}
	for range 4 {
		select {
		case <-processed:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for tick to be processed")
		}
	}
	select {
	case <-conn.closed:
		t.Error("connection closed unexpectedly after fail count was reset by success")
	default:
	}
}

// --- loadSigner ---

func TestLoadSigner_ValidKey(t *testing.T) {
	path := generateTestKey(t)
	signer, err := loadSigner(path)
	if err != nil {
		t.Fatalf("loadSigner returned error: %v", err)
	}
	if signer == nil {
		t.Fatal("loadSigner returned nil signer")
	}
	if signer.PublicKey() == nil {
		t.Error("signer has nil public key")
	}
}

func TestLoadSigner_FileNotFound(t *testing.T) {
	_, err := loadSigner(t.TempDir() + "/nonexistent_key")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestLoadSigner_InvalidData(t *testing.T) {
	path := t.TempDir() + "/bad_key"
	if err := os.WriteFile(path, []byte("this is not a private key"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadSigner(path)
	if err == nil {
		t.Error("expected error for invalid key data, got nil")
	}
}

// --- loadAuthorizedKeys ---

func writeAuthorizedKeys(t *testing.T, keys ...ssh.PublicKey) string {
	t.Helper()
	var sb strings.Builder
	for _, k := range keys {
		sb.Write(ssh.MarshalAuthorizedKey(k))
	}
	path := t.TempDir() + "/authorized_keys"
	if err := os.WriteFile(path, []byte(sb.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAuthorizedKeys_SingleKey(t *testing.T) {
	key := newTestPublicKey(t)
	path := writeAuthorizedKeys(t, key)
	got, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
}

func TestLoadAuthorizedKeys_MultipleKeys(t *testing.T) {
	keys := []ssh.PublicKey{newTestPublicKey(t), newTestPublicKey(t), newTestPublicKey(t)}
	path := writeAuthorizedKeys(t, keys...)
	got, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
}

func TestLoadAuthorizedKeys_SkipsBlankAndCommentLines(t *testing.T) {
	key := newTestPublicKey(t)
	content := "\n# this is a comment\n\n" + string(ssh.MarshalAuthorizedKey(key)) + "\n# another comment\n"
	path := t.TempDir() + "/authorized_keys"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
}

func TestLoadAuthorizedKeys_ContinuesPastInvalidLine(t *testing.T) {
	key := newTestPublicKey(t)
	content := string(ssh.MarshalAuthorizedKey(key)) + "not-a-valid-key\n" + string(ssh.MarshalAuthorizedKey(key))
	path := t.TempDir() + "/authorized_keys"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The two valid keys are loaded; the invalid line is skipped.
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (invalid line should be skipped)", len(got))
	}
}

func TestLoadAuthorizedKeys_EmptyFile(t *testing.T) {
	path := t.TempDir() + "/authorized_keys"
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadAuthorizedKeys(path)
	if err == nil {
		t.Error("expected error for empty file, got nil")
	}
}

func TestLoadAuthorizedKeys_OnlyComments(t *testing.T) {
	path := t.TempDir() + "/authorized_keys"
	if err := os.WriteFile(path, []byte("# comment only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadAuthorizedKeys(path)
	if err == nil {
		t.Error("expected error for comment-only file, got nil")
	}
}

func TestLoadAuthorizedKeys_FileNotFound(t *testing.T) {
	_, err := loadAuthorizedKeys(t.TempDir() + "/nonexistent")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

// --- handleCancelTCPIPForward ---

// cancelPayload returns a properly marshalled tcpip-forward cancel payload.
func cancelPayload(t *testing.T, addr string) []byte {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	port, _ := strconv.Atoi(portStr)
	return ssh.Marshal(struct {
		BindAddr string
		BindPort uint32
	}{host, uint32(port)})
}

func TestHandleCancelTCPIPForward_ClosesAndRemovesListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	var mu sync.Mutex
	listeners := map[string]net.Listener{addr: ln}

	req := &ssh.Request{WantReply: false, Payload: cancelPayload(t, addr)}
	handleCancelTCPIPForward(req, &mu, listeners, slog.Default())

	if len(listeners) != 0 {
		t.Errorf("listeners map should be empty after cancel, got %v", listeners)
	}
	// Listener must be closed: Accept should return an error immediately.
	_, err = ln.Accept()
	if err == nil {
		t.Error("expected error from closed listener, got nil")
	}
}

func TestHandleCancelTCPIPForward_UnknownAddrNoOp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Use a different addr in the payload — not in the map.
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	otherAddr := other.Addr().String()
	_ = other.Close()

	var mu sync.Mutex
	listeners := map[string]net.Listener{ln.Addr().String(): ln}

	req := &ssh.Request{WantReply: false, Payload: cancelPayload(t, otherAddr)}
	handleCancelTCPIPForward(req, &mu, listeners, slog.Default())

	if len(listeners) != 1 {
		t.Errorf("listeners map should be unchanged for unknown addr, got %v", listeners)
	}
}

func TestHandleCancelTCPIPForward_BadPayload(t *testing.T) {
	var mu sync.Mutex
	listeners := map[string]net.Listener{}
	// Malformed payload must not panic; WantReply=false avoids the nil-mux Reply call.
	req := &ssh.Request{WantReply: false, Payload: []byte("garbage")}
	handleCancelTCPIPForward(req, &mu, listeners, slog.Default())
	if len(listeners) != 0 {
		t.Error("bad payload must not modify the listeners map")
	}
}

// --- acceptAndForward ---

func TestAcceptAndForward_DataForwarded(t *testing.T) {
	fwdLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Use net.Pipe for the target side: test controls both ends without a second TCP listener.
	targetSide, dialerSide := net.Pipe()
	dialer := func() (net.Conn, error) { return dialerSide, nil }

	go acceptAndForward(context.Background(), fwdLn, dialer, slog.Default(), nil)
	t.Cleanup(func() { fwdLn.Close() })

	client, err := net.Dial("tcp", fwdLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer targetSide.Close()

	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(targetSide, buf); err != nil {
		t.Fatalf("target read: %v", err)
	}
	if string(buf) != "hello" {
		t.Errorf("got %q, want %q", buf, "hello")
	}
}

func TestAcceptAndForward_DialerErrorDropsConnection(t *testing.T) {
	fwdLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialer := func() (net.Conn, error) { return nil, errors.New("dial failed") }

	go acceptAndForward(context.Background(), fwdLn, dialer, slog.Default(), nil)
	t.Cleanup(func() { fwdLn.Close() })

	client, err := net.Dial("tcp", fwdLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// When the dialer fails, acceptAndForward closes the accepted conn.
	// The client should see EOF or a connection reset.
	buf := make([]byte, 1)
	_, err = client.Read(buf)
	if err == nil {
		t.Error("expected EOF after dialer failure, got nil")
	}
}

func TestAcceptAndForward_StreamsMetric(t *testing.T) {
	fwdLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	targetSide, dialerSide := net.Pipe()
	dialer := func() (net.Conn, error) { return dialerSide, nil }

	m := &state.Metrics{}
	go acceptAndForward(context.Background(), fwdLn, dialer, slog.Default(), m)
	t.Cleanup(func() { fwdLn.Close() })

	client, err := net.Dial("tcp", fwdLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer targetSide.Close()

	// Write data so we can confirm the forwarding goroutine has started
	// (Streams.Add(1) is called before CountedBiCopy, so it has run by the
	// time the first byte reaches the target side).
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(targetSide, buf); err != nil {
		t.Fatalf("target read: %v", err)
	}
	if got := m.Streams.Load(); got != 1 {
		t.Errorf("streams = %d, want 1 while connection is active", got)
	}
}
