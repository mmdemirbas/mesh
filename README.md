# mesh (Connection Swiss-Army Knife)

`mesh` is a mode-less, cross-platform, single-binary secure networking tool written in Go that acts
as an all-in-one replacement for `ssh`, `sshd`, `autossh`, `socat`, and SOCKS/HTTP proxy servers.

## Features

- **Mode-less**: A single `mesh` binary can listen for incoming connections while simultaneously
  dialing outbound to multiple peers.
- **Unified Listeners**: Natively bind SOCKS5, HTTP CONNECT proxies, TCP Relays (`socat`
  replacements), and even `sshd` servers together from a single configuration block.
- **Tuned Parallel SSH**: Within a single connection mapping, you can configure multiple isolated
  `forwards` sets. `mesh` automatically spawns parallel, high-throughput SSH connections for each
  set.
- **Unified Forwarding Types**: Connections seamlessly support standard port forwards alongside
  remote or local dynamic proxies via a simple `type` property.
- **Performance Tuned**: Defaults to high-performance, low-cost crypto (`chacha20-poly1305`,
  `curve25519-sha256`) and robust `KeepAlive` timings.
- **Subcommand Management**: Built-in daemon control via `up`, `status`, and `down`.
- **Clipboard Synchronization**: Seamlessly and securely synchronize your clipboard (text, images,
  and files) natively across your mesh network, with integrated UDP LAN discovery and explicit
  firewall bypassing capabilities.

## Building

Install Go and Taskfile.

MacOS:

```bash
brew install go go-task
```

Windows:

```powershell
choco install go go-task
```

Compile native executable (outputs to build/):

```
task build
```

Cross-compile for all platforms (outputs to dist/):

```
task dist
```

## Configuration

`mesh` relies entirely on explicit YAML declarations to eliminate the ambiguity of traditional `-R`
and `-L` mapping logic. Configurations are stored separately from the binary.

Configuration lookup is performed in the following order:

1. ./mesh.yaml
2. ./mesh.yml
3. ~/.mesh/conf/mesh.yaml
4. ~/.mesh/conf/mesh.yml

### IDE Autocompletion

To enable rich autocompletion, hover documentation, and validation in standard IDEs (VS Code,
IntelliJ, etc) without any plugins, add one of the following "magic comments" to the very top of
your YAML configuration file:

**For configurations located inside the `mesh` repository (Local):**

```yaml
# yaml-language-server: $schema=mesh.schema.json
```

**For standalone configurations installed elsewhere (Remote):**

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/mmdemirbas/mesh/main/configs/mesh.schema.json
```

### Example Schema Snippets

**Listeners Array**
Consolidates all your local inbound services. Valid types include `socks`, `http`, `relay`, and
`sshd`.

```yaml
myservice:
  listeners:
    - { type: socks, bind: "127.0.0.1:1080" }
    - { type: http,  bind: "127.0.0.1:1081", target: "127.0.0.1:1080" }
    - { type: sshd,  bind: "0.0.0.0:2222", host_key: ~/.ssh/keys/key, authorized_keys: ~/.ssh/keys/auth }
```

**Connections Array**
Dial outbound to other peers, map remote resources, and instantiate tunnels. Traffic types (
`forward`, `socks`, `http`) are seamlessly multiplexed.

```yaml
myservice:
  connections:
    - name: my-vps-tunnel
      targets:
        - ubuntu@my-vps.local:22  # Try mDNS first
        - ubuntu@12.34.56.78:22   # Fallback to public IP
      retry: 10s
      auth:
        key: ~/.ssh/keys/key
        known_hosts: ~/.ssh/keys/known_hosts
      forwards:
        - name: my-mappings
          local:
            - { type: forward, bind: "127.0.0.1:8080", target: "127.0.0.1:80" }
            - { type: socks, bind: "127.0.0.1:2080" }
          remote:
            - { type: forward, bind: "0.0.0.0:9090", target: "127.0.0.1:22" }
```

**Clipsync Array**
Seamlessly and securely synchronize your clipboard (text, images, and files) natively across your
mesh network, with integrated UDP LAN discovery and explicit firewall bypassing capabilities.

```yaml
myservice:
  clipsync:
    - bind: "0.0.0.0:7755"
      lan_discovery: true
      static_peers: [ "192.168.1.10:7755" ]
      allow_send_to: [ "all" ]
      allow_receive: [ "all" ]
```

Check out the `configs/` directory for our reference file:

1. `example.yml` — A comprehensively commented file showing every available `mesh` feature (Proxies,
   Relays, Servers, Connections, and Clipsync). Use this as a reference template for your own
   deployments!

## Usage

Start the daemon for a specific service using a target configuration file:

```bash
./mesh -f configs/example.yml server up &
```

Query the daemon status or stop it safely utilizing graceful shutdowns:

```bash
./mesh -f configs/example.yml server status
./mesh server down
```
