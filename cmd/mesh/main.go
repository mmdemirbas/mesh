package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/lmittmann/tint"
	"github.com/mmdemirbas/mesh/internal/clipsync"
	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/proxy"
	"github.com/mmdemirbas/mesh/internal/state"
	"github.com/mmdemirbas/mesh/internal/tunnel"
)

var version = "dev"

var ansiStripRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// addrKey is a pre-parsed, comparable sort key for an address string.
// Using a fixed-size [16]byte array avoids heap allocation from net.ParseIP.
type addrKey struct {
	ip    [16]byte // IPv4-mapped-to-IPv6 or raw IPv6
	port  uint16
	hasIP bool
	raw   string // original string, used as fallback for non-IP addresses
}

// makeAddrKey parses an address string into a sort key in a single pass.
func makeAddrKey(s string) addrKey {
	raw := s
	if at := strings.LastIndex(s, "@"); at != -1 {
		s = s[at+1:]
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		host = s
		portStr = ""
	}
	port, _ := strconv.Atoi(portStr)
	k := addrKey{port: uint16(port), raw: raw}

	// Fast path: try parsing IPv4 without allocation
	if ip4 := parseIPv4(host); ip4 != [4]byte{} || host == "0.0.0.0" {
		// IPv4-mapped IPv6: ::ffff:x.x.x.x
		k.ip[10] = 0xff
		k.ip[11] = 0xff
		k.ip[12] = ip4[0]
		k.ip[13] = ip4[1]
		k.ip[14] = ip4[2]
		k.ip[15] = ip4[3]
		k.hasIP = true
		return k
	}

	// Slow path: IPv6 or unparseable
	if ip := net.ParseIP(host); ip != nil {
		copy(k.ip[:], ip.To16())
		k.hasIP = true
	}
	return k
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
		if c >= '0' && c <= '9' {
			octet = octet*10 + int(c-'0')
			if octet > 255 {
				return [4]byte{}
			}
			digits++
		} else if c == '.' {
			if digits == 0 || dots >= 3 {
				return [4]byte{}
			}
			ip[dots] = byte(octet)
			dots++
			octet = 0
			digits = 0
		} else {
			return [4]byte{}
		}
	}
	if dots != 3 || digits == 0 {
		return [4]byte{}
	}
	ip[3] = byte(octet)
	return ip
}

