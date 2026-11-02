# Running Cortex on a remote machine

Cortex on a laptop is one process bound to loopback. On a remote machine — a VM, a
home server, a cluster pod — it is the same process with the same binding, and the
machine is reached over an encrypted tunnel that carries loopback across the network.
Your SSH key or cluster identity is the authentication; Cortex never learns it is
remote, and nothing about its security model changes.

## The rule

**Every Cortex port stays on loopback, and the tunnel is the only way in.** Never
expose a port directly, and never put one behind an ingress:

- The session API (`:47601`) is unauthenticated by design and serves raw prompts,
  completions and tool results.
- The forward proxy (`:47600`) carries your agent's decrypted traffic and its
  credentials.

One assumption carries over from the laptop: loopback is *yours*. On a shared
host every local account reaches both ports without the tunnel, so this model
assumes a machine you are the only user of — or one whose every account you
trust, the same caveat as a shared OpenShift project below.

A tunnel — `ssh -L`, `kubectl port-forward`, SSH over Tailscale — gives you both
encryption and "only me" without Cortex growing an auth subsystem. It also keeps the
session API's browser guards passing: a tunnelled request arrives with a loopback
`Host`, so erasing sessions (`DELETE /v1/sessions`) keeps working from afar.

## Ports

A local-style install binds, on the remote machine's loopback:

| Port | What it is |
|---|---|
| `47600` | Forward proxy — where agents point `HTTPS_PROXY` |
| `47601` | Session API — what `agentop` reads |
| `47602` | Stats — `/reload/status`, live config, pprof |
| `47604` | Health |

## Deploying

Cortex needs a writable `HOME` (it writes `~/.cortex`) and nothing else — no
Keycloak, no SPIRE, no cluster. Run the release binary with `--local`, which writes
and watches `~/.cortex/config.yaml`.

As a systemd user service (verified on Ubuntu 24.04, aarch64):

```ini
# ~/.config/systemd/user/cortex.service
[Unit]
Description=Cortex

[Service]
ExecStart=%h/.local/bin/cortex --local
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now cortex.service
loginctl enable-linger "$USER"   # keep it running after you log out
```

Or skip the hand-written unit: `agentop service install` (with agentop installed on
the server) writes and supervises an equivalent one, with quoted absolute paths and
restart backoff. It writes to the same `~/.config/systemd/user/cortex.service`, so
running it replaces a hand-written unit. In a container, run the binary with `HOME`
pointed at a volume.

## Connecting agentop

Forward the session API and point agentop at your end of the tunnel:

```sh
ssh -N -L 47601:localhost:47601 you@server
agentop observe --endpoint http://localhost:47601
```

Everything works through the tunnel: the session list, the live event stream, the
timeline, cost — and erasing sessions, because the request arrives with a loopback
`Host`. If you also run a local Cortex on the same machine, forward to a free port
(`-L 48601:localhost:47601`) and name it in `--endpoint`.

## Where the agents run

**On the remote box, beside Cortex** — nothing more to do. The agents use
`http://127.0.0.1:47600` as their proxy, exactly as on a laptop; see the per-agent
guides in [`agents/`](agents/).

**On your machine, through the remote Cortex** — forward the proxy port too and
point the agent at it:

```sh
ssh -N -L 47600:localhost:47600 you@server
```

Then set `HTTPS_PROXY=http://localhost:47600` for the agent, and trust the remote
bridge CA — copy it over and export it:

```sh
scp you@server:.cortex/ca/ca.crt ~/.cortex/remote-ca.pem
export NODE_EXTRA_CA_CERTS=~/.cortex/remote-ca.pem
```

`NODE_EXTRA_CA_CERTS` *adds* to Node's own roots, so the bare remote CA is the right
file for it. Variables that *replace* the trust store (`SSL_CERT_FILE`,
`GIT_SSL_CAINFO`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`) must not get it — they
would trust one private CA and nothing else — and the server's `bundle.crt` is no
substitute either, since it carries the *server's* public roots. Give those a file
holding your machine's roots plus the remote CA, e.g. your local `bundle.crt` with
`remote-ca.pem` appended. [`agents/any-agent.md`](agents/any-agent.md) lists which
variables add and which replace.

Two caveats:

- `agentop exec` assumes a local Cortex: pointed at a remote stats URL
  (`--cortex-stats-url`) it fetches the remote config but prints the *server's*
  proxy address and CA path. Set the two variables by hand for this shape, or run
  `exec` on the server itself.
- Every model call now crosses the tunnel. Fine on a LAN or over Tailscale; over a
  slow WAN link, expect the latency of the link on every request.

## Other targets

| Target | Tunnel |
|---|---|
| VM or server | `ssh -L` (above); `autossh` or SSH-over-Tailscale for a permanent one |
| Kubernetes | `kubectl port-forward`, or agentop's Namespaces → Pods picker — see [`kubernetes.md`](kubernetes.md) |
| OpenShift | one project per user, `oc port-forward` — project membership is the boundary; in a shared project, `edit` reaches every pod, so don't share |

On OpenShift, note that Kata / sandboxed containers silently break `port-forward`
(the runtime's implementation is a stub) — run the plain runc runtime for a pod you
want to reach this way.
