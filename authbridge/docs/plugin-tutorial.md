# Writing a Plugin

**Audience:** someone building their first authbridge plugin. Walks
from an empty file to a fully-registered plugin with config, recording,
body access, and tests.

**See also:**
- [`plugin-reference.md`](./plugin-reference.md) — field-level reference for config,
  invocation recording, and the registration contract.
- [`framework-architecture.md`](./framework-architecture.md) — how the pipeline
  composes plugins and the lifecycle of a request.

A step-by-step guide to building a new authbridge plugin. For reference-style
detail on config, registration, and invocation recording, see
[`plugin-reference.md`](./plugin-reference.md).

## What a plugin is

A plugin is a Go type that implements the `pipeline.Plugin` interface and
registers itself in the plugin registry. The pipeline invokes it on every
request (OnRequest) and, in reverse order, on every response (OnResponse).
Plugins can read `pctx`, mutate headers/body, reject the request, and record
diagnostic invocations that show up in `/v1/sessions` and in `abctl`.

## Step 1 — The minimal plugin

Create a file under `authbridge/authlib/plugins/hellolog.go`:

```go
package plugins

import (
	"context"
	"log/slog"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

type HelloLog struct{}

func NewHelloLog() *HelloLog { return &HelloLog{} }

func (p *HelloLog) Name() string { return "hello-log" }

func (p *HelloLog) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (p *HelloLog) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	slog.Info("hello-log: request", "path", pctx.Path)
	pctx.Observe("request_seen")
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *HelloLog) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	slog.Info("hello-log: response", "status", pctx.StatusCode)
	pctx.Observe("response_seen")
	return pipeline.Action{Type: pipeline.Continue}
}

func init() {
	RegisterPlugin("hello-log", func() pipeline.Plugin { return NewHelloLog() })
}
```

That's a complete plugin. Operator YAML adds it to a pipeline:

```yaml
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
      - name: hello-log
```

## Step 2 — Record what your plugin did

Every plugin should tell the operator what it did on each message.
The pipeline fills in `Plugin`, `Phase`, and `Path` automatically — you
supply only the action and reason. Use the one-liner that fits:

```go
pctx.Allow("authorized")          // gate plugin approved
pctx.Skip("path_bypass")          // plugin ran but didn't act
pctx.Observe("matched_tools/call") // parser extracted data
pctx.Modify("token_replaced")      // plugin mutated the message
```

For invocations that carry extra diagnostic context, populate
`Invocation.Details`:

```go
pctx.Record(pipeline.Invocation{
	Action: pipeline.ActionAllow,
	Reason: "authorized",
	Details: map[string]string{
		"token_subject": claims.Subject,
		"token_scopes":  strings.Join(claims.Scopes, " "),
	},
})
```

