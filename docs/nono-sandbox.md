# Running agents in a nono sandbox

[nono](https://nono.sh) runs agents in a sandbox to enable kernel-enforced
boundaries. Cortex can be used in series with nono: your agent sends its requests to
nono's proxy, nono's proxy sends them to Cortex on `127.0.0.1:47600`, and Cortex sends
them to the inference server.

Although the instructions below are specific to Claude Code, in principle, any agent that nono can sandbox can be routed through Cortex the same way; please contribute documentation if you have agent-specific instructions that you feel would be helpful.

## Before you start

1. Install Cortex without `--claude-code`. See [Getting started](getting-started.md).
   Check that `agentop doctor` shows ✓ for `installed`, `started` and `CA`, and that
   `agentop service status` says Cortex is healthy.
2. Don't point Claude Code at Cortex yourself; nono's proxy takes care of that. Skip
   `agentop configure claude-code enable` and `agentop setup --claude-code`: both set
   `HTTPS_PROXY` in `~/.claude/settings.json`, which overrides nono's proxy. If you've
   already run one, undo it with `agentop configure claude-code disable`.
3. You don't need an `env` block in `~/.claude/settings.json`, not even for model
   names. The nono profile sets what Claude Code needs, and `--main` and `--helper`
   (below) take care of the models. If you do have one, keep keys out of it, since the
   sandbox can read that file.

## Add the inference servers

Add servers to Cortex:

```sh
agentop server add gw1 https://gateway-one.example.com --main opus --helper haiku   # asks for the key
agentop server add gw2 https://gateway-two.example.com --main opus --helper haiku
agentop server use gw1 --agent claude-code                                          # set the default
agentop server                                                                      # lists the servers
```

To switch, run `agentop server use` again. New sessions go to the new server. See
[Choosing an inference server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server).

## Write the nono profile

Save this as `~/.config/nono/profiles/claude-cortex.json`:

```json
{
  "extends": ["claude"],
  "meta": { "name": "claude-cortex" },
  "network": {
    "upstream_proxy": "127.0.0.1:47600"
  },
  "environment": {
    "set_vars": {
      "ANTHROPIC_BASE_URL": "https://gateway-one.example.com",
      "ANTHROPIC_AUTH_TOKEN": "cortex-substitutes-the-real-key",
      "NODE_EXTRA_CA_CERTS": "$HOME/.cortex/ca/ca.crt",
      "SSL_CERT_FILE": "$HOME/.cortex/ca/bundle.crt",
      "GIT_SSL_CAINFO": "$HOME/.cortex/ca/bundle.crt",
      "REQUESTS_CA_BUNDLE": "$HOME/.cortex/ca/bundle.crt",
      "CURL_CA_BUNDLE": "$HOME/.cortex/ca/bundle.crt",
      "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
    }
  },
  "filesystem": {
    "read_file": [
      "$HOME/.cortex/ca/ca.crt",
      "$HOME/.cortex/ca/bundle.crt"
    ],
    "deny": ["$HOME/.cortex/config.yaml"]
  }
}
```

- `upstream_proxy` makes nono's proxy send everything on to Cortex.
- `ANTHROPIC_BASE_URL` names one of the servers you added. It has to be up even when
  Claude Code is routed to a different server
  ([why](agents/claude-code.md#router-behavior)).
- `ANTHROPIC_AUTH_TOKEN` is a placeholder. Cortex swaps in the real key.
- The CA variables let Claude Code, and the tools it runs, trust Cortex's certificate
  ([details](agents/claude-code.md#ca-trust)).
- `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` is optional. It turns off Claude Code's
  telemetry, feature flags and auto-updates.
- `filesystem` lets the sandbox read the two CA files, and hides
  `~/.cortex/config.yaml`, which holds your server keys in plain text.

Check the profile:

```sh
nono profile validate ~/.config/nono/profiles/claude-cortex.json
nono why --path ~/.cortex/config.yaml --op read --profile claude-cortex   # prints DENIED
nono why --path ~/.cortex/ca/ca.crt --op read --profile claude-cortex     # prints ALLOWED
```

## Run it

```sh
cd ~/some-project
nono run --profile claude-cortex --allow-cwd -- claude
```

Check: in another terminal, run `agentop observe`, send one prompt, and find a
`claude-code` row.

## Other agents

If you're adapting this for another agent, `upstream_proxy`, the CA variables and the
`filesystem` block carry over unchanged. Swap out the parts that name Claude Code: the
`extends` profile, the `ANTHROPIC_*` variables, `--agent claude-code` and the `claude`
command. And as with Claude Code, don't run `agentop configure <agent> enable`, since
it can send the agent around nono's proxy.

## Known issues

- **`agentop doctor` reports `✗ routed` with `fix: agentop setup --claude-code`.**
  Do not run the fix. It writes `HTTPS_PROXY` to `~/.claude/settings.json` again,
  and Claude Code then skips nono's proxy.
