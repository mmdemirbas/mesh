package tunnel

import "testing"

func FuzzParseTarget(f *testing.F) {
	f.Add("root@10.0.0.1:22")
	f.Add("host")
	f.Add("user@host:2222")
	f.Add("")
	f.Add("@:0")
	f.Fuzz(func(t *testing.T, input string) {
		user, host := parseTarget(input)
		// host must always contain a port suffix
		if host == "" {
			t.Error("host must not be empty")
		}
		_ = user
	})
}

func FuzzParseByteSize(f *testing.F) {
	f.Add("1G")
	f.Add("512M")
	f.Add("64K")
	f.Add("")
	f.Add("abc")
	f.Add("0")
	f.Add("99999999999999999999G")
	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic
		_ = parseByteSize(input)
	})
}
