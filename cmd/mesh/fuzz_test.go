package main

import (
	"testing"
)

func FuzzParseIPv4(f *testing.F) {
	f.Add("192.168.1.1")
	f.Add("0.0.0.0")
	f.Add("255.255.255.255")
	f.Add("")
	f.Add("abc")
	f.Add("1.2.3")
	f.Add("1.2.3.4.5")
	f.Add("::1")
	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic
		_ = parseIPv4(input)
	})
}

func FuzzParseAddr(f *testing.F) {
	f.Add("192.168.1.1:8080")
	f.Add("[::1]:443")
	f.Add("root@10.0.0.5:2222")
	f.Add("hostname")
	f.Add("")
	f.Add(":0")
	f.Add("@:0")
	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic
		_, _ = parseAddr(input)
	})
}
