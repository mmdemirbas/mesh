# Proxy & Tunnel Analysis Report

> Generated from a deep-dive code review session on 2026-03-26.
> Status: **Observations only — no changes applied.**
> Each item needs independent verification and careful testing before any action.

---

## Table of Contents

- [1. Architecture Context](#1-architecture-context)
- [2. Data Flow for Proxy Forward Sets](#2-data-flow-for-proxy-forward-sets)
- [3. Log Analysis](#3-log-analysis)
- [4. Code-Level Observations](#4-code-level-observations)
    - [4.1. SetLinger(0) in ApplyTCPKeepAlive](#41-setlinger0-in-applytcpkeepalive)
    - [4.2. SetDeadline silently ignored on SSH channels](#42-setdeadline-silently-ignored-on-ssh-channels)
    - [4.3. DialViaSocks5 deadline also no-op on SSH channels](#43-dialviasocks5-deadline-also-no-op-on-ssh-channels)
    - [4.4. Non-CONNECT HTTP proxy enters raw relay after first request](#44-non-connect-http-proxy-enters-raw-relay-after-first-request)
    - [4.5. io.MultiReader(br, conn) construct](#45-iomultireaderbr-conn-construct)
    - [4.6. Silent return on HTTP request parse errors](#46-silent-return-on-http-request-parse-errors)
    - [4.7. PermitOpen matching changes](#47-permitopen-matching-changes)
    - [4.8. CountedBiCopy tx/rx counter documentation](#48-countedbicopcopy-txrx-counter-documentation)
- [5. Potential Improvements](#5-potential-improvements)
- [6. Items Ruled Out](#6-items-ruled-out)

---

## 1. Architecture Context

The analysis was done against the following deployment topology (from `~/.mesh/conf/mesh.yaml`):

```
Mac (client node)                    Windows (server node)
  sshd on 0.0.0.0:2222                standalone socks on 127.0.0.1:1080
  standalone socks on 127.0.0.1:2080   standalone http on 127.0.0.1:1081
  standalone http on 127.0.0.1:2081    sshd on 127.0.0.1:1111
                                       |
                                       |  connection: mbp-tunnel
                                       |  Windows is SSH CLIENT -> Mac sshd:2222
                                       |
                                       +- win-proxy (remote forwards):
                                       |    type: socks, bind: 127.0.0.1:1080
                                       |    type: http,  bind: 127.0.0.1:1081
                                       |    -> runRemoteProxy on Windows
                                       |    -> listener opens on Mac via client.Listen
                                       |    -> proxy handler runs on Windows
                                       |    -> outbound dial from Windows (net.DialTimeout)
                                       |
                                       +- mbp-proxy (local forwards):
                                       |    type: socks, bind: 127.0.0.1:2080
                                       |    type: http,  bind: 127.0.0.1:2081
                                       |    -> runLocalProxy on Windows
                                       |    -> listener opens on Windows (local)
                                       |    -> outbound dial via client.Dial -> direct-tcpip
                                       |    -> Mac's SSH server dials the target
                                       |    -> traffic exits from Mac's network
                                       |
                                       +- syncthing (remote + local forwards)
                                       |    plain TCP (type: forward), fixed IP targets
                                       |
                                       +- mbp-sshd (remote + local forwards)
                                            plain TCP (type: forward), fixed IP targets
```

Additionally, Mac connects to a bastion at `138.2.134.182:5555` to make its sshd reachable
via a relay chain: `Windows -> relay -> bastion -> Mac`.

---

## 2. Data Flow for Proxy Forward Sets

### win-proxy (remote proxy forwards — `runRemoteProxy`)

```
curl on Mac
  |
  v  TCP connection
Mac:1081 (listener opened by Mac's SSH server via handleTCPIPForward)
  |
  v  forwarded-tcpip SSH channel (encrypted, chacha20-poly1305)
  |
Windows: runRemoteProxy accepts channel from client.Listen
  |
  v  proxy handler parses HTTP CONNECT / SOCKS5
  |
Windows: net.DialTimeout("tcp", "target:443", 10s)
  |
  v  direct TCP from Windows to internet (NOT through SSH tunnel)
  |
target server (e.g., google.com:443)
```

**Key point:** The Mac-to-Windows leg is fully SSH-encrypted. The Windows-to-target leg is a
direct TCP connection from Windows's network stack. This is intentional — the purpose of
`win-proxy` is to route traffic through Windows's network. The last hop being direct TCP is
by design, not a security gap: the proxy's job is to let Mac use Windows as an exit node.

The outbound dial uses `net.DialTimeout` with a **hardcoded 10-second** timeout
(`tunnel.go:888`). Standalone proxies use `net.Dial` with no explicit timeout (OS default,
typically 30-120 seconds depending on platform and sysctls).

### mbp-proxy (local proxy forwards — `runLocalProxy`)

```
app on Windows
  |
  v  TCP connection
Windows:2080 (listener opened locally via netutil.ListenReusable)
  |
  v  proxy handler parses HTTP CONNECT / SOCKS5
  |
Windows: client.Dial("tcp", "target:443")
  |
  v  direct-tcpip SSH channel (encrypted) to Mac's SSH server
  |
Mac: handleDirectTCPIP -> net.DialTimeout("tcp", "target:443", 10s)
  |
  v  direct TCP from Mac to internet
  |
target server
```

**Key point:** For `mbp-proxy`, the outbound dial goes through `direct-tcpip` channels on
Mac's SSH server. If `PermitOpen` were set to something restrictive on Mac's sshd, arbitrary
proxy destinations would be blocked. Currently `PermitOpen: any`, so this is not an issue.

### Standalone proxies (listeners section)

Both Mac and Windows have standalone socks/http listeners. These use `RunStandaloneProxies`
in `proxy.go`, which calls `net.Listen` and `ServeSocks`/`ServeHTTPProxy` with `net.Dial`
as the dialer (no SSH involvement at all). These are independent of the SSH tunnel.

### Note on port overlap

The `win-proxy` remote forwards bind `127.0.0.1:1080` and `1081` on **Mac** (via
`client.Listen`). The standalone listeners on **Windows** also bind `127.0.0.1:1080` and
`1081`. These don't conflict because they're on different machines. But both ports exist:
one on Mac (SSH-forwarded, handled by `runRemoteProxy` on Windows) and one on Windows
(standalone, handled by `RunStandaloneProxies` on Windows). Traffic to Mac:1080 goes through
the tunnel; traffic to Windows:1080 hits the standalone proxy directly.

---

## 3. Log Analysis

Log file: `~/.mesh/log/server.log` (synced from Windows)

### Error Type 1: i/o timeout

```
07:04:18.599 DBG HTTP CONNECT failed maven.cloudartifact.lfg.dragon.tools.huawei.com:443:
  dial tcp 7.219.137.67:443: i/o timeout
  component=ssh name=mbp-tunnel set=win-proxy
```

- **Source:** `win-proxy` forward set -> `runRemoteProxy` -> `net.DialTimeout` from Windows.
- **Observation:** DNS resolved successfully (IP address 7.219.137.67 is visible in the error).
  The TCP connection itself timed out. This means Windows cannot reach that IP within 10 seconds.
- **Possible causes:** Corporate firewall, routing issue, or the server being unreachable
  from Windows's network. Not a mesh code issue.
- **Note:** The 10-second timeout in `runRemoteProxy` (`tunnel.go:888`) is hardcoded.
  Standalone proxies use `net.Dial` which has a much longer OS-level default timeout. This
  difference could cause `runRemoteProxy` to time out on connections that standalone proxies
  would eventually complete. Whether that matters depends on whether the target is truly
  unreachable or just slow.

### Error Type 2: connectex WSAEACCES

```
08:09:25.810 DBG SOCKS connect failed edgedl.me.gvt1.com:80:
  dial tcp 34.104.35.123:80: connectex: An attempt was made to access a socket in a way
  forbidden by its access permissions.
  component=ssh name=mbp-tunnel set=win-proxy
```

- **Source:** Same path — `win-proxy` -> `runRemoteProxy` -> `net.DialTimeout` from Windows.
- **Observation:** DNS resolved successfully. The Windows kernel refused the outbound socket
  connection with `WSAEACCES` (error code 10013).
- **Possible causes:**
    - Windows Firewall/Defender blocking outbound connections to certain IPs
    - Corporate endpoint security software (ZScaler, CrowdStrike, etc.)
    - Hyper-V/WSL2 reserved port ranges (check:
      `netsh int ipv4 show excludedportrange protocol=tcp`)
    - WinSock LSP (Layered Service Provider) interference
- **Not a mesh code issue.** This is the Windows networking stack refusing the connection.

### Absence of PermitOpen rejections

No `direct-tcpip rejected by PermitOpen` entries found in any logs. This confirms:

- `PermitOpen: any` is working correctly
- The `win-proxy` path does not use `direct-tcpip` (it uses `runRemoteProxy` with direct dials)
- The `mbp-proxy` path uses `direct-tcpip` but `PermitOpen: any` allows everything

---

## 4. Code-Level Observations

### 4.1. SetLinger(0) in ApplyTCPKeepAlive

**File:** `internal/netutil/netutil.go:23`

```go
func ApplyTCPKeepAlive(conn net.Conn, period time.Duration) {
if tcpConn, ok := conn.(*net.TCPConn); ok {
_ = tcpConn.SetKeepAlive(true)
// ...
_ = tcpConn.SetNoDelay(true)
_ = tcpConn.SetLinger(0) // <-- this line
}
}
```

**What it does:** `SetLinger(0)` tells the kernel: when `Close()` is called, immediately
discard any unsent data in the TCP send buffer and send RST (reset) instead of FIN (graceful
close).

**Where it's called on real TCP connections (not SSH channels):**

- `proxy.go:60` — standalone proxy accepted connections
- `proxy.go:92` — standalone relay accepted connections
- `tunnel.go:1087` — `handleTCPIPForward` accepted connections (Mac-side, the curl-facing socket)
- Various other accept loops in tunnel.go

**Where it's a no-op:** On SSH channel connections (from `client.Listen` in `runRemoteProxy`),
the `*net.TCPConn` type assertion fails, and the function returns without doing anything.

**Risk scenario for the Mac-side TCP connection (`handleTCPIPForward`):**

1. Windows proxy handler writes `HTTP/1.1 502 Bad Gateway\r\n\r\n` to SSH channel
2. Mac-side `CountedBiCopy` relays those 35 bytes from SSH channel to the TCP socket's kernel
   send buffer
3. `conn.Close()` fires with `Linger(0)` — if those bytes haven't been ACKed by curl yet,
   the kernel discards them and sends RST
4. curl sees "connection reset by peer" instead of "502 Bad Gateway"

**Likelihood of actual data loss:** On localhost (curl and SSH server on the same Mac), the
ACK round-trip is sub-millisecond, so data loss is extremely unlikely for small writes like
the 502 response. Over a real network with latency, the risk increases. The risk is also
higher for large responses during the relay phase.

**Why it was probably added:** To prevent TIME_WAIT socket accumulation on busy proxy ports.
When a proxy handles many short-lived connections, each close creates a TIME_WAIT state that
lasts ~60 seconds. RST via `SetLinger(0)` avoids TIME_WAIT entirely.

**Alternative approach:** `SO_REUSEADDR` (already set via `ListenReusable` for local proxy
listeners) allows binding to ports in TIME_WAIT state without `SetLinger(0)`. Standalone
proxy listeners in `proxy.go` use plain `net.Listen` (no `SO_REUSEADDR`), which could be
changed. For non-listener sockets (accepted connections), TIME_WAIT on the accepted side is
normal and handled by the OS.

**Not yet verified:**

- Whether removing `SetLinger(0)` causes TIME_WAIT buildup under load
- Whether `SO_REUSEADDR` is sufficient alone for all listener sockets
- Whether any actual data loss has been observed in practice

### 4.2. SetDeadline silently ignored on SSH channels

**Files:** `internal/proxy/http.go:70`, `internal/proxy/socks5.go:37`

```go
// http.go:70
_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

// socks5.go:37
_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
```

**Purpose:** These 30-second deadlines protect against slowloris attacks — a client that
connects but sends data very slowly to hold resources indefinitely.

**Problem:** For `runRemoteProxy`, accepted connections are SSH channels (from `client.Listen`).
The Go SSH library's internal `chanConn` type does NOT implement `SetDeadline` — it returns an
error which the code silently ignores with `_ =`. This means:

- **Standalone proxies:** Deadline is applied (real TCP) — slowloris protection works.
- **runRemoteProxy:** Deadline silently fails — no slowloris protection through the SSH tunnel.
- **runLocalProxy:** Listener is real TCP (`ListenReusable`), so deadline works.

**Mitigation in practice:** The SSH tunnel itself has keepalive timeouts
(`ClientAliveInterval: 15`, `ClientAliveCountMax: 3` in the sshd config), which would
eventually kill the entire SSH connection if all channels stall. But this is coarse-grained
protection — it kills the entire connection (all forward sets), not just the offending proxy
stream.

**Possible fix approach:** Use `context.WithTimeout` to wrap the handler instead of
`SetDeadline`. A goroutine with a timer could close the connection after the timeout if the
handshake hasn't completed. This works regardless of the underlying connection type. Need to
be careful not to interfere with the ongoing relay phase after the handshake.

**Not yet verified:**

- Whether the Go SSH library might add `SetDeadline` support in the future
- Whether `context.WithTimeout` + `conn.Close()` is safe for all connection types
- Whether the SSH keepalive timeout is sufficient protection in practice

### 4.3. DialViaSocks5 deadline also no-op on SSH channels

**File:** `internal/proxy/http.go:156-157`

```go
_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
defer func () { _ = conn.SetDeadline(time.Time{}) }()
```

**Context:** This is inside `DialViaSocks5`, which protects the SOCKS5 handshake from tarpit
servers. The `conn` here is the connection to the SOCKS5 upstream server.

**When it matters:** `runLocalProxy` for HTTP with a SOCKS5 target:

```go
// tunnel.go:934-936
httpDialer := func (addr string) (net.Conn, error) {
if pxy.Target != "" {
return proxy.DialViaSocks5(sshDialer, pxy.Target, addr)
}
return sshDialer("tcp", addr)
}
```

Here, `sshDialer` returns an SSH channel via `client.Dial`. The SSH channel doesn't support
`SetDeadline`, so the 10-second handshake protection is silently disabled. If the remote
SOCKS5 server is unresponsive, the SOCKS5 handshake will hang indefinitely.

**Assessment:** Low risk in the current config because `mbp-proxy` doesn't have a `target`
field (it's empty), so `DialViaSocks5` is not called for that forward set. But if a target
were added in the future, this would become relevant.

### 4.4. Non-CONNECT HTTP proxy enters raw relay after first request

**File:** `internal/proxy/http.go:78-113`

For non-CONNECT requests (plain HTTP like `GET http://example.com/ HTTP/1.1`), the handler:

1. Parses one HTTP request with `http.ReadRequest(br)` (line 73)
2. Extracts target host from `req.Host` or `req.URL.Host` (lines 79-81)
3. Dials the target (line 91)
4. Writes the parsed request to the remote with `req.Write(remote)` (line 100)
5. Enters raw `BiCopy` relay between client and remote (lines 106-112)

**Observation:** After step 5, the proxy relays raw bytes without parsing. If the HTTP client
sends a second pipelined request to a **different** host, those bytes go to the server dialed
in step 3 — the wrong server.

**Standard behavior:** Most HTTP forward proxies handle each request independently, re-parsing
and re-routing after each response completes. The current implementation effectively turns
into a raw TCP pipe after the first request.

**For CONNECT:** This is correct — CONNECT establishes a tunnel and all subsequent bytes are
opaque TLS traffic. No re-parsing is expected or desired.

**For plain HTTP:** This is a semantic deviation from RFC 7230 Section 2.3 (HTTP/1.1 message
routing). Whether it matters depends on whether clients actually pipeline different-host
requests, which is rare in practice. Most modern clients use CONNECT for HTTPS (the dominant
case) and don't pipeline plain HTTP proxy requests.

**Not yet verified:**

- Whether any actual clients in the deployment rely on HTTP pipelining through the proxy
- Whether the single-request behavior causes observable issues

### 4.5. io.MultiReader(br, conn) construct

**File:** `internal/proxy/http.go:105, 135-137`

```go
bc := &bufferedConn{Conn: conn, r: io.MultiReader(br, conn)}
```

Where `br = bufio.NewReader(conn)`.

**Observation:** `br` wraps `conn`. When reading from `io.MultiReader(br, conn)`:

1. First reads from `br` — which internally reads from `conn` and buffers
2. `br` returns `io.EOF` only when `conn` returns `io.EOF`
3. At that point, `conn` also returns `io.EOF`, so the second reader contributes nothing

The `conn` argument in the MultiReader is functionally unreachable during normal operation.
The construct behaves identically to just using `br` as the reader.

**Assessment:** Not a bug — the code works correctly. The `conn` in the MultiReader is
redundant but harmless. It may have been written defensively to ensure no data is lost
between the buffered reader and the raw connection, but due to how `bufio.Reader` works
(it wraps the same `conn`), this safety net is never exercised.

**Not yet verified:** Whether there's a theoretical edge case with half-closed connections
where `br` returns EOF but `conn` still has data. Based on `bufio.Reader` internals this
should not be possible, but has not been tested explicitly.

### 4.6. Silent return on HTTP request parse errors

**File:** `internal/proxy/http.go:73-76`

```go
req, err := http.ReadRequest(br)
if err != nil {
return // silent close, no log, no response
}
```

**Observation:** If the HTTP request is malformed (garbage bytes, partial request, TLS
ClientHello sent to an HTTP proxy port, etc.), the handler returns silently. The client sees
the connection close with no response. No log entry is created, even at DEBUG level.

**Assessment:** This is a debugging blind spot. Malformed requests could indicate:

- Configuration errors (client sending HTTPS to a plain HTTP proxy port)
- Protocol confusion (SOCKS client connecting to HTTP port or vice versa)
- Port scanning or probing traffic
- Legitimate partial requests from clients that closed early

A DEBUG-level log with the error here would help diagnose issues without being noisy during
normal operation. The `handleSocks5` function similarly returns silently on various parse
errors (greeting mismatch, unsupported command, etc.) but at least logs dial failures.

### 4.7. PermitOpen matching changes

**File:** `internal/tunnel/tunnel.go:1154-1188`

The `PermitOpen` matching logic was changed in commit `3d6a788`. The old code had:

```go
// Old code (pre-3d6a788):
if p == target || p == req.DestAddr || (p == "*" && strings.HasSuffix(p, ...)) {
```

Two issues in the old code:

1. `p == "*" && strings.HasSuffix(p, fmt.Sprintf(":%d", req.DestPort))` was always false
   because `strings.HasSuffix("*", ":443")` is never true. The `*` wildcard didn't work.
2. `p == req.DestAddr` matched the hostname WITHOUT port, providing a broader match than
   intended. For example, `PermitOpen: "myhost"` would match any port on `myhost`.

The new code removed `p == req.DestAddr` and properly implemented wildcards:

```go
// New code (current):
if p == target { /* exact host:port match */ }
if strings.HasPrefix(p, "*:") { /* wildcard host, specific port: *:22 */ }
if strings.HasSuffix(p, ":*") { /* specific host, wildcard port: myhost:* */ }
```

**Assessment:** If someone was relying on `PermitOpen: "somehost"` (without port) to match
`somehost:443`, that would have worked before (via `p == req.DestAddr`) but fails now (requires
`somehost:443` or `somehost:*`). Currently `PermitOpen: any`, so this doesn't affect the
active configuration. But worth noting for future PermitOpen configurations.

**Separator question:** The current code uses `strings.Split(permitOpen, ",")` (comma-separated).
OpenSSH uses space-separated values for PermitOpen. If someone copies an OpenSSH PermitOpen
value like `"host1:22 host2:80"`, the comma-split would treat the whole string as one entry
and matching would fail. Need to decide which separator mesh intends to support and document
it clearly.

### 4.8. CountedBiCopy tx/rx counter documentation

**File:** `internal/netutil/netutil.go:47-71`

```go
// CountedBiCopy is like BiCopy but counts bytes transferred through atomic counters.
// tx counts bytes from a->b, rx counts bytes from b->a.
func CountedBiCopy(a, b io.ReadWriteCloser, tx, rx *atomic.Int64) {
go func () {
_, _ = io.CopyBuffer(&countingWriter{w: a, counter: tx}, b, *bufPtr)
// reads from b, writes to a, counts with tx
// So tx actually counts b->a, not a->b
}()
go func () {
_, _ = io.CopyBuffer(&countingWriter{w: b, counter: rx}, a, *bufPtr)
// reads from a, writes to b, counts with rx
// So rx actually counts a->b, not b->a
}()
}
```

**Observation:** The doc comment says "tx counts bytes from a->b" but the code counts bytes
flowing b->a with the tx counter (it reads from b, writes to a). The rx direction is similarly
reversed.

**Assessment:** Documentation-only issue. The counters work correctly. The callers pass the
right atomics for their intended semantics (they were written to match the code, not the doc).
But if someone reads the doc comment and relies on it for new code, they'd get the direction
backwards.

---

## 5. Potential Improvements

These are suggestions that emerged from the analysis. Each needs independent evaluation,
testing, and consideration of edge cases before implementation.

| # | Area                    | Description                                                                                                                                                                                                          | Files                  | Risk if not done                                               | Effort  |
|---|-------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------|----------------------------------------------------------------|---------|
| 1 | `SetLinger(0)`          | Consider removing from `ApplyTCPKeepAlive`. Keep keepalive and nodelay. Verify SO_REUSEADDR covers the port-reuse need.                                                                                              | `netutil.go`           | Theoretical data loss on close under load                      | Low     |
| 2 | Parse error logging     | Add DEBUG log in `handleHTTPProxy` when `http.ReadRequest` fails, including the error message                                                                                                                        | `http.go`              | Debugging blind spot for malformed requests                    | Low     |
| 3 | SSH channel deadlines   | Replace `conn.SetDeadline` with a context-based or goroutine-based timeout for the handshake phase. Works on all connection types including SSH channels.                                                            | `http.go`, `socks5.go` | No slowloris protection when proxied through SSH tunnel        | Medium  |
| 4 | DialTimeout consistency | The 10s timeout in `runRemoteProxy` (`tunnel.go:888`) is hardcoded and shorter than `net.Dial`'s OS default (30-120s). Consider making it configurable or using a longer default to match standalone proxy behavior. | `tunnel.go`            | Premature timeouts on slow networks                            | Low     |
| 5 | CountedBiCopy docs      | Fix the tx/rx direction in the doc comment to match the actual code behavior                                                                                                                                         | `netutil.go`           | Confusion for future contributors                              | Trivial |
| 6 | PermitOpen separator    | Decide whether comma or space separation is intended. OpenSSH uses spaces. Document the choice. Consider supporting both.                                                                                            | `tunnel.go`            | Potential misconfiguration if copying from OpenSSH             | Low     |
| 7 | HTTP pipelining         | Document the single-request-then-relay behavior for non-CONNECT requests, or implement per-request routing                                                                                                           | `http.go`              | Incorrect routing for pipelined HTTP requests (rare edge case) | Medium  |

---

## 6. Items Ruled Out

These were investigated during the analysis and determined to NOT be causing issues:

- **PermitOpen blocking proxy traffic:** `PermitOpen: any` confirmed in config. No rejections
  in logs. The `win-proxy` path doesn't use `direct-tcpip` at all — it uses `runRemoteProxy`
  with direct `net.DialTimeout` from Windows.
- **DNS resolution failure:** Log entries show resolved IP addresses in all error messages.
  DNS works correctly on Windows. The errors are at the TCP connect level, not DNS.
- **SSH channel data delivery:** The 502 responses are being generated and returned to clients,
  confirming the SSH channel reads and writes work correctly. The proxy successfully parses
  requests before the dial step fails.
- **Context cancellation:** The proxy handler runs to completion (dials, gets error, returns
  502). No evidence of premature context cancellation cutting off the dial.
- **Metrics integration regression (commit `29d4844`):** Only added `*state.Metrics` parameter
  threading and counter tracking. No behavioral changes to dialing, relay logic, or error
  handling. Before/after comparison of the diff confirms no side effects.
- **Error handling refactor (commit `6d7b20f`):** Only added `_ =` to suppress lint warnings
  about unchecked error returns. No behavioral changes — the same functions are called with
  the same arguments; the return values were already being discarded.
- **Error handling refactor (commit `e9acd80`):** Same pattern — only `_ =` additions for
  `SetDeadline` and `Reply` calls. No behavioral changes.
- **`ApplyTCPKeepAlive` on SSH channels:** The `*net.TCPConn` type assertion fails, making
  the entire function a no-op for SSH channel connections. No unintended side effects — the
  SSH protocol handles its own keepalive and flow control.
- **Security of the proxy data path:** The Mac-to-Windows leg is fully SSH-encrypted. The
  Windows-to-target leg being direct TCP is by design (the proxy's exit point). This is not
  a security gap.
- **`bufferedConn.CloseWrite` on SSH channels:** The `CloseWrite()` method (http.go:29-34)
  checks for the `CloseWrite() error` interface. SSH channels do implement this interface,
  so half-close signaling works correctly through the tunnel.
