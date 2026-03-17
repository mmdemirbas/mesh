package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/state"
)

func TestParseIPv4(t *testing.T) {
	tests := []struct {
		input string
		want  [4]byte
	}{
		{"192.168.1.1", [4]byte{192, 168, 1, 1}},
		{"10.0.0.1", [4]byte{10, 0, 0, 1}},
		{"0.0.0.0", [4]byte{0, 0, 0, 0}},
		{"255.255.255.255", [4]byte{255, 255, 255, 255}},
		{"127.0.0.1", [4]byte{127, 0, 0, 1}},
		{"1.2.3.4", [4]byte{1, 2, 3, 4}},
		// Invalid cases — should return zero
		{"", [4]byte{}},
		{"256.0.0.1", [4]byte{}},
		{"1.2.3", [4]byte{}},
		{"1.2.3.4.5", [4]byte{}},
		{"abc", [4]byte{}},
		{"::1", [4]byte{}},
		{"1.2.3.4a", [4]byte{}},
		{".1.2.3", [4]byte{}},
		{"1..2.3", [4]byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseIPv4(tt.input)
			if got != tt.want {
				t.Errorf("parseIPv4(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseAddr(t *testing.T) {
	tests := []struct {
		input    string
		wantIP   net.IP
		wantPort int
	}{
		{"192.168.1.1:8080", net.ParseIP("192.168.1.1"), 8080},
		{"10.0.0.1:22", net.ParseIP("10.0.0.1"), 22},
		{"0.0.0.0:0", net.ParseIP("0.0.0.0"), 0},
		{"127.0.0.1:65535", net.ParseIP("127.0.0.1"), 65535},
		{"[::1]:443", net.ParseIP("::1"), 443},
		{"[fe80::1]:80", net.ParseIP("fe80::1"), 80},
		{"user@192.168.1.1:22", net.ParseIP("192.168.1.1"), 22},
		{"root@10.0.0.5:2222", net.ParseIP("10.0.0.5"), 2222},
		{"192.168.1.1", net.ParseIP("192.168.1.1"), 0},
		{"::1", net.ParseIP("::1"), 0}, // SplitHostPort fails, but ParseIP succeeds on bare "::1"
		{"not-an-ip:80", nil, 80},
		{"hostname", nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			gotIP, gotPort := parseAddr(tt.input)
			if tt.wantIP == nil && gotIP != nil {
				t.Errorf("parseAddr(%q) IP = %v, want nil", tt.input, gotIP)
			} else if tt.wantIP != nil && !tt.wantIP.Equal(gotIP) {
				t.Errorf("parseAddr(%q) IP = %v, want %v", tt.input, gotIP, tt.wantIP)
			}
			if gotPort != tt.wantPort {
				t.Errorf("parseAddr(%q) port = %d, want %d", tt.input, gotPort, tt.wantPort)
			}
		})
	}
}

func TestCompareAddr(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool // a < b
	}{
		// Same IP, different ports — sort by port
		{"same ip lower port first", "192.168.1.1:80", "192.168.1.1:443", true},
		{"same ip higher port second", "192.168.1.1:443", "192.168.1.1:80", false},
		{"same ip same port", "192.168.1.1:80", "192.168.1.1:80", false},

		// Different IPs — sort by IP numerically
		{"2.x before 10.x", "2.0.0.1:80", "10.0.0.1:80", true},
		{"10.x after 2.x", "10.0.0.1:80", "2.0.0.1:80", false},
		{"sequential IPs", "192.168.1.1:80", "192.168.1.2:80", true},
		{"0.0.0.0 before 127.0.0.1", "0.0.0.0:80", "127.0.0.1:80", true},
		{"127.0.0.1 before 192.168.0.1", "127.0.0.1:80", "192.168.0.1:80", true},

		// Different IPs, different ports — IP takes precedence
		{"ip precedence over port", "10.0.0.1:9999", "192.168.1.1:22", true},

		// IPv6
		{"ipv6 same ip different port", "[::1]:80", "[::1]:443", true},
		{"ipv6 loopback before ipv4 loopback", "[::1]:80", "127.0.0.1:80", true}, // ::1 = 0...001, 127.0.0.1 = 0...7f000001

		// With user@ prefix
		{"user prefix stripped for comparison", "root@10.0.0.1:22", "admin@10.0.0.2:22", true},
		{"user prefix same host port", "root@10.0.0.1:22", "admin@10.0.0.1:80", true},

		// Non-IP hostnames — string fallback
		{"hostname fallback alpha", "alpha:80", "beta:80", true},
		{"hostname fallback reverse", "beta:80", "alpha:80", false},

		// Mixed: IP vs non-IP — falls back to string comparison
		{"ip vs hostname fallback", "192.168.1.1:80", "hostname:80", true}, // "1" < "h"

		// Bare IPs without ports
		{"bare ip comparison", "10.0.0.1", "10.0.0.2", true},
		{"bare ip equal", "10.0.0.1", "10.0.0.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareAddr(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("compareAddr(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCompareAddrSort(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			"mixed IPs and ports",
			[]string{"192.168.1.1:443", "10.0.0.1:80", "10.0.0.1:22", "2.0.0.1:80", "192.168.1.1:80"},
			[]string{"2.0.0.1:80", "10.0.0.1:22", "10.0.0.1:80", "192.168.1.1:80", "192.168.1.1:443"},
		},
		{
			"same IP multiple ports",
			[]string{"127.0.0.1:8080", "127.0.0.1:80", "127.0.0.1:443", "127.0.0.1:22"},
			[]string{"127.0.0.1:22", "127.0.0.1:80", "127.0.0.1:443", "127.0.0.1:8080"},
		},
		{
			"common listener addresses",
			[]string{"0.0.0.0:8080", "127.0.0.1:8080", "0.0.0.0:22", "192.168.1.100:443"},
			[]string{"0.0.0.0:22", "0.0.0.0:8080", "127.0.0.1:8080", "192.168.1.100:443"},
		},
		{
			"with user prefix",
			[]string{"root@192.168.1.5:22", "admin@10.0.0.1:22", "user@10.0.0.1:2222"},
			[]string{"admin@10.0.0.1:22", "user@10.0.0.1:2222", "root@192.168.1.5:22"},
		},
		{
			"single element",
			[]string{"10.0.0.1:80"},
			[]string{"10.0.0.1:80"},
		},
		{
			"empty slice",
			[]string{},
			[]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make([]string, len(tt.input))
			copy(got, tt.input)
			sort.Slice(got, func(i, j int) bool { return compareAddr(got[i], got[j]) })
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("sorted[%d] = %q, want %q\nfull result: %v", i, got[i], tt.want[i], got)
					break
				}
			}
		})
	}
}

func BenchmarkParseAddr(b *testing.B) {
	addrs := []string{
		"192.168.1.1:8080",
		"[fe80::1]:443",
		"root@10.0.0.5:2222",
		"hostname:80",
		"10.0.0.1",
	}
	for _, addr := range addrs {
		b.Run(addr, func(b *testing.B) {
			for range b.N {
				parseAddr(addr)
			}
		})
	}
}

func BenchmarkCompareAddr(b *testing.B) {
	pairs := [][2]string{
		{"192.168.1.1:80", "192.168.1.1:443"},
		{"10.0.0.1:80", "192.168.1.1:80"},
		{"[::1]:80", "127.0.0.1:80"},
		{"root@10.0.0.1:22", "admin@10.0.0.2:22"},
		{"alpha:80", "beta:80"},
	}
	for _, p := range pairs {
		b.Run(p[0]+"_vs_"+p[1], func(b *testing.B) {
			for range b.N {
				compareAddr(p[0], p[1])
			}
		})
	}
}

func TestLogRing(t *testing.T) {
	r := newLogRing(3)

	// Empty ring
	if lines := r.Lines(); len(lines) != 0 {
		t.Errorf("empty ring has %d lines", len(lines))
	}

	// Add fewer than capacity
	r.Write([]byte("line1\n"))
	r.Write([]byte("line2\n"))
	lines := r.Lines()
	if len(lines) != 2 || lines[0] != "line1" || lines[1] != "line2" {
		t.Errorf("got %v, want [line1 line2]", lines)
	}

	// Fill and wrap around
	r.Write([]byte("line3\n"))
	r.Write([]byte("line4\n"))
	lines = r.Lines()
	if len(lines) != 3 || lines[0] != "line2" || lines[1] != "line3" || lines[2] != "line4" {
		t.Errorf("after wrap: got %v, want [line2 line3 line4]", lines)
	}
}

func TestLogRing_MultiLineWrite(t *testing.T) {
	r := newLogRing(5)
	r.Write([]byte("a\nb\nc\n"))
	lines := r.Lines()
	if len(lines) != 3 || lines[0] != "a" || lines[1] != "b" || lines[2] != "c" {
		t.Errorf("got %v, want [a b c]", lines)
	}
}

func TestLogRing_LinesIsACopy(t *testing.T) {
	r := newLogRing(3)
	r.Write([]byte("x\n"))
	lines := r.Lines()
	lines[0] = "mutated"
	if r.Lines()[0] == "mutated" {
		t.Error("Lines() returned a reference, not a copy")
	}
}

func TestRenderStatus_Empty(t *testing.T) {
	cfg := &config.Config{}
	output := renderStatus(cfg, nil, "testnode")
	if !strings.Contains(output, "testnode") {
		t.Error("output should contain the node name")
	}
	if !strings.Contains(output, "Configuration") {
		t.Error("output should contain the header")
	}
}

func TestRenderStatus_WithListeners(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
			{Type: "http", Bind: "127.0.0.1:3128"},
		},
	}
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
		"proxy:127.0.0.1:3128": {Type: "proxy", ID: "127.0.0.1:3128", Status: state.Failed, Message: "bind error"},
	}
	output := renderStatus(cfg, activeState, "testnode")
	if !strings.Contains(output, "listeners") {
		t.Error("output should contain 'listeners' section")
	}
	if !strings.Contains(output, "1080") {
		t.Error("output should contain port 1080")
	}
	if !strings.Contains(output, "listening") {
		t.Error("output should show listening status")
	}
	if !strings.Contains(output, "failed") {
		t.Error("output should show failed status")
	}
}

func TestRenderStatus_WithConnections(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "remote",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{
						Name: "web",
						Local: []config.Forward{
							{Type: "forward", Bind: "127.0.0.1:8080", Target: "10.0.0.1:80"},
						},
					},
				},
			},
		},
	}
	output := renderStatus(cfg, nil, "testnode")
	if !strings.Contains(output, "remote") {
		t.Error("output should contain connection name")
	}
	if !strings.Contains(output, "8080") {
		t.Error("output should contain forward bind port")
	}
}

func BenchmarkCompareAddrSort(b *testing.B) {
	sizes := []int{10, 50, 100}
	for _, n := range sizes {
		addrs := make([]string, n)
		for i := range n {
			addrs[i] = fmt.Sprintf("%d.%d.%d.%d:%d", i/64%256, i/16%256, i/4%256, i%256, 1000+i%1000)
		}
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			buf := make([]string, n)
			for range b.N {
				copy(buf, addrs)
				sort.Slice(buf, func(i, j int) bool { return compareAddr(buf[i], buf[j]) })
			}
		})
	}
}
