package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/gateway"
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
	_, _ = r.Write([]byte("line1\n"))
	_, _ = r.Write([]byte("line2\n"))
	lines := r.Lines()
	if len(lines) != 2 || lines[0] != "line1" || lines[1] != "line2" {
		t.Errorf("got %v, want [line1 line2]", lines)
	}

	// Fill and wrap around
	_, _ = r.Write([]byte("line3\n"))
	_, _ = r.Write([]byte("line4\n"))
	lines = r.Lines()
	if len(lines) != 3 || lines[0] != "line2" || lines[1] != "line3" || lines[2] != "line4" {
		t.Errorf("after wrap: got %v, want [line2 line3 line4]", lines)
	}
}

func TestLogRing_MultiLineWrite(t *testing.T) {
	r := newLogRing(5)
	_, _ = r.Write([]byte("a\nb\nc\n"))
	lines := r.Lines()
	if len(lines) != 3 || lines[0] != "a" || lines[1] != "b" || lines[2] != "c" {
		t.Errorf("got %v, want [a b c]", lines)
	}
}

func TestLogRing_LinesIsACopy(t *testing.T) {
	r := newLogRing(3)
	_, _ = r.Write([]byte("x\n"))
	lines := r.Lines()
	lines[0] = "mutated"
	if r.Lines()[0] == "mutated" {
		t.Error("Lines() returned a reference, not a copy")
	}
}

func TestLogRing_PlainLines(t *testing.T) {
	r := newLogRing(3)

	// Write lines with ANSI color codes.
	_, _ = r.Write([]byte("\033[31mred\033[0m\n"))
	_, _ = r.Write([]byte("plain line\n"))
	_, _ = r.Write([]byte("\033[1m\033[36mbold cyan\033[0m\n"))

	raw := r.Lines()
	plain := r.PlainLines()

	if len(raw) != 3 || len(plain) != 3 {
		t.Fatalf("got %d raw, %d plain, want 3 each", len(raw), len(plain))
	}

	// Raw lines retain ANSI codes.
	if !strings.Contains(raw[0], "\033[31m") {
		t.Errorf("raw[0] should contain ANSI codes, got %q", raw[0])
	}

	// Plain lines have ANSI codes stripped.
	wantPlain := []string{"red", "plain line", "bold cyan"}
	for i, want := range wantPlain {
		if plain[i] != want {
			t.Errorf("plain[%d] = %q, want %q", i, plain[i], want)
		}
	}

	// Verify wrap: write enough to overflow and check both arrays stay in sync.
	_, _ = r.Write([]byte("\033[33myellow\033[0m\n"))
	raw = r.Lines()
	plain = r.PlainLines()
	if len(raw) != 3 || len(plain) != 3 {
		t.Fatalf("after wrap: got %d raw, %d plain, want 3 each", len(raw), len(plain))
	}
	if plain[0] != "plain line" || plain[1] != "bold cyan" || plain[2] != "yellow" {
		t.Errorf("after wrap: plain = %v, want [plain line, bold cyan, yellow]", plain)
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no escapes", "hello world", "hello world"},
		{"empty string", "", ""},
		{"color code", "\033[31mred\033[0m", "red"},
		{"bold + color", "\033[1m\033[36mbold\033[0m", "bold"},
		{"blink", "\033[5mblink\033[0m", "blink"},
		{"multiple codes", "\033[31mA\033[32mB\033[0mC", "ABC"},
		{"unicode preserved", "\033[31m🟢 ok\033[0m", "🟢 ok"},
		{"cursor movement", "\033[2Jcleared", "cleared"},
		{"no final byte", "\033[", "\033["},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripANSI(tt.input)
			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRuneWidth(t *testing.T) {
	tests := []struct {
		name    string
		r       rune
		wantNon int // width when eastAsianWidth=false
		wantEA  int // width when eastAsianWidth=true
	}{
		{"ASCII", 'A', 1, 1},
		{"emoji", 0x1F7E2, 2, 2},        // 🟢
		{"CJK ideograph", 0x4E2D, 2, 2}, // 中
		{"circle white", '○', 1, 2},     // U+25CB Ambiguous
		{"circle black", '●', 1, 2},     // U+25CF Ambiguous
		{"bullseye", '◎', 1, 2},         // U+25CE Ambiguous
		{"arrow up-down", '↕', 1, 2},    // U+2195 Ambiguous
		{"VS16", 0xFE0F, 0, 0},
		{"VS15", 0xFE0E, 0, 0},
		{"medium white circle", 0x26AA, 2, 2}, // ⚪ Wide (W), always 2
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := eastAsianWidth

			eastAsianWidth = false
			if got := runeWidth(tt.r); got != tt.wantNon {
				t.Errorf("runeWidth(%U) [non-EA] = %d, want %d", tt.r, got, tt.wantNon)
			}

			eastAsianWidth = true
			if got := runeWidth(tt.r); got != tt.wantEA {
				t.Errorf("runeWidth(%U) [EA] = %d, want %d", tt.r, got, tt.wantEA)
			}

			eastAsianWidth = old
		})
	}
}

func TestVisibleLen(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"plain ASCII", "hello", 5},
		{"empty", "", 0},
		{"with color", "\033[31mred\033[0m", 3},
		{"emoji double-width", "🟢", 2},
		{"emoji with color", "\033[32m🟢\033[0m ok", 5}, // 2(emoji) + 1(space) + 1(o) + 1(k)
		{"multiple escapes", "\033[1m\033[36mAB\033[0m", 2},
		{"no content", "\033[31m\033[0m", 0},
		{"multibyte non-emoji", "café", 4}, // 'é' is 2 bytes but 1 column
		{"CJK ideograph", "中", 2},
		{"emoji with VS16", "⚪\uFE0F", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := visibleLen(tt.input)
			if got != tt.want {
				t.Errorf("visibleLen(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestRenderStatus_Empty(t *testing.T) {
	cfg := &config.Config{}
	output, _ := renderStatus(cfg, nil, nil, "testnode")
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
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
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

func TestRenderStatus_WithGateway(t *testing.T) {
	cfg := &config.Config{
		Gateway: []gateway.GatewayCfg{
			{Name: "claude-audit", Bind: "127.0.0.1:3459", Upstream: "https://api.anthropic.com", ClientAPI: gateway.APIAnthropic, UpstreamAPI: gateway.APIAnthropic},
			{Name: "oneapi-bridge", Bind: "127.0.0.1:3457", Upstream: "https://oneapi.example.com/v1/chat/completions", ClientAPI: gateway.APIAnthropic, UpstreamAPI: gateway.APIOpenAI},
		},
	}
	activeState := map[string]state.Component{
		"gateway:claude-audit":  {Type: "gateway", ID: "claude-audit", Status: state.Listening, BoundAddr: "127.0.0.1:3459"},
		"gateway:oneapi-bridge": {Type: "gateway", ID: "oneapi-bridge", Status: state.Failed, Message: "listen 127.0.0.1:3457: address already in use"},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	for _, want := range []string{"gateway", "claude-audit", "3459", "oneapi-bridge", "3457", "a2a", "a2o", "listening", "failed"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, output)
		}
	}
	if !strings.Contains(output, "https://api.anthropic.com") {
		t.Errorf("output missing upstream URL\n--- output ---\n%s", output)
	}
	// Verify column alignment: direction tags and bind addresses must start at the same column.
	var dirCols, bindCols []int
	for _, line := range strings.Split(output, "\n") {
		plain := stripANSI(line)
		for _, dir := range []string{" a2a ", " a2o ", " o2a ", " o2o "} {
			if idx := strings.Index(plain, dir); idx >= 0 {
				dirCols = append(dirCols, idx)
				// Bind address follows direction: "a2o 127.0.0.1:..."
				addrStart := idx + len(dir)
				bindCols = append(bindCols, addrStart)
			}
		}
	}
	if len(dirCols) >= 2 {
		for i := 1; i < len(dirCols); i++ {
			if dirCols[i] != dirCols[0] {
				t.Errorf("gateway direction columns misaligned: %v\n--- output ---\n%s", dirCols, output)
				break
			}
		}
	}
	if len(bindCols) >= 2 {
		for i := 1; i < len(bindCols); i++ {
			if bindCols[i] != bindCols[0] {
				t.Errorf("gateway bind address columns misaligned: %v\n--- output ---\n%s", bindCols, output)
				break
			}
		}
	}
}

func TestRenderStatus_GatewayEmptyAPIKey(t *testing.T) {
	cfg := &config.Config{
		Gateway: []gateway.GatewayCfg{
			{Name: "test-gw", Bind: "127.0.0.1:3457", Upstream: "http://upstream:4000/v1/chat/completions", ClientAPI: gateway.APIAnthropic, UpstreamAPI: gateway.APIOpenAI},
		},
	}
	activeState := map[string]state.Component{
		"gateway:test-gw": {Type: "gateway", ID: "test-gw", Status: state.Listening, BoundAddr: "127.0.0.1:3457", Message: "MY_API_KEY is empty"},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	if !strings.Contains(output, "MY_API_KEY is empty") {
		t.Errorf("output should show API key warning\n--- output ---\n%s", output)
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
	output, _ := renderStatus(cfg, nil, nil, "testnode")
	if !strings.Contains(output, "remote") {
		t.Error("output should contain connection name")
	}
	if !strings.Contains(output, "8080") {
		t.Error("output should contain forward bind port")
	}
}

func TestHumanLogHandler(t *testing.T) {
	var buf bytes.Buffer
	textHandler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Strip time and level for test predictability
			if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
				return slog.Attr{}
			}
			return a
		},
	})
	logger := slog.New(&humanLogHandler{Handler: textHandler})

	tests := []struct {
		name    string
		msg     string
		attrs   []slog.Attr
		want    string // substring that must be present in the output message
		wantNot string // substring that must NOT be in the message attr (key consumed)
	}{
		{
			"target inlined",
			"Connected",
			[]slog.Attr{slog.String("target", "root@10.0.0.1:22")},
			"Connected root@10.0.0.1:22",
			"target=",
		},
		{
			"timing details parenthesized",
			"Connected",
			[]slog.Attr{
				slog.String("target", "host:22"),
				slog.String("tcp", "45ms"),
				slog.String("ssh", "120ms"),
			},
			"(tcp: 45ms, ssh: 120ms)",
			"",
		},
		{
			"error appended with colon",
			"SSH handshake failed",
			[]slog.Attr{
				slog.String("target", "host:22"),
				slog.String("error", "connection refused"),
			},
			": connection refused",
			"",
		},
		{
			"unknown attrs pass through",
			"Something",
			[]slog.Attr{slog.String("custom_key", "custom_val")},
			"custom_key=custom_val",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			logger.LogAttrs(context.TODO(), slog.LevelInfo, tt.msg, tt.attrs...)
			output := buf.String()
			if !strings.Contains(output, tt.want) {
				t.Errorf("output %q does not contain %q", output, tt.want)
			}
			if tt.wantNot != "" && strings.Contains(output, tt.wantNot) {
				t.Errorf("output %q should not contain %q (key should be consumed)", output, tt.wantNot)
			}
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		budget int
		want   string
	}{
		{"shorter than budget", "connection refused", 60, "connection refused"},
		{"exactly budget", "abcdefghij", 10, "abcdefghij"},
		{"longer than budget", "dial tcp 10.0.0.1:22: i/o timeout after multiple retries", 20, "dial tcp 10.0.0.1:2…"},
		{"budget of 1 returns input", "anything", 1, "anything"},
		{"multibyte clipped safely", "hëllo world", 5, "hëll…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateMessage(tt.input, tt.budget); got != tt.want {
				t.Errorf("truncateMessage(%q, %d) = %q, want %q", tt.input, tt.budget, got, tt.want)
			}
		})
	}
}