See [`plugin-reference.md`](./plugin-reference.md#emitting-session-events) for the
full field set and the 5-value action vocabulary.

## Step 3 — Reject a request

Return a `Reject` action when your plugin should stop the pipeline:

```go
if !allowed {
	return pipeline.Deny("policy.forbidden", "caller not permitted")
}
```

Helper constructors exist for the common cases:

```go
pipeline.Deny(code, reason)                           // generic deny
pipeline.DenyStatus(401, code, reason)                // override status
pipeline.Challenge("realm", "missing credentials")    // 401 + WWW-Authenticate
pipeline.RateLimited(30*time.Second, "", "slow down") // 429 + Retry-After
```

When you want to emit an invocation AND reject in one call, use
`pctx.DenyAndRecord`:

```go
return pctx.DenyAndRecord("caller_not_allowed", "policy.forbidden", "caller not permitted")
```

### Ship in observe mode first

For any guardrail or policy plugin that can false-positive — PII
detection, prompt-injection scoring, jailbreak classifiers — roll it
out under `on_error: observe` before flipping to `enforce`:

```yaml
- name: your-new-guardrail
  on_error: observe          # log would-blocks, don't actually block
  config: { ... }
```

In `observe`, the framework converts the plugin's `Deny` into a
pass-through and marks the deny `Invocation` with `Shadow: true`. Your
plugin code is unchanged — the rollout knob lives outside the plugin.
After a soak period, compare the shadow-deny rate against your
acceptable false-positive threshold, then flip to `enforce`. See
[`plugin-reference.md`](./plugin-reference.md#on_error-policy) for
full semantics.

## Step 4 — Add config

If your plugin needs configurable knobs, implement
`pipeline.Configurable`:

```go
type HelloConfig struct {
	Greeting string `json:"greeting"`
}

type HelloLog struct {
	cfg HelloConfig
}

func (p *HelloLog) Configure(raw json.RawMessage) error {
	if len(raw) == 0 {
		p.cfg.Greeting = "hello" // default
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // reject typos loudly
	if err := dec.Decode(&p.cfg); err != nil {
		return fmt.Errorf("hello-log config: %w", err)
	}
	if p.cfg.Greeting == "" {
		p.cfg.Greeting = "hello"
	}
	return nil
}
```

Operator YAML:

```yaml
- name: hello-log
  config:
    greeting: "hola"
```

See [`plugin-reference.md`](./plugin-reference.md#the-four-step-configure-pattern)
for the strict-decode / defaults / validate / construct pattern.

## Step 5 — Body access

If your plugin needs to read the request or response body (e.g., to
parse JSON, scan for credentials), declare `ReadsBody`:

```go
func (p *HelloLog) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
```

The listener buffers the body so `pctx.Body` (request) and
`pctx.ResponseBody` (response) are populated. Without the declaration,
both stay nil even if you try to read them.

### Mutating the body

If your plugin needs to **rewrite** the body — prompt-redaction, output
filtering, content transformation — declare `WritesBody` and call
`pctx.SetBody` / `pctx.SetResponseBody`:

```go
func (p *Redactor) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesBody: true} // implies ReadsBody
}

func (p *Redactor) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	cleaned := redactSSNs(pctx.Body)
	pctx.SetBody(cleaned) // auto-records a modify-action Invocation
	return pipeline.Action{Type: pipeline.Continue}
}
```

The listener propagates the rewritten bytes to the upstream (or downstream,
for `SetResponseBody`) with a correct `Content-Length` and a cleared
`Content-Encoding`. `SetBody` also emits a `modify`-action Invocation with
`Reason: "body_rewritten"` plus a `body-mutation/event` entry in
`pctx.Extensions.Custom` carrying the length delta and sha256 before/after
(never the raw body content).

**Rules enforced by `pipeline.New`:**
- At most one `WritesBody` plugin per pipeline. Two mutators = ambiguous
  ordering → build fails at startup.
- A `WritesBody` plugin must run **after** any `ReadsBody`-only plugin.
  Readers see the original bytes; a mutator in front would silently
  feed them post-rewrite content.

Don't assign `pctx.Body = newBytes` directly — the listener won't
propagate the mutation and no Invocation fires. Always use `SetBody`.

See [`framework-architecture.md` §6, "Body mutation"](./framework-architecture.md#body-mutation)
for the full lifecycle, per-listener wire details, and content-encoding
policy.

## Step 6 — Out-of-tree plugins

A plugin living in another Go module follows the same pattern, but
imports the registry instead of sharing its package:

```go
// github.com/acme/my-plugin/myplugin.go
package myplugin

import (
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins"
)

type MyPlugin struct{}

func (p *MyPlugin) Name() string { return "my-plugin" }
// ... Capabilities / OnRequest / OnResponse ...

func init() {
	plugins.RegisterPlugin("my-plugin", func() pipeline.Plugin { return &MyPlugin{} })
}
```

The operator's authbridge build picks it up with a single side-effect
import:

```go
// authbridge/cmd/authbridge/plugins_extra.go
package main

import _ "github.com/acme/my-plugin"
```

No fork of rossocortex required.

## Step 7 — Test your plugin

Tests that call `OnRequest` / `OnResponse` directly need to set the
framework attribution fields on `pctx` so `Record` fills `Plugin` and
`Phase` correctly. The `invokeOnRequest` / `invokeOnResponse` helpers
in `plugins_test.go` do this:

```go
func TestHelloLog_Observes(t *testing.T) {
	p := NewHelloLog()
	pctx := &pipeline.Context{Direction: pipeline.Inbound, Path: "/x"}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("want Continue, got %v", action.Type)
	}
	if pctx.Extensions.Invocations == nil ||
		len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("expected one invocation, got %+v", pctx.Extensions.Invocations)
	}
	inv := pctx.Extensions.Invocations.Inbound[0]
	if inv.Plugin != "hello-log" || inv.Reason != "request_seen" {
		t.Errorf("invocation = %+v", inv)
	}
}
```

For test isolation (a fake plugin registered in one test should not
leak into another) use `t.Cleanup` with `UnregisterPlugin`:

```go
func TestScenario(t *testing.T) {
	plugins.RegisterPlugin("fake", func() pipeline.Plugin { return &fakePlugin{} })
	t.Cleanup(func() { plugins.UnregisterPlugin("fake") })
	// ...
}
```

## Step 8 — Expose content to guardrails (parser plugins)

If your plugin is a **parser** whose extension carries user-visible
text, implement [`contracts.ContentSource`](../authlib/contracts/content.go)
so guardrails can inspect it without importing your package. See
[`plugin-reference.md` "Exposing content to guardrails"](./plugin-reference.md#exposing-content-to-guardrails)
for the role vocabulary and the mapping table across in-tree parsers.

Minimal example for an Inference-like parser whose extension stores
`Messages []struct{ Role, Content string }`:

```go
import "github.com/rossoctl/rossocortex/authbridge/authlib/contracts"

// Compile-time assertion — catches interface drift at build time.
var _ contracts.ContentSource = (*MyExtension)(nil)

func (e *MyExtension) Fragments() []contracts.Fragment {
    out := make([]contracts.Fragment, 0, len(e.Messages))
    for _, m := range e.Messages {
        if m.Content == "" {
            continue
        }
        out = append(out, contracts.Fragment{Role: m.Role, Text: m.Content})
    }
    return out
}
```

That's the whole addition. Guardrails call `pctx.ContentSources()`,
iterate, and see your messages alongside every other parser's output.

**Role normalization.** The minimal example above emits `m.Role` as-is.
If your protocol uses role names that differ from the standard vocabulary
(e.g., A2A uses `"agent"` where the standard is `"assistant"`), remap
them to the `contracts.Role*` constants inside `Fragments` so guardrails
match uniformly across protocols. See
[`A2AExtension.Fragments`](../authlib/pipeline/content.go) for a reference
implementation that rewrites `"agent"` → `"assistant"`.

Skip this step if your protocol is binary, control-plane only, or
otherwise carries no text a guardrail would scan.

## Optional interfaces

Beyond the four required methods, plugins may implement:

| Interface | When |
|---|---|
| `pipeline.Configurable` | Takes YAML config. See Step 4. |
| `pipeline.Initializer` | Needs one-time setup before serving (load a model, warm a cache, spawn a goroutine). |
| `pipeline.Shutdowner` | Needs graceful cleanup on pod termination (flush audit events, close connections). |
| `pipeline.Readier` | Has deferred initialization that affects `/readyz` (e.g., waiting on a credential file). |

All optional. A plugin that doesn't implement them is treated as
"always ready, no init, no shutdown." Definitions are in
`authbridge/authlib/pipeline/plugin.go`.

## Gotchas

- **Don't hold pctx across goroutines.** The pipeline resets pctx's
  framework fields after each plugin returns. Recording an invocation
  from a spawned goroutine attributes it to whichever plugin the
  pipeline happens to be dispatching at the time — usually garbage.
- **DisallowUnknownFields or nothing.** Strict decode in Configure is
  not optional. A misspelled key at startup is always a bug; lenient
  decode hides it until it misbehaves at 3am.
- **Name collisions panic.** Two plugins registering under the same
  name panic at process start. Fix: pick a unique name.

## Cross-references

- [`plugin-reference.md`](./plugin-reference.md) — reference detail on config
  patterns, the invocation contract, the 5-value action vocabulary,
  and the registration rules.
- [`framework-architecture.md`](./framework-architecture.md) — how the pipeline
  composes plugins, the Run / RunResponse dispatch order, and the
  lifecycle hooks.
- [`pipeline/plugin.go`](../pipeline/plugin.go) — the Plugin interface
  and all optional interfaces (Initializer / Shutdowner / Readier /
  Configurable).
