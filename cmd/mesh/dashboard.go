package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/state"
)

// logRing is a fixed-size ring buffer that captures recent log lines for
// the admin API's /api/logs endpoint. Both raw (ANSI-colored) and
// pre-stripped plain lines are stored to avoid repeated regex/scan work
// when the endpoint serves plain text.
type logRing struct {
	mu    sync.Mutex
	raw   []string
	plain []string
	size  int
	pos   int
	full  bool
}

func newLogRing(size int) *logRing {
	return &logRing{raw: make([]string, size), plain: make([]string, size), size: size}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	// Trim trailing newlines.
	for len(p) > 0 && p[len(p)-1] == '\n' {
		p = p[:len(p)-1]
	}
	// Scan for line boundaries directly — avoids string(p) + strings.Split allocations.
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '\n' {
			if i > start {
				line := string(p[start:i])
				r.raw[r.pos] = line
				r.plain[r.pos] = stripANSI(line)
				r.pos = (r.pos + 1) % r.size
				if r.pos == 0 {
					r.full = true
				}
			}
			start = i + 1
		}
	}
	return n, nil
}

// Lines returns the buffered log lines (with ANSI codes) in chronological order.
func (r *logRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string{}, r.raw[:r.pos]...)
	}
	out := make([]string, 0, r.size)
	out = append(out, r.raw[r.pos:]...)
	out = append(out, r.raw[:r.pos]...)
	return out
}

// PlainLines returns the buffered log lines (ANSI stripped) in chronological order.
func (r *logRing) PlainLines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string{}, r.plain[:r.pos]...)
	}
	out := make([]string, 0, r.size)
	out = append(out, r.plain[r.pos:]...)
	out = append(out, r.plain[:r.pos]...)
	return out
}

// renderDashboardFrame builds one complete frame of the dashboard output.
// All line breaks use \r\n because the terminal is in raw mode where \n
// alone only moves the cursor down without returning to column 1.
func renderDashboardFrame(lines []string, start, end, vpHeight int, nodeNames []string, configPath, logFilePath, adminURL string, startTime time.Time) string {
	const eol = "\033[K\r\n"

	var buf strings.Builder
	buf.WriteString("\033[H") // cursor home
	writeDashboardHeader(&buf, nodeNames, configPath, logFilePath, adminURL, startTime)
	buf.WriteString(eol) // blank line after header

	for i := start; i < end; i++ {
		buf.WriteString(lines[i])
		buf.WriteString(eol)
	}
	for i := end - start; i < vpHeight; i++ {
		buf.WriteString(eol)
	}
	return buf.String()
}

// writeDashboardHeader emits the header lines (node label, clock, uptime,
// paths) without a leading cursor-home escape. Shared between the full
// frame renderer and the header-only refresh path.
func writeDashboardHeader(buf *strings.Builder, nodeNames []string, configPath, logFilePath, adminURL string, startTime time.Time) {
	const eol = "\033[K\r\n"

	uptime := time.Since(startTime).Truncate(time.Second)
	nodesLabel := strings.Join(nodeNames, ", ")
	fmt.Fprintf(buf, "%s%smesh %s%s %s%s%s | pid %d | %s | up %s",
		cBold, cCyan, nodesLabel, cReset, cGray, version, cReset, os.Getpid(), time.Now().Format("15:04:05"), uptime)
	buf.WriteString(eol)
	if configPath != "" {
		fmt.Fprintf(buf, "  %sconfig: %s%s", cGray, configPath, cReset)
		buf.WriteString(eol)
	}
	if logFilePath != "" {
		fmt.Fprintf(buf, "  %slog:    %s%s", cGray, logFilePath, cReset)
		buf.WriteString(eol)
	}
	if adminURL != "" {
		fmt.Fprintf(buf, "  %sui:     %s%s", cGray, adminURL, cReset)
		buf.WriteString(eol)
	}
}

// renderDashboardHeaderOnly refreshes just the header region (cursor home
// + header lines). The dashboard uses this on every tick while the body
// is unchanged so the clock and uptime advance without rewriting — and
// thus without touching — the rest of the screen.
func renderDashboardHeaderOnly(nodeNames []string, configPath, logFilePath, adminURL string, startTime time.Time) string {
	var buf strings.Builder
	buf.WriteString("\033[H")
	writeDashboardHeader(&buf, nodeNames, configPath, logFilePath, adminURL, startTime)
	return buf.String()
}

