# Memory Analysis Report — mesh

Static analysis of memory usage patterns, potential leaks, and architectural concerns.

## 1. Buffer Allocations

| Finding                          | Location                         | Size                   | Frequency               | Verdict         |
|----------------------------------|----------------------------------|------------------------|-------------------------|-----------------|
| `sync.Pool` for BiCopy buffers   | `netutil/netutil.go:27-33`       | 32 KB                  | Per-connection, pooled  | **OK**          |
| `io.CopyBuffer` with pooled buf  | `netutil/netutil.go:56,65,81,90` | 32 KB (from pool)      | Per-connection          | **OK**          |
| `bufio.NewReader` for HTTP proxy | `proxy/http.go:72`               | 4 KB (default)         | Per-connection          | **OK**          |
| SOCKS5 handshake buffer          | `proxy/socks5.go:46`             | 258 B                  | Per-connection          | **OK**          |
| `io.Copy` in shell PTY           | `tunnel/shell_unix.go:183,187`   | 32 KB (stdlib default) | Per-shell-session       | **WASTE** — low |
| UDP beacon buffer                | `clipsync/clipsync.go:1251`      | 2 KB                   | Once at startup, reused | **OK**          |
| Small protocol buffers           | `proxy/http.go:163,196,217`      | 2-257 B                | Per-connection          | **OK**          |

**Summary:** Buffer management is excellent. The hot path (BiCopy) uses `sync.Pool` correctly with
proper Get/Put pairs. The only unpooled 32 KB allocation is the shell PTY `io.Copy`, but shell
sessions are rare and short-lived — negligible impact.

## 2. Goroutine Lifecycle

48 total goroutine launch sites found. All have clear shutdown paths.

| Category                              | Count | Exit Mechanism           |
|---------------------------------------|-------|--------------------------|
| Context-aware (`<-ctx.Done()`)        | ~35   | Context cancellation     |
| WaitGroup-tracked (`defer wg.Done()`) | ~8    | Parent WaitGroup         |
| Channel/IO-bound                      | ~5    | Connection/channel close |

**One minor issue found:**

| Finding                        | Location                               | Verdict                |
|--------------------------------|----------------------------------------|------------------------|
| Duplicate UDP close goroutines | `clipsync/clipsync.go:1245` AND `1252` | **SMELL** — negligible |

Two goroutines both do `<-ctx.Done(); conn.Close()` on the same UDP socket. Harmless (UDP close is
idempotent) but violates the project's own `sync.Once` close convention. Appears to be a copy-paste
artifact.

No goroutine leaks found. Every goroutine is tied to either context cancellation, connection close,
or channel close.

## 3. Long-lived Allocations & Metrics State

### LEAK: Metrics entries never deleted in production

`state/state.go:41` — `metrics sync.Map` grows monotonically.

`DeleteMetrics()` exists at line 105 but is only called in tests — never in production code.
Meanwhile:

| Caller                               | Key Pattern                    | Component deleted? | Metrics deleted? |
|--------------------------------------|--------------------------------|--------------------|------------------|
| `tunnel.go:762` (remote fwd)         | `"forward:name [set] bind"`    | Yes (line 797)     | **No**           |
| `tunnel.go:810` (local fwd)          | `"forward:name [set] bind"`    | Yes (line 832)     | **No**           |
| `tunnel.go:845` (remote proxy)       | `"forward:name [set] bind"`    | Yes (line 880)     | **No**           |
| `tunnel.go:899` (local proxy)        | `"forward:name [set] bind"`    | Yes (line 921)     | **No**           |
| `tunnel.go:1067` (dynamic tcpip-fwd) | `"dynamic:addr\|parentBind"`   | Yes (line 1070)    | **No**           |
| `tunnel.go:143` (server)             | `"server:bind"`                | Never              | **No**           |
| `proxy/proxy.go:41,80` (proxy/relay) | `"proxy:bind"`, `"relay:bind"` | Never              | **No**           |

**Impact assessment:**

- **"forward" and "proxy"/"relay"/"server" metrics**: Keys are deterministic from config. On
  reconnect, the same key is reused (metrics are reset via `.Store(0)` at lines 763-764, 811-812,
  etc). These don't grow unboundedly — low impact.
- **"dynamic" metrics**: `compID = actualAddr + "|" + parentBind` at line 1061. If the client
  requests `:0` (random port), each dynamic tunnel gets a unique OS-assigned port, creating a new
  metrics entry that is never cleaned up. Over time with many dynamic tunnel cycles, this grows
  without bound. Each `Metrics` struct is ~40 bytes (4 atomics), so medium impact over weeks/months
  of uptime with active dynamic tunnels.

**Verdict: LEAK — medium** (for dynamic metrics); **SMELL — low** (for forward/proxy metrics,
orphaned but bounded)

### Component state for "connection" and "server" never deleted

`state.Global.Delete()` is never called for `"connection"` or `"server"` types. However, these keys
are deterministic from config (e.g., `"connection:myhost [default]"`, `"server:0.0.0.0:2222"`), so
the map is bounded by the number of configured components. The entries are just updated in-place on
reconnect.

**Verdict: OK** — bounded by config, no real growth.

### Log ring buffer

`cmd/mesh/main.go:234` — `newLogRing(15)` — fixed at 15 entries. Old entries are overwritten. OK —
bounded.

