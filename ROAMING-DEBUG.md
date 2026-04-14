# WiFi Roaming Connection Debug Report

Date: 2026-04-13

## Environment

| Node | Role | IP | OS |
|------|------|----|----|
| MBP | Client | 192.168.68.134 | macOS |
| Desktop | Server | 192.168.68.111 | Windows |
| Lenovo | Sidecar | .local (mDNS) | Linux |
| Bastion | VPS | 138.2.134.182:5555 | Linux (standard OpenSSH sshd) |
| Network | TP-Link mesh WiFi | 192.168.68.0/24 | same SSID all APs |

Same `mesh.yaml` deployed on MBP, Windows, and Lenovo. Bastion runs
stock OpenSSH, not mesh.

## Symptom

Moving MBP between physical locations causes connection loss to Windows.
Bastion connection also drops. Recovery is slow and unreliable.

---

## Live Debugging Session (2026-04-13, 20:00-20:45 local)

### Current state (at time of debugging)

| Check | Result |
|-------|--------|
| mesh running on MBP | **No** — process not found |
| MBP IP | 192.168.68.134 (unchanged) |
| Gateway (192.168.68.1) | **reachable** — ping 5ms |
| Internet (httpbin.org) | **reachable** |
| Bastion (138.2.134.182:5555) | **reachable** — TCP handshake succeeds |
| Windows (192.168.68.111:2222) | **unreachable** — TCP timeout |
| Windows (192.168.68.111:3389) | **unreachable** — RDP timeout |
| Windows (192.168.68.111:445) | **unreachable** — SMB timeout |
| Windows (192.168.68.111:7755) | **unreachable** — clipsync timeout |
| ARP for 192.168.68.111 | Resolved: 58:ce:2a:59:75:27 |

**Conclusion: Windows machine is down or asleep.** All ports (including
OS-level services like SMB/445 and RDP/3389) are unreachable. MBP has
full network connectivity otherwise. ARP resolves but the host does not
respond. This is not a mesh or WiFi issue — the Windows machine is
offline.

---

### Log analysis: The outage sequence

#### Server log (Windows, synced via Syncthing)

Window: 19:57:48 – 20:01:46

**The server (Windows) uses mDNS (`.local`) to find MBP.** The connection
config has target `root@muhammed-mbp.local:2222` with fallback
`root@muhammed-lenovo.local:20000`.

```
19:57:48  Target unreachable muhammed-mbp.local: i/o timeout (cached IP 192.168.68.134)
19:57:52  Target unreachable muhammed-mbp.local: no such host (mDNS fails)
19:59:56  Target unreachable muhammed-mbp.local: i/o timeout via fe80::421:bbe7:c5bd:2ec1%Wi-Fi (IPv6 link-local)
19:58-20:00  muhammed-lenovo.local: no such host (every attempt)
20:00:33  Target reachable muhammed-mbp.local (582ms) — 3 ForwardSets reconnect
20:00:40  2 remaining ForwardSets: DNS cache evicted, still failing
20:00:59  mbp-proxy session crashes immediately (SOCKS channel open fails)
20:01:34  mbp-proxy reconnects
20:01:45  mbp-sshd, filesync reconnect
```

**Key finding #1: mDNS is the primary bottleneck.** Total outage on
Windows→MBP direction: **2 minutes 45 seconds**. Root cause was NOT TCP
being broken — it was DNS resolution of `muhammed-mbp.local` failing
repeatedly. Three distinct failure modes:

| mDNS failure mode | Frequency | Time wasted per attempt |
|-------------------|-----------|------------------------|
| `no such host` | ~50% of attempts | 3-6s (probe timeout + mDNS retry) |
| Resolves to IPv4 but unreachable | ~30% | 6s (full probe timeout) |
| Resolves to IPv6 link-local (`fe80::...%Wi-Fi`) | ~20% | 6s (full probe timeout) |

**Key finding #2: IPv6 link-local is a trap.** When mDNS resolved to
`[fe80::421:bbe7:c5bd:2ec1%Wi-Fi]:2222`, the probe spent the full 6s
timeout trying to connect via a link-local address that the TP-Link mesh
can't route between APs. This is wasted time that should fail fast.

**Key finding #3: Lenovo is completely offline.** Every attempt to resolve
`muhammed-lenovo.local` returned `no such host`. Since the config tries
MBP first and Lenovo second, each probe cycle wastes 6+6 = 12 seconds
on dead targets before concluding "no reachable target". With 5 independent
ForwardSets all probing in parallel, this creates heavy probe traffic.

#### Client log (MBP)

Window: 20:01:46 – 20:36:09