func TestRenderStatus_LongErrorMessagesTruncated(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
		},
	}
	longErr := strings.Repeat("dial tcp 10.0.0.1:22: i/o timeout ", 10)
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Failed, Message: longErr},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	plain := stripANSI(output)

	if strings.Contains(plain, longErr) {
		t.Errorf("long error message should have been truncated, but appeared in full:\n%s", plain)
	}
	if !strings.Contains(plain, "…") {
		t.Errorf("truncated message should include ellipsis:\n%s", plain)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0"},
		{1, "1B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1K"},
		{1536, "2K"},
		{10240, "10K"},
		{1048576, "1.0M"},
		{1572864, "1.5M"},
		{10485760, "10.0M"},
		{1073741824, "1.0G"},
		{1610612736, "1.5G"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.input), func(t *testing.T) {
			got := formatBytes(tt.input)
			if got != tt.want {
				t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{0, "0s"},
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m0s"},
		{90 * time.Second, "1m30s"},
		{5*time.Minute + 15*time.Second, "5m15s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{60 * time.Minute, "1h0m"},
		{2*time.Hour + 13*time.Minute, "2h13m"},
		{23*time.Hour + 59*time.Minute, "23h59m"},
		{24 * time.Hour, "1d0h"},
		{3*24*time.Hour + 5*time.Hour, "3d5h"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatDuration(tt.input)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRenderStatus_TargetSymbols(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22", "root@10.0.0.2:22"},
				Forwards: []config.ForwardSet{
					{Name: "fwd"},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "root@10.0.0.1:22",
		},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	// Connected target should have green ● (with ANSI)
	if !strings.Contains(output, "●") {
		t.Error("connected target should show ● symbol")
	}
	// Disconnected target should have ○
	if !strings.Contains(output, "○") {
		t.Error("disconnected target should show ○ symbol")
	}
}

func TestRenderStatus_ForwardSetPeerSymbol(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{Name: "fwd"},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "root@10.0.0.1:22",
			PeerAddr: "10.0.0.1:22",
		},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	// The forward set's peer line should also have ● (not →)
	// Count ● occurrences — should be at least 2 (target line + peer line)
	count := strings.Count(output, "●")
	if count < 2 {
		t.Errorf("expected at least 2 ● symbols (target + peer), got %d", count)
	}
}

func TestRenderStatus_PeerAddrHidden_WhenSameAsTarget(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{Name: "fwd"},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "root@10.0.0.1:22",
			PeerAddr: "10.0.0.1:22",
		},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	// PeerAddr "10.0.0.1:22" is contained in target "root@10.0.0.1:22", so should NOT show in parens
	if strings.Contains(output, "(10.0.0.1:22)") {
		t.Error("peer address should be hidden when contained in target string")
	}
}

func TestRenderStatus_PeerAddrShown_WhenDifferent(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@myhost.example.com:22"},
				Forwards: []config.ForwardSet{
					{Name: "fwd"},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "root@myhost.example.com:22",
			PeerAddr: "93.184.216.34:22",
		},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	if !strings.Contains(output, "93.184.216.34") {
		t.Error("peer address should be shown when different from target")
	}
}

func TestRenderStatus_WithMetrics(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{
						Name: "fwd",
						Local: []config.Forward{
							{Type: "forward", Bind: "127.0.0.1:8080", Target: "10.0.0.1:80"},
						},
					},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "root@10.0.0.1:22",
		},
	}
	m := &state.Metrics{}
	m.StartTime.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	m.BytesTx.Store(1048576) // 1MB
	m.BytesRx.Store(2097152) // 2MB
	m.Streams.Store(3)
	metricsMap := map[string]*state.Metrics{
		"forward:tunnel [fwd] 127.0.0.1:8080": m,
	}
	rawOutput, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	output := stripANSI(rawOutput)
	if !strings.Contains(output, "↑1.0M") {
		t.Error("output should contain ↑1.0M")
	}
	if !strings.Contains(output, "↓2.0M") {
		t.Error("output should contain ↓2.0M")
	}
	if !strings.Contains(output, "3↔") {
		t.Error("output should contain 3↔")
	}
	if !strings.Contains(output, "2h") {
		t.Error("output should contain 2h uptime")
	}
}

func TestRenderStatus_AlwaysShowsTargetLine(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{Name: "fwd"},
				},
			},
		},
	}

	tests := []struct {
		name    string
		state   map[string]state.Component
		wantSub string
	}{
		{
			"starting shows starting label",
			map[string]state.Component{
				"connection:tunnel [fwd]": {Status: state.Starting},
			},
			"[starting]",
		},
		{
			"connecting shows connecting label",
			map[string]state.Component{
				"connection:tunnel [fwd]": {Status: state.Connecting},
			},
			"[connecting]",
		},
		{
			"retrying shows error message",
			map[string]state.Component{
				"connection:tunnel [fwd]": {Status: state.Retrying, Message: "no reachable target"},
			},
			"no reachable target",
		},
		{
			"failed shows error",
			map[string]state.Component{
				"connection:tunnel [fwd]": {Status: state.Failed, Message: "auth failed"},
			},
			"auth failed",
		},
		{
			"connected shows target",
			map[string]state.Component{
				"connection:tunnel [fwd]": {Status: state.Connected, Message: "root@10.0.0.1:22"},
			},
			"10.0.0.1",
		},
		{
			"nil state shows starting",
			nil,
			"[starting]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, _ := renderStatus(cfg, tt.state, nil, "testnode")
			if !strings.Contains(output, tt.wantSub) {
				t.Errorf("output should contain %q, got:\n%s", tt.wantSub, output)
			}
		})
	}
}

func TestRenderStatus_ListenerMetrics(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
		},
	}
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
	}
	m := &state.Metrics{}
	m.StartTime.Store(time.Now().Add(-30 * time.Minute).UnixNano())
	m.BytesTx.Store(5242880) // 5MB
	m.BytesRx.Store(1024)    // 1K
	metricsMap := map[string]*state.Metrics{
		"proxy:127.0.0.1:1080": m,
	}
	rawOutput, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	output := stripANSI(rawOutput)
	if !strings.Contains(output, "5.0M") {
		t.Error("output should contain 5.0M TX bytes for listener")
	}
	if !strings.Contains(output, "1K") {
		t.Error("output should contain 1K RX bytes for listener")
	}
	if !strings.Contains(output, "30m") {
		t.Error("output should contain 30m uptime for listener")
	}
}

