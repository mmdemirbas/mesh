package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
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
	methods, err := client.buildAuthMethods("test")
	if err != nil {
		t.Fatalf("buildAuthMethods failed: %v", err)
	}
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
	methods, err := client.buildAuthMethods("test")
	if err != nil {
		t.Fatalf("buildAuthMethods failed: %v", err)
	}
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
	_, err := client.buildAuthMethods("test")
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
	methods, err := client.buildAuthMethods("test")
	if err != nil {
		t.Fatalf("buildAuthMethods failed: %v", err)
	}
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
	if got := v.(*atomic.Int64).Load(); got != n {
		t.Errorf("concurrent count = %d, want %d", got, n)
	}
}
