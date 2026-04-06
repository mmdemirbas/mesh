# mesh

[![CI](https://github.com/mmdemirbas/mesh/actions/workflows/ci.yml/badge.svg)](https://github.com/mmdemirbas/mesh/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A single-binary, cross-platform networking tool that replaces `ssh`, `sshd`, `autossh`, `socat`, and SOCKS/HTTP proxy servers.

## Why mesh?

| What you replace | How mesh does it |
|---|---|
| `ssh -L` / `ssh -R` | YAML `forwards` with `local` and `remote` lists |
| `ssh -D` (SOCKS proxy) | `type: socks` in any forward direction |
| `autossh` | Built-in reconnect with configurable retry + keepalive |
| `socat` TCP relay | `type: relay` listener |
| `sshd` | `type: sshd` listener with key auth |
| SOCKS/HTTP proxy server | `type: socks` or `type: http` listener |
| docker-compose with 20+ sshpass containers | `mode: multiplex` — one connection per target, all managed |
| clipboard sync tools | Built-in `clipsync` with UDP LAN discovery |

## Features

- **Single binary, all modes** — listen, connect, forward, and proxy from one process
- **Live dashboard** — `mesh up` shows a `top`-like status screen that auto-refreshes (alternate screen buffer, zero flicker)
- **Multiplex connections** — `mode: multiplex` connects to ALL targets simultaneously for fleet management
- **Flexible auth** — SSH agent, key files, or `password_command` (fetch from Keychain, `pass`, 1Password CLI)
- **Parallel SSH sessions** — each `ForwardSet` gets its own SSH connection for throughput isolation
- **Clipboard sync** — text, images, and files across your network with UDP LAN discovery
- **Cross-platform** — macOS, Linux, Windows (including Windows SSH server support)
- **16 SSH options** — Ciphers, MACs, KexAlgorithms, HostKeyAlgorithms, IPQoS, RekeyLimit, and more

## Installation

### From source

Requires Go 1.26+ and [Task](https://taskfile.dev/).

```bash
# macOS
brew install go go-task

# Build
task build          # → build/mesh

# Or cross-compile all platforms
task dist           # → dist/mesh-{darwin,linux,windows}-{amd64,arm64}
```

### Add to PATH

```bash
task setup:unix     # macOS/Linux — adds build/ to PATH
task setup:windows  # Windows — adds build\ to PATH
```

## Quick Start

**1. Create a config file** (`mesh.yaml`):

```yaml
mynode:
  listeners:
    - { type: socks, bind: "127.0.0.1:1080" }

  connections:
    - name: my-server
      targets: ["ubuntu@my-server.local:22"]
      retry: 10s
      auth:
        key: ~/.ssh/id_ed25519
        known_hosts: ~/.ssh/known_hosts
      forwards:
        - name: web
          local:
            - { type: forward, bind: "127.0.0.1:8080", target: "127.0.0.1:80" }
```

**2. Start:**

```bash
mesh up mynode
```

A live dashboard appears showing connection status, listeners, and recent log lines. Logs go to `~/.mesh/log/mynode.log`.

**3. Other commands:**

```bash
mesh status mynode       # one-shot status check
mesh status mynode -w    # live watch mode
mesh config mynode       # show parsed config without starting
mesh down mynode         # graceful shutdown
mesh up                  # start all nodes in the config file
mesh --version           # print version
mesh help                # detailed help with all SSH options
```

## Configuration

mesh uses YAML with a JSON schema for IDE autocompletion. Config is looked up in order:

1. `./mesh.yaml` or `./mesh.yml`
2. `~/.mesh/conf/mesh.yaml` or `~/.mesh/conf/mesh.yml`
3. Explicit: `mesh -f /path/to/config.yaml up mynode`

### IDE Autocompletion

Add to the top of your YAML:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/mmdemirbas/mesh/main/configs/mesh.schema.json
```

### Examples

**Listeners** — SOCKS proxy, HTTP proxy, TCP relay, SSH server:

```yaml
mynode:
  listeners:
    - { type: socks, bind: "127.0.0.1:1080" }
    - { type: http,  bind: "127.0.0.1:3128", target: "127.0.0.1:1080" }
    - { type: relay, bind: "0.0.0.0:4444", target: "192.168.1.50:80" }
    - type: sshd
      bind: "0.0.0.0:2222"
      host_key: ~/.ssh/server_key
      authorized_keys: ~/.ssh/authorized_keys
      shell: ["bash", "-l"]
```

**Connections** — failover (default) or multiplex mode:

```yaml
mynode:
  connections:
    # Failover: tries targets in order until one succeeds
    - name: my-vps
      targets:
        - ubuntu@my-vps.local:22
        - ubuntu@12.34.56.78:22
      retry: 10s
      auth:
        key: ~/.ssh/id_ed25519
        known_hosts: ~/.ssh/known_hosts
      forwards:
        - name: tunnels
          local:
            - { type: forward, bind: "127.0.0.1:8080", target: "127.0.0.1:80" }
            - { type: socks,   bind: "127.0.0.1:2080" }
          remote:
            - { type: forward, bind: "0.0.0.0:9090", target: "127.0.0.1:22" }

    # Multiplex: connects to ALL targets simultaneously
    - name: cluster
      mode: multiplex
      targets:
        - root@192.168.13.30
        - root@192.168.13.66
        - root@192.168.13.106
      retry: 10s
      auth:
        password_command: "pass show cluster/ssh"
      options:
        StrictHostKeyChecking: "no"
```

**Authentication** — three methods, tried in order (most secure first):

```yaml
auth:
  agent: true                                          # SSH agent (keys never leave the agent)
  key: ~/.ssh/id_ed25519                               # private key file
  password_command: "security find-generic-password -s mesh -w"  # external password tool
  known_hosts: ~/.ssh/known_hosts                      # server verification
```

**Clipboard sync:**

```yaml
mynode:
  clipsync:
    - bind: "0.0.0.0:7755"
      lan_discovery: true
      static_peers: ["192.168.1.10:7755"]
      allow_send_to: ["all"]
      allow_receive: ["all"]
      poll_interval: "3s"  # optional, default 3s
```

See [`configs/example.yaml`](configs/example.yaml) for a comprehensive reference with all options documented.

## SSH Options

All options can be set at connection or forward-set level:

| Option | Description |
|---|---|
| `Ciphers` | Encryption algorithms |
| `MACs` | Message authentication codes |
| `KexAlgorithms` | Key exchange methods |
| `HostKeyAlgorithms` | Accepted server host key types |
| `ConnectTimeout` | Connection timeout in seconds |
| `IPQoS` | IP QoS/DSCP values (e.g., `lowdelay throughput`) |
| `RekeyLimit` | Bytes before re-keying (e.g., `1G`, `500M`) |
| `TCPKeepAlive` | OS-level TCP keepalive in seconds |
| `ServerAliveInterval` | Client keepalive interval in seconds |
| `ServerAliveCountMax` | Max unanswered keepalives |
| `ClientAliveInterval` | Server keepalive interval in seconds |
| `ClientAliveCountMax` | Max unanswered server keepalives |
| `ExitOnForwardFailure` | Stop on forward failure (`yes`/`no`) |
| `GatewayPorts` | Remote forward bind policy (`yes`/`no`/`clientspecified`) |
| `PermitOpen` | Restrict tunneled destinations (comma or space separated, e.g., `*:22,host:80`) |
| `StrictHostKeyChecking` | Host key verification (`no` to disable — insecure) |

## Development

```bash
task build          # build for current platform
task test           # run all tests
task bench          # run benchmarks
task lint           # go vet + golangci-lint
task all            # lint + test + build
task clean          # remove build artifacts
```

### Testing

290+ tests across 7 packages, all race-free:

```bash
go test -race -count=1 ./...
```

### Project Structure

```
cmd/mesh/           CLI, dashboard, status rendering
internal/
  config/           YAML config, validation
  tunnel/           SSH client + server, forwarding
  proxy/            SOCKS5 + HTTP proxy
  netutil/          TCP helpers (BiCopy, keepalive)
  clipsync/         Clipboard sync (UDP discovery, HTTP push/pull)
  state/            Thread-safe component state
```

See [CLAUDE.md](CLAUDE.md) for architecture details and conventions.

## Security

- SSH agent and key-based auth preferred over passwords
- Passwords fetched from external tools via `password_command` — never stored in config
- Config file permission warnings (world-readable files)
- SHA-256 for all hashing
- Rate limiting on SSH server authentication
- Bounded peer discovery (max 32 dynamic peers)
- Path traversal protection on all file operations

See [SECURITY.md](SECURITY.md) for the vulnerability disclosure policy.

## License

[Apache License 2.0](LICENSE)