// TestRenderStatus_SshdAggregatesDynamicMetrics verifies the sshd listener row
// shows aggregated metrics from both server-level (direct-tcpip) and all dynamic
// reverse forward components.
func TestRenderStatus_SshdAggregatesDynamicMetrics(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "sshd", Bind: "0.0.0.0:2222"},
		},
	}
	activeState := map[string]state.Component{
		"server:0.0.0.0:2222": {Type: "server", ID: "0.0.0.0:2222", Status: state.Listening},
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:11111|0.0.0.0:2222",
			Status: state.Listening, Message: "root@10.0.0.1:22",
		},
		"dynamic:127.0.0.1:18384|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:18384|0.0.0.0:2222",
			Status: state.Listening, Message: "root@10.0.0.1:22",
		},
	}

	// Server metrics: 1K tx, 2K rx (from direct-tcpip)
	serverM := &state.Metrics{}
	serverM.StartTime.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	serverM.BytesTx.Store(1024)
	serverM.BytesRx.Store(2048)
	serverM.Streams.Store(1)

	// Dynamic forward 1: 10K tx, 6MB rx, 1 stream (syncthing traffic)
	dyn1M := &state.Metrics{}
	dyn1M.StartTime.Store(time.Now().Add(-50 * time.Minute).UnixNano())
	dyn1M.BytesTx.Store(10240)
	dyn1M.BytesRx.Store(6291456) // 6MB
	dyn1M.Streams.Store(1)

	// Dynamic forward 2: 0 tx/rx (idle)
	dyn2M := &state.Metrics{}
	dyn2M.StartTime.Store(time.Now().Add(-50 * time.Minute).UnixNano())

	metricsMap := map[string]*state.Metrics{
		"server:0.0.0.0:2222":                  serverM,
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": dyn1M,
		"dynamic:127.0.0.1:18384|0.0.0.0:2222": dyn2M,
	}

	rawOutput, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(rawOutput)

	// Find the sshd listener line (contains "sshd")
	var sshdLine string
	for _, line := range lines {
		if strings.Contains(line, "sshd") {
			sshdLine = line
			break
		}
	}
	if sshdLine == "" {
		t.Fatal("sshd listener line not found in output")
	}

	// Aggregated: tx = 1024 + 10240 = 11264 → "11K", rx = 2048 + 6291456 = 6293504 → "6.0M"
	// Streams: 1 + 1 + 0 = 2
	if !strings.Contains(sshdLine, "11K") {
		t.Errorf("sshd line should show aggregated ↑11K, got: %s", sshdLine)
	}
	if !strings.Contains(sshdLine, "6.0M") {
		t.Errorf("sshd line should show aggregated ↓6.0M, got: %s", sshdLine)
	}
	if !strings.Contains(sshdLine, "2↔") {
		t.Errorf("sshd line should show aggregated 2↔ streams, got: %s", sshdLine)
	}
}

// TestRenderStatus_DynamicPortRowsHaveNoMetrics verifies that dynamic reverse
// forward sub-rows under an sshd listener do not show per-row metrics. Their
// bytes roll up into the parent sshd listener row, so showing them again on
// each sub-row would duplicate the numbers and add noise. The sshd aggregation
// itself is covered by TestRenderStatus_SshdAggregatesDynamicMetrics.
func TestRenderStatus_DynamicPortRowsHaveNoMetrics(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "sshd", Bind: "0.0.0.0:2222"},
		},
	}
	activeState := map[string]state.Component{
		"server:0.0.0.0:2222": {Type: "server", ID: "0.0.0.0:2222", Status: state.Listening},
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:11111|0.0.0.0:2222",
			Status: state.Listening, Message: "root@10.0.0.1:22",
		},
		"dynamic:127.0.0.1:18384|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:18384|0.0.0.0:2222",
			Status: state.Listening, Message: "root@10.0.0.1:22",
		},
	}

	dyn1M := &state.Metrics{}
	dyn1M.StartTime.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	dyn1M.BytesTx.Store(9000)
	dyn1M.BytesRx.Store(6291456) // 6MB
	dyn1M.Streams.Store(1)

	dyn2M := &state.Metrics{}
	dyn2M.StartTime.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	dyn2M.BytesTx.Store(0)
	dyn2M.BytesRx.Store(0)

	metricsMap := map[string]*state.Metrics{
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": dyn1M,
		"dynamic:127.0.0.1:18384|0.0.0.0:2222": dyn2M,
	}

	rawOutput, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(rawOutput)

	// Find the dynamic forward lines (contain "~")
	var dyn11111Line, dyn18384Line string
	for _, line := range lines {
		if strings.Contains(line, "11111") && strings.Contains(line, "~") {
			dyn11111Line = line
		}
		if strings.Contains(line, "18384") && strings.Contains(line, "~") {
			dyn18384Line = line
		}
	}

	if dyn11111Line == "" {
		t.Fatal("dynamic 11111 line not found")
	}
	if dyn18384Line == "" {
		t.Fatal("dynamic 18384 line not found")
	}

	// Neither dynamic sub-row should carry per-row metrics markers (↑/↓/↔).
	for _, tc := range []struct {
		name, line string
	}{
		{"dyn11111", dyn11111Line},
		{"dyn18384", dyn18384Line},
	} {
		for _, marker := range []string{"↑", "↓", "↔"} {
			if strings.Contains(tc.line, marker) {
				t.Errorf("%s sub-row should not contain %q, got: %s", tc.name, marker, tc.line)
			}
		}
	}
}

// TestRenderStatus_SshdDynamicOnlyMetrics verifies sshd aggregation works when
// the server itself has no metrics (no direct-tcpip traffic) but dynamic
// forwards have traffic.
func TestRenderStatus_SshdDynamicOnlyMetrics(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "sshd", Bind: "0.0.0.0:2222"},
		},
	}
	activeState := map[string]state.Component{
		"server:0.0.0.0:2222": {Type: "server", ID: "0.0.0.0:2222", Status: state.Listening},
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:11111|0.0.0.0:2222",
			Status: state.Listening, Message: "root@10.0.0.1:22",
		},
	}

	// No server metrics at all — only dynamic
	dynM := &state.Metrics{}
	dynM.StartTime.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	dynM.BytesTx.Store(50000)
	dynM.BytesRx.Store(100000)
	dynM.Streams.Store(2)

	metricsMap := map[string]*state.Metrics{
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": dynM,
	}

	rawOutput, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(rawOutput)

	var sshdLine string
	for _, line := range lines {
		if strings.Contains(line, "sshd") {
			sshdLine = line
			break
		}
	}
	if sshdLine == "" {
		t.Fatal("sshd listener line not found")
	}

	// sshd should show the dynamic forward's metrics: ↑49K ↓98K 2↔
	if !strings.Contains(sshdLine, "49K") {
		t.Errorf("sshd line should show dynamic ↑49K, got: %s", sshdLine)
	}
	if !strings.Contains(sshdLine, "98K") {
		t.Errorf("sshd line should show dynamic ↓98K, got: %s", sshdLine)
	}
	if !strings.Contains(sshdLine, "2↔") {
		t.Errorf("sshd line should show dynamic 2↔ streams, got: %s", sshdLine)
	}
}

// TestRenderStatus_GrandTotalNoDoubleCounting verifies the grand total in the
// title bar doesn't double-count bytes that appear in both server and dynamic
// metrics. Server metrics should only contain direct-tcpip traffic; dynamic
// metrics should only contain reverse forward traffic.
func TestRenderStatus_GrandTotalNoDoubleCounting(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "sshd", Bind: "0.0.0.0:2222"},
		},
	}
	activeState := map[string]state.Component{
		"server:0.0.0.0:2222": {Type: "server", ID: "0.0.0.0:2222", Status: state.Listening},
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:11111|0.0.0.0:2222",
			Status: state.Listening, Message: "root@10.0.0.1:22",
		},
	}

	// Server: 1K tx, 2K rx (direct-tcpip only, no propagated bytes)
	serverM := &state.Metrics{}
	serverM.StartTime.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	serverM.BytesTx.Store(1024)
	serverM.BytesRx.Store(2048)

	// Dynamic: 10K tx, 20K rx (reverse forward traffic)
	dynM := &state.Metrics{}
	dynM.StartTime.Store(time.Now().Add(-30 * time.Minute).UnixNano())
	dynM.BytesTx.Store(10240)
	dynM.BytesRx.Store(20480)

	metricsMap := map[string]*state.Metrics{
		"server:0.0.0.0:2222":                  serverM,
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": dynM,
	}

	rawOutput, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	output := stripANSI(rawOutput)

	// Grand total: tx = 1024 + 10240 = 11264 → "11K", rx = 2048 + 20480 = 22528 → "22K"
	// The title line has the grand total
	lines := extractLines(rawOutput)
	if len(lines) == 0 {
		t.Fatal("no output lines")
	}
	titleLine := lines[0]

	if !strings.Contains(titleLine, "11K") {
		t.Errorf("grand total should show ↑11K (1K server + 10K dynamic), got title: %s", titleLine)
	}
	if !strings.Contains(titleLine, "22K") {
		t.Errorf("grand total should show ↓22K (2K server + 20K dynamic), got title: %s", titleLine)
	}

	// Verify it does NOT show doubled values (↑21K or ↓42K would indicate double-counting)
	if strings.Contains(output, "↑21K") || strings.Contains(output, "↑20K") {
		t.Error("grand total appears to double-count TX bytes")
	}
	if strings.Contains(output, "↓42K") || strings.Contains(output, "↓40K") {
		t.Error("grand total appears to double-count RX bytes")
	}
}

