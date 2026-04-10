# DONE.md

Completed items. Moved out of [PLAN.md](PLAN.md) to keep the roadmap focused on
active work. Most rows are one-liners; a few still carry the pre-implementation
design notes that informed the change — kept for context.

## Log

| ID   | Item                                | Notes |
|------|-------------------------------------|-------|
| P1   | Profile and optimize CPU + memory   | Regex → byte scanning, dashboard dirty check, log ring allocation, metrics caching, SSE JSON encoder reuse. Commits `363f775`, `a27bbfa`, `3bb6b4d`. |
| F8   | SSH signal forwarding               | Unix: `syscall.Kill(-pid, sig)`. Windows: `Process.Kill()` for KILL/TERM/INT/HUP. |
| R8   | systemd / launchd plist             | Promoted to D2. |
| E1   | Server-side panic recovery          | `recover()` on all SSH channel, SOCKS5, HTTP proxy handler goroutines. |
| E3   | `postHTTP` nil panic fix            | Error check on `http.NewRequestWithContext`; malformed peer logged and skipped. |
| C1   | `keepalive@openssh.com` support     | Server replies `true` to OpenSSH client keepalives. |
| S2   | Clipsync peer authentication        | `canReceiveFrom` validates against loopback, static peers, and discovered peers. |
| S3   | Gzip decompression bomb defense     | `io.LimitReader` on gzip decoder in clipsync (200 MB) and filesync (40 MB). |
| E2   | Gateway marshal error handling      | `json.Marshal` errors return 500 instead of empty 200. |
| W1   | Cross-platform `password_command`   | Build-tagged `shellCommand()`: `sh -c` on Unix, `pwsh.exe`/`cmd.exe` on Windows. |
| W2   | Cross-platform ignore matching      | `filepath.Match` → `path.Match` for forward-slash consistency. |
| W3   | Cross-platform atomic rename        | `renameReplace()` helper: remove-then-rename fallback for Windows. |
| S12  | Windows port hijacking defense      | `SO_REUSEADDR` → `SO_EXCLUSIVEADDRUSE` on Windows. |
| S4   | Admin loopback-only bind            | Config validation rejects non-loopback `admin_addr`. |
| S5   | Per-connection channel limit        | `atomic.Int64` counter, reject above 1000 with `ssh.ResourceShortage`. |
| C2   | SSH user defaults to OS username    | `os/user.Current().Username` instead of hardcoded `root`. |
| U1   | Forward.Type defaults to forward    | `validateForwards` applies default when Type is empty. |
| P4   | Delta response size cap             | Capped at 256 MB instead of 4 GB. |
| S9   | Truncate upstream error logs        | Gateway upstream error body truncated to 512 bytes. |
| S6   | IPv4-mapped IPv6 loopback           | `net.ParseIP(ip).IsLoopback()` in filesync. |
| E4   | Keepalive interval parse warning    | Logs warning on non-integer `ServerAliveInterval`. |
| E5   | SSH key error context               | `loadSigner`/`loadAuthorizedKeys` errors include file path. |
| W6   | UNC home directory safety           | `filepath.VolumeName()` instead of `home[:2]` in Windows session env. |
| S7   | Reject wildcard remote forward bind | `clientspecified` rejects 0.0.0.0 / ::; requires `GatewayPorts=yes`. |
| DOC1 | JSON Schema completeness            | Added `accept_env`, `banner`, `motd` to Listener definition. |
| P9   | `hashFile` hex encoding             | `hex.EncodeToString` instead of `fmt.Sprintf("%x")`. |
| P10  | `hashEqual` compiler intrinsic      | `bytes.Equal` instead of manual byte loop. |
| Q5   | Named timing constants              | `defaultTCPKeepAlive`, `defaultHandshakeTimeout`, `defaultSSHClientTimeout`, `defaultServerAliveInterval`. |
| Q7   | Keepalive sentinel constant         | `keepaliveForwardSet` named constant replaces magic string coupling. |
| Q8   | Gateway marshal error handling      | Removed `mustMarshal`; explicit `json.Marshal` with error propagation at all sites. |
| E6   | Rate limiter WaitN error            | `rateLimitedReader.Read` propagates `WaitN` error to callers. |
| E8   | SnapshotFull atomicity comment      | Corrected to reflect sync.Map.Range independence from mu.RLock. |
| P11  | Pre-parse PermitOpen                | `permitOpenPolicy` struct with map-based O(1) matching at server startup. |
| P12  | Normalize GetOption keys            | `normalizeOptions` lowercases at load; `GetOption` O(1) map lookup. |
| W4   | Windows permission check            | Build-tagged no-op in `perm_windows.go`. |
| W5   | expandHome backslash support        | Handles standalone `~`, `~/`, and `~\`. |
| C5   | Unique stream message IDs           | `generateMsgID()` with crypto/rand hex per stream response. |
| C8   | Reject n > 1                        | `translateOpenAIRequest` returns error for n > 1. |
| C9   | Temperature clamp warning           | Logs warning before clamping to Anthropic max 1.0. |
| D7   | Health check endpoint               | `GET /healthz` returns 200 "ok" on admin server. |
| D4   | Stale PID file detection            | `upCmd` checks running PID, rejects if alive, removes if stale. |
| U5   | Exit code documentation             | Documented 0/1/3 in `printUsage` Exit Codes section. |
| U10  | `mesh down` exit code               | Exits 3 when no nodes were stopped. |
| Q2   | Metrics.Reset() method              | Deduplicated 4 identical reset blocks in tunnel.go. |
| DOC2 | `printUsage` help command           | Added `help` to commands list. |
| DOC3 | `printUsage` tagline                | Updated to mention clipsync, filesync, gateway. |
| DOC4 | SSH options completeness            | Added `StrictHostKeyChecking` to help text. |
| DOC5 | README API endpoints                | Added /healthz, /api/filesync/*, /api/clipsync/activity. |
| DOC6 | README dist claim                   | Fixed to show 4 actual binaries. |
| DOC9 | Schema options description          | Updated to list all 16 supported SSH option keys. |
| DEP1 | Go 1.26.2 upgrade                   | Fixed 4 active CVEs (x509 auth bypass, chain, policy; TLS 1.3 KeyUpdate DoS). |
| DOC7 | CLAUDE.md CGO claim                 | Corrected to note darwin omits CGO_ENABLED. |
| DOC8 | CLAUDE.md platform-code claim       | Updated to reflect ongoing runtime.GOOS migration. |
| Q1   | Shared gzip package                 | Extracted internal/gziputil from clipsync and filesync duplicates. |
| P5   | Pooled gzip.Writer                  | sync.Pool in gziputil.Encode avoids ~300 KB allocation per request. |
| E7   | Node name in validation errors      | main.go wraps Validate() errors with node name for multi-node clarity. |
| S10  | Reject world-writable configs       | checkInsecurePermissions returns error instead of warning; LoadUnvalidated fails on 0022 perms. |
| W8   | Build-tagged checkPid/killPid       | Moved to pid_unix.go / pid_windows.go. Removed runtime.GOOS from main.go. |
| P6   | Avoid proto.Marshal for size        | processPayload receives body size from caller instead of re-marshaling. |
| P7   | Active file count from scan         | scan() returns count; activeCount() method replaces 4 inline loops. |
| P8   | Temp cleanup merged into scan       | Stale .mesh-tmp-* cleaned during walk instead of separate traversal. |
| P13  | SnapshotFull in dashboard           | Single call replaces separate Snapshot + SnapshotMetrics. |
| U8   | Dynamic log tail lines              | Computed from viewport height instead of hardcoded 10. |
| U2   | mesh down without config            | Discovers running nodes from ~/.mesh/run/mesh-*.pid. |
| U6   | Help text completeness              | Mentions filesync, gateway, admin web UI. |
| U7   | Config-not-found guidance           | Shows search paths and usage hint instead of raw OS error. |
| U4   | YAML line numbers in errors         | Parse errors include the line number of the failing node. |
| U9   | Web UI tab-gated fetches            | Only fetches APIs needed by the active tab. |
| N6   | Tree-table component grouping       | Dashboard groups components by type with collapsible headers. |
| Q6   | Generic activeNodes registry        | internal/nodeutil.Registry[T] replaces duplicate patterns in clipsync/filesync. |
| Q3   | Extract connectSSH helper           | Shared dial+keepalive+handshake for runForwardSet and runForwardSetForTarget. |
| Q4   | Extract doUpstreamRequest           | Shared upstream HTTP lifecycle for handleA2O and handleO2A. |
| D9   | Data-path benchmarks                | 16 benchmarks across 5 packages: BiCopy, scan, blockHash, state, gzip. |
| R5   | Demo tape for GIF generation        | VHS format demo.tape added; run `vhs demo.tape` to generate. |
| W7   | Clipsync build-tagged platform code | 6 runtime.GOOS switches → clipboard_darwin.go, clipboard_linux.go, clipboard_windows.go, clipboard_other.go. |
| D5   | Test parallelism                    | t.Parallel() added to 343 test functions and subtests across all packages. |
| S8   | Document PermitOpen limitation      | Added code comment and help text: string-based matching, use IPs for strict enforcement. |
| DEP2 | Isolate schema-gen module           | cmd/schema-gen has its own go.mod with replace directive. buger/jsonparser removed from main module. |
| DEP3 | Remove Charmbracelet TUI            | Replaced bubbletea/bubbles with raw terminal + golang.org/x/term. 21 transitive modules removed. |
| N3   | File/image copy for clipsync        | file_copy config flag, configurable max_file_copy_size, gated file poll in clipboard loop. |
| N4   | Filesync activity history           | SyncActivity ring buffer, GET /api/filesync/activity, Recent Activity table in web UI. |
| F2   | `mesh init` command                 | Interactive config generator: node name, role, SSH config. Writes minimal YAML. |
| F5   | SFTP subsystem                      | subsystem "sftp" handler using github.com/pkg/sftp. Read-only, configurable root. |
| F6   | SSH agent forwarding                | auth-agent-req@openssh.com handler. Temp Unix socket, SSH_AUTH_SOCK env injection, bidirectional proxy. |
| B1   | Fix stale state eviction            | `evictStale` was removing `Listening`/`Connected` entries after 1 hour. Stable states now exempt from eviction. |
| B2   | Fix dashboard defer order + leaks   | Raw mode restored before alt-screen leave; stdin goroutine leaked on ctx.Done; SIGWINCH not wired into event loop; double buildContent on keypress. |
| B3   | Fix SFTP Serve blocking reqs loop   | `sftp.Serve()` blocked the channel request drain loop, stalling the SSH mux. Moved to goroutine. Reply deferred until NewServer confirms success. |
| B4   | Fix agent fwd double close + leak   | `ln.Close()` called from two goroutines without sync.Once. `io.Copy` used receive-one pattern instead of WaitGroup. |
| B5   | Fix maxRequestBodySize scaling      | Compile-time constant did not scale with configured `max_file_copy_size`. Now computed from `maxFileSize` at startup. |
| B6   | Fix schema-gen AddGoComments path   | Relative path `./internal/config` was wrong when running from `cmd/schema-gen/`. Fixed to `../../internal/config`. |
| D11  | End-to-end Linux test harness       | `e2e/harness/` primitives, four scenarios (S1 SSH bastion, S2 filesync, S3 clipsync, S4 gateway + stub-llm), `e2e/churn/` stress suite, `e2e/compose/` manual playground. Wired into `task check` with `FAST=1` escape hatch. Details below. |
| B7   | Fix peerMatchesAddr IPv6 canon      | `peerMatchesAddr` did literal string compare; equal IPv6 addresses in different canonical forms (short vs. expanded, mixed case) silently rejected with 403. Parse via `net.ParseIP` and compare with `net.IP.Equal`. Test `ae922b6`, fix `1c3954c`. |
| B8   | Resolve filesync peer hostnames     | Hostnames were never resolved, so `server:7756` never matched a request from its DNS IP. `FilesyncCfg.Resolve` expands each peer host via `net.LookupHost` into `FolderCfg.AllowedPeerHosts`; `isPeerConfigured` compares against that. Test `ae922b6`, fix `a610184`. |
| B9   | loadFormatsFromDir per-format cap   | Docstring promised `MaxFileCopySize` overrode the 50 MB constant, but the assembler hardcoded it. Threaded `maxFileSize int64` as parameter; `readClipboardFormats` forwards `n.maxFileSize`. Test `dff020e`, fix `8c945c5`. |
| H1   | Address and host equality audit     | Filesync peers covered by B7/B8. Two additional findings in clipsync: `canReceiveFrom` used literal host string compare (test `51fa334`, fix `faf3155`), `Broadcast` echo-suppression did the same with sender origin (test `aef49e1`, fix `be3668a`). Both now route through `peerHostEqual`. Tunnel `PermitOpen` is string-based by design (see S8 in DONE.md); proxy has no address equality. |

## Historical Notes

Pre-implementation design notes kept for context after the work landed.

### S8: PermitOpen bypass via alternate hostnames

**Goal:** Prevent clients from bypassing `PermitOpen` restrictions by supplying a hostname that resolves to the same IP as a blocked target.

**Approach:**
- Option A (document-only): Add a code comment on `parsePermitOpen` and a user-facing note in `printHelp` explaining that matching is string-based on the `DestAddr` field from the SSH protocol, not on resolved IPs, and that operators should use IP addresses in `PermitOpen` if strict enforcement is needed.
- Option B (resolve at check time): In `handleDirectTCPIP`, after policy denies by name, attempt a DNS lookup of `DestAddr` and re-check each resolved IP against the policy. Log the lookup result. Cap lookup timeout at 2s via `context.WithTimeout`.
- Recommend Option A as the safer default (avoids TOCTOU, DNS lookup latency on every channel open, and DNS poisoning risk). Option B as an opt-in `PermitOpenResolve: yes` SSH option.

**Key decisions:** Whether resolution should happen at all inside the SSH server, or whether operators are responsible for using IPs in policy. The current pre-parsed `permitOpenPolicy` struct can be extended with an `allowResolve bool` field.

**Risks/dependencies:** DNS resolution inside the SSH server adds latency and a new external dependency. TOCTOU is inherent to any resolve-then-connect pattern. Option A has no risk.

**Effort:** Option A is XS (comment + help text). Option B is M (context-aware DNS lookup, tests for bypass scenario).

### DEP2: cmd/schema-gen pulls CVE-affected dep

**Goal:** Isolate `cmd/schema-gen` and its `buger/jsonparser` dependency from the main module to prevent CVE exposure in the production binary.

**Approach:**
- Create `cmd/schema-gen/go.mod` as a separate Go module (e.g. `github.com/mmdemirbas/mesh/tools/schema-gen`).
- Move `cmd/schema-gen/main.go` imports to use a `replace` directive pointing to the parent module's `internal/config` package, or vendor the relevant structs.
- Update `Taskfile.yml` `schema` task to `cd cmd/schema-gen && go generate` or `go run .` within that directory.
- Add `.gitignore` entry for `cmd/schema-gen/go.sum` if desired, or commit it.
- After extraction, run `go mod tidy` on the main module and confirm `buger/jsonparser` is no longer in `go.sum`.

**Key decisions:** Whether `cmd/schema-gen` needs to import `internal/config` (yes, it does — `config.Config` is the schema root). A `replace` directive in the tool module pointing to `../..` handles this cleanly.

**Risks/dependencies:** The `replace` directive means the tool module is not independently publishable, but that is acceptable for a dev tool. CI must `cd cmd/schema-gen && go mod tidy` separately.

**Effort:** S — module extraction is mechanical. The `replace` directive pattern is well-understood.

### DEP3: Charmbracelet TUI pulls 17 transitive modules

**Goal:** Reduce the transitive dependency footprint introduced by `charmbracelet/bubbletea` and `charmbracelet/bubbles`.

**Approach:**
- Audit which Charmbracelet features are actually used: `bubbletea` is used for the `viewport` component in the web UI or dashboard. Identify the exact call sites.
- If only `viewport` (scroll buffer) is used, replace it with a hand-rolled ring-buffer + ANSI scroll approach using raw `golang.org/x/term` — which is already a direct dependency.
- If `lipgloss` styling is used, evaluate replacing it with the existing `cReset`/`cBold` ANSI vars already in the codebase.
- If Charmbracelet provides significant value (interactive widgets, input handling), keep it and accept the transitive cost, but document the decision.
- Run `go mod why github.com/charmbracelet/bubbletea` and `go mod graph` to map actual usage.

**Key decisions:** Whether the UX value of Charmbracelet justifies 17 extra modules in the binary. If the dashboard already uses raw ANSI (which it does — alternate screen, in-place overwrite), the TUI library may be redundant.

**Risks/dependencies:** Removing Charmbracelet without a replacement risks regressions in any interactive component that depends on its event loop. Full audit required before removal.

**Effort:** M for audit + replacement of one component. L if multiple components depend on the library.

### N3: File/image copy support for clipsync

**Goal:** Allow users to copy a file or image on one machine and paste it on another via the existing clipsync push mechanism.

**Approach:**
- Extend `SyncPayload` protobuf to include a `files` repeated field: `{name, size, data bytes}`.
- On the sender: when the clipboard contains file paths (macOS pasteboard `NSFilenamesPboardType`, Windows `CF_HDROP`), read each file up to `maxSyncFileSize` (50 MB, already defined) and populate the `files` field.
- On the receiver: write received files to a staging directory (`~/.mesh/clipsync-received/`), then set the clipboard to point to those paths.
- Image clipboard (`image/png`, `image/jpeg`): already flows through as raw bytes in the existing `formats` field. No change needed.
- Large files (>50 MB): log and skip for now; the PLAN.md note calls for a lazy-copy design as a follow-on.
- Gate behind `file_sync: true` config flag on `ClipsyncCfg` to avoid surprising users.

**Key decisions:** Where to stage received files (temp dir vs. fixed location). Fixed location (`~/.mesh/clipsync-received/`) is safer for paste operations. Cleanup policy (delete on next sync? TTL?).

**Risks/dependencies:** OS clipboard APIs for setting file paths vary significantly: macOS uses pasteboard item file URLs, Windows uses `CF_HDROP`, Linux uses `XDG_CURRENT_DESKTOP`-dependent methods. Build-tagged clipboard writers needed for each platform.

**Effort:** M for small-file path (protobuf extension + per-platform clipboard write). L if lazy-copy for large files is included.

### N4: Action history in web UI

**Goal:** Surface a chronological log of clipboard sync events and file sync events in the admin web UI.

**Approach:**
- Clipsync already maintains `activityHistory []*ClipActivity` (ring buffer of 20) exposed via `GET /api/clipsync/activity`. This is the foundation.
- Add a parallel `syncHistory` ring buffer to the filesync `Node` struct: `{time, folder, peer, direction, files int, bytes int64, err string}`. Populate it on each completed delta sync in the `syncWith` path.
- Expose via `GET /api/filesync/activity` (analogous to the clipsync endpoint).
- In the web UI, add an "Activity" tab (or section within Clipsync/Filesync tabs) that polls `/api/clipsync/activity` and `/api/filesync/activity` every 5 seconds and renders a combined timeline table.
- Gate polling by active tab (U9 fix) to avoid unnecessary requests.

**Key decisions:** Combined vs. per-component activity feed. Per-component is simpler and matches the existing API shape.

**Risks/dependencies:** The filesync `syncHistory` struct must be goroutine-safe (use a mutex-protected ring, same pattern as `logRing`). Size of the ring (20 entries matches clipsync; 50 may be better for filesync which is more active).

**Effort:** S for the filesync ring buffer and API endpoint. M for the web UI tab, depending on current UI structure.

### F2: `mesh init` command

**Goal:** Provide an interactive config generator that scaffolds a starter `mesh.yaml` for new users.

**Approach:**
- Add `case "init":` to the `main.go` switch. No config file required for this command.
- Use `bufio.Scanner` on `os.Stdin` for prompts (no new dependency; raw terminal not required).
- Ask in sequence: node name, role (client/server/both), listen address (for server), SSH key path, known_hosts path.
- Write a minimal YAML to the config path (default `~/.config/mesh/mesh.yaml` or path from `-f` flag).
- If the file already exists, ask before overwriting.
- Validate the generated config with `config.Load()` before writing.
- Print next steps: `mesh up`, `mesh status`.

**Key decisions:** Interactive prompts vs. flag-driven non-interactive mode. Flags (`--node`, `--role`, `--listen`) allow scripted use; prompts serve new users. Both can be supported with prompts falling back to flags.

**Risks/dependencies:** No new dependencies. The existing `config.Load` and `validate` functions provide validation. Windows line endings in the generated YAML must be `\n` (Go's `os.WriteFile` is platform-agnostic on content).

**Effort:** S — the command is self-contained and requires no library additions.

### F5: SFTP subsystem

**Goal:** Handle `subsystem sftp` requests in the SSH server, enabling `scp`, `sftp`, and `rsync` over mesh tunnels.

**Approach:**
- Add `github.com/pkg/sftp` as a dependency (requires approval per project rules).
- In `handleSession` (tunnel.go), detect `subsystem` channel requests with name `"sftp"`.
- Create an `sftp.NewRequestServer(channel, sftp.Handlers{...})` with read/write/list/stat handlers rooted at the authenticated user's home directory or a configurable `sftp_root` path on the listener.
- Respect `chroot_dir` if set (similar to OpenSSH's `ChrootDirectory`); use `filepath.Clean` + prefix check for path traversal safety.
- Add `sftp_enabled: true` and optional `sftp_root: /path` to `ListenerCfg`.
- Build-tag: SFTP handlers are pure Go; no platform-specific code needed.

**Key decisions:** Whether to expose the full filesystem or a chrooted subtree. Chrooted is safer and should be the default. Whether to support write operations or read-only initially.

**Risks/dependencies:** `github.com/pkg/sftp` adds a dependency — requires explicit approval. The library is mature and widely used. Without it, a from-scratch SFTP implementation is not feasible.

**Effort:** M — adding the dependency and wiring the subsystem handler is straightforward. Path traversal hardening and `sftp_root` config add a day of work.

### F6: SSH agent forwarding

**Goal:** Forward the client's SSH agent to remote sessions so users can use their local keys on remote hosts without copying private keys.

**Approach:**
- In `handleSession`, detect `auth-agent-req@openssh.com` channel request.
- Create a temp Unix socket at a path like `/tmp/mesh-agent-<sessionID>.sock`.
- Set `SSH_AUTH_SOCK` in the session environment (inject into `execEnv` before starting the shell/command).
- Start a goroutine that accepts connections on the socket and proxies them through a new SSH channel of type `auth-agent@openssh.com` back to the client.
- Clean up the socket in `defer` when the session ends.
- Unix-only: gate behind `//go:build !windows` build tag.