// buildDashboardBody renders the status lines for every node from a state
// snapshot. Output is deterministic given identical inputs so the render
// loop can compare consecutive frames and skip redraws when nothing
// changed. The log tail lives in the admin UI, not the CLI dashboard.
func buildDashboardBody(cfgs map[string]*config.Config, nodeNames []string, full state.FullSnapshot) ([]string, int) {
	var bodyLines []string
	var maxWidth int
	for _, name := range nodeNames {
		out, w := renderStatus(cfgs[name], full.Components, full.Metrics, name)
		bodyLines = append(bodyLines, strings.Split(strings.TrimRight(out, "\n"), "\n")...)
		if w > maxWidth {
			maxWidth = w
		}
	}
	return bodyLines, maxWidth
}

// runDashboard renders a live status screen using the terminal's alternate
// screen buffer. Keyboard input (q/ctrl-c to quit, arrow keys and page
// up/down to scroll) is handled via raw terminal mode. The dashboard
// refreshes every second and on input, overwriting in place to avoid
// scrollback pollution. When the dashboard exits (via ctx cancellation or
// user quit), cancel is called to unblock the main goroutine.
func runDashboard(ctx context.Context, cancel context.CancelFunc, cfgs map[string]*config.Config, nodeNames []string, configPath string, logFilePath string, adminURL string) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		cancel()
		return
	}

	os.Stdout.WriteString("\033[?1049h") // enter alt screen
	os.Stdout.WriteString("\033[?25l")   // hide cursor

	// Cleanup order (LIFO): restore raw→cooked FIRST, then leave alt screen.
	// The alt-screen-leave escape must be written while still in raw mode,
	// and raw mode must be restored before the shell resumes output.
	defer func() {
		os.Stdout.WriteString("\033[?25h")   // show cursor
		os.Stdout.WriteString("\033[?1049l") // leave alt screen
		term.Restore(fd, oldState)           // restore cooked mode last
	}()

	headerHeight := 2 // header line + blank line
	if configPath != "" {
		headerHeight++
	}
	if logFilePath != "" {
		headerHeight++
	}
	if adminURL != "" {
		headerHeight++
	}

	startTime := time.Now()
	scrollOffset := 0
	autoScroll := true // follow tail when at bottom
	lastBody := ""

	termSize := func() (int, int) {
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			return 80, 24
		}
		return w, h
	}

	render := func(force bool) {
		_, height := termSize()
		vpHeight := height - headerHeight
		if vpHeight < 1 {
			vpHeight = 1
		}

		lines, _ := buildDashboardBody(cfgs, nodeNames, state.Global.SnapshotFull())

		totalLines := len(lines)
		maxScroll := totalLines - vpHeight
		if maxScroll < 0 {
			maxScroll = 0
		}
		if autoScroll {
			scrollOffset = maxScroll
		}
		if scrollOffset > maxScroll {
			scrollOffset = maxScroll
		}
		if scrollOffset < 0 {
			scrollOffset = 0
		}
		// Re-enable auto-scroll when user scrolls to the bottom.
		if scrollOffset >= maxScroll {
			autoScroll = true
		}

		start := scrollOffset
		end := start + vpHeight
		if end > totalLines {
			end = totalLines
		}

		// Capture the exact byte sequence that will fill the body region.
		// Including vpHeight and start in the key means terminal resizes and
		// scroll shifts naturally invalidate the cache.
		var sig strings.Builder
		fmt.Fprintf(&sig, "%d|%d|", vpHeight, start)
		for i := start; i < end; i++ {
			sig.WriteString(lines[i])
			sig.WriteByte('\n')
		}
		body := sig.String()

		if !force && body == lastBody {
			// Body is unchanged — redraw only the header region so the clock
			// and uptime advance without rewriting the rest of the screen.
			// This is what keeps the dashboard flicker-free between ticks.
			os.Stdout.WriteString(renderDashboardHeaderOnly(nodeNames, configPath, logFilePath, adminURL, startTime))
			return
		}
		lastBody = body

		frame := renderDashboardFrame(lines, start, end, vpHeight, nodeNames, configPath, logFilePath, adminURL, startTime)
		os.Stdout.WriteString(frame)
	}

	// Input reader goroutine. Cannot be cancelled (blocking Read on stdin),
	// but exits via ctx.Done check on the send path. The goroutine will remain
	// blocked on Read until the next keystroke or process exit — this is
	// unavoidable without closing stdin.
	inputCh := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(inputCh)
				return
			}
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case inputCh <- b:
			case <-ctx.Done():
				return
			}
		}
	}()

	winchCh, stopWinch := winchSignal()
	defer stopWinch()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	render(true)

	for {
		select {
		case <-ctx.Done():
			return
		case <-winchCh:
			render(true)
		case <-ticker.C:
			render(false)
		case input, ok := <-inputCh:
			if !ok {
				cancel()
				return
			}
			changed := false
			for i := 0; i < len(input); i++ {
				if input[i] == 'q' || input[i] == 3 { // q or ctrl-c
					cancel()
					return
				}
				if input[i] == 27 && i+2 < len(input) && input[i+1] == '[' {
					// CSI escape sequence
					switch input[i+2] {
					case 'A': // up
						scrollOffset--
						autoScroll = false
						changed = true
					case 'B': // down
						scrollOffset++
						changed = true
					case '5': // page up (\033[5~)
						_, h := termSize()
						scrollOffset -= h - headerHeight
						autoScroll = false
						changed = true
						if i+3 < len(input) && input[i+3] == '~' {
							i++
						}
					case '6': // page down (\033[6~)
						_, h := termSize()
						scrollOffset += h - headerHeight
						changed = true
						if i+3 < len(input) && input[i+3] == '~' {
							i++
						}
					}
					i += 2
					continue
				}
				switch input[i] {
				case 'k': // vim up
					scrollOffset--
					autoScroll = false
					changed = true
				case 'j': // vim down
					scrollOffset++
					changed = true
				case 'G': // vim end
					autoScroll = true
					changed = true
				case 'g': // vim home
					scrollOffset = 0
					autoScroll = false
					changed = true
				}
			}
			if changed {
				render(true)
			}
		}
	}
}

