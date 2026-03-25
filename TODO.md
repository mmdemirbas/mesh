# TODO

## Security

- **TLS for Clipsync** — Clipboard sync uses unencrypted HTTP. Most impactful remaining security improvement.
  - Design needed: mTLS vs pre-shared keys, cert management, config schema, backward compat for local-only setups.

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
- Fuzz testing — `go test -fuzz` on parsers (`parseIPv4Port`, `parseByteSize`, `parseTarget`, config YAML)
- Benchmark CI — track regressions across commits

## Features

- **Web UI** — HTML dashboard via admin HTTP endpoint (SSE/WebSocket). Backend is ready (`renderStatus`, admin server).
- **Config hot-reload** — Watch config file, diff changes, restart affected components. Needs lifecycle management.
- **`mesh init`** — Generate a starter config interactively.
- **Prometheus metrics** — Optional endpoint for connection count, bytes transferred, uptime.

---

- We cannot track per-forwarindg metrics, right? Currently we are showing metrics per forwarding set. A forwarding set could contain multiple forwardnigs at ones. I'm more interested in that than forwarding-set level metrics. If we can achieve this, we can remove the forwarding-set level metrics.
- Is it possible to collect metrics on the listeners also?
- After adding that much details to the dashboard, it started feeling a bit unorganized. Let's align gray texts  properly.


- move node name after subcommand and make it optional -> `mesh subcommand [node] [args]`
- this change will require to support multiple nodes for the subcommands.
- this will make the tool easier to understand and use, just like `docker compose ...`

- I see 10MB memory usage in Windows task manager, and 12MB in MacOS. Feels a bit bigger than necessary. What are we using this memory for? Can we reduce it? How? Let's discuss and plan before proceeding to make any changes.

- This app should be extremely lightweight and energy-efficient. Let's check where we are spending energy and how can we use it more efficiently. Let's discuss and plan before proceeding to make any changes.
