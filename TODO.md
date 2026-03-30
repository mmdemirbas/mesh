# TODO

## Security

- **TLS for Clipsync** — Clipboard sync uses unencrypted HTTP. Most impactful remaining security
  improvement.
    - Design needed: mTLS vs pre-shared keys, cert management, config schema, backward compat for
      local-only setups.

## Release

- Semantic versioning via git tags (`v1.0.0`)
- `CHANGELOG.md`

## Packaging

- Verify `go install github.com/mmdemirbas/mesh/cmd/mesh@latest` works
- Homebrew formula
- Dockerfile
- systemd unit file / launchd plist

## Documentation

- README: demo GIF showing the live dashboard

## Testing

- Integration tests — real SSH client↔server handshake, real clipsync between two nodes
- Fuzz testing — `go test -fuzz` on parsers (`parseIPv4Port`, `parseByteSize`, `parseTarget`, config
  YAML)
- Benchmark CI — track regressions across commits

## Features

- **Web UI** — HTML dashboard via admin HTTP endpoint (SSE/WebSocket). Backend is ready (
  `renderStatus`, admin server).
- **Config hot-reload** — Watch config file, diff changes, restart affected components. Needs
  lifecycle management.
- **`mesh init`** — Generate a starter config interactively.
- **Prometheus metrics** — Optional endpoint for connection count, bytes transferred, uptime.

## sshd Parity

Goal: mesh sshd listener should be indistinguishable from OpenSSH sshd to clients.

- **User switching** — Run the shell as the authenticated user, not the mesh process user. Requires
  `setuid`/`setgid` on Unix (mesh must run as root or have `CAP_SETUID`/`CAP_SETGID`). On Windows,
  requires `CreateProcessAsUser`. Without this, all sessions share the mesh process identity.

- **SFTP subsystem** — Handle `subsystem` channel requests for `sftp`. Implement using
  `github.com/pkg/sftp` server. Enables `scp`, `sftp`, and `rsync` over SSH. Needs file permission
  enforcement matching the authenticated user.

- **SSH agent forwarding** — Handle `auth-agent-req@openssh.com` global request and
  `auth-agent@openssh.com` channel type. Forward the agent socket into the session so the user can
  hop to further hosts without exposing keys. Requires creating a Unix domain socket per session and
  setting `SSH_AUTH_SOCK` in the shell environment.

- **X11 forwarding** — Handle `x11-req` session request and `x11` channel type. Proxy X11
  connections back to the client's display. Requires Xauth cookie handling
  (`MIT-MAGIC-COOKIE-1`). Low priority — mostly relevant for Linux desktop users.

- **Environment variables from client** — Handle `env` session requests. OpenSSH clients send
  `LANG`, `LC_*`, and other vars via `SendEnv`. Currently ignored. Should merge client-sent vars into
  the session environment, with a configurable allowlist (like `AcceptEnv` in sshd_config).

- **Signal forwarding** — Handle `signal` session requests. When the client sends Ctrl+C, the SSH
  protocol delivers a `signal` request with `SIGINT`. Currently not handled — the shell only
  receives signals through the PTY. Without a PTY (e.g., `ssh host 'long-command'`), there is no
  way to interrupt.

- **Exit signal reporting** — Send `exit-signal` (not just `exit-status`) when the shell is killed
  by a signal (e.g., SIGSEGV, SIGKILL). Clients like OpenSSH display "Connection to host closed"
  vs. showing the signal name.

- **Banner and MOTD** — Support `Banner` config option (sent before auth) and post-auth message of
  the day. Minor UX feature.

---

- add client subcommands to emulate ssh behaviour to quickly run on-the-fly connections without
  requiring yaml changes.

- DECIDE - relay vs forward => standardize the terminology