### Rate limiter map

`tunnel/tunnel.go:60-114` — `map[string]*limiterEntry` keyed by IP. Has eviction: every 5 min evicts
entries idle >10 min. Hard cap at 10,000 entries with aggressive 2-min eviction under pressure. Each
entry is ~100 bytes, so worst case ~1 MB.

**Verdict: OK** — properly bounded with eviction.

### Clipsync peer maps

`clipsync/clipsync.go:74-75` — `peers map[string]time.Time` and `peerHashes map[string]string`.
Cleanup every 10s, evicts peers idle >15s. Properly cleaned including state deletion at line 1470.

**Verdict: OK** — well-managed.

## 4. SSH Library Usage

Excellent resource management across the board:

- All `ssh.NewServerConn` / `ssh.NewClientConn` properly closed with `defer` (lines 187, 647-648
  with `sync.Once`)
- All channels accepted with `defer ch.Close()` or explicit close after BiCopy
- All request channels properly consumed (either `ssh.DiscardRequests()` or `for req := range reqs`
  loops)
- `sync.Once`-guarded close for multi-goroutine scenarios (shell handlers, client connections)
- Handshake deadlines set and cleared properly

**Verdict: OK** — no SSH resource leaks.

## 5. Metrics/Tracking State

| Store                    | Bounded?                 | Eviction?                                      | Verdict  |
|--------------------------|--------------------------|------------------------------------------------|----------|
| `state.components` map   | Yes (by config)          | Partial (forwards/dynamic yes, conn/server no) | **OK**   |
| `state.metrics` sync.Map | **No** (dynamic entries) | **None**                                       | **LEAK** |
| `logRing`                | Yes (15 entries)         | Ring overwrites                                | **OK**   |
| Rate limiter map         | Yes (10K cap)            | 5-min ticker                                   | **OK**   |
| Clipsync peers           | Yes (15s TTL)            | 10-sec ticker                                  | **OK**   |

## 6. sync.Pool Usage

| Hot Path                                               | Pooled?                         | Verdict                                           |
|--------------------------------------------------------|---------------------------------|---------------------------------------------------|
| BiCopy / CountedBiCopy (all proxy + tunnel data relay) | Yes (`copyBufPool`, 32 KB)      | **OK**                                            |
| Shell PTY io.Copy                                      | No (stdlib 32 KB per direction) | **WASTE — negligible** (rare, short-lived)        |
| SOCKS5 handshake buf (258 B)                           | No                              | **OK** (too small to matter)                      |
| bufio.NewReader (4 KB per HTTP proxy conn)             | No                              | **OK** (small, bounded by concurrent connections) |

No additional sync.Pool candidates worth adding. The only hot path that matters (data relay) is
already pooled.

## Findings Summary

| #  | Category  | Finding                                                                                         | Impact                  | File:Line                                    |
|----|-----------|-------------------------------------------------------------------------------------------------|-------------------------|----------------------------------------------|
| 1  | **LEAK**  | `state.metrics` never cleaned for "dynamic" entries — grows unbounded with dynamic tunnel churn | Medium                  | `state/state.go:41`, `tunnel/tunnel.go:1067` |
| 2  | **SMELL** | `state.metrics` orphaned for "forward" entries — component deleted but metrics persist          | Low (bounded by config) | `tunnel/tunnel.go:762,810,845,899`           |
| 3  | **SMELL** | `DeleteMetrics()` implemented and tested but never called in production                         | Low                     | `state/state.go:105`                         |
| 4  | **SMELL** | Duplicate UDP close goroutines without `sync.Once`                                              | Negligible              | `clipsync/clipsync.go:1245,1252`             |
| 5  | **WASTE** | Shell PTY `io.Copy` allocates 32 KB buffers not from pool                                       | Negligible              | `tunnel/shell_unix.go:183,187`               |
| 6  | **OK**    | BiCopy uses `sync.Pool` correctly on the hot path                                               | —                       | `netutil/netutil.go:27-33`                   |
| 7  | **OK**    | All 48 goroutines have clear shutdown paths                                                     | —                       | —                                            |
| 8  | **OK**    | All SSH channels, connections, sessions properly closed                                         | —                       | —                                            |
| 9  | **OK**    | Log ring bounded at 15 entries                                                                  | —                       | `main.go:234`                                |
| 10 | **OK**    | Rate limiter bounded with eviction                                                              | —                       | `tunnel/tunnel.go:60-114`                    |

## Is 10-12 MB justified?

**Mostly yes.** The Go runtime itself accounts for ~5-7 MB (stack, GC metadata, runtime structures).
The remaining ~3-5 MB is reasonable for: SSH library state, TLS buffers, config structs, and the
pooled 32 KB buffers.

**The one thing worth fixing** is finding #1: add `state.Global.DeleteMetrics("dynamic", compID)`
alongside the existing `state.Global.Delete("dynamic", compID)` at `tunnel/tunnel.go:1070`. Same
pattern should be applied to the forward cleanup paths (lines 797, 832, 880, 921). This is a
one-line fix per call site and prevents long-running servers from leaking ~40 bytes per dynamic
tunnel teardown indefinitely.

Finding #4 (duplicate close goroutine) is a trivial cleanup — delete the duplicate at line 1252.
