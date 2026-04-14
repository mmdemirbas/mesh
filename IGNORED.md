# IGNORED.md

Items considered and deliberately skipped. Each entry explains why.

| ID | Item | Reason |
|----|------|--------|
| RD1 | [Add static IP fallbacks to `.local` targets](#rd1-static-ip-fallbacks-for-local-targets) | DNS cache (P15) already provides this dynamically. |

---

## RD1: Static IP fallbacks for `.local` targets

**Source:** ROAMING-DEBUG.md recommendation #1.

**Suggestion:** Add `root@192.168.68.134:2222` as a fallback target after
`root@muhammed-mbp.local:2222` in the config, so when mDNS fails the
static IP is tried immediately.

**Why ignored:** The DNS result cache (P15, commit `384423c`) already
does this dynamically. `probeTarget` stores the resolved IP from every
successful `.local` connection in `resolvedAddrCache` (5-minute TTL).
When mDNS fails on subsequent probes, the cache is consulted as an
automatic fallback — no config duplication needed. The cache also
self-heals: entries are evicted on connection failure, and refreshed on
every successful connection. Hardcoding IPs in config would be fragile
(DHCP leases change) and redundant with the cache.
