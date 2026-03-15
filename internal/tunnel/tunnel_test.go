package tunnel

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

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
