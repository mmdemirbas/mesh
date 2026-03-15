package clipsync

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/mmdemirbas/mesh/internal/config"
)

func TestContains(t *testing.T) {
	tests := []struct {
		slice []string
		item  string
		want  bool
	}{
		{[]string{"a", "b", "c"}, "b", true},
		{[]string{"a", "b", "c"}, "d", false},
		{[]string{}, "a", false},
		{nil, "a", false},
		{[]string{"all"}, "all", true},
		{[]string{"none"}, "all", false},
	}

	for _, tt := range tests {
		got := contains(tt.slice, tt.item)
		if got != tt.want {
			t.Errorf("contains(%v, %q) = %v, want %v", tt.slice, tt.item, got, tt.want)
		}
	}
}

func TestContainsIP(t *testing.T) {
	tests := []struct {
		name  string
		slice []string
		host  string
		want  bool
	}{
		{"exact match", []string{"192.168.1.1"}, "192.168.1.1", true},
		{"no match", []string{"192.168.1.1"}, "10.0.0.1", false},
		{"IPv6-mapped IPv4", []string{"192.168.1.5"}, "::ffff:192.168.1.5", true},
		{"IPv6 match", []string{"::1"}, "::1", true},
		{"empty slice", []string{}, "192.168.1.1", false},
		{"non-IP fallback", []string{"hostname"}, "hostname", true},
		{"non-IP no match", []string{"host1"}, "host2", false},
		{"multiple entries", []string{"10.0.0.1", "192.168.1.1"}, "192.168.1.1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsIP(tt.slice, tt.host)
			if got != tt.want {
				t.Errorf("containsIP(%v, %q) = %v, want %v", tt.slice, tt.host, got, tt.want)
			}
		})
	}
}

func TestHashBytes(t *testing.T) {
	input := []byte("hello world")
	h := md5.Sum(input)
	want := hex.EncodeToString(h[:])

	got := hashBytes(input)
	if got != want {
		t.Errorf("hashBytes(%q) = %q, want %q", input, got, want)
	}
}

func TestHashBytesEmpty(t *testing.T) {
	got := hashBytes(nil)
	h := md5.Sum(nil)
	want := hex.EncodeToString(h[:])
	if got != want {
		t.Errorf("hashBytes(nil) = %q, want %q", got, want)
	}
}

func TestHashBytesDeterministic(t *testing.T) {
	input := []byte("test data")
	h1 := hashBytes(input)
	h2 := hashBytes(input)
	if h1 != h2 {
		t.Error("hashBytes not deterministic")
	}
}

func TestHashFilePaths(t *testing.T) {
	// Order-independent
	h1 := hashFilePaths([]string{"/a/b", "/c/d"})
	h2 := hashFilePaths([]string{"/c/d", "/a/b"})
	if h1 != h2 {
		t.Error("hashFilePaths should be order-independent")
	}
}

func TestHashFilePathsDifferentSets(t *testing.T) {
	h1 := hashFilePaths([]string{"/a", "/b"})
	h2 := hashFilePaths([]string{"/a", "/c"})
	if h1 == h2 {
		t.Error("different file sets should produce different hashes")
	}
}

func TestHashFilePathsEmpty(t *testing.T) {
	h := hashFilePaths([]string{})
	if h == "" {
		t.Error("hashFilePaths of empty slice should return a valid hash")
	}
}

func TestHashFormats(t *testing.T) {
	formats := []ClipFormat{
		{MimeType: "text/plain", Data: []byte("hello")},
		{MimeType: "text/html", Data: []byte("<b>hello</b>")},
	}

	// Order-independent
	reversed := []ClipFormat{formats[1], formats[0]}
	h1 := hashFormats(formats)
	h2 := hashFormats(reversed)
	if h1 != h2 {
		t.Error("hashFormats should be order-independent (sorted by MimeType)")
	}
}

func TestHashFormatsDifferentData(t *testing.T) {
	f1 := []ClipFormat{{MimeType: "text/plain", Data: []byte("hello")}}
	f2 := []ClipFormat{{MimeType: "text/plain", Data: []byte("world")}}
	if hashFormats(f1) == hashFormats(f2) {
		t.Error("different data should produce different hashes")
	}
}

func TestHashFormatsEmpty(t *testing.T) {
	h := hashFormats(nil)
	if h == "" {
		t.Error("hashFormats(nil) should return a valid hash")
	}
}