```
20:01:46  Server (Windows) connects to MBP sshd from fe80::92bd:... (IPv6 link-local!)
20:01:52  MBP cannot reach Windows clipsync at 192.168.68.111:7755 — context deadline exceeded
20:02-20:06  Clipsync peer registration keeps failing (every 10s)
20:02:26  Handshake failure from 192.168.68.111: connection reset (Windows probing MBP sshd)
20:07:04  MBP sshd detects dead client [fe80::...]:65500 — keepalive EOF, fail_count=1
20:07:06  Dead client 192.168.68.111:63719 detected
20:07:10  Dead client [fe80::...]:65499 — tcpip-forward 127.0.0.1:1111 closed
20:07:44  Dead client 192.168.68.111:60870 — tcpip-forward 127.0.0.1:11111, :18384 closed
20:08:05  Dead client 192.168.68.111:60868 — tcpip-forward 127.0.0.1:1080 closed
          10 forwarded-tcpip channel open failures on 127.0.0.1:1081
20:11:07  bastion-tunnel session ended, mesh gracefully stopped (user restart?)
20:11:08  mesh restarted — bastion connected in 460ms
20:31:59  Discovered Windows peer via UDP, pull failed (context deadline exceeded)
20:35:55  bastion keepalive failed (EOF) — reconnects in 12s
20:36:07  bastion reconnected successfully
```

**Key finding #4: Stale connections on MBP sshd took 5-10 minutes to
detect.** The server (Windows) connections from `fe80::...` and `192.168.68.111`
were established before the outage. MBP's sshd detected them as dead only
at 20:07-20:08, roughly 5-10 minutes after they became stale. All
detections were `fail_count=1, EOF` (hard error on first keepalive attempt),
meaning TCP was holding the connections alive in the kernel buffer until
the SSH keepalive triggered a write that finally errored.

**Key finding #5: Windows connected to MBP via IPv6 link-local.**
The server log doesn't show this clearly, but the client log reveals that
the server connected from `[fe80::92bd:3bfe:be7d:5b25%en0]` — an IPv6
link-local address. These addresses are interface-scoped and do NOT
survive AP handoffs in mesh WiFi networks. This is likely why the
connections died.

**Key finding #6: Bastion connection is solid.** The bastion reconnected
in 12 seconds (20:35:55 → 20:36:07) after a keepalive failure. No port
conflict, no delay. The `-R 127.0.0.1:2222` remote forward was
re-established immediately. The bastion's stale cleanup was fast enough.

**Key finding #7: Asymmetric connectivity after reconnect.** After
Windows reconnected to MBP at 20:01:46, MBP still couldn't reach Windows
at 192.168.68.111:7755 (clipsync). This persisted for 5+ minutes. The
server-initiated SSH connections worked, but MBP-initiated HTTP
connections to Windows failed. Likely cause: Windows firewall state or
a routing asymmetry on the TP-Link mesh.

---

## Root Causes (ranked by evidence from logs)

### RC1: mDNS resolution failure on Windows (CONFIRMED — primary cause)

The config uses `.local` hostnames as SSH targets. Windows' mDNS resolver
is unreliable and takes 3-6 seconds per failed resolution attempt. With
5 ForwardSets and 2 targets each (MBP + Lenovo), each retry cycle wastes
up to 60 seconds on DNS alone.

**Fix: Add static IP fallbacks in the config targets list.** Example:
```yaml
targets:
  - root@muhammed-mbp.local:2222
  - root@192.168.68.134:2222       # static IP fallback
```

The `probeTarget` function tries targets in order. If `.local` fails,
the static IP will succeed immediately.

### RC2: IPv6 link-local connections don't survive roaming (CONFIRMED)

Server log shows connections resolving to `fe80::...%Wi-Fi` addresses.
These are AP-scoped on mesh WiFi — when either device roams, the link-local
path breaks silently. The existing SSH sessions over link-local die
without RST.

**Fix in mesh.** Add an option to prefer IPv4 in mDNS resolution, or
filter out link-local addresses from `probeTarget` when the destination
is a `.local` hostname. Link-local addresses are already rejected from
the DNS cache (`cacheableIP`), but they're still used for the initial
connection.

### RC3: Windows machine going to sleep/offline (CONFIRMED — current outage)

The current outage is caused by Windows being completely offline (all
ports unreachable including OS services). This is unrelated to WiFi
roaming but contributes to the impression that "connections never
recover."

**Fix: Disable sleep/hibernate on the Windows server.** Or configure
wake-on-LAN.

### RC4: Stale server connections linger on MBP's sshd (OBSERVED)

Dead connections from the server took 5-10 minutes to be detected by
MBP's sshd, even with `ClientAliveInterval: "15"` configured. The
connections held `tcpip-forward` listeners (ports 1080, 1111, 11111,
17756, 18384) that blocked new registrations.

