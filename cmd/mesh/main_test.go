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

func TestRenderStatus_Empty(t *testing.T) {
	cfg := &config.Config{}
	output := renderStatus(cfg, nil, nil, "testnode")
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
	output := renderStatus(cfg, activeState, nil, "testnode")
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
	output := renderStatus(cfg, nil, nil, "testnode")
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
	output := renderStatus(cfg, activeState, nil, "testnode")
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
	output := renderStatus(cfg, activeState, nil, "testnode")
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
	output := renderStatus(cfg, activeState, nil, "testnode")
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
	output := renderStatus(cfg, activeState, nil, "testnode")
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
	output := renderStatus(cfg, activeState, metricsMap, "testnode")
	if !strings.Contains(output, "↑") || !strings.Contains(output, "↓") {
		t.Error("output should contain ↑ and ↓ byte indicators")
	}
	if !strings.Contains(output, "1.0M") {
		t.Error("output should contain formatted TX bytes")
	}
	if !strings.Contains(output, "2.0M") {
		t.Error("output should contain formatted RX bytes")
	}
	if !strings.Contains(output, "3↔") {
		t.Error("output should contain active stream count")
	}
	if !strings.Contains(output, "2h") {
		t.Error("output should contain uptime")
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
			output := renderStatus(cfg, tt.state, nil, "testnode")
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
	output := renderStatus(cfg, activeState, metricsMap, "testnode")
	if !strings.Contains(output, "5.0M") {
		t.Error("output should contain formatted TX bytes for listener")
	}
	if !strings.Contains(output, "1K") {
		t.Error("output should contain formatted RX bytes for listener")
	}
	if !strings.Contains(output, "30m") {
		t.Error("output should contain uptime for listener")
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
	output := renderStatus(cfg, activeState, nil, "testnode")
	if !strings.Contains(output, "client/tunnel/fwd") {
		t.Error("dynamic port should show mesh node identity in parentheses")
	}
	if !strings.Contains(output, "root@") {
		t.Error("dynamic port should show SSH client address")
	}
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	return ansiStripRe.ReplaceAllString(s, "")
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
	output := renderStatus(cfg, activeState, nil, "testnode")
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
	output := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	wantStatusCol := 24
	wantMetricsCol := 42
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
	output := renderStatus(cfg, activeState, nil, "testnode")
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
	output := renderStatus(cfg, activeState, metricsMap, "testnode")
	lines := extractLines(output)

	// Both forward lines (──▶ and ◀──) should have ↑ at the same column
	wantCol := 54
	for _, line := range lines {
		if !strings.Contains(line, "──▶") && !strings.Contains(line, "◀──") {
			continue
		}
		col := findColumn(line, "↑")
		if col >= 0 && col != wantCol {
			t.Errorf("↑ at col %d, want %d in: %s", col, wantCol, line)
		}
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
	output := renderStatus(cfg, activeState, metricsMap, "testnode")
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
	output := renderStatus(cfg, activeState, metricsMap, "testnode")
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
	output := renderStatus(cfg, activeState, metricsMap, "testnode")
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