**Key decisions:** Socket path and cleanup. Using `os.MkdirTemp` for the socket directory is cleaner than a fixed `/tmp` path. Whether to support agent forwarding in `send-only` direction only or bidirectionally.

**Risks/dependencies:** The `auth-agent@openssh.com` channel type must be opened on the *existing* SSH connection, not a new one. The client must also support agent forwarding on their end. Unix socket support on macOS and Linux is consistent; not applicable to Windows.

**Effort:** M — the Unix socket proxy loop is the core complexity. Session lifecycle integration (env injection, cleanup) adds another half-day.

### D11: End-to-end Linux test harness

**Goal:** Exercise real mesh binaries across multiple containers to catch integration bugs that unit tests cannot reach. Cover SSH tunneling, filesync, clipsync, and gateway flows with deterministic, automated scenarios. Integrate as the final gate of `task check` so no release can ship with a broken end-to-end path.

**Layout as shipped:**

```
e2e/
  Dockerfile.e2e           alpine + mesh binary + stub-llm + fake xclip
  fixtures/
    fake-xclip             shell script — reads/writes /tmp/mesh-clip/<target>
    configs/               per-scenario YAML templates
  harness/                 testcontainers-go helpers, build tag: e2e || e2e_churn
    network.go             per-test bridge network
    node.go                Node: Start, Exec, WriteFile, ReadFile, Admin*, metrics
    artifacts.go           on-failure dump rooted at e2e/build/artifacts/
    fixtures.go            config template loader (runtime.Caller-anchored path)
    sshkeys.go             ed25519 KeyPair helper
    wait.go                Eventually + WaitForComponent admin-state polling
  scenarios/               build tag: e2e
    ssh_bastion_test.go    S1
    filesync_test.go       S2
    clipsync_test.go       S3
    gateway_test.go        S4
  churn/                   build tag: e2e_churn (nightly only)
    filesync_churn_test.go
  stub/                    canned-response HTTP server for the gateway (e2e tag)
  compose/                 manual playground (docker-compose.yaml + keys + README)
```

