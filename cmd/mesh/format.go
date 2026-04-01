package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mmdemirbas/mesh/internal/state"
)

var ansiStripRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// ANSI color codes. Disabled when NO_COLOR env var is set (https://no-color.org/).
var (
	cReset   = "\033[0m"
	cBold    = "\033[1m"
	cRed     = "\033[31m"
	cGreen   = "\033[32m"
	cYellow  = "\033[33m"
	cBlue    = "\033[34m"
	cMagenta = "\033[35m"
	cCyan    = "\033[36m"
	cGray    = "\033[90m"
	cBlink   = "\033[5m"
)

func init() {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		cReset = ""
		cBold = ""
		cRed = ""
		cGreen = ""
		cYellow = ""
		cBlue = ""
		cMagenta = ""
		cCyan = ""
		cGray = ""
		cBlink = ""
	}
}

// addrKey is a pre-parsed, comparable sort key for an address string.
// The IP is stored as two uint64s for single-instruction comparison on 64-bit CPUs.
type addrKey struct {
	ipHi  uint64 // upper 8 bytes of IPv6/mapped-IPv4
	ipLo  uint64 // lower 8 bytes
	port  uint16
	hasIP bool
	raw   string // original string, used as fallback for non-IP addresses
}

// makeAddrKey parses an address string into a sort key.
// For the common case of [user@]IPv4:port, it does a single-pass parse with
// no calls to net.SplitHostPort, net.ParseIP, or strconv.Atoi.
func makeAddrKey(s string) addrKey {
	raw := s
	// Strip user@ prefix
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			s = s[i+1:]
			break
		}
	}

	// Fast path: try to parse entire "IPv4:port" in one scan
	if k, ok := parseIPv4Port(s, raw); ok {
		return k
	}

	// Slow path: IPv6 or hostname — use stdlib
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		host = s
		portStr = ""
	}
	port := atoiUint16(portStr)
	k := addrKey{port: port, raw: raw}

	if ip := net.ParseIP(host); ip != nil {
		ip16 := ip.To16()
		k.ipHi = uint64(ip16[0])<<56 | uint64(ip16[1])<<48 | uint64(ip16[2])<<40 | uint64(ip16[3])<<32 |
			uint64(ip16[4])<<24 | uint64(ip16[5])<<16 | uint64(ip16[6])<<8 | uint64(ip16[7])
		k.ipLo = uint64(ip16[8])<<56 | uint64(ip16[9])<<48 | uint64(ip16[10])<<40 | uint64(ip16[11])<<32 |
			uint64(ip16[12])<<24 | uint64(ip16[13])<<16 | uint64(ip16[14])<<8 | uint64(ip16[15])
		k.hasIP = true
	}
	return k
}

// parseIPv4Port parses "A.B.C.D:port" in a single scan.
// Returns (key, true) on success. On failure returns (_, false).
func parseIPv4Port(s, raw string) (addrKey, bool) {
	var ip [4]byte
	octet := 0
	dots := 0
	digits := 0
	port := 0
	inPort := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if inPort {
				if port > 6553 || (port == 6553 && c > '5') {
					return addrKey{}, false // prevent overflow before multiplication
				}
				port = port*10 + int(c-'0')
			} else {
				octet = octet*10 + int(c-'0')
				if octet > 255 {
					return addrKey{}, false
				}
				digits++
			}
		case c == '.' && !inPort:
			if digits == 0 || dots >= 3 {
				return addrKey{}, false
			}
			ip[dots] = byte(octet)
			dots++
			octet = 0
			digits = 0
		case c == ':' && !inPort && dots == 3 && digits > 0:
			ip[3] = byte(octet)
			inPort = true
		default:
			return addrKey{}, false
		}
	}

	// Handle bare IPv4 without port (e.g., "10.0.0.1")
	if !inPort {
		if dots != 3 || digits == 0 {
			return addrKey{}, false
		}
		ip[3] = byte(octet)
	}

	// IPv4-mapped IPv6: ::ffff:A.B.C.D stored as uint64 pair
	k := addrKey{
		ipHi:  0,
		ipLo:  uint64(0xff)<<40 | uint64(0xff)<<32 | uint64(ip[0])<<24 | uint64(ip[1])<<16 | uint64(ip[2])<<8 | uint64(ip[3]),
		port:  uint16(port),
		hasIP: true,
		raw:   raw,
	}
	return k, true
}

// parseIPv4 parses an IPv4 dotted-quad without allocation.
// Returns [4]byte{} on failure (caller must check for "0.0.0.0" separately).
func parseIPv4(s string) [4]byte {
	var ip [4]byte
	octet := 0
	dots := 0
	digits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			octet = octet*10 + int(c-'0')
			if octet > 255 {
				return [4]byte{}
			}
			digits++
		case c == '.':
			if digits == 0 || dots >= 3 {
				return [4]byte{}
			}
			ip[dots] = byte(octet)
			dots++
			octet = 0
			digits = 0
		default:
			return [4]byte{}
		}
	}
	if dots != 3 || digits == 0 {
		return [4]byte{}
	}
	ip[3] = byte(octet)
	return ip
}

// atoiUint16 parses a small non-negative integer without allocation or error handling.
func atoiUint16(s string) uint16 {
	var n uint16
	for i := 0; i < len(s); i++ {
		n = n*10 + uint16(s[i]-'0')
	}
	return n
}

