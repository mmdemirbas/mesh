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

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/state"
	"golang.org/x/term"
)

// logRing is a fixed-size ring buffer that captures recent log lines for the dashboard.
type logRing struct {
	mu    sync.Mutex
	lines []string
	size  int
	pos   int
	full  bool
}

func newLogRing(size int) *logRing {
	return &logRing{lines: make([]string, size), size: size}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Split into lines; log handlers write one record at a time but may include a trailing newline.
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		r.lines[r.pos] = line
		r.pos = (r.pos + 1) % r.size
		if r.pos == 0 {
			r.full = true
		}
	}
	return len(p), nil
}

// Lines returns the buffered log lines in chronological order.
func (r *logRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string{}, r.lines[:r.pos]...)
	}
	out := make([]string, 0, r.size)
	out = append(out, r.lines[r.pos:]...)
	out = append(out, r.lines[:r.pos]...)
	return out
}

// runDashboard renders a live status screen that refreshes periodically.
// It uses the terminal's alternate screen buffer (like vim, top, htop) so the
// dashboard doesn't pollute scrollback and the user's previous terminal content
// is restored when the dashboard exits. Rendering overwrites in-place line by
// line to avoid flicker — no full screen clear is needed.
func runDashboard(ctx context.Context, cfgs map[string]*config.Config, nodeNames []string, configPath string, logFilePath string, ring *logRing) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	startTime := time.Now()

	// Enter alternate screen buffer and hide cursor
	fmt.Print("\033[?1049h\033[?25l")
	defer fmt.Print("\033[?25h\033[?1049l") // show cursor, leave alternate screen

	render := func() {
		var lines []string

		// Header
		uptime := time.Since(startTime).Truncate(time.Second)
		nodesLabel := strings.Join(nodeNames, ", ")
		header := fmt.Sprintf("%s%smesh %s%s | pid %d | %s | up %s",
			cBold, cCyan, nodesLabel, cReset, os.Getpid(), time.Now().Format("15:04:05"), uptime)
		lines = append(lines, header)
		if configPath != "" {
			lines = append(lines, fmt.Sprintf("  %sconfig: %s%s", cGray, configPath, cReset))
		}
		if logFilePath != "" {
			lines = append(lines, fmt.Sprintf("  %slog:    %s%s", cGray, logFilePath, cReset))
		}
		lines = append(lines, "")

		// Status body — render each node
		snap := state.Global.Snapshot()
		metrics := state.Global.SnapshotMetrics()
		var maxWidth int
		for _, name := range nodeNames {
			statusOutput, statusWidth := renderStatus(cfgs[name], snap, metrics, name)
			lines = append(lines, strings.Split(strings.TrimRight(statusOutput, "\n"), "\n")...)
			if statusWidth > maxWidth {
				maxWidth = statusWidth
			}
		}

		// Log tail — fill remaining terminal height
		logLines := ring.Lines()
		if len(logLines) > 0 {
			termHeight := 24
			if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h > 0 {
				termHeight = h
			}
			available := termHeight - len(lines) - 1 // -1 for separator
			if available > len(logLines) {
				available = len(logLines)
			}
			if available > 0 {
				lines = append(lines, cGray+strings.Repeat("─", maxWidth)+cReset)
				lines = append(lines, logLines[len(logLines)-available:]...)
			}
		}

		// Overwrite in-place: cursor home, then each line + clear-to-EOL.
		// After all lines, clear from cursor to end of screen (removes stale content).
		var buf strings.Builder
		buf.WriteString("\033[H") // cursor home — no clear
		for _, line := range lines {
			buf.WriteString(line)
			buf.WriteString("\033[K\n") // clear to end of line, then newline
		}
		buf.WriteString("\033[J") // clear from cursor to end of screen
		fmt.Print(buf.String())
	}

	winch := winchSignal()

	render()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			render()
		case <-winch:
			render()
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

	visibleLen := func(str string) int {
		stripped := ansiStripRe.ReplaceAllString(str, "")
		n := 0
		for _, r := range stripped {
			// Emoji indicators (🟢🟡🔴⚪) are 2-column-wide in terminals
			if r >= 0x1F000 {
				n += 2
			} else {
				n++
			}
		}
		return n
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
	titleBase := fmt.Sprintf("%s⚙ Configuration: %s%s%s", cBold, cCyan, nodeName, cReset)

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
			indicator, st, _ := getComponentInfo("clipsync", cs.Bind)
			addRow("", indicator, colorAddr(cs.Bind), "", "", st, "", metricsSnapshot{})

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
					addRow("   ", "~", colorAddr(parts[0]), arrowRight, colorAddr(cleanIPv6(comp.Message)), "", annotation, readMetrics(metricsMap["dynamic:"+comp.ID]))
				}
			}
		}
		addHeader("")
	}

	if len(cfg.Connections) > 0 {
		for _, c := range cfg.Connections {
			// Pre-compute connection-level aggregate metrics
			var connAgg metricsSnapshot
			for _, fset := range c.Forwards {
				for _, fwd := range fset.Local {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					connAgg.add(readMetrics(metricsMap["forward:"+compID]))
				}
				for _, fwd := range fset.Remote {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					connAgg.add(readMetrics(metricsMap["forward:"+compID]))
				}
			}
			addRow("", "", sectionTitle(c.Name), "", "", "", "", connAgg)

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
				// Aggregate forward-set metrics from child forwards
				var fsetAgg metricsSnapshot
				for _, fwd := range fset.Local {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					fsetAgg.add(readMetrics(metricsMap["forward:"+compID]))
				}
				for _, fwd := range fset.Remote {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					fsetAgg.add(readMetrics(metricsMap["forward:"+compID]))
				}
				addRow("", indicator, sectionTitle(fset.Name), "", "", st, "", fsetAgg)

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
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
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
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
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

		if r.metrics != "" {
			currentLen := visibleLen(line)
			if currentLen < metricsPadCol {
				line += strings.Repeat(" ", metricsPadCol-currentLen)
			} else {
				line += " "
			}
			line += r.metrics
		} else if r.annotation != "" && r.status == "" && !anyMetrics {
			// Annotation-only row without metrics context: append inline.
			line += " " + r.annotation
		} else if r.annotation != "" && r.status == "" && statusBlock == "" {
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
