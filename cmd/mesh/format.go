package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmdemirbas/mesh/internal/state"
)

// visibleLen returns the terminal display width of s, skipping ANSI CSI escape
// sequences and using runeWidth for character width classification.
func visibleLen(s string) int {
	n := 0
	prevWidth := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			// Try to find CSI final byte (0x40-0x7E).
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			if j < len(s) {
				i = j + 1 // complete CSI: skip entirely
				continue
			}
			// Incomplete CSI: treat ESC as a regular character.
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w := runeWidth(r)
		if r == 0xFE0F && prevWidth == 1 {
			w = 1 // VS16 upgrades a 1-wide char to 2-wide emoji presentation
		}
		n += w
		prevWidth = w
		i += size
	}
	return n
}

// visibleSlice returns the portion of s that would appear in a terminal
// column range [startCol, startCol+maxWidth) if s were printed at
// column 0. ANSI CSI escape sequences are copied through without
// counting toward width, and any SGR state that was active at startCol
// is prepended so color/bold survives the horizontal cut. A trailing
// \033[0m is appended when the output contains any SGR so it does not
// bleed into the rest of the line. Wide characters (e.g. CJK, emoji)
// that would straddle a column boundary are dropped rather than split.
func visibleSlice(s string, startCol, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	var out strings.Builder
	out.Grow(len(s))

	// First pass: walk up to startCol, tracking active SGR state so we
	// can re-emit it at the start of the slice. Zero-width SGR sequences
	// are consumed even after col has reached startCol, so a reset
	// (\033[0m) sitting exactly at the cut point clears state rather
	// than leaking a stale color into the output. When startCol is 0
	// there is nothing to skip, so all escapes pass through the copy
	// phase verbatim (preserving escape-only inputs intact).
	var activeSGR strings.Builder
	col := 0
	prevWidth := 0
	i := 0
	if startCol > 0 {
		for i < len(s) {
			if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && (s[j] < '@' || s[j] > '~') {
					j++
				}
				if j < len(s) {
					if s[j] == 'm' {
						// \033[0m resets state; any other SGR extends it.
						if j == i+3 && s[i+2] == '0' {
							activeSGR.Reset()
						} else {
							activeSGR.WriteString(s[i : j+1])
						}
					}
					i = j + 1
					continue
				}
			}
			if col >= startCol {
				break
			}
			r, size := utf8.DecodeRuneInString(s[i:])
			cw := runeWidth(r)
			if r == 0xFE0F && prevWidth == 1 {
				cw = 1
			}
			col += cw
			prevWidth = cw
			i += size
		}
	}

	sawSGR := false
	if activeSGR.Len() > 0 {
		out.WriteString(activeSGR.String())
		sawSGR = true
	}

	// Second pass: copy up to maxWidth columns of visible content.
	w := 0
	for i < len(s) && w < maxWidth {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			if j < len(s) {
				out.WriteString(s[i : j+1])
				if s[j] == 'm' {
					sawSGR = true
				}
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		cw := runeWidth(r)
		if r == 0xFE0F && prevWidth == 1 {
			cw = 1
		}
		if w+cw > maxWidth {
			break
		}
		out.WriteString(s[i : i+size])
		w += cw
		prevWidth = cw
		i += size
	}
	if sawSGR && w > 0 {
		out.WriteString("\033[0m")
	}
	return out.String()
}

// truncateToVisibleWidth is the startCol=0 case of visibleSlice.
func truncateToVisibleWidth(s string, maxWidth int) string {
	return visibleSlice(s, 0, maxWidth)
}