// TestRenderStatus_OnlyProducerRowsShowMetrics pins the contract that per-row
// metrics (↑/↓/↔) are rendered only on rows that directly produce traffic —
// listeners (including sshd, which rolls up its dynamic reverse forwards) and
// individual forward rows. Grouping headers (connection name, forward-set
// name) and dynamic sub-rows must stay clean so that bytes are not duplicated
// on the dashboard.
func TestRenderStatus_OnlyProducerRowsShowMetrics(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "sshd", Bind: "0.0.0.0:2222"},
		},
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{
						Name: "fwd",
						Local: []config.Forward{
							{Type: "forward", Bind: "127.0.0.1:8080", Target: "10.0.0.1:80"},
							{Type: "forward", Bind: "127.0.0.1:8081", Target: "10.0.0.1:81"},
						},
					},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"server:0.0.0.0:2222": {Type: "server", ID: "0.0.0.0:2222", Status: state.Listening},
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:11111|0.0.0.0:2222",
			Status: state.Listening, Message: "root@10.0.0.1:22",
		},
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "root@10.0.0.1:22",
		},
	}

	// Dynamic reverse forward under sshd: 5K tx, 10K rx, 1 stream.
	dynM := &state.Metrics{}
	dynM.StartTime.Store(time.Now().Add(-20 * time.Minute).UnixNano())
	dynM.BytesTx.Store(5120)
	dynM.BytesRx.Store(10240)
	dynM.Streams.Store(1)

	// Local forward 8080: 3K tx, 7K rx, 2 streams.
	fwd1M := &state.Metrics{}
	fwd1M.StartTime.Store(time.Now().Add(-20 * time.Minute).UnixNano())
	fwd1M.BytesTx.Store(3072)
	fwd1M.BytesRx.Store(7168)
	fwd1M.Streams.Store(2)

	// Local forward 8081: 2K tx, 3K rx, 1 stream.
	fwd2M := &state.Metrics{}
	fwd2M.StartTime.Store(time.Now().Add(-20 * time.Minute).UnixNano())
	fwd2M.BytesTx.Store(2048)
	fwd2M.BytesRx.Store(3072)
	fwd2M.Streams.Store(1)

	metricsMap := map[string]*state.Metrics{
		"dynamic:127.0.0.1:11111|0.0.0.0:2222": dynM,
		"forward:tunnel [fwd] 127.0.0.1:8080":  fwd1M,
		"forward:tunnel [fwd] 127.0.0.1:8081":  fwd2M,
	}

	rawOutput, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(rawOutput)

	var (
		sshdLine     string
		dynLine      string
		connNameLine string
		fsetNameLine string
		fwd8080Line  string
		fwd8081Line  string
	)
	for _, line := range lines {
		switch {
		case strings.Contains(line, "sshd"):
			sshdLine = line
		case strings.Contains(line, "11111") && strings.Contains(line, "~"):
			dynLine = line
		case strings.Contains(line, "tunnel") && !strings.Contains(line, "fwd") && !strings.Contains(line, "10.0.0.1"):
			connNameLine = line
		case strings.Contains(line, "fwd") && !strings.Contains(line, "127.0.0.1") && !strings.Contains(line, "10.0.0.1"):
			fsetNameLine = line
		case strings.Contains(line, "127.0.0.1:8080"):
			fwd8080Line = line
		case strings.Contains(line, "127.0.0.1:8081"):
			fwd8081Line = line
		}
	}

	// Producer rows must carry metrics. formatBytes uses %.0fK for KiB, so
	// 3072 → "3K", 10240 → "10K".
	producers := []struct {
		name, line string
		wantTx     string
		wantRx     string
		wantStream string
	}{
		// sshd listener aggregates the dynamic reverse forward under it.
		{"sshd listener", sshdLine, "↑5K", "↓10K", "1↔"},
		{"forward 8080", fwd8080Line, "↑3K", "↓7K", "2↔"},
		{"forward 8081", fwd8081Line, "↑2K", "↓3K", "1↔"},
	}
	for _, p := range producers {
		if p.line == "" {
			t.Fatalf("%s line not found in output", p.name)
		}
		for _, want := range []string{p.wantTx, p.wantRx, p.wantStream} {
			if !strings.Contains(p.line, want) {
				t.Errorf("%s should contain %q, got: %s", p.name, want, p.line)
			}
		}
	}

	// Non-producer (grouping / rolled-up) rows must NOT carry per-row metrics.
	nonProducers := []struct {
		name, line string
	}{
		{"connection name", connNameLine},
		{"forward-set name", fsetNameLine},
		{"dynamic sub-row", dynLine},
	}
	for _, np := range nonProducers {
		if np.line == "" {
			t.Fatalf("%s line not found in output", np.name)
		}
		for _, marker := range []string{"↑", "↓", "↔"} {
			if strings.Contains(np.line, marker) {
				t.Errorf("%s should not contain %q, got: %s", np.name, marker, np.line)
			}
		}
	}
}

// TestMetricsSnapshot_Add verifies aggregation arithmetic including streams.
func TestMetricsSnapshot_Add(t *testing.T) {
	a := metricsSnapshot{uptime: 5 * time.Minute, tx: 100, rx: 200, streams: 2}
	b := metricsSnapshot{uptime: 10 * time.Minute, tx: 300, rx: 400, streams: 3}
	a.add(b)
	if a.tx != 400 {
		t.Errorf("tx = %d, want 400", a.tx)
	}
	if a.rx != 600 {
		t.Errorf("rx = %d, want 600", a.rx)
	}
	if a.streams != 5 {
		t.Errorf("streams = %d, want 5", a.streams)
	}
	if a.uptime != 10*time.Minute {
		t.Errorf("uptime = %v, want 10m", a.uptime)
	}
}

// TestReadMetrics_NilSafe verifies readMetrics handles nil without panic.
func TestReadMetrics_NilSafe(t *testing.T) {
	snap := readMetrics(nil)
	if snap.tx != 0 || snap.rx != 0 || snap.streams != 0 || snap.uptime != 0 || snap.tokensIn != 0 || snap.tokensOut != 0 {
		t.Error("readMetrics(nil) should return zero snapshot")
	}
}

// TestReadMetrics_TokensSurfaced verifies token atomics are read into the snapshot.
func TestReadMetrics_TokensSurfaced(t *testing.T) {
	m := &state.Metrics{}
	m.TokensIn.Store(123)
	m.TokensOut.Store(456)
	snap := readMetrics(m)
	if snap.tokensIn != 123 || snap.tokensOut != 456 {
		t.Errorf("tokens = (%d, %d), want (123, 456)", snap.tokensIn, snap.tokensOut)
	}
}

// TestFormatMetricsSnap_RendersTokensWhenNonZero verifies the gateway-only
// token suffix appears in the rendered string.
func TestFormatMetricsSnap_RendersTokensWhenNonZero(t *testing.T) {
	snap := metricsSnapshot{uptime: time.Minute, tx: 100, rx: 200, tokensIn: 1500, tokensOut: 700}
	out := stripANSI(formatMetricsSnap(snap))
	for _, want := range []string{"tok", "↑1.5k", "↓700"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}
	// Non-gateway snapshot (tokens both zero) must not include "tok".
	bare := metricsSnapshot{uptime: time.Minute, tx: 1, rx: 2}
	if strings.Contains(stripANSI(formatMetricsSnap(bare)), "tok") {
		t.Errorf("non-gateway snapshot should not show token suffix")
	}
}

// TestFormatTokens_Compact verifies the human-readable token count formatter.
func TestFormatTokens_Compact(t *testing.T) {
	tests := map[int64]string{
		0:         "0",
		999:       "999",
		1500:      "1.5k",
		12345:     "12.3k",
		1_000_000: "1.0M",
	}
	for n, want := range tests {
		if got := formatTokens(n); got != want {
			t.Errorf("formatTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestReadMetrics_ReadsAtomics verifies readMetrics reads all atomic fields correctly.
func TestReadMetrics_ReadsAtomics(t *testing.T) {
	m := &state.Metrics{}
	m.BytesTx.Store(1000)
	m.BytesRx.Store(2000)
	m.Streams.Store(5)
	m.StartTime.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	snap := readMetrics(m)
	if snap.tx != 1000 {
		t.Errorf("tx = %d, want 1000", snap.tx)
	}
	if snap.rx != 2000 {
		t.Errorf("rx = %d, want 2000", snap.rx)
	}
	if snap.streams != 5 {
		t.Errorf("streams = %d, want 5", snap.streams)
	}
	if snap.uptime < 59*time.Minute || snap.uptime > 61*time.Minute {
		t.Errorf("uptime = %v, want ~1h", snap.uptime)
	}
}

// TestFormatMetricsSnap_StreamsWithZeroBytes verifies streams display even when
// bytes are zero (as long as uptime > 0).
func TestFormatMetricsSnap_StreamsWithZeroBytes(t *testing.T) {
	snap := metricsSnapshot{
		uptime:  1 * time.Minute,
		tx:      0,
		rx:      0,
		streams: 3,
	}
	output := stripANSI(formatMetricsSnap(snap))
	if !strings.Contains(output, "3↔") {
		t.Errorf("should show 3↔ streams even with zero bytes, got: %q", output)
	}
}

// TestFormatMetricsSnap_ZeroUptimeWithStreams verifies that non-zero streams
// alone are NOT enough to produce output (uptime or bytes required).
func TestFormatMetricsSnap_ZeroUptimeWithStreams(t *testing.T) {
	snap := metricsSnapshot{
		uptime:  0,
		tx:      0,
		rx:      0,
		streams: 3,
	}
	output := formatMetricsSnap(snap)
	if output != "" {
		t.Errorf("zero uptime + zero bytes should produce empty string even with streams, got: %q", stripANSI(output))
	}
}

func TestRenderStatus_DynamicPortNodeName(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "sshd", Bind: "0.0.0.0:2222"},
		},
	}
	activeState := map[string]state.Component{
		"server:0.0.0.0:2222": {Type: "server", ID: "0.0.0.0:2222", Status: state.Listening},
		"dynamic:127.0.0.1:9999|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:9999|0.0.0.0:2222",
			Status: state.Listening, Message: "root@127.0.0.1:54321",
			PeerAddr: "client/tunnel/fwd",
		},
	}
	rawOutput, _ := renderStatus(cfg, activeState, nil, "testnode")
	output := stripANSI(rawOutput)
	if !strings.Contains(output, "(client/tunnel/fwd)") {
		t.Error("dynamic port should show mesh node identity in parentheses")
	}
	if !strings.Contains(output, "root@") {
		t.Error("dynamic port should show SSH client address")
	}
}

// findColumn returns the visual column of the first occurrence of substr in
// the ANSI-stripped version of line, accounting for wide characters (emoji).
// Returns -1 if not found.
func findColumn(line, substr string) int {
	stripped := stripANSI(line)
	idx := strings.Index(stripped, substr)
	if idx < 0 {
		return -1
	}
	// Count visual columns up to the byte offset
	col := 0
	for _, r := range stripped[:idx] {
		if r >= 0x1F000 {
			col += 2
		} else {
			col++
		}
	}
	return col
}