func (k addrKey) less(other addrKey) bool {
	if k.hasIP && other.hasIP {
		cmp := bytes.Compare(k.ip[:], other.ip[:])
		if cmp != 0 {
			return cmp < 0
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
	return net.IP(k.ip[:]), int(k.port)
}

// compareAddr compares two address strings semantically by IP then port.
func compareAddr(a, b string) bool {
	return makeAddrKey(a).less(makeAddrKey(b))
}

// sortAddrs sorts a string slice of addresses semantically by IP then port.
// It pre-parses all addresses once, avoiding repeated parsing inside the comparator.
func sortAddrs(addrs []string) {
	keys := make([]addrKey, len(addrs))
	for i, a := range addrs {
		keys[i] = makeAddrKey(a)
	}
	sort.Sort(addrSorter{addrs: addrs, keys: keys})
}

type addrSorter struct {
	addrs []string
	keys  []addrKey
}

func (s addrSorter) Len() int           { return len(s.addrs) }
func (s addrSorter) Less(i, j int) bool { return s.keys[i].less(s.keys[j]) }
func (s addrSorter) Swap(i, j int) {
	s.addrs[i], s.addrs[j] = s.addrs[j], s.addrs[i]
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
}

func main() {
	var configPath string
	flag.StringVar(&configPath, "f", "", "Path to config file")
	flag.StringVar(&configPath, "file", "", "Path to config file")
	flag.Usage = printUsage
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		printUsage()
		os.Exit(1)
	}

	nodeName := args[0]
	command := args[1]

	if configPath == "" {
		configPath = getDefaultConfigPath()
	}

	switch command {
	case "up":
		upCmd(nodeName, configPath)
	case "status":
		statusCmd(nodeName, configPath)
	case "down":
		downCmd(nodeName)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	const (
		reset  = "\033[0m"
		bold   = "\033[1m"
		blue   = "\033[34m"
		cyan   = "\033[36m"
		gray   = "\033[90m"
		yellow = "\033[33m"
	)
	fmt.Println(bold + cyan + "mesh" + reset + " " + gray + version + reset + " - Human-friendly networking tool")
	fmt.Println()
	fmt.Println("All-in-one replacement for ssh, sshd, autossh, socat, and SOCKS/HTTP proxy servers.")
	fmt.Println()
	fmt.Println(bold + "Usage:" + reset)
	fmt.Println("  mesh " + yellow + "[-f config.yaml] " + reset + cyan + "<node> <command>" + reset + " [arguments]")
	fmt.Println()
	fmt.Println(bold + "Commands:" + reset)
	fmt.Println("  " + blue + "up" + reset + "     Start the specified mesh node")
	fmt.Println("  " + blue + "down" + reset + "   Stop the currently running mesh node")
	fmt.Println("  " + blue + "status" + reset + " Check if the mesh node is running and show its active configuration")
	fmt.Println()
	fmt.Println(bold + "Examples:" + reset)
	fmt.Println("  " + gray + "# Start the 'server' node using the default configuration file" + reset)
	fmt.Println("  mesh server " + blue + "up" + reset + " &")
	fmt.Println()
	fmt.Println("  " + gray + "# Start utilizing a specific configuration file" + reset)
	fmt.Println("  mesh " + yellow + "-f" + reset + " configs/example.yml server " + blue + "up" + reset)
	fmt.Println()
	fmt.Println("  " + gray + "# Gracefully stop the 'server' node" + reset)
	fmt.Println("  mesh server " + blue + "down" + reset)
	fmt.Println()
}

func getDefaultConfigPath() string {
	// 1. Check current directory (prioritize yaml over yml)
	if _, err := os.Stat("mesh.yaml"); err == nil {
		return "mesh.yaml"
	}
	if _, err := os.Stat("mesh.yml"); err == nil {
		return "mesh.yml"
	}

	// 2. Check ~/.mesh/conf
	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".mesh", "conf", "mesh.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		p = filepath.Join(home, ".mesh", "conf", "mesh.yml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Fallback to default
	return "mesh.yaml"
}

func upCmd(nodeName, configPath string) {
	pid, err := readPidFile(nodeName)
	if err == nil && pid != 0 && checkPid(pid) {
		fmt.Printf("⨯ mesh node %q is already running (pid %d).\n", nodeName, pid)
		os.Exit(1)
	}

	logHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: "15:04:05.000",
	})
	log := slog.New(&padMessageHandler{Handler: logHandler, width: 30})
	slog.SetDefault(log)

	cfg, err := config.Load(configPath, nodeName)
	if err != nil {
		log.Error("Config load failed", "path", configPath, "error", err)
		os.Exit(1)
	}

	var logLevel slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	logHandler = tint.NewHandler(os.Stderr, &tint.Options{
		Level:      logLevel,
		TimeFormat: "15:04:05.000",
	})
	log = slog.New(&padMessageHandler{Handler: logHandler, width: 30})
	slog.SetDefault(log)

	log.Info("mesh starting", "version", version, "node", nodeName, "config", configPath)

	if err := writePidFile(nodeName); err != nil {
		log.Error("Failed to write pidfile", "error", err)
	} else {
		defer removePidFile(nodeName)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("Shutting down", "signal", sig)
		cancel()
	}()

	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		port := adminLn.Addr().(*net.TCPAddr).Port
		os.WriteFile(portFilePath(nodeName), []byte(strconv.Itoa(port)), 0600)
		defer os.Remove(portFilePath(nodeName))

		adminSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(state.Global.Snapshot())
		})}
		go adminSrv.Serve(adminLn)
		// Ensure admin server shuts down when context is cancelled
		context.AfterFunc(ctx, func() { adminSrv.Close() })
	}

	var wg sync.WaitGroup

	// 1. Listeners (proxies, relays, ssh servers)
	var proxies, relays []config.Listener
	for _, l := range cfg.Listeners {
		switch l.Type {
		case "socks", "http":
			proxies = append(proxies, l)
		case "relay":
			relays = append(relays, l)
		case "sshd":
			l := l
			wg.Add(1)
			go func() {
				defer wg.Done()
				s := tunnel.NewSSHServer(l, log)
				if err := s.Run(ctx); err != nil {
					log.Error("SSH server failed", "listen", l.Bind, "error", err)
				}
			}()
		}
	}
	if len(proxies) > 0 {
		proxy.RunStandaloneProxies(ctx, proxies, log, &wg)
	}
	if len(relays) > 0 {
		proxy.RunStandaloneRelays(ctx, relays, log, &wg)
	}

	// 4. Outbound connections (Multi-set forwards)
	for _, conn := range cfg.Connections {
		conn := conn
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := tunnel.NewSSHClient(conn, log)
			if err := c.Run(ctx); err != nil {
				log.Error("Connection failed", "name", conn.Name, "error", err)
			}
		}()
	}

	// 5. Clipsync
	for _, cs := range cfg.Clipsync {
		cs := cs
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := clipsync.Start(ctx, cs)
			if err != nil {
				log.Error("Clipsync failed to start", "error", err)
			}
		}()
	}

	// Block until a signal triggers context cancellation
	<-ctx.Done()

	// Wait a moment for graceful shutdown of spawned servers/clients
	wg.Wait()
	log.Info("mesh gracefully stopped")
}