// stripANSI removes ANSI CSI escape sequences from s.
// Incomplete sequences (no final byte before end of string) are preserved.
func stripANSI(s string) string {
	if strings.IndexByte(s, '\x1b') < 0 {
		return s // fast path: no escape sequences
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			// Try to find CSI final byte (0x40-0x7E).
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			if j < len(s) {
				i = j + 1 // complete CSI: strip
				continue
			}
			// Incomplete CSI: preserve raw bytes (fall through).
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

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

// formatBytes returns a human-readable byte count with a space
// separator and two-letter unit (e.g. "1.2 GB", "340 KB", "0 B").
// Math is binary (KiB = 1024); the short KB/MB/GB labels match the
// convention used across the rest of the dashboard.
func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// formatDuration returns an HH:MM:SS clock-face duration. Hours grow
// past two digits for uptimes over 99h (e.g. "172:30:00" for a week);
// zero pad keeps columns aligned in the dashboard metrics region.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Second)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// metricsSnapshot holds a point-in-time copy of metrics values.
type metricsSnapshot struct {
	uptime              time.Duration
	tx, rx              int64
	streams             int32
	tokensIn, tokensOut int64 // gateway-only; zero everywhere else
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
		uptime:    uptime,
		tx:        m.BytesTx.Load(),
		rx:        m.BytesRx.Load(),
		streams:   m.Streams.Load(),
		tokensIn:  m.TokensIn.Load(),
		tokensOut: m.TokensOut.Load(),
	}
}

// add accumulates another snapshot into this one, keeping the longest uptime.
func (s *metricsSnapshot) add(o metricsSnapshot) {
	s.tx += o.tx
	s.rx += o.rx
	s.streams += o.streams
	s.tokensIn += o.tokensIn
	s.tokensOut += o.tokensOut
	if o.uptime > s.uptime {
		s.uptime = o.uptime
	}
}

// formatTokens returns "Nk" / "M" / etc. for compact token display.
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// colorBytes formats a byte count with bold number and non-bold unit in the given color.
// formatBytes always returns "<num> <unit>"; split on the space so the
// unit (" B", " KB", ...) stays non-bold while the number is bold.
func colorBytes(b int64, color string) string {
	if b == 0 {
		return cGray + "0 B" + cReset
	}
	raw := formatBytes(b)
	idx := strings.IndexByte(raw, ' ')
	if idx < 0 {
		return cBold + color + raw + cReset
	}
	num := raw[:idx]
	unit := raw[idx:]
	return cBold + color + num + cReset + color + unit
}

// formatMetricsSnap returns a compact metrics string.
// Upload (↑) in cyan, download (↓) in magenta, numbers bold, units non-bold.
// Token counters (gateway-only) are appended as "tok ↑in ↓out" when nonzero.
func formatMetricsSnap(s metricsSnapshot) string {
	if s.uptime <= 0 && s.tx == 0 && s.rx == 0 && s.tokensIn == 0 && s.tokensOut == 0 {
		return ""
	}
	r := cGray + fmt.Sprintf("%-8s ", formatDuration(s.uptime))
	r += cCyan + "↑" + colorBytes(s.tx, cCyan) + " " + cMagenta + "↓" + colorBytes(s.rx, cMagenta)
	if s.streams > 0 {
		r += " " + cBold + cReset + fmt.Sprintf("%d", s.streams) + cGray + "↔"
	}
	if s.tokensIn > 0 || s.tokensOut > 0 {
		r += " " + cGray + "tok " + cCyan + "↑" + cBold + formatTokens(s.tokensIn) + cReset + " " + cMagenta + "↓" + cBold + formatTokens(s.tokensOut) + cReset
	}
	r += cReset
	return r
}

// formatMetricsAligned returns a metrics string with tx and rx padded to
// the given widths so that ↓ and ↔ columns align across rows.
func formatMetricsAligned(s metricsSnapshot, txWidth, rxWidth int) string {
	if s.uptime <= 0 && s.tx == 0 && s.rx == 0 && s.tokensIn == 0 && s.tokensOut == 0 {
		return ""
	}
	r := cGray + fmt.Sprintf("%-8s ", formatDuration(s.uptime))
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
	if s.tokensIn > 0 || s.tokensOut > 0 {
		r += " " + cGray + "tok " + cCyan + "↑" + cBold + formatTokens(s.tokensIn) + cReset + " " + cMagenta + "↓" + cBold + formatTokens(s.tokensOut) + cReset
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

// truncateMessage clips a status message to budget runes, replacing the
// tail with an ellipsis when longer. Preserves the head so the reader
// sees the most-identifying prefix (typically the error class). budget
// counts runes, not bytes, so multi-byte UTF-8 content is clipped safely.
func truncateMessage(s string, budget int) string {
	if budget <= 1 || utf8.RuneCountInString(s) <= budget {
		return s
	}
	n := 0
	for i := range s {
		if n == budget-1 {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// formatTLSStatus returns a colored label for a TLS status value, or "" if empty.
func formatTLSStatus(s string) string {
	switch s {
	case "encrypted · verified":
		return cGreen + "encrypted · verified" + cReset
	case "encrypted":
		return cGray + "encrypted" + cReset
	case "CERT MISMATCH":
		return cRed + "CERT MISMATCH" + cReset
	default:
		return ""
	}
}

// directionSymbol returns a compact symbol for a filesync direction mode.
// dry-run and disabled reuse their underlying flow arrows — the special
// mode is surfaced in the status bracket ([dry-run], [disabled]) so the
// glyph column stays a clean up/down/both signal.
func directionSymbol(dir string) string {
	switch dir {
	case "send-receive", "dry-run":
		return "↕"
	case "send-only":
		return "↑"
	case "receive-only":
		return "↓"
	case "disabled":
		return "·"
	default:
		return "?"
	}
}
