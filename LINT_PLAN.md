# Lint Fix Plan

Generated: 2026-04-01. Tracks `task quality` findings (126 issues).

## Status

| # | Category | File(s) | Issue | Status |
|---|---|---|---|---|
| 1 | bug | `internal/proxy/http.go:223` | IPv6 buffer panic: `resp` is 10 bytes, `resp[:18]` panics | ✅ done |
| 2 | dead code | `cmd/mesh/format.go:367` | `formatMetrics` unused | ✅ done |
| 3 | staticcheck | `cmd/mesh/dashboard.go:251` | S1031: unnecessary nil check before range | ✅ done |
| 4 | staticcheck | `internal/clipsync/clipsync.go:1213,1224,1234` | QF1012: `WriteString(fmt.Sprintf(...))` → `fmt.Fprintf` | ✅ done |
| 5 | gocritic | `cmd/mesh/dashboard.go:208` | `strings.Index` → `strings.Cut` | ✅ done |
| 6 | gocritic | `cmd/mesh/dashboard.go:362,716` | if-else chain → switch | ✅ done |
| 7 | gocritic | `internal/proxy/socks5.go:41` | unlambda: replace wrapper func with `net.Dial` | ✅ done |
| 8 | gocritic | `internal/tunnel/tunnel.go:393` | if-else chain → switch | ✅ done |
| 9 | security | `cmd/mesh/main.go:287` | G301: dir perm 0755 → 0750 | ✅ done |
| 10 | security | `internal/clipsync/clipsync.go:106` | G301: dir perm 0755 → 0750 | ✅ done |
| 11 | security | `internal/clipsync/clipsync.go:287,563` | G306: file perm 0644 → 0600 | ✅ done |
| 12 | security | `cmd/mesh/main.go:361` | G112: missing ReadHeaderTimeout (Slowloris) | ✅ done |
| 13 | nolint | `internal/tunnel/tunnel.go:90` | G408: PublicKeyCallback — rate-limiter state is intentional | ✅ done |
| 14 | nolint | `internal/tunnel/tunnel.go:400` | G106: InsecureIgnoreHostKey — explicit user opt-out | ✅ done |
| 15 | nolint | `internal/tunnel/tunnel.go:1305` | G404: math/rand for jitter — non-security use | ✅ done |
| 16 | nolint | `internal/clipsync/clipsync.go:654` | G602: false positive — sha256 always [32]byte | ✅ done |
| 17 | nolint | `internal/clipsync/clipsync.go:315,454` | G704: SSRF — peer URLs are user-configured | ✅ done |
| 18 | nolint | `internal/clipsync/clipsync.go:735` | G204: subprocess — intentional password_command | ✅ done |
| 19 | nolint | `internal/tunnel/tunnel.go:376` | G204: subprocess — intentional password_command | ✅ done |
| 20 | nolint | `internal/tunnel/shell_unix.go:181` | G204: subprocess — intentional shell feature | ✅ done |
| 21 | nolint | `cmd/mesh/main.go:793` | G204: subprocess — intentional shell feature | ✅ done |
| 22 | security | `internal/clipsync/clipsync.go:292,314,321` | G706: log injection — sanitize peer addr in logs | todo |
| 23 | errcheck | `internal/clipsync/clipsync.go` (prod) | unchecked Close/Remove errors | todo |
| 24 | errcheck | test files | unchecked Close errors in tests | todo |
| 25 | complexity | `internal/tunnel/tunnel.go` | gocognit/cyclop — structural refactor | defer |
| 26 | complexity | `cmd/mesh/main.go`, test files | cyclop — structural refactor | defer |

## Current State (2026-04-01)

All actionable issues resolved. 41 remaining issues are pure complexity warnings
(cyclop/gocognit/funlen) requiring structural refactoring — deferred.

| Category | Before | After |
|---|---|---|
| bug | 1 | 0 |
| unused | 1 | 0 |
| errcheck | 50 | 0 |
| gosec | 26 | 0 |
| staticcheck | 4 | 0 |
| gocritic | 5 | 0 |
| cyclop | 7 | 8* |
| gocognit | 31 | 31 |
| funlen | 2 | 2 |

*cyclop +1: `parseIPv4` switch refactor added one branch, bumping it over threshold.


## Commit Plan

- **Commit 1**: #1 bug fix (IPv6 panic)
- **Commit 2**: #2–8 dead code + staticcheck + gocritic
- **Commit 3**: #9–12 security hardening (permissions + Slowloris)
- **Commit 4**: #13–21 nolint suppressions for accepted false positives
- **Commit 5**: #22 log injection sanitization
- **Commit 6**: #23–24 errcheck (prod then tests)