type padMessageHandler struct {
	slog.Handler
	width int
}

func (h *padMessageHandler) Handle(ctx context.Context, r slog.Record) error {
	if len(r.Message) < h.width {
		r.Message += strings.Repeat(" ", h.width-len(r.Message))
	}
	return h.Handler.Handle(ctx, r)
}

func statusCmd(nodeName, configPath string) {
	const (
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

	pid, err := readPidFile(nodeName)
	if err != nil || pid == 0 {
		fmt.Printf("%s⨯ mesh node %q is not running.%s\n", cRed, nodeName, cReset)
		os.Exit(3)
	}

	if !checkPid(pid) {
		fmt.Printf("%s⨯ mesh node %q is dead but pidfile exists (pid %d).%s\n", cRed, nodeName, pid, cReset)
		os.Exit(1)
	}

	fmt.Printf("%s✔ mesh node %q is running (pid %d).%s\n\n", cGreen, nodeName, pid, cReset)

	var activeState map[string]state.Component
	if portData, err := os.ReadFile(portFilePath(nodeName)); err == nil {
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/", string(portData))); err == nil {
			defer resp.Body.Close()
			json.NewDecoder(resp.Body).Decode(&activeState)
		}
	}

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

	logHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: "15:04:05.000",
	})
	log := slog.New(&padMessageHandler{Handler: logHandler, width: 30})
	slog.SetDefault(log)

	cfgs, err := config.LoadUnvalidated(configPath)
	if err != nil {
		fmt.Printf("%s⚠ Could not load configuration to show details: %v%s\n", cYellow, err, cReset)
		os.Exit(0)
	}
	cfg, ok := cfgs[nodeName]
	if !ok {
		fmt.Printf("%s⚠ Node %q not found in config%s\n", cYellow, nodeName, cReset)
		os.Exit(0)
	}

	fmt.Printf("%s⚙ Configuration: %s%s%s\n", cBold, cCyan, nodeName, cReset)
	fmt.Println(cGray + strings.Repeat("─", 80) + cReset)

	visibleLen := func(str string) int {
		return utf8.RuneCountInString(ansiStripRe.ReplaceAllString(str, ""))
	}

	colorAddr := func(addr string) string {
		if addr == "" {
			return ""
		}
		idx := strings.LastIndex(addr, ":")
		if idx == -1 {
			return cCyan + addr + cReset
		}
		host := addr[:idx]
		port := addr[idx+1:]

		atIdx := strings.Index(host, "@")
		if atIdx != -1 {
			user := host[:atIdx]
			host = host[atIdx+1:]
			return cGray + user + "@" + cReset + cCyan + host + cReset + cGray + ":" + cReset + cMagenta + port + cReset
		}
		return cCyan + host + cReset + cGray + ":" + cReset + cMagenta + port + cReset
	}

	type row struct {
		isHeader  bool
		text      string
		indent    string
		indicator string
		left      string
		arrow     string
		right     string
		status    string
	}
	var rows []row

	addHeader := func(text string) {
		rows = append(rows, row{isHeader: true, text: text})
	}
	addRow := func(indent, ind, left, arrow, right, st string) {
		rows = append(rows, row{indent: indent, indicator: ind, left: left, arrow: arrow, right: right, status: st})
	}

	arrowRight := cCyan + "──▶" + cReset
	arrowLeft := cMagenta + "◀──" + cReset

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

	// padForProto pads a colored address so protocol labels start at the same column.
	padForProto := func(colored string) string {
		if pad := maxProtoAddr - visibleLen(colored); pad > 0 {
			return colored + strings.Repeat(" ", pad)
		}
		return colored
	}

	// Map dynamic forwards by the listen port of their parent listener
	// to handle cases where 0.0.0.0 in config routes to 127.0.0.1 during actual connections.
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
				// Also store by full bind as fallback
				dynamicByParent[parentBind] = append(dynamicByParent[parentBind], comp)
			}
		}
	}

	cleanIPv6 := func(peer string) string {
		// Example: root@[fe80::92bd:3bfe:be7d:5b25%en0]:54633 -> root@fe80::92bd:3bfe:be7d:5b25:54633
		peer = strings.ReplaceAll(peer, "[", "")
		peer = strings.ReplaceAll(peer, "]", "")

		// Remove interface zone indexes like %en0 if they exist
		if idx := strings.Index(peer, "%"); idx != -1 {
			if colonIdx := strings.LastIndex(peer, ":"); colonIdx != -1 {
				// Keep the port
				peer = peer[:idx] + peer[colonIdx:]
			} else {
				peer = peer[:idx]
			}
		}
		return peer
	}

	sectionTitle := func(name string) string {
		return cBold + cCyan + name + cReset
	}

	if len(cfg.Clipsync) > 0 {
		addHeader(sectionTitle("clipsync"))
		for _, cs := range cfg.Clipsync {
			indicator, st, _ := getComponentInfo("clipsync", cs.Bind)
			addRow("", indicator, colorAddr(cs.Bind), "", "", st)

			// Collect peers: from live state if available, else fall back to config static peers
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
				icon := "~" // discovered at runtime
				if p.label == "static" {
					icon = "·" // configured/fixed
				}
				addRow("   ", icon, colorAddr(p.addr), "", cGray+p.label+cReset, "")
			}
		}
		addHeader("")
	}

	if len(cfg.Listeners) > 0 {
		addHeader(sectionTitle("listeners"))
		for _, l := range cfg.Listeners {
			indicator, st, _ := getComponentInfo(l.Type, l.Bind)
			if l.Type == "sshd" {
				indicator, st, _ = getComponentInfo("server", l.Bind)
				left := padForProto(colorAddr(l.Bind)) + " " + cBlue + strings.ToLower(l.Type) + cReset
				addRow("", indicator, left, "", "", st)
			} else if l.Type == "relay" {
				indicator, st, _ = getComponentInfo("relay", l.Bind)
				left := colorAddr(l.Bind)
				arrow := arrowRight
				right := colorAddr(l.Target)
				addRow("", indicator, left, arrow, right, st)
			} else {
				// Proxy
				indicator, st, _ = getComponentInfo("proxy", l.Bind)
				left := padForProto(colorAddr(l.Bind)) + " " + cBlue + strings.ToLower(l.Type) + cReset
				var right = ""
				var arrow = ""
				if l.Target != "" {
					right = colorAddr(l.Target)
					arrow = arrowRight
				}
				addRow("", indicator, left, arrow, right, st)
			}

			_, searchPort, err := net.SplitHostPort(l.Bind)
			if err != nil {
				searchPort = l.Bind
			}

			// Try matching by full bind first, then by port
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
				// Deduplicate if we somehow got both (since we populate both keys sometimes)
				seenID := make(map[string]bool)
				for _, comp := range dyns {
					if seenID[comp.ID] {
						continue
					}
					seenID[comp.ID] = true
					parts := strings.Split(comp.ID, "|")
					actualAddr := parts[0]
					left := colorAddr(actualAddr)
					right := colorAddr(cleanIPv6(comp.Message))

					addRow("   ", "~", left, arrowRight, right, "")
				}
			}
		}
		addHeader("")
	}

	if len(cfg.Connections) > 0 {
		for _, c := range cfg.Connections {
			addHeader(sectionTitle(c.Name))

			connectedTargets := make(map[string]struct{})
			for _, fset := range c.Forwards {
				id := c.Name + " [" + fset.Name + "]"
				_, _, comp := getComponentInfo("connection", id)
				if (comp.Status == state.Connected || comp.Status == state.Connecting) && comp.Message != "" {
					connectedTargets[comp.Message] = struct{}{}
				}
			}

			for _, t := range c.Targets {
				ind := "○"
				if _, ok := connectedTargets[t]; ok {
					ind = "●"
				}
				addRow(" ", ind, colorAddr(t), "", "", "")
			}

			for _, fset := range c.Forwards {
				id := c.Name + " [" + fset.Name + "]"
				indicator, st, _ := getComponentInfo("connection", id)

				left := sectionTitle(fset.Name)
				addRow("", indicator, left, "", "", st)

				indent := "   "

				for _, fwd := range fset.Local {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					_, _, comp := getComponentInfo("forward", compID)

					lStr := colorAddr(fwd.Bind)
					if comp.BoundAddr != "" && comp.BoundAddr != fwd.Bind {
						lStr = colorAddr(comp.BoundAddr) + " " + cGray + "(from " + fwd.Bind + ")" + cReset
					}

					if fwd.Type == "forward" {
						rStr := colorAddr(fwd.Target)
						addRow(indent, "", lStr, arrowRight, rStr, "")
					} else { // socks, http
						lStr = padForProto(lStr) + " " + cBlue + strings.ToLower(fwd.Type) + cReset
						rStr := ""
						if fwd.Target != "" {
							rStr = colorAddr(fwd.Target)
						} else {
							rStr = cGray + "🔒 tunnel" + cReset
						}
						addRow(indent, "", lStr, arrowRight, rStr, "")
					}
				}
				for _, fwd := range fset.Remote {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					_, _, comp := getComponentInfo("forward", compID)

					rStr := colorAddr(fwd.Bind)
					if comp.BoundAddr != "" && comp.BoundAddr != fwd.Bind {
						rStr += colorAddr(comp.BoundAddr) + " " + cGray + "(from " + fwd.Bind + ")" + cReset
					}

					if fwd.Type == "forward" {
						lStr := colorAddr(fwd.Target)
						rStr := colorAddr(fwd.Bind)
						addRow(indent, "", lStr, arrowLeft, rStr, "")
					} else { // socks, http
						lStr := ""
						if fwd.Target != "" {
							lStr = colorAddr(fwd.Target)
						} else {
							lStr = cGray + "🔒 tunnel" + cReset
						}
						rStr := padForProto(colorAddr(fwd.Bind)) + " " + cBlue + strings.ToLower(fwd.Type) + cReset
						addRow(indent, "", lStr, arrowLeft, rStr, "")
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
			left := colorAddr(comp.ID)
			right := colorAddr(cleanIPv6(comp.Message))
			addRow("", "↳", left, arrowRight, right, "")
		}
		addHeader("")
	}

	maxTotalLeft := 0
	for _, r := range rows {
		if !r.isHeader && r.left != "" && r.arrow != "" {
			l := len(r.indent)
			if r.indicator != "" {
				l += 2 // '⚪ ' indicator plus space
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

			l := visibleLen(line)
			if l > maxLineLen {
				maxLineLen = l
			}
		}
	}

	statusPadCol := maxLineLen + 1

	// Don't pad out further than 60 characters so short rows don't push statuses too far out
	if statusPadCol > 60 {
		statusPadCol = 60
	}

	for _, r := range rows {
		if r.isHeader {
			fmt.Println(r.text)
			continue
		}

		line := r.text

		if r.status != "" {
			lineLen := visibleLen(line)
			if lineLen < statusPadCol {
				line += strings.Repeat(" ", statusPadCol-lineLen)
			} else {
				line += " "
			}
			line += r.status
		}
		fmt.Println(strings.TrimRight(line, " "))
	}

	os.Exit(0)
}

func downCmd(nodeName string) {
	pid, err := readPidFile(nodeName)
	if err != nil || pid == 0 {
		fmt.Printf("mesh node %q is not running.\n", nodeName)
		return
	}

	if !checkPid(pid) {
		fmt.Printf("mesh node %q is not running (stale pidfile).\n", nodeName)
		removePidFile(nodeName)
		return
	}

	fmt.Printf("Stopping mesh node %q (pid %d)...\n", nodeName, pid)
	if err := killPid(pid, syscall.SIGTERM); err != nil {
		fmt.Printf("Error sending SIGTERM: %v\n", err)
		os.Exit(1)
	}
	// Wait for the process to actually exit (up to 10 seconds)
	for i := 0; i < 100; i++ {
		if !checkPid(pid) {
			removePidFile(nodeName)
			fmt.Println("Stopped.")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("Warning: process did not exit within 10 seconds.")
}

func portFilePath(nodeName string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir, err = os.UserHomeDir()
		if err != nil {
			dir = os.TempDir()
		}
	}
	os.MkdirAll(filepath.Join(dir, "mesh"), 0700)
	return filepath.Join(dir, "mesh", fmt.Sprintf("mesh-%s.port", nodeName))
}

func pidFilePath(nodeName string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir, err = os.UserHomeDir()
		if err != nil {
			dir = os.TempDir()
		}
	}
	os.MkdirAll(filepath.Join(dir, "mesh"), 0700)
	return filepath.Join(dir, "mesh", fmt.Sprintf("mesh-%s.pid", nodeName))
}

func writePidFile(nodeName string) error {
	pid := os.Getpid()
	data := []byte(strconv.Itoa(pid))
	return os.WriteFile(pidFilePath(nodeName), data, 0600)
}

func readPidFile(nodeName string) (int, error) {
	data, err := os.ReadFile(pidFilePath(nodeName))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

func removePidFile(nodeName string) {
	os.Remove(pidFilePath(nodeName))
}

func checkPid(pid int) bool {
	if runtime.GOOS == "windows" {
		// FindProcess always succeeds on Windows. Instead, explicitly poll tasklist.
		cmd := exec.Command("tasklist", "/NH", "/FI", fmt.Sprintf("PID eq %d", pid))
		output, err := cmd.Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(output), strconv.Itoa(pid))
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, sending signal 0 checks if the process exists
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func killPid(pid int, sig syscall.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(sig); err != nil {
		// Fallback to Kill if the OS (e.g. Windows) doesn't support the specific signal.
		return process.Kill()
	}
	return nil
}
