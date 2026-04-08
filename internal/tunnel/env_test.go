package tunnel

import "testing"

func TestEnvMatches(t *testing.T) {
	allow := []string{"LANG", "LC_*", "TERM"}

	tests := []struct {
		name string
		want bool
	}{
		{"LANG", true},
		{"TERM", true},
		{"LC_ALL", true},
		{"LC_CTYPE", true},
		{"LC_", true},   // matches "LC_*" prefix
		{"HOME", false},
		{"LANGUAGE", false}, // not a prefix of "LANG"
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := envMatches(tt.name, allow); got != tt.want {
				t.Errorf("envMatches(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestEnvMatches_EmptyAllowlist(t *testing.T) {
	if envMatches("LANG", nil) {
		t.Error("empty allowlist should reject all")
	}
}