func TestFindColumn_EmojiWidth(t *testing.T) {
	// 🟢 is 4 bytes but 2 visual columns
	line := "🟢 hello ↑world"
	col := findColumn(line, "↑")
	// 🟢(2) + space(1) + hello(5) + space(1) = 9
	if col != 9 {
		t.Errorf("findColumn with emoji: got %d, want 9", col)
	}

	// No emoji
	line2 := "abc ↑def"
	col2 := findColumn(line2, "↑")
	if col2 != 4 {
		t.Errorf("findColumn without emoji: got %d, want 4", col2)
	}

	// With ANSI codes
	line3 := "\033[32m🟢\033[0m hello \033[90m↑world\033[0m"
	col3 := findColumn(line3, "↑")
	if col3 != 9 {
		t.Errorf("findColumn with ANSI+emoji: got %d, want 9", col3)
	}

	// Verify byte offset vs visual column difference
	stripped := stripANSI("🟢 test ↑x")
	byteIdx := strings.Index(stripped, "↑")
	visCol := findColumn("🟢 test ↑x", "↑")
	// byteIdx should be 11 (🟢=4, ' '=1, 'test'=4, ' '=1, ↑=here)
	// visCol should be 8 (🟢=2, ' '=1, 'test'=4, ' '=1)
	t.Logf("byteIdx=%d visCol=%d", byteIdx, visCol)
	if byteIdx != 10 {
		t.Errorf("byte index: got %d, want 10", byteIdx)
	}
	if visCol != 8 {
		t.Errorf("visual column: got %d, want 8", visCol)
	}
}

// extractLines returns ANSI-stripped, non-empty lines from rendered output.
func extractLines(output string) []string {
	var result []string
	for _, line := range strings.Split(output, "\n") {
		stripped := strings.TrimRight(stripANSI(line), " ")
		if stripped != "" {
			result = append(result, stripped)
		}
	}
	return result
}

func TestAlignment_ListenerStatusColumn(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
			{Type: "http", Bind: "127.0.0.1:3128"},
			{Type: "sshd", Bind: "0.0.0.0:2222"},
		},
	}
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
		"proxy:127.0.0.1:3128": {Type: "proxy", ID: "127.0.0.1:3128", Status: state.Listening},
		"server:0.0.0.0:2222":  {Type: "server", ID: "0.0.0.0:2222", Status: state.Listening},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	lines := extractLines(output)

	// Expect all 3 listener lines to have identical format with aligned [listening]
	//   🟢 127.0.0.1:1080 socks [listening]
	//   🟢 127.0.0.1:3128 http  [listening]
	//   🟢 0.0.0.0:2222   sshd  [listening]
	// The [listening] column must be identical (27 in this layout)
	wantCol := 24
	for _, line := range lines {
		col := findColumn(line, "[listening]")
		if col >= 0 && col != wantCol {
			t.Errorf("[listening] at col %d, want %d in: %s", col, wantCol, line)
		}
	}
}

func TestAlignment_MetricsColumnSameAcrossRows(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
			{Type: "http", Bind: "127.0.0.1:3128"},
		},
	}
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
		"proxy:127.0.0.1:3128": {Type: "proxy", ID: "127.0.0.1:3128", Status: state.Listening},
	}
	m1 := &state.Metrics{}
	m1.StartTime.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	m1.BytesTx.Store(1048576) // "1.0M"
	m2 := &state.Metrics{}
	m2.StartTime.Store(time.Now().Add(-30 * time.Minute).UnixNano())
	m2.BytesTx.Store(1024) // "1K"
	metricsMap := map[string]*state.Metrics{
		"proxy:127.0.0.1:1080": m1,
		"proxy:127.0.0.1:3128": m2,
	}
	output, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	wantStatusCol := 24
	wantMetricsCol := 43
	for _, line := range lines {
		if !strings.Contains(line, "[listening]") {
			continue
		}
		statusCol := findColumn(line, "[listening]")
		if statusCol != wantStatusCol {
			t.Errorf("[listening] at col %d, want %d in: %s", statusCol, wantStatusCol, line)
		}
		metricsCol := findColumn(line, "↑")
		if metricsCol >= 0 && metricsCol != wantMetricsCol {
			t.Errorf("↑ at col %d, want %d in: %s", metricsCol, wantMetricsCol, line)
		}
	}
}

func TestAlignment_DownloadColumnAligned(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
			{Type: "http", Bind: "127.0.0.1:3128"},
		},
	}
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
		"proxy:127.0.0.1:3128": {Type: "proxy", ID: "127.0.0.1:3128", Status: state.Listening},
	}
	m1 := &state.Metrics{}
	m1.StartTime.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	m1.BytesTx.Store(1048576) // "1.0M" (4 chars)
	m1.BytesRx.Store(512)     // "512B" (4 chars)
	m2 := &state.Metrics{}
	m2.StartTime.Store(time.Now().Add(-30 * time.Minute).UnixNano())
	m2.BytesTx.Store(1024)     // "1K"   (2 chars)
	m2.BytesRx.Store(10485760) // "10.0M" (5 chars)
	metricsMap := map[string]*state.Metrics{
		"proxy:127.0.0.1:1080": m1,
		"proxy:127.0.0.1:3128": m2,
	}
	output, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	// Both rows should have ↓ at the same column despite different ↑ value widths
	var downCols []int
	for _, line := range lines {
		if !strings.Contains(line, "[listening]") {
			continue
		}
		col := findColumn(line, "↓")
		if col >= 0 {
			downCols = append(downCols, col)
		}
	}
	if len(downCols) < 2 {
		t.Fatalf("expected 2 lines with ↓, got %d", len(downCols))
	}
	if downCols[0] != downCols[1] {
		t.Errorf("↓ columns not aligned: %d vs %d", downCols[0], downCols[1])
	}
}

func TestAlignment_ArrowsAtSameColumn(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{
						Name: "fwd",
						Local: []config.Forward{
							{Type: "forward", Bind: "127.0.0.1:80", Target: "10.0.0.1:80"},
							{Type: "forward", Bind: "127.0.0.1:8443", Target: "10.0.0.1:443"},
						},
					},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "root@10.0.0.1:22",
		},
	}
	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	lines := extractLines(output)

	wantCol := 18
	for _, line := range lines {
		col := findColumn(line, "──▶")
		if col >= 0 && col != wantCol {
			t.Errorf("──▶ at col %d, want %d in: %s", col, wantCol, line)
		}
	}
}

func TestAlignment_MixedDirectionMetrics(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{
						Name: "fwd",
						Local: []config.Forward{
							{Type: "forward", Bind: "127.0.0.1:8080", Target: "10.0.0.1:80"},
						},
						Remote: []config.Forward{
							{Type: "forward", Bind: "10.0.0.1:2222", Target: "127.0.0.1:22"},
						},
					},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "root@10.0.0.1:22",
		},
	}
	m := &state.Metrics{}
	m.StartTime.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	m.BytesTx.Store(100000)
	metricsMap := map[string]*state.Metrics{
		"forward:tunnel [fwd] 127.0.0.1:8080": m,
		"forward:tunnel [fwd] 10.0.0.1:2222":  m,
	}
	output, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	// Both forward lines (──▶ and ◀──) should have ↑ at the same column.
	var upCols []int
	for _, line := range lines {
		if !strings.Contains(line, "──▶") && !strings.Contains(line, "◀──") {
			continue
		}
		col := findColumn(line, "↑")
		if col >= 0 {
			upCols = append(upCols, col)
		}
	}
	if len(upCols) < 2 {
		t.Fatalf("expected 2 forward lines with ↑, got %d", len(upCols))
	}
	if upCols[0] != upCols[1] {
		t.Errorf("↑ columns not aligned across directions: %d vs %d", upCols[0], upCols[1])
	}
}

func TestAlignment_StatusBeforeMetrics(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
		},
	}
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
	}
	m := &state.Metrics{}
	m.StartTime.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	m.BytesTx.Store(1024)
	metricsMap := map[string]*state.Metrics{
		"proxy:127.0.0.1:1080": m,
	}
	output, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	for _, line := range lines {
		if !strings.Contains(line, "[listening]") {
			continue
		}
		// [listening] must end before ↑ starts, with at least 1 space gap
		statusEnd := findColumn(line, "[listening]") + len("[listening]")
		metricsStart := findColumn(line, "↑")
		if metricsStart < 0 {
			t.Error("expected ↑ in listener line")
			continue
		}
		if metricsStart <= statusEnd {
			t.Errorf("[listening] ends at col %d but ↑ starts at col %d — should have gap", statusEnd, metricsStart)
		}
	}
}

func TestAlignment_AnnotationBeforeMetrics(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "sshd", Bind: "0.0.0.0:2222"},
		},
	}
	activeState := map[string]state.Component{
		"server:0.0.0.0:2222": {Type: "server", ID: "0.0.0.0:2222", Status: state.Listening},
		"dynamic:127.0.0.1:1080|0.0.0.0:2222": {
			Type: "dynamic", ID: "127.0.0.1:1080|0.0.0.0:2222",
			Status: state.Listening, Message: "root@127.0.0.1:50000",
			PeerAddr: "client/tunnel/fwd",
		},
	}
	m := &state.Metrics{}
	m.StartTime.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	m.BytesTx.Store(4096)
	metricsMap := map[string]*state.Metrics{
		"server:0.0.0.0:2222":                 m,
		"dynamic:127.0.0.1:1080|0.0.0.0:2222": m,
	}
	output, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	for _, line := range lines {
		annEnd := strings.Index(line, "(client/tunnel/fwd)")
		metricsStart := strings.Index(line, "↑")
		if annEnd >= 0 && metricsStart >= 0 {
			annEnd += len("(client/tunnel/fwd)")
			if metricsStart < annEnd {
				t.Errorf("annotation ends at %d but metrics starts at %d — overlap", annEnd, metricsStart)
			}
		}
	}
}

