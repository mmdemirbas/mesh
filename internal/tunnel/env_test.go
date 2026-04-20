package tunnel

import "testing"

func TestEnvMatches(t *testing.T) {
	t.Parallel()
	allow := []string{"LANG", "LC_*", "TERM"}

	tests := []struct {
		name string
		want bool
	}{
		{"LANG", true},
		{"TERM", true},
		{"LC_ALL", true},
		{"LC_CTYPE", true},
		{"LC_", true}, // matches "LC_*" prefix
		{"HOME", false},
		{"LANGUAGE", false}, // not a prefix of "LANG"
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := envMatches(tt.name, allow); got != tt.want {
				t.Errorf("envMatches(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestEnvMatches_EmptyAllowlist(t *testing.T) {
	t.Parallel()
	if envMatches("LANG", nil) {
		t.Error("empty allowlist should reject all")
	}
}

func TestAcceptEnvRequest(t *testing.T) {
	t.Parallel()
	allow := []string{"LANG", "LC_*"}

	cases := []struct {
		name         string
		currentCount int
		envName      string
		envValue     string
		wantOK       bool
		wantReason   string
	}{
		{"admits allowlisted var under caps", 5, "LANG", "en_US.UTF-8", true, ""},
		{"admits wildcard-matched var", 5, "LC_ALL", "C", true, ""},
		{"rejects disallowed name", 5, "PATH", "/usr/bin", false, "env var not in allowlist"},
		{"rejects at count cap", maxAcceptedEnvVars, "LANG", "en", false, "env var count cap reached"},
		{"rejects oversize value", 0, "LANG", string(make([]byte, maxEnvValueSize+1)), false, "env var value exceeds size cap"},
		{"admits value exactly at size cap", 0, "LANG", string(make([]byte, maxEnvValueSize)), true, ""},
		{"count cap fires before allowlist check", maxAcceptedEnvVars, "PATH", "/x", false, "env var count cap reached"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ok, reason := acceptEnvRequest(c.currentCount, c.envName, c.envValue, allow)
			if ok != c.wantOK {
				t.Errorf("acceptEnvRequest(count=%d, %q) ok=%v, want %v (reason=%q)",
					c.currentCount, c.envName, ok, c.wantOK, reason)
			}
			if reason != c.wantReason {
				t.Errorf("acceptEnvRequest(count=%d, %q) reason=%q, want %q",
					c.currentCount, c.envName, reason, c.wantReason)
			}
		})
	}
}