func TestExtractCFHTMLFragment(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"standard CF_HTML",
			"Version:0.9\r\n<html><body>\r\n<!--StartFragment--><b>hello</b><!--EndFragment-->\r\n</body></html>",
			"<b>hello</b>",
		},
		{
			"with whitespace",
			"<!--StartFragment-->  some text  <!--EndFragment-->",
			"some text",
		},
		{
			"no start marker",
			"<html>no markers</html>",
			"",
		},
		{
			"no end marker",
			"<!--StartFragment-->partial",
			"",
		},
		{
			"markers reversed",
			"<!--EndFragment-->bad<!--StartFragment-->",
			"",
		},
		{
			"empty fragment",
			"<!--StartFragment--><!--EndFragment-->",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCFHTMLFragment(tt.input)
			if got != tt.want {
				t.Errorf("extractCFHTMLFragment() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildCFHTML(t *testing.T) {
	fragment := "<b>hello</b>"
	result := buildCFHTML(fragment)

	// Should contain the fragment
	if !strings.Contains(result, fragment) {
		t.Error("result should contain the original fragment")
	}

	// Should contain CF_HTML markers
	if !strings.Contains(result, "Version:0.9") {
		t.Error("missing Version header")
	}
	if !strings.Contains(result, "StartHTML:") {
		t.Error("missing StartHTML header")
	}
	if !strings.Contains(result, "EndHTML:") {
		t.Error("missing EndHTML header")
	}
	if !strings.Contains(result, "<!--StartFragment-->") {
		t.Error("missing StartFragment marker")
	}
	if !strings.Contains(result, "<!--EndFragment-->") {
		t.Error("missing EndFragment marker")
	}
}

func TestBuildCFHTMLRoundTrip(t *testing.T) {
	fragment := "<p>test content</p>"
	cfhtml := buildCFHTML(fragment)
	extracted := extractCFHTMLFragment(cfhtml)

	if extracted != fragment {
		t.Errorf("roundtrip failed: got %q, want %q", extracted, fragment)
	}
}

func TestParsePathList(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single path", "/home/user/file.txt", []string{"/home/user/file.txt"}},
		{"multiple paths", "/a/b\n/c/d\n/e/f", []string{"/a/b", "/c/d", "/e/f"}},
		{"with empty lines", "/a\n\n/b\n", []string{"/a", "/b"}},
		{"whitespace lines", "  /a  \n  \n  /b  ", []string{"/a", "/b"}},
		{"empty string", "", nil},
		{"only whitespace", "   \n  \n  ", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePathList(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d; got %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseURIList(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single file URI", "file:///home/user/file.txt", []string{"/home/user/file.txt"}},
		{"multiple URIs", "file:///a/b\nfile:///c/d", []string{"/a/b", "/c/d"}},
		{"skip comments", "# comment\nfile:///a/b", []string{"/a/b"}},
		{"skip empty lines", "file:///a\n\nfile:///b", []string{"/a", "/b"}},
		{"skip non-file URIs", "http://example.com\nfile:///a/b", []string{"/a/b"}},
		{"empty string", "", nil},
		{"only comments", "# comment1\n# comment2", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseURIList(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d; got %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCanSendTo(t *testing.T) {
	tests := []struct {
		name      string
		allowSend []string
		addr      string
		isUDP     bool
		want      bool
	}{
		{"all allows everything", []string{"all"}, "192.168.1.1:7755", false, true},
		{"none blocks everything", []string{"none"}, "192.168.1.1:7755", false, false},
		{"udp keyword allows UDP", []string{"udp"}, "192.168.1.1:7755", true, true},
		{"udp keyword blocks non-UDP", []string{"udp"}, "192.168.1.1:7755", false, false},
		{"specific IP match", []string{"192.168.1.1"}, "192.168.1.1:7755", false, true},
		{"specific IP no match", []string{"192.168.1.1"}, "10.0.0.1:7755", false, false},
		{"full addr match", []string{"192.168.1.1:7755"}, "192.168.1.1:7755", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.ClipsyncCfg{AllowSendTo: tt.allowSend}
			n := &Node{config: cfg, sendToIPs: parseIPList(cfg.AllowSendTo)}
			got := n.canSendTo(tt.addr, tt.isUDP)
			if got != tt.want {
				t.Errorf("canSendTo(%q, %v) = %v, want %v", tt.addr, tt.isUDP, got, tt.want)
			}
		})
	}
}

func TestCanReceiveFrom(t *testing.T) {
	tests := []struct {
		name     string
		allowRcv []string
		addr     string
		want     bool
	}{
		{"all allows everything", []string{"all"}, "192.168.1.1:7755", true},
		{"none blocks everything", []string{"none"}, "192.168.1.1:7755", false},
		{"specific IP match", []string{"192.168.1.1"}, "192.168.1.1:7755", true},
		{"specific IP no match", []string{"192.168.1.1"}, "10.0.0.1:7755", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.ClipsyncCfg{AllowReceive: tt.allowRcv}
			n := &Node{config: cfg, receiveIPs: parseIPList(cfg.AllowReceive)}
			got := n.canReceiveFrom(tt.addr)
			if got != tt.want {
				t.Errorf("canReceiveFrom(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestCheckHash(t *testing.T) {
	n := &Node{}

	// First call with a hash should return true (different from initial empty)
	if !n.checkHash("abc123") {
		t.Error("first checkHash should return true")
	}

	// Same hash again should return false
	if n.checkHash("abc123") {
		t.Error("same hash should return false")
	}

	// Different hash should return true
	if !n.checkHash("def456") {
		t.Error("different hash should return true")
	}
}