func TestAlignment_WideContentNoOverlap(t *testing.T) {
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
		},
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"verylonguser@very-long-hostname.example.com:22"},
				Forwards: []config.ForwardSet{
					{
						Name: "fwd",
						Local: []config.Forward{
							{Type: "forward", Bind: "127.0.0.1:8080", Target: "very-long-hostname.example.com:80"},
						},
					},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Connected, Message: "verylonguser@very-long-hostname.example.com:22",
		},
	}
	m := &state.Metrics{}
	m.StartTime.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	m.BytesTx.Store(1024)
	metricsMap := map[string]*state.Metrics{
		"proxy:127.0.0.1:1080":                m,
		"forward:tunnel [fwd] 127.0.0.1:8080": m,
	}
	output, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	// Every line with both status and metrics must have status before metrics
	for _, line := range lines {
		for _, status := range []string{"[connected]", "[listening]"} {
			sIdx := strings.Index(line, status)
			mIdx := strings.Index(line, "↑")
			if sIdx >= 0 && mIdx >= 0 && mIdx < sIdx+len(status) {
				t.Errorf("metrics overlaps with %s in: %s", status, line)
			}
		}
	}
}

func TestAlignment_StatusRightAligned_SingleSpaceBeforeMetrics(t *testing.T) {
	// Two listeners with metrics: [listening] (11 chars) and a connection
	// with [retrying] (10 chars). The gap between the END of each status
	// and the start of metrics must always be exactly 1 space.
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
		},
		Connections: []config.Connection{
			{
				Name:    "tunnel",
				Targets: []string{"root@10.0.0.1:22"},
				Forwards: []config.ForwardSet{
					{
						Name: "fwd",
						Local: []config.Forward{
							{Type: "forward", Bind: "127.0.0.1:8080", Target: "10.0.0.1:80"},
						},
					},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
		"connection:tunnel [fwd]": {
			Type: "connection", ID: "tunnel [fwd]",
			Status: state.Retrying, Message: "dial timeout",
		},
	}
	m := &state.Metrics{}
	m.StartTime.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	m.BytesTx.Store(1024)
	metricsMap := map[string]*state.Metrics{
		"proxy:127.0.0.1:1080":                m,
		"forward:tunnel [fwd] 127.0.0.1:8080": m,
	}
	output, _ := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	// All status texts should end at the same visual column, and the gap
	// between that column and the duration field should be exactly 1 space.
	var statusEndCols []int
	for _, line := range lines {
		for _, status := range []string{"[listening]", "[retrying (dial timeout)]"} {
			col := findColumn(line, status)
			if col < 0 {
				continue
			}
			endCol := col + len(status)
			statusEndCols = append(statusEndCols, endCol)
		}
	}
	if len(statusEndCols) < 2 {
		t.Fatalf("expected at least 2 status texts, got %d", len(statusEndCols))
	}
	// All status end columns must be the same (right-aligned)
	for i := 1; i < len(statusEndCols); i++ {
		if statusEndCols[i] != statusEndCols[0] {
			t.Errorf("status end columns differ: %d vs %d (all should be equal for right-alignment)", statusEndCols[0], statusEndCols[i])
		}
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

func TestRenderDashboardFrame_UsesRawNewlines(t *testing.T) {
	t.Parallel()
	lines := []string{"line1", "line2", "line3"}
	frame := renderDashboardFrame(lines, 0, 3, 5, []string{"test-node"}, "/tmp/cfg.yaml", "/tmp/mesh.log", "http://127.0.0.1:7777", time.Now(), 3, true)

	// In raw terminal mode, every newline must be \r\n, not bare \n.
	// Strip all \r\n first, then check no bare \n remains.
	stripped := strings.ReplaceAll(frame, "\r\n", "")
	if strings.Contains(stripped, "\n") {
		t.Errorf("dashboard frame contains bare \\n without \\r; raw mode requires \\r\\n for all line breaks")
	}

	// Must contain at least one \r\n (header + lines + blanks).
	if !strings.Contains(frame, "\r\n") {
		t.Error("dashboard frame contains no \\r\\n at all")
	}
}

func TestRenderDashboardFrame_ContainsHeaderFields(t *testing.T) {
	t.Parallel()
	frame := renderDashboardFrame(nil, 0, 0, 5, []string{"my-node"}, "/etc/mesh.yaml", "/var/log/mesh.log", "http://localhost:7777", time.Now(), 0, true)

	for _, want := range []string{"my-node", "/etc/mesh.yaml", "/var/log/mesh.log", "localhost:7777"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame missing expected header content %q", want)
		}
	}
}

func TestRenderDashboardFrame_ViewportLines(t *testing.T) {
	t.Parallel()
	lines := []string{"alpha", "bravo", "charlie", "delta"}
	// Scroll: show lines[1:3] in a viewport of height 4
	frame := renderDashboardFrame(lines, 1, 3, 4, []string{"n"}, "", "", "", time.Now(), 4, false)

	if !strings.Contains(frame, "bravo") {
		t.Error("frame should contain 'bravo' (lines[1])")
	}
	if !strings.Contains(frame, "charlie") {
		t.Error("frame should contain 'charlie' (lines[2])")
	}
	if strings.Contains(frame, "alpha") {
		t.Error("frame should NOT contain 'alpha' (scrolled past)")
	}
	if strings.Contains(frame, "delta") {
		t.Error("frame should NOT contain 'delta' (not in viewport)")
	}
}

func TestRenderDashboardFrame_FooterLiveAndPaused(t *testing.T) {
	t.Parallel()
	lines := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	// Eight lines, viewport of 3 → content overflows, footer shows position.
	live := stripANSI(renderDashboardFrame(lines, 5, 8, 3, []string{"n"}, "", "", "", time.Now(), 8, true))
	paused := stripANSI(renderDashboardFrame(lines, 1, 4, 3, []string{"n"}, "", "", "", time.Now(), 8, false))

	if !strings.Contains(live, "q quit") {
		t.Error("footer must show the q-quit keybinding hint")
	}
	if !strings.Contains(live, "↑↓/jk scroll") {
		t.Error("footer must show the scroll hint")
	}
	if !strings.Contains(live, "[LIVE 6-8/8]") {
		t.Errorf("LIVE footer should show current range, got: %q", live)
	}
	if !strings.Contains(paused, "[paused 2-4/8]") {
		t.Errorf("paused footer should show current range, got: %q", paused)
	}
}

func TestRenderDashboardFrame_FooterOmittedWhenContentFits(t *testing.T) {
	t.Parallel()
	// Three lines in a viewport of 5 — everything visible, no scroll indicator needed.
	frame := stripANSI(renderDashboardFrame([]string{"a", "b", "c"}, 0, 3, 5, []string{"n"}, "", "", "", time.Now(), 3, true))

	if !strings.Contains(frame, "q quit") {
		t.Error("footer keybinding hints must always be present")
	}
	if strings.Contains(frame, "LIVE") || strings.Contains(frame, "paused") {
		t.Errorf("scroll indicator should be omitted when content fits, got: %q", frame)
	}
}

// TestBuildDashboardBody_Deterministic pins the stability contract: two
// calls with the same config and state snapshot must return byte-identical
// output. The render loop relies on this to skip redraws between ticks; if
// buildDashboardBody ever becomes non-deterministic (e.g. iterates a map
// without sorting), the body-diff cache breaks and the dashboard starts
// glitching on every frame.
func TestBuildDashboardBody_Deterministic(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
			{Type: "http", Bind: "127.0.0.1:3128"},
		},
		Connections: []config.Connection{
			{
				Name:    "bastion",
				Targets: []string{"root@10.0.0.1:22"},
			},
		},
	}
	full := state.FullSnapshot{
		Components: map[string]state.Component{
			"proxy:127.0.0.1:1080":    {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
			"proxy:127.0.0.1:3128":    {Type: "proxy", ID: "127.0.0.1:3128", Status: state.Listening},
			"connection:bastion":      {Type: "connection", ID: "bastion", Status: state.Connected},
			"target:bastion|10.0.0.1": {Type: "target", ID: "bastion|10.0.0.1", Status: state.Connected},
		},
		Metrics: map[string]*state.Metrics{},
	}
	cfgs := map[string]*config.Config{"node1": cfg}
	nodeNames := []string{"node1"}

	first, _ := buildDashboardBody(cfgs, nodeNames, full)
	firstJoined := strings.Join(first, "\n")
	for i := 0; i < 10; i++ {
		next, _ := buildDashboardBody(cfgs, nodeNames, full)
		if got := strings.Join(next, "\n"); got != firstJoined {
			t.Fatalf("iteration %d diverged from first call\nfirst: %q\n  got: %q", i, firstJoined, got)
		}
	}
}

// TestBuildDashboardBody_NoLogs verifies the log tail was removed from the
// CLI dashboard body. Logs remain available via the admin UI / log file;
// the dashboard body must be exactly the status output, nothing appended.
func TestBuildDashboardBody_NoLogs(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
		},
	}
	full := state.FullSnapshot{
		Components: map[string]state.Component{
			"proxy:127.0.0.1:1080": {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
		},
		Metrics: map[string]*state.Metrics{},
	}
	lines, _ := buildDashboardBody(map[string]*config.Config{"node": cfg}, []string{"node"}, full)
	got := strings.Join(lines, "\n")

	statusOut, _ := renderStatus(cfg, full.Components, full.Metrics, "node")
	want := strings.TrimRight(statusOut, "\n")

	if got != want {
		t.Errorf("buildDashboardBody should return only renderStatus output — anything extra is a log-tail regression\nwant: %q\n got: %q", stripANSI(want), stripANSI(got))
	}
}