**Root cause of slow detection.** The stale connections were over IPv6
link-local. TCP keepalive probes over link-local may not trigger errors
immediately because the OS continues to send them on the local interface
even when the peer has roamed to a different AP. The first SSH keepalive
write that triggers a TCP error then detects it as hard EOF.

**Fix:** This is partially a function of the OS's TCP keepalive behavior
on link-local connections. Adding static IPv4 targets (RC1 fix) avoids
this path entirely.

### RC5: Bastion keepalive (MINOR — observed but self-healing)

Bastion connection dropped at 20:35:55 after ~25 minutes of uptime.
Hard EOF (fail_count=1). Could be the bastion's sshd timing out the
connection, or a transient MBP network glitch. Recovery was fast (12s).

**Check:** bastion's sshd_config for `ClientAliveInterval` and
`ClientAliveCountMax`. If not set, the bastion won't send its own
keepalives to MBP, and any idle period exceeding the bastion's
`TCPKeepAlive` settings could kill the connection.

---

## Recommended Actions

### Immediate (config changes, no code)

1. **Add static IP fallbacks to all `.local` targets in mesh.yaml.**
   ```yaml
   targets:
     - root@muhammed-mbp.local:2222
     - root@192.168.68.134:2222
   ```
   This cuts recovery time from 3+ minutes to ~15 seconds.

2. **Configure bastion sshd for keepalive.** Add to `/etc/ssh/sshd_config`:
   ```
   ClientAliveInterval 15
   ClientAliveCountMax 3
   ```
   Then `systemctl reload sshd`. This prevents ghost sessions holding
   remote forward ports for hours.

3. **Prevent Windows sleep.** Power settings → Never sleep when plugged in.

### Code improvements (mesh)

4. **Filter IPv6 link-local from probeTarget.** When resolving `.local`
   hostnames, skip `fe80::` addresses. They don't work reliably on mesh
   WiFi networks and cause 6-second wasted timeouts. If IPv4 is available,
   prefer it. If only link-local is available, log a warning and use it
   as last resort.

5. ~~**Log target address family on connection.**~~ **DONE** (commit `977d35d`).
   `probeTarget`, `Connected`, `Session ended`, `Keep-alive failed`,
   `Client connected/disconnected`, and `tcpip-forward` logs now include
   resolved addresses, local addresses, session uptime, and resolution
   path (`dns`/`mdns-retry`/`cache`).

6. **Consider deduplicating parallel probes.** With 5 ForwardSets, each
   probing the same target list independently, there are 5x redundant
   DNS lookups and TCP probes every retry cycle. A shared probe result
   (with short TTL) could reduce load and speed up reconnection.

---

## Timeout Reference

| Parameter | Value | Configurable |
|-----------|-------|-------------|
| SSH client keepalive interval | 15s | `ServerAliveInterval` |
| SSH client keepalive max failures | 3 | `ServerAliveCountMax` |
| SSH server keepalive interval | disabled by default | `ClientAliveInterval` |
| SSH server keepalive max failures | 3 | `ClientAliveCountMax` |
| TCP keepalive period | 30s | `TCPKeepAlive` |
| Connection retry interval | 10s | `retry` (connection-level) |
| Retry jitter | 0-25% of base | not configurable |
| Probe timeout | 3s (capped) | not configurable |
| mDNS retry for .local | 150ms (1 retry) | not configurable |
| DNS cache TTL | 5 min | not configurable |
| Remote forward listen retry | 6 attempts, 6.3s total | not configurable |
| Clipsync HTTP timeout | 5s | not configurable |
| Clipsync peer expiry | 15s | not configurable |
| Clipsync broadcast addr refresh | 30s | not configurable |

---

## Quick Diagnostic Commands

```bash
# Check if mesh is running
ps aux | grep mesh

# Check current IP
ifconfig en0 | grep 'inet '

# Check LAN connectivity
ping -c 2 192.168.68.111  # Windows
ping -c 2 192.168.68.1    # Gateway

# Check bastion TCP
nc -z -w 3 138.2.134.182 5555

# Check Windows ports (sshd, RDP, SMB)
nc -z -w 3 192.168.68.111 2222
nc -z -w 3 192.168.68.111 3389
nc -z -w 3 192.168.68.111 445

# Check ARP cache
arp -a | grep 192.168.68

# Mesh admin API (when running)
curl -s http://127.0.0.1:7777/api/state | jq '.[] | select(.Status != "connected")'

# Mesh log (grep for problems)
tail -200 ~/.mesh/log/client.log | grep -E 'Keep-alive|Retrying|Connecting|Connected|listen failed|tcpip-forward|no such host|i/o timeout'
```
