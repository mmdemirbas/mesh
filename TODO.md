# TODO

## Security

- **TLS for Clipsync** — Clipboard sync uses unencrypted HTTP. Most impactful remaining security improvement.
  - Design needed: mTLS vs pre-shared keys, cert management, config schema, backward compat for local-only setups.

## CI/CD & Release

- GitHub Actions CI — tests + race detector + lint on every push/PR
- `golangci-lint` config
- `go vuln check` in CI
- Release workflow — automated cross-platform binaries on git tag (goreleaser)
- Semantic versioning via git tags (`v1.0.0`)
- `CHANGELOG.md`

## Packaging

- Verify `go install github.com/mmdemirbas/mesh/cmd/mesh@latest` works
- Homebrew formula
- Dockerfile
- systemd unit file / launchd plist

## Documentation

- README: installation instructions, quick-start guide, architecture overview
- README: comparison table vs alternatives (autossh, sshuttle, frp, etc.)
- README: demo GIF showing the live dashboard

## Testing

- Integration tests — real SSH client↔server handshake, real clipsync between two nodes
- Fuzz testing — `go test -fuzz` on parsers (`parseIPv4Port`, `parseByteSize`, `parseTarget`, config YAML)
- Benchmark CI — track regressions across commits

## Features

- **Web UI** — HTML dashboard via admin HTTP endpoint (SSE/WebSocket). Backend is ready (`renderStatus`, admin server).
- **Config hot-reload** — Watch config file, diff changes, restart affected components. Needs lifecycle management.
- **Shell completions** — bash, zsh, fish.
- **`mesh init`** — Generate a starter config interactively.
- **Prometheus metrics** — Optional endpoint for connection count, bytes transferred, uptime.