// TestRenderDashboardHeaderOnly_HeaderFieldsPresent locks in the header
// refresh path. When the body is unchanged between ticks, runDashboard
// writes only this string — so it must carry node name, clock/uptime,
// and the configured paths.
func TestRenderDashboardHeaderOnly_HeaderFieldsPresent(t *testing.T) {
	t.Parallel()
	start := time.Now().Add(-42 * time.Second)
	out := renderDashboardHeaderOnly([]string{"node1"}, "/tmp/cfg.yaml", "/tmp/mesh.log", "http://127.0.0.1:7777", start)

	for _, want := range []string{"node1", "/tmp/cfg.yaml", "/tmp/mesh.log", "127.0.0.1:7777", "up 42s"} {
		if !strings.Contains(out, want) {
			t.Errorf("header-only output missing %q\ngot: %q", want, stripANSI(out))
		}
	}
	// Must start with cursor-home so it overwrites the existing header
	// in place rather than scrolling new content onto the screen.
	if !strings.HasPrefix(out, "\033[H") {
		t.Error("header-only output must start with cursor-home escape")
	}
}

// TestRenderDashboardHeaderOnly_NoBodyRegion guards the flicker-free
// property: the header-only refresh must never reach into the body
// region. It writes at most one line per header field and nothing else.
func TestRenderDashboardHeaderOnly_NoBodyRegion(t *testing.T) {
	t.Parallel()
	start := time.Now()
	out := renderDashboardHeaderOnly([]string{"n"}, "cfg", "log", "ui", start)

	// Full frame has the header + blank separator + body + trailing fill.
	// Header-only must be strictly shorter for the same inputs, proving
	// no body is emitted.
	full := renderDashboardFrame([]string{"row1", "row2", "row3"}, 0, 3, 10, []string{"n"}, "cfg", "log", "ui", start, 3, true)
	if len(out) >= len(full) {
		t.Errorf("header-only output (%d bytes) should be strictly shorter than full frame (%d bytes)", len(out), len(full))
	}

	// Four CRLFs: mesh line, cfg, log, ui. No trailing blank line (that
	// belongs to the body region).
	if got := strings.Count(out, "\r\n"); got != 4 {
		t.Errorf("header-only output should contain exactly 4 \\r\\n, got %d\noutput: %q", got, stripANSI(out))
	}
}