// renderStatus builds the status dashboard output as a string.
// It can be called from both the live dashboard (in-process state) and
// the statusCmd (state fetched via HTTP from a running node).
func renderStatus(cfg *config.Config, activeState map[string]state.Component, metricsMap map[string]*state.Metrics, nodeName string) (string, int) {
	var w strings.Builder

	writeln := func(s string) { w.WriteString(s); w.WriteByte('\n') }

	getComponentInfo := func(compType, id string) (string, string, state.Component) {
		if activeState == nil {
			return "⚪️", cGray + "[starting]" + cReset, state.Component{}
		}
		comp, ok := activeState[compType+":"+id]
		if !ok {
			return "⚪️", cGray + "[starting]" + cReset, state.Component{}
		}
		color := cGray
		indicator := "⚪️"
		switch comp.Status {
		case state.Listening, state.Connected:
			color = cGreen
			indicator = "🟢"
		case state.Connecting, state.Retrying:
			color = cYellow
			indicator = "🟡"
		case state.Failed:
			color = cRed
			indicator = "🔴"
		}
		msg := string(comp.Status)
		if comp.Message != "" {
			if comp.Status == state.Failed || comp.Status == state.Retrying {
				msg += " (" + comp.Message + ")"
			}
		}
		return indicator, color + "[" + msg + "]" + cReset, comp
	}

	colorAddr := func(addr string) string {
		if addr == "" {
			return ""
		}
		// Extract optional user@ prefix
		user := ""
		hostPort := addr
		if u, h, ok := strings.Cut(addr, "@"); ok {
			user, hostPort = u, h
		}
		host, port, err := net.SplitHostPort(hostPort)
		if err != nil {
			// No port — plain host or unparseable
			return cCyan + addr + cReset
		}
		prefix := ""
		if user != "" {
			prefix = cGray + user + "@" + cReset
		}
		return prefix + cCyan + host + cReset + cGray + ":" + cReset + cMagenta + port + cReset
	}

	type row struct {
		isHeader   bool
		text       string
		indent     string
		indicator  string
		left       string
		arrow      string
		right      string
		status     string          // bracket status like [listening], [connected]
		annotation string          // gray info like (server/tunnel/fwd) or (peer addr)
		metrics    string          // formatted counters (populated during layout)
		snap       metricsSnapshot // raw metrics for deferred formatting
	}
	var rows []row

	addHeader := func(text string) {
		rows = append(rows, row{isHeader: true, text: text})
	}
	addRow := func(indent, ind, left, arrow, right, status, annotation string, snap metricsSnapshot) {
		rows = append(rows, row{indent: indent, indicator: ind, left: left, arrow: arrow, right: right, status: status, annotation: annotation, snap: snap})
	}

	arrowRight := cCyan + "──▶" + cReset
	arrowLeft := cMagenta + "◀──" + cReset

	// Compute grand total for the header
	var grandTotal metricsSnapshot
	for _, m := range metricsMap {
		grandTotal.add(readMetrics(m))
	}
	titleBase := cBold + "⚙ Configuration: " + cCyan + nodeName + cReset

	// Pre-scan: find the widest bind address among protocol-tagged rows for alignment.
	maxProtoAddr := 0
	for _, l := range cfg.Listeners {
		if l.Type == "socks" || l.Type == "http" || l.Type == "sshd" {
			if n := len(l.Bind); n > maxProtoAddr {
				maxProtoAddr = n
			}
		}
	}
	for _, conn := range cfg.Connections {
		for _, fset := range conn.Forwards {
			for _, fwd := range fset.Local {
				if fwd.Type == "socks" || fwd.Type == "http" {
					if n := len(fwd.Bind); n > maxProtoAddr {
						maxProtoAddr = n
					}
				}
			}
			for _, fwd := range fset.Remote {
				if fwd.Type == "socks" || fwd.Type == "http" {
					if n := len(fwd.Bind); n > maxProtoAddr {
						maxProtoAddr = n
					}
				}
			}
		}
	}

	padForProto := func(colored string) string {
		if pad := maxProtoAddr - visibleLen(colored); pad > 0 {
			return colored + strings.Repeat(" ", pad)
		}
		return colored
	}

	dynamicByParent := make(map[string][]state.Component)
	for k, comp := range activeState {
		if strings.HasPrefix(k, "dynamic:") {
			parts := strings.Split(comp.ID, "|")
			if len(parts) == 2 {
				parentBind := parts[1]
				_, port, err := net.SplitHostPort(parentBind)
				if err == nil {
					dynamicByParent[port] = append(dynamicByParent[port], comp)
				}
				dynamicByParent[parentBind] = append(dynamicByParent[parentBind], comp)
			}
		}
	}

	// Strip IPv6 zone IDs (e.g. %en0) which are interface-specific and noisy.
	cleanIPv6 := func(peer string) string {
		if idx := strings.Index(peer, "%"); idx != -1 {
			// Find the closing bracket or port separator after the zone
			rest := ""
			if endIdx := strings.IndexAny(peer[idx:], "]:"); endIdx != -1 {
				rest = peer[idx+endIdx:]
			}
			peer = peer[:idx] + rest
		}
		return peer
	}

	sectionTitle := func(name string) string {
		return cBold + cCyan + name + cReset
	}

	// --- Build rows for each section ---

	if len(cfg.Clipsync) > 0 {
		addHeader(sectionTitle("clipsync"))
		for _, cs := range cfg.Clipsync {
			indicator, st, comp := getComponentInfo("clipsync", cs.Bind)
			addRow("", indicator, colorAddr(cs.Bind), "", "", st, "", readMetrics(metricsMap["clipsync:"+cs.Bind]))
			// Show last clipboard activity if available.
			if comp.Message != "" {
				addRow("   ", "⌁", cGray+comp.Message+cReset, "", "", "", "", metricsSnapshot{})
			}

			type peerEntry struct{ addr, label string }
			var peerList []peerEntry
			prefix := "clipsync-peer:" + cs.Bind + "|"
			if activeState != nil {
				for k, comp := range activeState {
					if strings.HasPrefix(k, prefix) {
						peerList = append(peerList, peerEntry{strings.TrimPrefix(k, prefix), comp.Message})
					}
				}
				sort.Slice(peerList, func(i, j int) bool { return compareAddr(peerList[i].addr, peerList[j].addr) })
			} else {
				for _, addr := range cs.StaticPeers {
					peerList = append(peerList, peerEntry{addr, "static"})
				}
			}
			for _, p := range peerList {
				icon := "~"
				if p.label == "static" {
					icon = "·"
				}
				addRow("   ", icon, colorAddr(p.addr), "", cGray+p.label+cReset, "", "", metricsSnapshot{})
			}
		}
		addHeader("")
	}

	if len(cfg.Filesync) > 0 {
		addHeader(sectionTitle("filesync"))
		for _, fs := range cfg.Filesync {
			indicator, st, _ := getComponentInfo("filesync", fs.Bind)
			addRow("", indicator, colorAddr(fs.Bind), "", "", st, "", readMetrics(metricsMap["filesync:"+fs.Bind]))

			// Peers — show each named peer once with aggregated state across all folders.
			if len(fs.Peers) > 0 {
				addHeader("  " + cGray + "peers" + cReset)
				var peerNames []string
				for name := range fs.Peers {
					peerNames = append(peerNames, name)
				}
				sort.Strings(peerNames)

				maxNameLen := 0
				for _, name := range peerNames {
					if len(name) > maxNameLen {
						maxNameLen = len(name)
					}
				}

				for _, name := range peerNames {
					addrs := fs.Peers[name]
					bestInd, bestSt := "⚪️", cGray+"[starting]"+cReset
					bestPrio := 0
					if activeState != nil {
						for _, addr := range addrs {
							for _, folder := range fs.ResolvedFolders {
								if folder.Direction == "disabled" {
									continue
								}
								ind, pst, comp := getComponentInfo("filesync-peer", folder.ID+"|"+addr)
								prio := 0
								switch comp.Status {
								case state.Connected, state.Listening:
									prio = 3
								case state.Connecting:
									prio = 2
								case state.Retrying, state.Failed:
									prio = 1
								}
								if prio > bestPrio {
									bestPrio = prio
									bestInd = ind
									bestSt = pst
								}
							}
						}
					}
					paddedName := cBold + name + cReset + strings.Repeat(" ", maxNameLen-len(name))
					addRow("    ", bestInd, paddedName+"  "+colorAddr(addrs[0]), "", "", bestSt, "", metricsSnapshot{})
				}
			}

			// Folders — direction symbol, aligned paths, file count, last sync time.
			if len(fs.ResolvedFolders) > 0 {
				addHeader("  " + cGray + "folders" + cReset)

				maxIDLen := 0
				for _, folder := range fs.ResolvedFolders {
					if len(folder.ID) > maxIDLen {
						maxIDLen = len(folder.ID)
					}
				}

				for _, folder := range fs.ResolvedFolders {
					dirSym := directionSymbol(folder.Direction)
					_, _, comp := getComponentInfo("filesync-folder", folder.ID)

					var fSt string
					switch {
					case folder.Direction == "disabled":
						fSt = cGray + "[disabled]" + cReset
					case activeState == nil:
						fSt = cGray + "[starting]" + cReset
					case comp.Status == state.Connected:
						fSt = cGreen + "[idle]" + cReset
					case comp.Status == state.Connecting:
						if comp.Message == "scanning" {
							fSt = cYellow + "[scanning]" + cReset
						} else {
							fSt = cYellow + "[syncing]" + cReset
						}
					case comp.Status == state.Retrying:
						fSt = cYellow + "[retrying]" + cReset
					case comp.Status == state.Failed:
						fSt = cRed + "[failed]" + cReset
					default:
						fSt = cGray + "[starting]" + cReset
					}

					// Build a combined status string: "[idle] 1234 files  synced 5m ago"
					if comp.FileCount > 0 {
						fSt += " " + cGray + fmt.Sprintf("%d files", comp.FileCount) + cReset
					}
					if !comp.LastSync.IsZero() {
						ago := time.Since(comp.LastSync).Truncate(time.Second)
						fSt += "  " + cGray + "synced " + formatDuration(ago) + " ago" + cReset
					}

					paddedID := folder.ID + strings.Repeat(" ", maxIDLen-len(folder.ID))
					left := paddedID + "  " + cGray + folder.Path + cReset
					addRow("    ", dirSym, left, "", "", fSt, "", metricsSnapshot{})
				}
			}
		}
		addHeader("")
	}

	if len(cfg.Listeners) > 0 {
		addHeader(sectionTitle("listeners"))
		for _, l := range cfg.Listeners {
			switch l.Type {
			case "sshd":
				indicator, st, _ := getComponentInfo("server", l.Bind)
				// Aggregate server's own metrics + all dynamic reverse forward metrics
				serverAgg := readMetrics(metricsMap["server:"+l.Bind])
				_, sp, _ := net.SplitHostPort(l.Bind)
				ld := dynamicByParent[l.Bind]
				if len(ld) == 0 {
					ld = dynamicByParent[sp]
				}
				for _, comp := range ld {
					serverAgg.add(readMetrics(metricsMap["dynamic:"+comp.ID]))
				}
				left := padForProto(colorAddr(l.Bind)) + " " + cReset + strings.ToLower(l.Type)
				addRow("", indicator, left, "", "", st, "", serverAgg)
			case "relay":
				indicator, st, _ := getComponentInfo("relay", l.Bind)
				addRow("", indicator, colorAddr(l.Bind), arrowRight, colorAddr(l.Target), st, "", readMetrics(metricsMap["relay:"+l.Bind]))
			default:
				indicator, st, _ := getComponentInfo("proxy", l.Bind)
				left := padForProto(colorAddr(l.Bind)) + " " + cReset + strings.ToLower(l.Type)
				arrow, right := "", ""
				if l.Target != "" {
					right = colorAddr(l.Target)
					arrow = arrowRight
				}
				addRow("", indicator, left, arrow, right, st, "", readMetrics(metricsMap["proxy:"+l.Bind]))
			}

			_, searchPort, err := net.SplitHostPort(l.Bind)
			if err != nil {
				searchPort = l.Bind
			}
			dyns := dynamicByParent[l.Bind]
			if len(dyns) == 0 {
				dyns = dynamicByParent[searchPort]
			}
			if len(dyns) > 0 {
				sort.Slice(dyns, func(i, j int) bool {
					a := strings.SplitN(dyns[i].ID, "|", 2)[0]
					b := strings.SplitN(dyns[j].ID, "|", 2)[0]
					return compareAddr(a, b)
				})
				seenID := make(map[string]bool)
				for _, comp := range dyns {
					if seenID[comp.ID] {
						continue
					}
					seenID[comp.ID] = true
					parts := strings.Split(comp.ID, "|")
					annotation := ""
					if comp.PeerAddr != "" {
						annotation = formatPeerIdentity(comp.PeerAddr)
					}
					// Dynamic sub-rows show identity only — bytes are rolled up into
					// the parent sshd listener row, so per-row metrics would duplicate them.
					addRow("   ", "~", colorAddr(parts[0]), arrowRight, colorAddr(cleanIPv6(comp.Message)), "", annotation, metricsSnapshot{})
				}
			}
		}
		addHeader("")
	}

	if len(cfg.Connections) > 0 {
		for _, c := range cfg.Connections {
			// Connection name is a grouping header; metrics live on the
			// individual forward rows below.
			addRow("", "", sectionTitle(c.Name), "", "", "", "", metricsSnapshot{})

			type targetInfo struct {
				status   state.Status
				peerAddr string
			}
			connectedTargets := make(map[string]targetInfo)
			for _, fset := range c.Forwards {
				id := c.Name + " [" + fset.Name + "]"
				_, _, comp := getComponentInfo("connection", id)
				if comp.Message != "" {
					existing, seen := connectedTargets[comp.Message]
					// Connected takes priority over Connecting
					if !seen || (comp.Status == state.Connected && existing.status != state.Connected) {
						connectedTargets[comp.Message] = targetInfo{status: comp.Status, peerAddr: comp.PeerAddr}
					}
				}
			}
			for _, t := range c.Targets {
				ind := "○"
				annotation := ""
				if info, ok := connectedTargets[t]; ok {
					switch info.status {
					case state.Connected:
						ind = cGreen + "●" + cReset
						if info.peerAddr != "" && !strings.Contains(t, info.peerAddr) {
							annotation = cGray + "(" + info.peerAddr + ")" + cReset
						}
					case state.Connecting, state.Retrying:
						ind = cBlink + cYellow + "●" + cReset
					}
				}
				addRow(" ", ind, colorAddr(t), "", "", "", annotation, metricsSnapshot{})
			}

			for _, fset := range c.Forwards {
				id := c.Name + " [" + fset.Name + "]"
				indicator, st, comp := getComponentInfo("connection", id)
				// Forward-set name is a grouping header; metrics live on the
				// individual forward rows below.
				addRow("", indicator, sectionTitle(fset.Name), "", "", st, "", metricsSnapshot{})

				// Always show a target line under each forward set
				{
					ind := "○"
					var targetStr string
					targetAnnotation := ""
					switch comp.Status {
					case state.Connected:
						ind = cGreen + "●" + cReset
						targetStr = colorAddr(comp.Message)
						if comp.PeerAddr != "" && !strings.Contains(comp.Message, comp.PeerAddr) {
							targetAnnotation = cGray + "(" + comp.PeerAddr + ")" + cReset
						}
					case state.Connecting:
						ind = cBlink + cYellow + "●" + cReset
						targetStr = cGray + "[connecting]" + cReset
					case state.Retrying:
						ind = cBlink + cYellow + "●" + cReset
						if comp.Message != "" {
							targetStr = cYellow + comp.Message + cReset
						} else {
							targetStr = cGray + "[retrying]" + cReset
						}
					case state.Failed:
						ind = cRed + "✕" + cReset
						if comp.Message != "" {
							targetStr = cRed + comp.Message + cReset
						} else {
							targetStr = cRed + "[failed]" + cReset
						}
					default:
						targetStr = cGray + "[starting]" + cReset
					}
					addRow("   ", ind, targetStr, "", "", "", targetAnnotation, metricsSnapshot{})
				}

				indent := "   "
				for _, fwd := range fset.Local {
					compID := c.Name + " [" + fset.Name + "] " + fwd.Bind
					_, _, fwdComp := getComponentInfo("forward", compID)
					snap := readMetrics(metricsMap["forward:"+compID])
					lStr := colorAddr(fwd.Bind)
					if fwdComp.BoundAddr != "" && fwdComp.BoundAddr != fwd.Bind {
						lStr = colorAddr(fwdComp.BoundAddr) + " " + cGray + "(from " + fwd.Bind + ")" + cReset
					}
					if fwd.Type == "forward" {
						addRow(indent, "", lStr, arrowRight, colorAddr(fwd.Target), "", "", snap)
					} else {
						lStr = padForProto(lStr) + " " + cReset + strings.ToLower(fwd.Type)
						rStr := cGray + "🔒 tunnel" + cReset
						if fwd.Target != "" {
							rStr = colorAddr(fwd.Target)
						}
						addRow(indent, "", lStr, arrowRight, rStr, "", "", snap)
					}
				}
				for _, fwd := range fset.Remote {
					compID := c.Name + " [" + fset.Name + "] " + fwd.Bind
					snap := readMetrics(metricsMap["forward:"+compID])
					if fwd.Type == "forward" {
						addRow(indent, "", colorAddr(fwd.Target), arrowLeft, colorAddr(fwd.Bind), "", "", snap)
					} else {
						lStr := cGray + "🔒 tunnel" + cReset
						if fwd.Target != "" {
							lStr = colorAddr(fwd.Target)
						}
						rStr := padForProto(colorAddr(fwd.Bind)) + " " + cReset + strings.ToLower(fwd.Type)
						addRow(indent, "", lStr, arrowLeft, rStr, "", "", snap)
					}
				}
			}
		}
		addHeader("")
	}

	var unmappedDynamic []state.Component
	for k, comp := range activeState {
		if strings.HasPrefix(k, "dynamic:") {
			parts := strings.Split(comp.ID, "|")
			if len(parts) != 2 {
				unmappedDynamic = append(unmappedDynamic, comp)
			}
		}
	}
	if len(unmappedDynamic) > 0 {
		sort.Slice(unmappedDynamic, func(i, j int) bool {
			return compareAddr(unmappedDynamic[i].ID, unmappedDynamic[j].ID)
		})
		addHeader(cMagenta + "dynamic ports (unmapped)" + cReset)
		for _, comp := range unmappedDynamic {
			addRow("", "↳", colorAddr(comp.ID), arrowRight, colorAddr(cleanIPv6(comp.Message)), "", "", metricsSnapshot{})
		}
		addHeader("")
	}

	// --- Layout alignment ---

	maxTotalLeft := 0
	for _, r := range rows {
		if !r.isHeader && r.left != "" && r.arrow != "" {
			l := len(r.indent)
			if r.indicator != "" {
				l += visibleLen(r.indicator) + 1 // indicator + space
			}
			l += visibleLen(r.left)
			if l > maxTotalLeft {
				maxTotalLeft = l
			}
		}
	}

	maxLineLen := 0
	for i, r := range rows {
		if !r.isHeader {
			line := r.indent
			if r.indicator != "" {
				line += r.indicator + " "
			}
			line += r.left
			if r.arrow != "" || r.right != "" {
				currentLen := visibleLen(line)
				padLen := 0
				if maxTotalLeft > currentLen {
					padLen = maxTotalLeft - currentLen
				}
				line += strings.Repeat(" ", padLen+1) + r.arrow + " " + r.right
			}
			rows[i].text = line
			if l := visibleLen(line); l > maxLineLen {
				maxLineLen = l
			}
		}
	}

	statusPadCol := maxLineLen + 1
	if statusPadCol > 60 {
		statusPadCol = 60
	}

	// Compute metrics column: based on content + status + annotation width.
	hasSnap := func(s metricsSnapshot) bool {
		return s.uptime > 0 || s.tx > 0 || s.rx > 0
	}
	metricsPadCol := 0
	for _, r := range rows {
		if r.isHeader || !hasSnap(r.snap) {
			continue
		}
		lineLen := visibleLen(r.text)
		statusStart := statusPadCol
		if lineLen >= statusStart {
			statusStart = lineLen + 1
		}
		col := statusStart + visibleLen(r.status)
		if r.annotation != "" {
			col += 1 + visibleLen(r.annotation) // space + annotation
		}
		if col > metricsPadCol {
			metricsPadCol = col
		}
	}
	metricsPadCol++ // at least one space before metrics

	// Compute max tx/rx byte string widths for column alignment across rows.
	maxTxWidth, maxRxWidth := 0, 0
	for _, r := range rows {
		if r.isHeader || !hasSnap(r.snap) {
			continue
		}
		if l := len(formatBytes(r.snap.tx)); l > maxTxWidth {
			maxTxWidth = l
		}
		if l := len(formatBytes(r.snap.rx)); l > maxRxWidth {
			maxRxWidth = l
		}
	}
	if hasSnap(grandTotal) {
		if l := len(formatBytes(grandTotal.tx)); l > maxTxWidth {
			maxTxWidth = l
		}
		if l := len(formatBytes(grandTotal.rx)); l > maxRxWidth {
			maxRxWidth = l
		}
	}

	// Format row metrics with aligned tx/rx columns.
	for i, r := range rows {
		if r.isHeader || !hasSnap(r.snap) {
			continue
		}
		rows[i].metrics = formatMetricsAligned(r.snap, maxTxWidth, maxRxWidth)
	}

	// Build final lines, then compute separator width from actual content.
	var builtLines []string

	// Title: align ↑↓ with the ↑↓ in row metrics (skip the duration column).
	titleLine := titleBase
	if hasSnap(grandTotal) {
		durationWidth := 7 // formatMetricsAligned uses "%-6s " for duration
		padTo := metricsPadCol + durationWidth
		titleLen := visibleLen(titleLine)
		if titleLen < padTo {
			titleLine += strings.Repeat(" ", padTo-titleLen)
		} else {
			titleLine += " "
		}
		txRaw := formatBytes(grandTotal.tx)
		txPad := ""
		if p := maxTxWidth - len(txRaw); p > 0 {
			txPad = strings.Repeat(" ", p)
		}
		titleLine += cCyan + "↑" + colorBytes(grandTotal.tx, cCyan) + txPad + " " + cMagenta + "↓" + colorBytes(grandTotal.rx, cMagenta) + cReset
	}
	builtLines = append(builtLines, titleLine)

	// When metrics are present, right-align status+annotation so they end
	// exactly one space before the metrics column. This keeps the gap between
	// status and metrics consistent across rows with different status widths.
	anyMetrics := metricsPadCol > 1

	for _, r := range rows {
		if r.isHeader {
			builtLines = append(builtLines, r.text)
			continue
		}
		line := r.text

		// Build status block: status + optional annotation (when either status
		// or annotation should be right-aligned alongside metrics).
		statusBlock := ""
		switch {
		case r.status != "" && r.annotation != "":
			statusBlock = r.status + " " + r.annotation
		case r.status != "":
			statusBlock = r.status
		case r.annotation != "" && r.metrics != "":
			// Annotation without status, but with metrics: treat annotation as
			// part of the right-aligned block so it stays close to metrics.
			statusBlock = r.annotation
		}

		if anyMetrics && statusBlock != "" {
			// Right-align: status block ends at metricsPadCol - 1.
			sbWidth := visibleLen(statusBlock)
			targetStart := metricsPadCol - 1 - sbWidth
			lineLen := visibleLen(line)
			if targetStart > lineLen {
				line += strings.Repeat(" ", targetStart-lineLen)
			} else {
				line += " "
			}
			line += statusBlock
		} else if statusBlock != "" {
			// No metrics context — left-align status at statusPadCol.
			lineLen := visibleLen(line)
			if lineLen < statusPadCol {
				line += strings.Repeat(" ", statusPadCol-lineLen)
			} else {
				line += " "
			}
			line += statusBlock
		}

		switch {
		case r.metrics != "":
			currentLen := visibleLen(line)
			if currentLen < metricsPadCol {
				line += strings.Repeat(" ", metricsPadCol-currentLen)
			} else {
				line += " "
			}
			line += r.metrics
		case r.annotation != "" && r.status == "" && !anyMetrics:
			// Annotation-only row without metrics context: append inline.
			line += " " + r.annotation
		case r.annotation != "" && r.status == "" && statusBlock == "":
			// Annotation-only row (no metrics on this row, but metrics elsewhere):
			// still append inline since it's not part of a right-aligned block.
			line += " " + r.annotation
		}

		builtLines = append(builtLines, strings.TrimRight(line, " "))
	}

	// Separator as wide as the widest visible line.
	maxWidth := 0
	for _, line := range builtLines {
		if vw := visibleLen(line); vw > maxWidth {
			maxWidth = vw
		}
	}
	if maxWidth < 80 {
		maxWidth = 80
	}
	separator := cGray + strings.Repeat("─", maxWidth) + cReset

	// Write title, separator, then all rows.
	writeln(builtLines[0]) // title
	writeln(separator)
	for _, line := range builtLines[1:] {
		writeln(line)
	}

	return w.String(), maxWidth
}