**Image & build.** Single image `mesh-e2e:local` based on `alpine:3.20` with `ca-certificates tcpdump openssh-client iproute2 coreutils curl bash jq`. The mesh binary, the stub-llm binary, and the fake xclip shim are baked in at build time — the automated scenarios never volume-mount them. `task build:linux`, `task build:stub-linux`, and `task build:e2e-image` are content-hash gated so repeat runs are no-ops.

**Scenario mechanism.** Admin APIs bind to `127.0.0.1` per config validation, so the harness reaches them through `docker exec curl` rather than host port mappings. Mesh's filesync peer validator is IP-based with no DNS resolution, so the automated filesync / churn scenarios override the entrypoint with a sh wrapper that polls `getent hosts` at startup, substitutes the resolved IP into a placeholder in the YAML, and then execs mesh. The fake xclip lets clipsync be driven by writing and reading `/tmp/mesh-clip/<target>` — **no production code change**, no debug endpoint. `peer1` starts with a no-op wait strategy in scenarios where bidirectional DNS resolution would otherwise deadlock testcontainers' sequential `Started` hook.

**Scenarios as shipped:**

- **S1 SSH bastion tunnel.** `client → bastion → server` with `PermitOpen server:22`. Asserts `connection:bastion` → `connected`, `forward:ssh-to-server` → `listening`, `ssh -p 2222 root@127.0.0.1 whoami` → `root`, non-zero `mesh_bytes_{tx,rx}_total`, bastion stop/restart drives the connection through `retrying` back to `connected`.
- **S2 filesync two-peer.** `send-receive` folder with five phases: 10-file initial propagation; reverse edit + no leftover `.mesh-tmp-*`; delete propagation; conflict file on simultaneous edits; restart convergence.
- **S3 clipsync discovery + push.** Three peers in group `test`, a fourth in group `other`. Bidirectional text propagation through the fake xclip, group isolation, and a 40 MB payload round trip (chosen under the strict 50 MB `defaultMaxSyncFileSize` per-format cap — tighter than the 100 MB `maxClipboardPayload` total).
- **S4 LLM gateway.** Five gateway instances against a single stub container. Non-streaming A→O and O→A happy paths, upstream 529 → client 503, malformed upstream body → client 502 (the implementation returns `BadGateway` rather than the plan's 500), and an SSE streaming round trip.

**Churn suite.** Three tests, all under the 2-minute per-test budget: 1000-file propagation, 30-file rename storm (hard assertion on add-side convergence; rename-side delete backlog is logged as a soft signal — a real but out-of-scope filesync limitation), and bidirectional concurrent edits under fsnotify pressure. Full suite runs in ~140s.

**Compose playground.** Static-IP topology (172.30.0.0/24) with `client`, `bastion`, `server`, `stub-llm`. All four features active at once. Pre-generated ed25519 playground keys live in `e2e/compose/keys/` — explicitly documented as playground-only, not for production. Manual-only; not invoked by any automated test.

**Task integration.** `task check` runs the gates in order: `go vet` → `staticcheck` → `gofmt -l` → `go mod tidy -diff` → `go test -race -count=1 ./...` → `go build ./...` → `task e2e`. `FAST=1 task check` skips only the final e2e step. `task e2e`, `task e2e:churn`, and `task e2e:full` run the e2e, churn, and combined lanes directly; `task e2e:compose:up` / `down` manage the playground.

**Dependencies.** `github.com/testcontainers/testcontainers-go` is the only new dependency. Every file that imports it carries `//go:build e2e` or `//go:build e2e || e2e_churn`, so `go build ./...` and the release binary never link it even though it appears in `go.mod`. Docker is a hard prerequisite for the e2e lane; machines without Docker must use `FAST=1 task check`.

**Key decisions (locked as shipped):**

- Fake `xclip` over a debug endpoint — keeps production code clean.
- Baked-in binary over volume mount — determinism over rebuild speed.
- `task check` runs everything by default; `FAST=1` is the opt-out.
- Compose is a full-topology playground, not a scenario mirror.
- Per-test bridge network, no host port mapping for the admin API.

**Findings surfaced while implementing.** None of these were fixed as part of D11; they are flagged here for follow-up:

1. Filesync peer validation is exact-string IP matching with no DNS. Breaks container-name configs; worked around with an sh wrapper in the scenarios and with static IPs in the compose playground.
2. Clipsync has two size caps — `maxClipboardPayload` (100 MB total) and `defaultMaxSyncFileSize` (50 MB per format, strictly enforced at read time). The stricter per-format cap is used even for `text/plain`. The 50 MB figure from the plan had to become 40 MB in the test to fit the actual cap.
3. Gateway reports upstream parse failures as HTTP 502 with an Anthropic-shaped error body, not 500 as the plan stated. S4 was updated to match reality.
4. Under the churn workload, rename-delete propagation lags add propagation by a wide margin — a 30-file rename leaves ~20 old names on the peer after a minute of polling. The churn test documents and tolerates this without papering over it.