func (k addrKey) less(other addrKey) bool {
	if k.hasIP && other.hasIP {
		if k.ipHi != other.ipHi {
			return k.ipHi < other.ipHi
		}
		if k.ipLo != other.ipLo {
			return k.ipLo < other.ipLo
		}
		return k.port < other.port
	}
	if k.raw != other.raw {
		return k.raw < other.raw
	}
	return k.port < other.port
}

// parseAddr extracts the IP and port from an address string.
// Handles "host:port", "user@host:port", or just "host".
func parseAddr(s string) (net.IP, int) {
	k := makeAddrKey(s)
	if !k.hasIP {
		return nil, int(k.port)
	}
	ip := make(net.IP, 16)
	ip[0] = byte(k.ipHi >> 56)
	ip[1] = byte(k.ipHi >> 48)
	ip[2] = byte(k.ipHi >> 40)
	ip[3] = byte(k.ipHi >> 32)
	ip[4] = byte(k.ipHi >> 24)
	ip[5] = byte(k.ipHi >> 16)
	ip[6] = byte(k.ipHi >> 8)
	ip[7] = byte(k.ipHi)
	ip[8] = byte(k.ipLo >> 56)
	ip[9] = byte(k.ipLo >> 48)
	ip[10] = byte(k.ipLo >> 40)
	ip[11] = byte(k.ipLo >> 32)
	ip[12] = byte(k.ipLo >> 24)
	ip[13] = byte(k.ipLo >> 16)
	ip[14] = byte(k.ipLo >> 8)
	ip[15] = byte(k.ipLo)
	return ip, int(k.port)
}

// formatBytes returns a human-readable byte count (e.g. "1.2M", "340K", "0").
func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(b)/float64(1<<10))
	case b > 0:
		return fmt.Sprintf("%dB", b)
	default:
		return "0"
	}
}

// formatDuration returns a compact duration string (e.g. "2h13m", "45s", "3d5h").
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

// metricsSnapshot holds a point-in-time copy of metrics values.
type metricsSnapshot struct {
	uptime  time.Duration
	tx, rx  int64
	streams int32
}

// readMetrics reads a single Metrics into a snapshot.
func readMetrics(m *state.Metrics) metricsSnapshot {
	if m == nil {
		return metricsSnapshot{}
	}
	startNano := m.StartTime.Load()
	var uptime time.Duration
	if startNano > 0 {
		uptime = time.Since(time.Unix(0, startNano)).Truncate(time.Second)
	}
	return metricsSnapshot{
		uptime:  uptime,
		tx:      m.BytesTx.Load(),
		rx:      m.BytesRx.Load(),
		streams: m.Streams.Load(),
	}
}

// add accumulates another snapshot into this one, keeping the longest uptime.
func (s *metricsSnapshot) add(o metricsSnapshot) {
	s.tx += o.tx
	s.rx += o.rx
	s.streams += o.streams
	if o.uptime > s.uptime {
		s.uptime = o.uptime
	}
}

// colorBytes formats a byte count with bold number and non-bold unit in the given color.
func colorBytes(b int64, color string) string {
	raw := formatBytes(b)
	if raw == "0" {
		return cGray + "0" + cReset
	}
	unit := raw[len(raw)-1:]
	num := raw[:len(raw)-1]
	return cBold + color + num + cReset + color + unit
}

// formatMetricsSnap returns a compact metrics string.
// Upload (↑) in cyan, download (↓) in magenta, numbers bold, units non-bold.
func formatMetricsSnap(s metricsSnapshot) string {
	if s.uptime <= 0 && s.tx == 0 && s.rx == 0 {
		return ""
	}
	r := cGray + fmt.Sprintf("%-6s ", formatDuration(s.uptime))
	r += cCyan + "↑" + colorBytes(s.tx, cCyan) + " " + cMagenta + "↓" + colorBytes(s.rx, cMagenta)
	if s.streams > 0 {
		r += " " + cBold + cReset + fmt.Sprintf("%d", s.streams) + cGray + "↔"
	}
	r += cReset
	return r
}

// formatMetricsAligned returns a metrics string with tx and rx padded to
// the given widths so that ↓ and ↔ columns align across rows.
func formatMetricsAligned(s metricsSnapshot, txWidth, rxWidth int) string {
	if s.uptime <= 0 && s.tx == 0 && s.rx == 0 {
		return ""
	}
	r := cGray + fmt.Sprintf("%-6s ", formatDuration(s.uptime))
	txRaw := formatBytes(s.tx)
	r += cCyan + "↑" + colorBytes(s.tx, cCyan)
	if pad := txWidth - len(txRaw); pad > 0 {
		r += strings.Repeat(" ", pad)
	}
	rxRaw := formatBytes(s.rx)
	r += " " + cMagenta + "↓" + colorBytes(s.rx, cMagenta)
	if pad := rxWidth - len(rxRaw); pad > 0 {
		r += strings.Repeat(" ", pad)
	}
	if s.streams > 0 {
		r += " " + cBold + cReset + fmt.Sprintf("%d", s.streams) + cGray + "↔"
	}
	r += cReset
	return r
}

// formatPeerIdentity colors a "node/connection/forward" identity string.
// Increasing visibility: node=gray, connection=default, forward=cyan.
func formatPeerIdentity(identity string) string {
	parts := strings.SplitN(identity, "/", 3)
	if len(parts) == 3 {
		return cGray + "(" + parts[0] + "/" + cReset + parts[1] + cGray + "/" + cCyan + parts[2] + cGray + ")" + cReset
	}
	return cGray + "(" + identity + ")" + cReset
}

// compareAddr compares two address strings semantically by IP then port.
func compareAddr(a, b string) bool {
	return makeAddrKey(a).less(makeAddrKey(b))
}
