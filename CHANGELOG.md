# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- LLM API gateway: bidirectional translation between Anthropic Messages API and OpenAI Chat Completions API. Supports streaming (SSE), tool use, image content, model name remapping, and error translation. Configured via `client_api` and `upstream_api` (each `anthropic` or `openai`); the pair selects translation (`a2o`, `o2a`) or transparent passthrough (`a2a`, `o2o`)

## [0.0.1] - 2026-04-08

First tracked release. Captures the current state of the project.

### Added
- SSH client with failover and multiplex connection modes
- SSH server (sshd) with key auth, PTY support, env var forwarding, banner/MOTD
- SOCKS5 and HTTP CONNECT proxy servers
- TCP relay listener
- Port forwarding (local and remote) with per-forward-set SSH connections
- Clipboard sync with UDP LAN discovery, group isolation, protobuf push/pull
- Folder sync with named peers, config-level defaults, delta index exchange, block-level delta transfer, and bandwidth throttling
- Live CLI dashboard with alternate screen buffer
- Web dashboard at `127.0.0.1:7777/ui` with tabs for status, filesync, clipsync, logs, metrics, API, debug
- Full log file viewer in web UI with search and level filtering
- Prometheus metrics endpoint (`/api/metrics`)
- YAML config with JSON schema for IDE autocompletion
- Cross-platform support: macOS, Linux, Windows
- Shell completions for bash, zsh, fish
- Gzip compression on all protobuf index and clipboard transfers
- TTL-based eviction for state maps to prevent memory leaks
- Total clipboard payload size limit (100 MB) to prevent OOM
- SSH exit-signal reporting per RFC 4254 section 6.10
- Windows shell defaults to pwsh.exe (modern PowerShell)
- Outgoing delta index: subsequent syncs send only entries newer than last sent sequence per peer

### Changed
- Clipsync config simplified: `lan_discovery` (bool) + `group` (string) replaced by `lan_discovery_group` (list of strings). `allow_send_to` and `allow_receive` removed.

[0.0.1]: https://github.com/mmdemirbas/mesh/releases/tag/v0.0.1
