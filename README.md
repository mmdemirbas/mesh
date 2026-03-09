# mesh (Connection Swiss-Army Knife)

`mesh` is a mode-less, cross-platform networking tool written in Go that acts as an all-in-one replacement for `ssh`, `sshd`, `autossh`, `socat`, and SOCKS/HTTP proxy servers.

## Features

- **Mode-less**: A single `mesh` binary can listen for incoming connections while simultaneously dialing outbound to multiple peers.
- **Standalone Proxies**: Native support for binding SOCKS5, HTTP CONNECT proxies, and raw TCP Relays (`socat` replacements) independent of SSH.
- **Tuned Parallel SSH**: Within a single connection mapping, you can configure multiple isolated `forwards` sets. `mesh` automatically spawns parallel, high-throughput SSH connections for each set.
- **Performance Tuned**: Hardcodes high-performance, low-cost crypto (`chacha20-poly1305`, `curve25519-sha256`) and robust `KeepAlive` timings by default.
- **Subcommand Management**: Built-in daemon control via `serve`, `status`, and `stop`.

## Building

A `build.sh` (macOS/Linux) and `build.ps1` (Windows) script are provided to cross-compile the application natively.

```bash
# Compile binaries into the bin/ directory
./build.sh
```

## Configuration

`mesh` relies entirely on explicit YAML declarations to eliminate the ambiguity of traditional `-R` and `-L` mapping logic. Configurations are stored separately from the binary.

Check out the `configs/` directory for our reference file:
1. `example.yml` — A comprehensively commented file showing every available `mesh` feature (Proxies, Relays, Servers, Connections). Use this as a reference template for your own deployments!

## Usage

Start the daemon using your desired target configuration:
```bash
./bin/mesh-linux-amd64 serve -config configs/example.yml &
```

Query the daemon status or stop it safely utilizing graceful shutdowns:
```bash
./bin/mesh-linux-amd64 status
./bin/mesh-linux-amd64 stop
```