func TestRenderStatus_FilesyncSingleLinePerFolder(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cfg := &config.Config{
		Filesync: []config.FilesyncCfg{
			{
				Bind:  "0.0.0.0:7756",
				Peers: map[string]config.PeerDef{"mbp": {Addresses: []string{"192.168.68.134:7756"}}, "hw": {Addresses: []string{"192.168.68.111:7756"}}},
				ResolvedFolders: []config.FolderCfg{
					{ID: "code", Path: "/home/user/code", Peers: []string{"192.168.68.134:7756", "192.168.68.111:7756"}, PeerNames: []string{"mbp", "hw"}, Direction: "send-receive"},
					{ID: "docs", Path: "/home/user/docs", Peers: []string{"192.168.68.134:7756"}, PeerNames: []string{"mbp"}, Direction: "send-only"},
					{ID: "archive", Path: "/home/user/archive", Peers: []string{"192.168.68.111:7756"}, PeerNames: []string{"hw"}, Direction: "disabled"},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"filesync:0.0.0.0:7756":                  {Type: "filesync", ID: "0.0.0.0:7756", Status: state.Listening},
		"filesync-folder:code":                   {Type: "filesync-folder", ID: "code", Status: state.Connected, FileCount: 12345, TotalSize: 1536 * 1024 * 1024, LastSync: now.Add(-30 * time.Second)},
		"filesync-folder:docs":                   {Type: "filesync-folder", ID: "docs", Status: state.Scanning, FileCount: 89, TotalSize: 2 * 1024 * 1024, LastSync: now.Add(-5 * time.Minute)},
		"filesync-folder:archive":                {Type: "filesync-folder", ID: "archive", Status: state.Connected},
		"filesync-peer:code|192.168.68.134:7756": {Type: "filesync-peer", ID: "code|192.168.68.134:7756", Status: state.Connected},
		"filesync-peer:code|192.168.68.111:7756": {Type: "filesync-peer", ID: "code|192.168.68.111:7756", Status: state.Retrying},
		"filesync-peer:docs|192.168.68.134:7756": {Type: "filesync-peer", ID: "docs|192.168.68.134:7756", Status: state.Connecting},
	}

	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	plain := stripANSI(output)

	// Single line per folder — no peer sub-rows.
	for _, want := range []string{"code", "docs", "archive"} {
		count := 0
		for _, line := range strings.Split(plain, "\n") {
			if strings.Contains(line, want) && (strings.Contains(line, "[idle]") || strings.Contains(line, "[scanning]") || strings.Contains(line, "[disabled]")) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("folder %q: want exactly 1 status line, got %d\n%s", want, count, plain)
		}
	}

	// File counts present.
	if !strings.Contains(plain, "12345") {
		t.Errorf("output missing file count 12345\n%s", plain)
	}
	if !strings.Contains(plain, "89") {
		t.Errorf("output missing file count 89\n%s", plain)
	}

	// Total size present.
	if !strings.Contains(plain, "1.5G") {
		t.Errorf("output missing total size 1.5G\n%s", plain)
	}
	if !strings.Contains(plain, "2.0M") {
		t.Errorf("output missing total size 2.0M\n%s", plain)
	}

	// Sync time (HH:MM:SS format, no "synced ... ago").
	if strings.Contains(plain, "synced") {
		t.Errorf("output should not contain 'synced' text\n%s", plain)
	}

	// Peer column header with names.
	if !strings.Contains(plain, "mbp") {
		t.Errorf("output missing peer name 'mbp'\n%s", plain)
	}
	if !strings.Contains(plain, "hw") {
		t.Errorf("output missing peer name 'hw'\n%s", plain)
	}

	// No old-style per-peer sub-rows.
	if strings.Contains(plain, "synced") || strings.Contains(plain, "waiting") || strings.Contains(plain, "retrying") {
		// Check that these are not peer sub-row statuses.
		for _, line := range strings.Split(plain, "\n") {
			stripped := strings.TrimSpace(line)
			if strings.HasPrefix(stripped, "●") || strings.HasPrefix(stripped, "○") {
				t.Errorf("found old-style peer sub-row: %q", stripped)
			}
		}
	}

	// Disabled folder should not show peer indicators.
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "archive") && strings.Contains(line, "●") {
			t.Errorf("disabled folder 'archive' should not have peer indicators: %q", line)
		}
	}

	t.Logf("Rendered output:\n%s", plain)
}

func TestRenderStatus_FilesyncPeerColumnAlignment_EastAsian(t *testing.T) {
	old := eastAsianWidth
	eastAsianWidth = true
	defer func() { eastAsianWidth = old }()

	cfg := &config.Config{
		Filesync: []config.FilesyncCfg{
			{
				Bind:  "0.0.0.0:7756",
				Peers: map[string]config.PeerDef{"mbp": {Addresses: []string{"10.0.0.1:7756"}}},
				ResolvedFolders: []config.FolderCfg{
					{ID: "code", Path: `C:\Users\user\code`, Peers: []string{"10.0.0.1:7756"}, PeerNames: []string{"mbp"}, Direction: "send-receive"},
					{ID: "documents", Path: `C:\Users\user\Documents`, Peers: []string{"10.0.0.1:7756"}, PeerNames: []string{"mbp"}, Direction: "send-receive"},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"filesync:0.0.0.0:7756":                 {Type: "filesync", ID: "0.0.0.0:7756", Status: state.Listening},
		"filesync-folder:code":                  {Type: "filesync-folder", ID: "code", Status: state.Starting},
		"filesync-folder:documents":             {Type: "filesync-folder", ID: "documents", Status: state.Starting},
		"filesync-peer:code|10.0.0.1:7756":      {Type: "filesync-peer", ID: "code|10.0.0.1:7756"},
		"filesync-peer:documents|10.0.0.1:7756": {Type: "filesync-peer", ID: "documents|10.0.0.1:7756"},
	}

	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	plain := stripANSI(output)

	// visCol counts visible columns using runeWidth so ambiguous chars (◎, ○)
	// count as 2 columns when eastAsianWidth is true.
	visCol := func(s, sub string) int {
		idx := strings.Index(s, sub)
		if idx < 0 {
			return -1
		}
		col := 0
		for i, r := range s {
			if i >= idx {
				return col
			}
			col += runeWidth(r)
		}
		return -1
	}

	lines := strings.Split(plain, "\n")
	var headerLine string
	for _, line := range lines {
		if strings.Contains(line, "mbp") && !strings.Contains(line, "[") {
			headerLine = line
			break
		}
	}
	if headerLine == "" {
		t.Fatalf("peer header line not found\n%s", plain)
	}

	mbpCol := visCol(headerLine, "mbp")

	for _, line := range lines {
		if !strings.Contains(line, "code") || strings.Contains(line, "documents") {
			continue
		}
		if !strings.Contains(line, "[") {
			continue
		}
		dotCol := visCol(line, "○")
		if dotCol < 0 {
			t.Errorf("code folder should have ○ indicator\n%s", plain)
			break
		}
		if dotCol != mbpCol {
			t.Errorf("code ○ at visible column %d, want mbp column %d\nheader: %q\n  line: %q", dotCol, mbpCol, headerLine, line)
		}
		break
	}

	t.Logf("Rendered output:\n%s", plain)
}

func TestRenderStatus_FilesyncStatusPaddingWithMetrics(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Filesync: []config.FilesyncCfg{
			{
				Bind:  "0.0.0.0:7756",
				Peers: map[string]config.PeerDef{"mbp": {Addresses: []string{"127.0.0.1:27756"}}},
				ResolvedFolders: []config.FolderCfg{
					{ID: "code", Path: `C:\Users\mwx1313262\code`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "hw-Desktop", Path: `C:\Users\mwx1313262\Desktop`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "hw-Documents", Path: `C:\Users\mwx1313262\Documents`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "hw-Downloads", Path: `C:\Users\mwx1313262\Downloads`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "hw-OneBox", Path: `C:\Users\mwx1313262\OneBox`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "hw-Pictures", Path: `C:\Users\mwx1313262\Pictures`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "m2-repo", Path: `C:\Users\mwx1313262\.m2\repository`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "mesh-code", Path: `C:\Users\mwx1313262\code-2\mesh`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "mesh-conf", Path: `C:\Users\mwx1313262\.mesh\conf`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "mesh-log", Path: `C:\Users\mwx1313262\.mesh\log`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
					{ID: "spark-kit", Path: `C:\Users\mwx1313262\code\spark-kit`, Peers: []string{"127.0.0.1:27756"}, PeerNames: []string{"mbp"}, Direction: "dry-run"},
				},
			},
		},
		Listeners: []config.Listener{
			{Type: "socks", Bind: "127.0.0.1:1080"},
			{Type: "http", Bind: "127.0.0.1:1081"},
		},
		Clipsync: []config.ClipsyncCfg{
			{Bind: "0.0.0.0:7755"},
		},
		Gateway: []gateway.GatewayCfg{
			{Name: "claude-audit", Bind: "127.0.0.1:8082", Upstream: "https://api.anthropic.com", ClientAPI: "anthropic", UpstreamAPI: "openai"},
		},
	}
	activeState := map[string]state.Component{
		"filesync:0.0.0.0:7756": {Type: "filesync", ID: "0.0.0.0:7756", Status: state.Listening},
		"proxy:127.0.0.1:1080":  {Type: "proxy", ID: "127.0.0.1:1080", Status: state.Listening},
		"proxy:127.0.0.1:1081":  {Type: "proxy", ID: "127.0.0.1:1081", Status: state.Listening},
		"clipsync:0.0.0.0:7755": {Type: "clipsync", ID: "0.0.0.0:7755", Status: state.Listening},
		"gateway:claude-audit":  {Type: "gateway", ID: "claude-audit", Status: state.Listening},
	}
	for _, f := range cfg.Filesync[0].ResolvedFolders {
		activeState["filesync-folder:"+f.ID] = state.Component{Type: "filesync-folder", ID: f.ID, Status: state.Starting}
		activeState["filesync-peer:"+f.ID+"|127.0.0.1:27756"] = state.Component{Type: "filesync-peer", ID: f.ID + "|127.0.0.1:27756"}
	}
	fsM := &state.Metrics{}
	fsM.StartTime.Store(time.Now().Add(-30 * time.Second).UnixNano())
	proxyM := &state.Metrics{}
	proxyM.StartTime.Store(time.Now().Add(-30 * time.Second).UnixNano())
	gwM := &state.Metrics{}
	gwM.StartTime.Store(time.Now().Add(-30 * time.Second).UnixNano())
	metricsMap := map[string]*state.Metrics{
		"filesync:0.0.0.0:7756": fsM,
		"proxy:127.0.0.1:1080":  proxyM,
		"proxy:127.0.0.1:1081":  proxyM,
		"gateway:claude-audit":  gwM,
	}

	output, _ := renderStatus(cfg, activeState, metricsMap, "server")
	plain := stripANSI(output)

	// All status ']' brackets must be at the same visual column across all
	// sections. Use visibleLen (not byte offset) because multi-byte characters
	// like emoji and box-drawing shift byte positions.
	var bracketCols []int
	var bracketLines []string
	for _, line := range strings.Split(plain, "\n") {
		for _, tag := range []string{"[listening]", "[loading]", "[starting]", "[connected]", "[scanning]", "[idle]"} {
			idx := strings.Index(line, tag)
			if idx < 0 {
				continue
			}
			visualCol := visibleLen(line[:idx+len(tag)])
			bracketCols = append(bracketCols, visualCol)
			bracketLines = append(bracketLines, line)
			if idx >= 2 && line[idx-1] != ' ' {
				t.Errorf("no space before %s:\n  %q", tag, line)
			}
		}
	}
	if len(bracketCols) > 1 {
		for i := 1; i < len(bracketCols); i++ {
			if bracketCols[i] != bracketCols[0] {
				t.Errorf("status ']' visual columns not aligned: col[0]=%d vs col[%d]=%d\n  line[0]: %q\n  line[%d]: %q",
					bracketCols[0], i, bracketCols[i], bracketLines[0], i, bracketLines[i])
			}
		}
	}

	t.Logf("Rendered output:\n%s", plain)
}

func TestRenderStatus_FilesyncPeerColumnAlignment(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Filesync: []config.FilesyncCfg{
			{
				Bind:  "0.0.0.0:7756",
				Peers: map[string]config.PeerDef{"hw": {Addresses: []string{"127.0.0.1:17756"}}, "lenovo": {Addresses: []string{"10.0.0.2:7756"}}},
				ResolvedFolders: []config.FolderCfg{
					{ID: "code", Path: "/home/user/code", Peers: []string{"127.0.0.1:17756"}, PeerNames: []string{"hw"}, Direction: "dry-run"},
					{ID: "mesh-code", Path: "/home/user/dev/mesh", Peers: []string{"127.0.0.1:17756", "10.0.0.2:7756"}, PeerNames: []string{"hw", "lenovo"}, Direction: "dry-run"},
					{ID: "mesh-keys", Path: "/home/user/.mesh/keys", Peers: []string{"10.0.0.2:7756"}, PeerNames: []string{"lenovo"}, Direction: "dry-run"},
				},
			},
		},
	}
	activeState := map[string]state.Component{
		"filesync:0.0.0.0:7756":                   {Type: "filesync", ID: "0.0.0.0:7756", Status: state.Listening},
		"filesync-folder:code":                    {Type: "filesync-folder", ID: "code", Status: state.Starting},
		"filesync-folder:mesh-code":               {Type: "filesync-folder", ID: "mesh-code", Status: state.Starting},
		"filesync-folder:mesh-keys":               {Type: "filesync-folder", ID: "mesh-keys", Status: state.Starting},
		"filesync-peer:code|127.0.0.1:17756":      {Type: "filesync-peer", ID: "code|127.0.0.1:17756"},
		"filesync-peer:mesh-code|127.0.0.1:17756": {Type: "filesync-peer", ID: "mesh-code|127.0.0.1:17756"},
		"filesync-peer:mesh-code|10.0.0.2:7756":   {Type: "filesync-peer", ID: "mesh-code|10.0.0.2:7756"},
		"filesync-peer:mesh-keys|10.0.0.2:7756":   {Type: "filesync-peer", ID: "mesh-keys|10.0.0.2:7756"},
	}

	output, _ := renderStatus(cfg, activeState, nil, "testnode")
	plain := stripANSI(output)

	// Find the header row with peer names and extract column positions.
	lines := strings.Split(plain, "\n")
	var headerLine string
	for _, line := range lines {
		if strings.Contains(line, "hw") && strings.Contains(line, "lenovo") && !strings.Contains(line, "[") {
			headerLine = line
			break
		}
	}
	if headerLine == "" {
		t.Fatalf("peer header line not found\n%s", plain)
	}

	// visCol returns the visible column index of the first occurrence of sub in s,
	// counting each rune as 1 column (matching visibleLen's treatment of non-emoji).
	visCol := func(s, sub string) int {
		idx := strings.Index(s, sub)
		if idx < 0 {
			return -1
		}
		col := 0
		for i := range s {
			if i == idx {
				return col
			}
			col++
		}
		return -1
	}

	hwCol := visCol(headerLine, "hw")
	lenovoCol := visCol(headerLine, "lenovo")

	// code folder: has hw peer only → ○ under hw, nothing under lenovo.
	for _, line := range lines {
		if !strings.Contains(line, "code") || strings.Contains(line, "mesh-code") {
			continue
		}
		if !strings.Contains(line, "[") {
			continue
		}
		dotCol := visCol(line, "○")
		if dotCol < 0 {
			t.Errorf("code folder should have ○ indicator\n%s", plain)
			break
		}
		if dotCol != hwCol {
			t.Errorf("code ○ at visible column %d, want hw column %d\nheader: %q\n  line: %q", dotCol, hwCol, headerLine, line)
		}
		break
	}

	// mesh-keys folder: has lenovo peer only → ○ under lenovo.
	for _, line := range lines {
		if !strings.Contains(line, "mesh-keys") {
			continue
		}
		dotCol := visCol(line, "○")
		if dotCol < 0 {
			t.Errorf("mesh-keys folder should have ○ indicator\n%s", plain)
			break
		}
		if dotCol != lenovoCol {
			t.Errorf("mesh-keys ○ at visible column %d, want lenovo column %d\nheader: %q\n  line: %q", dotCol, lenovoCol, headerLine, line)
		}
		break
	}

	// mesh-code folder: has both → two ○ at hw and lenovo columns.
	for _, line := range lines {
		if !strings.Contains(line, "mesh-code") {
			continue
		}
		first := visCol(line, "○")
		// Find second ○ after the first.
		rest := line[strings.Index(line, "○")+len("○"):]
		second := -1
		if idx := strings.Index(rest, "○"); idx >= 0 {
			second = first + 1 + visCol(rest, "○")
		}
		if first < 0 || second < 0 {
			t.Errorf("mesh-code folder should have two ○ indicators\n%s", plain)
			break
		}
		if first != hwCol {
			t.Errorf("mesh-code first ○ at visible column %d, want hw column %d\nheader: %q\n  line: %q", first, hwCol, headerLine, line)
		}
		if second != lenovoCol {
			t.Errorf("mesh-code second ○ at visible column %d, want lenovo column %d\nheader: %q\n  line: %q", second, lenovoCol, headerLine, line)
		}
		break
	}

	t.Logf("Rendered output:\n%s", plain)
}
