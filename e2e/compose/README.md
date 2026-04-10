# mesh end-to-end playground

This directory is a **manual** staging environment for mesh. It wires four
containers into a single docker network so every feature is exercising the
same topology simultaneously:

| Container   | IP            | Role                                                         |
|-------------|---------------|--------------------------------------------------------------|
| `client`    | 172.30.0.10   | Dials the bastion, runs a local forward, gateway, clipsync, filesync |
| `bastion`   | 172.30.0.20   | sshd with `PermitOpen server:22`                             |
| `server`    | 172.30.0.30   | sshd target and filesync peer                                |
| `stub-llm`  | 172.30.0.50   | Canned OpenAI responses for the gateway                      |

The automated scenario suite under `e2e/scenarios/` is the regression gate.
This compose file is only for reproducing bugs by hand, testing new features
interactively, and exploring the admin UI under real load.

## Prerequisites

- Docker with bridge networking
- `task` (go-task.github.io)
- `task build:e2e-image` has been run — this bakes the mesh binary and
  stub-llm into the `mesh-e2e:local` image the compose file references.

## Bring it up

```
task build:e2e-image
task e2e:compose:up   # equivalent to: docker compose -f docker-compose.yaml up -d
```

Tear it down with:

```
task e2e:compose:down
```

`docker compose logs -f <service>` shows each container's mesh log stream.
The per-container admin UI is at `http://127.0.0.1:7777/ui` (reachable via
`docker exec -it mesh-<service> curl ...` or port-forwarded as needed).

## Drive the features

Open a shell inside the client container:

```
docker exec -it mesh-client bash
```

### SSH tunnel via bastion

```
ssh -p 2222 \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -i /etc/mesh/keys/client_key \
    root@127.0.0.1 whoami
```

The client's local forward `0.0.0.0:2222` rides the SSH connection to the
bastion and lands on `server:22` via the bastion's `PermitOpen` allow-list.
Expected output: `root`.

### Filesync

```
# client side
echo "hello from client" > /root/sync/greeting.txt

# server side — a few seconds later
docker exec mesh-server cat /root/sync/greeting.txt
```

Both ends run `send-receive` on the `shared` folder so edits flow both ways.

### Clipsync

The image ships with a fake `xclip` that stores clipboard data under
`/tmp/mesh-clip/<target>`. Copying is simulated by writing to that file:

```
docker exec mesh-client sh -c 'printf "copied on client" > /tmp/mesh-clip/UTF8_STRING'

# a few seconds later — should contain the same text
docker exec mesh-server  cat /tmp/mesh-clip/UTF8_STRING
docker exec mesh-bastion cat /tmp/mesh-clip/UTF8_STRING
```

### LLM gateway

The client runs an `anthropic-to-openai` gateway on `127.0.0.1:3100`
pointing at the stub-llm container. Clients can POST Anthropic-style
requests and will see Anthropic-shaped responses even though the upstream
is the stub's canned OpenAI JSON.

```
docker exec mesh-client curl -sS http://127.0.0.1:3100/v1/messages \
    -H 'Content-Type: application/json' \
    --data '{"model":"claude-opus","max_tokens":32,
             "messages":[{"role":"user","content":"hello"}]}'
```

## Keys

The `keys/` directory ships pre-generated ed25519 keys for the playground.
They are **not** production keys — anyone with this repository has the
private key. Do not reuse them anywhere real.

Regenerate with:

```
cd keys && rm -f *_key* authorized_keys
ssh-keygen -t ed25519 -N "" -C client@playground       -f client_key
ssh-keygen -t ed25519 -N "" -C server-host@playground  -f server_host_key
ssh-keygen -t ed25519 -N "" -C bastion-host@playground -f bastion_host_key
cp client_key.pub authorized_keys
```

## Troubleshooting

- `docker compose logs -f client` prints mesh's own log stream.
- `docker exec mesh-client curl -s http://127.0.0.1:7777/api/state | jq .`
  dumps the state snapshot used by the dashboard.
- `docker network inspect e2e_playground` shows which IPs are assigned.
  (The subnet `172.30.0.0/24` is defined in docker-compose.yaml.)
- If a service is stuck on `retrying`, check the log for the specific
  error — mesh reports the underlying failure in the state `message`.
