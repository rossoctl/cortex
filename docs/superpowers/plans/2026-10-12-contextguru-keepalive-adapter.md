# context-guru plugin: keep-alive adapter and Responses support — Implementation Plan (Phases 2 + 4)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the `context-guru` plugin the idle keep-alive (prompt-cache pings, plus the engine's summary substitute) and OpenAI Responses API support, on the engine's new `keepalive` package, with all decisions left in the engine.

**Architecture:** The plugin becomes a keep-alive host. `OnRequest` calls `Keeper.Arrive`; `OnFinish` (after `inference-parser` has settled usage) calls `Keeper.Record`; `Init`/`Shutdown` start and stop the keeper. A `cortexDispatcher` holds the caller's credential masked per thread and sends pings on a standalone `http.Client` with the plugin's sentinel header. Cortex's injected pricing resolver backs the keeper's `Pricer`. Compaction moves from `apply.BodyWithModel` to `apply.BodyOpts`, which adds the Responses envelope. `marker_mode` is capped at `summary` because Cortex cannot run the expand loop.

**Tech Stack:** Go 1.26.5, `github.com/rossoctl/context-guru` (engine `main` + Plan 1), Cortex `core/pipeline`, `core/cost/pricing`.

**Spec:** rossoctl/context-guru PR #381, `docs/proposals/cortex-integration.md` at commit `03c5b6a` (§3, §4.1, §5, §6, §8, §9.1, §10 Phases 2 and 4). Depends on the engine plan `rossoctl/context-guru` `docs/superpowers/plans/2026-10-12-export-keepalive-keeper.md` (Plan 1), whose exported API this plan consumes.

## Global Constraints

- Work only in the worktree `.worktrees/contextguru-keepalive`, branch `feat/contextguru-keepalive`. Never touch the shared top-level checkout. Never run `agentop service restart`, `agentop setup --restart` or `make dev-install`: they replace the one Cortex instance every session shares (`CLAUDE.md`, "Worktrees do not isolate the running Cortex").
- Go: `export PATH=/opt/homebrew/bin:$PATH`; run module tests with `GOWORK=off` from the module directory, as CI does.
- `gofmt -l <path>` (always with a path) and `go vet ./...` in every module touched; `go mod tidy -diff` in every module whose `go.mod` changes — `ci.yaml`'s `go-tidy-check` gates on it.
- **All decisions stay in context-guru.** The plugin moves bytes and holds a credential; it never decides when to ping, how much a ping may cost, or when to stop.
- **Fail open.** No keep-alive error may change a request's outcome. `OnFinish` must finish well inside the 2 s `DefaultFinishTimeout`.
- **The keep-alive is off by default** and opt-in per plugin config.
- Credential handling follows the engine's Appendix A via `credmask`: masked at rest, zeroized on release, never logged.
- Naming: "Cortex" for the product in prose; keep `AuthBridge` only for artifacts (`CLAUDE.md`, "Naming").
- Commits: `git commit -s`, conventional-commit messages, `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`; never `Co-Authored-By`. PR titles use a capitalized prefix (`Feat:`).
- The `replace` directive added in Task 1 must not reach `main`. Task 9 replaces it with a tagged engine version.

## Deliberate deviations from the spec (review these)

1. **Only `main`-role requests are recorded.** Cortex has no thread ID; it has `InferenceExtension.AgentRole` (`main`/`subagent`). Recording a subagent request under the session's key would replace the main thread's entry — the exact bug engine #423 fixed. So `OnFinish` records only `AgentRole != subagent` requests, as thread `""`. Subagent caches are not kept warm in v1.
2. **No `CostRecorder` yet (spec §9.3).** The spec says the adapter should refuse to start the keep-alive when nothing records what it spends. `CostRecorder` does not exist. This plan logs every ping's cost at INFO through the plugin's `logEmitter` (`KeepAlivePing`) and documents that pings do not reach Cortex's cost ledger. **This needs the Cortex maintainers' agreement before merge**; the alternative is to keep the feature behind the build tag only until §9.3 lands.
3. **`StartedAt` is the listener's request start** (`pctx.StartedAt`), not the upstream request's start. It is earlier by the pipeline's own time, so the first ping lands slightly *earlier* — the safe direction (the provider's lifetime runs from request start; a late ping would pay a cache write).
4. **Packaging (Task 8) is a proposal, not done here.** The spec's §9.1 says "ship in the laptop build as opt-in", but the `local` profile excludes `contextguru` to save ~16 MiB (`scripts/profile-tags/profiles.go:75`). Changing that is a product decision; Task 8 only prepares the change.

## Review Focus

1. **A request that fails or is denied** (`pctx.StatusCode` 4xx/5xx, or a 499 client cancel). Expected: no entry, no credential held. The engine's own status gate covers it; pin it in a test so a future change to what `OnFinish` passes cannot break it.
2. **A sentinel request** (the plugin's own engine LLM call or its own ping transiting the proxy). Expected: neither `Arrive` nor `Record`; nothing is held for it.
3. **A config hot-reload** (old and new plugin instances side by side for up to 30 s). Expected: the old instance's `Shutdown` releases every held credential; the new instance starts empty.
4. **A request with no session ID.** Expected: nothing recorded — the keeper refuses an empty session; pin it.
5. **A streamed response** (SSE). Expected: `OnFinish` runs after the stream ends with the settled token counts, and `Record` sees the same compacted request body that was sent.

---

## File Structure

| File | Responsibility |
|---|---|
| `core/plugins/contextguru/plugin.go` (modify) | Config (`keep_alive` block), `Configure` (cap markers, build keeper), `OnRequest` (`BodyOpts`, `Arrive`), `Init`, `Shutdown`, `OnFinish`, `SetPricingResolver` |
| `core/plugins/contextguru/keepalive.go` (new) | `keepAliveConfig`, its translation to `keepalive.Policy`, `recordFor(pctx)` — the pure mapping from a finished request to a `keepalive.Record` |
| `core/plugins/contextguru/dispatcher.go` (new) | `cortexDispatcher`: hold / Dispatch / Release, ping-response usage parsing |
| `core/plugins/contextguru/pricer.go` (new) | `resolverPricer`: Cortex `pricing.Resolver` → `keepalive.Pricer` |
| `core/plugins/contextguru/*_test.go` (new) | One test file per new file, plus additions to `plugin_test.go` |
| `core/go.mod`, `cmd/cortex/go.mod`, `cmd/cortex-envoy/go.mod` | Engine version (Task 1 `replace`, Task 9 tag) |
| `docs/plugin-catalog.md` | `keep_alive` fields; the ordering note; the ledger caveat |
| `demos/context-guru/context-guru-tls-bridge.yaml` | `/v1/responses` in `paths` |

---

### Task 1: Build against the Phase 1 engine

**Files:**
- Modify: `core/go.mod`, `cmd/cortex/go.mod`, `cmd/cortex-envoy/go.mod`, and their `go.sum`

- [ ] **Step 1: Point the three modules at the Phase 1 worktree**

The engine worktree is `/Users/davidboaz/Documents/GitHub/context-guru/.worktrees/keeper-export` (Plan 1, all tasks committed). In each of the three `go.mod` files add, below the existing `replace` lines:

```
// DEV ONLY — Plan 2 Task 9 replaces this with a tagged context-guru release.
replace github.com/rossoctl/context-guru => /Users/davidboaz/Documents/GitHub/context-guru/.worktrees/keeper-export
```

- [ ] **Step 2: Tidy and run the existing tests**

```bash
export PATH=/opt/homebrew/bin:$PATH
for m in core cmd/cortex cmd/cortex-envoy; do (cd $m && GOWORK=off go mod tidy); done
cd core && GOWORK=off go vet ./plugins/contextguru/ && GOWORK=off go test -count=1 ./plugins/contextguru/
cd ../cmd/cortex && GOWORK=off go build -tags include_plugin_contextguru,include_plugin_inferenceparser ./...
```

Expected: PASS and a clean build. The spec (§9.1) says the bump needs no code change; if the build breaks, the break is an engine API change that Plan 1 introduced — fix it in Plan 1, not here.

- [ ] **Step 3: Commit**

```bash
git add core/go.mod core/go.sum cmd/cortex/go.mod cmd/cortex/go.sum cmd/cortex-envoy/go.mod cmd/cortex-envoy/go.sum
git commit -s -m "build: develop the context-guru plugin against the engine's keepalive branch" \
  -m "DEV ONLY: a local replace directive. Replaced by a tagged release before merge." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: `apply.BodyOpts` and the Responses API (spec Phase 4)

**Files:**
- Modify: `core/plugins/contextguru/plugin.go` (`Configure` default paths; `OnRequest`; `providerFor` → `dialectFor`)
- Test: `core/plugins/contextguru/plugin_test.go`

**Interfaces:**
- Produces: `func dialectFor(path string) (bschemas.ModelProvider, string)` — returns `(Anthropic, "")` for `…/v1/messages`, `(OpenAI, "responses")` for `…/v1/responses`, `(OpenAI, "")` otherwise. Replaces `providerFor`.

- [ ] **Step 1: Write the failing tests**

Replace `TestProviderFor` with:

```go
func TestDialectFor(t *testing.T) {
	cases := []struct {
		path     string
		provider bschemas.ModelProvider
		api      string
	}{
		{"/v1/messages", bschemas.Anthropic, ""},
		{"/anthropic/v1/messages", bschemas.Anthropic, ""},
		{"/v1/responses", bschemas.OpenAI, "responses"},
		{"/openai/v1/responses", bschemas.OpenAI, "responses"},
		{"/v1/chat/completions", bschemas.OpenAI, ""},
	}
	for _, c := range cases {
		p, api := dialectFor(c.path)
		if p != c.provider || api != c.api {
			t.Errorf("dialectFor(%q) = (%s, %q), want (%s, %q)", c.path, p, api, c.provider, c.api)
		}
	}
}

func TestConfigure_DefaultPathsIncludeResponses(t *testing.T) {
	p := configured(t, `{}`)
	for _, want := range []string{"/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1/responses"} {
		if !p.gated(want) {
			t.Errorf("default paths do not gate %s", want)
		}
	}
}

func TestOnRequest_CompactsAResponsesBody(t *testing.T) {
	p := configured(t, `{"engine": `+collapseEngine+`}`)
	body := []byte(`{"model":"gpt-5.6","input":[` +
		`{"type":"message","role":"user","content":"run it"},` +
		`{"type":"function_call","call_id":"c1","name":"sh","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":` + strconv.Quote(bigToolOutput()) + `}]}`)
	pctx := inferencePctx("/v1/responses", body)
	invokeReq(t, p, pctx)
	if len(pctx.Body) >= len(body) {
		t.Fatalf("Responses body not compacted: %d >= %d bytes", len(pctx.Body), len(body))
	}
	if !json.Valid(pctx.Body) {
		t.Fatal("compacted Responses body is not valid JSON")
	}
}
```

(`collapseEngine`, `bigToolOutput`, `inferencePctx`, `invokeReq` and `configured` already exist in `plugin_test.go`; `collapseEngine` is a JSON-quoted YAML string there — if it is raw YAML instead, wrap it with `strconv.Quote`.)

- [ ] **Step 2: Run to verify they fail**

Run: `cd core && GOWORK=off go test ./plugins/contextguru/ -run 'Dialect|DefaultPaths|Responses'`
Expected: FAIL — `undefined: dialectFor`.

- [ ] **Step 3: Implement**

In `Configure`, the default becomes `[]string{"/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1/responses"}`.

Replace `providerFor` (`plugin.go:287-294`) with:

```go
// dialectFor maps the request path to the bifrost provider the engine parses the body as, and
// the wire envelope within it: OpenAI's Responses API ("responses") is a different body shape
// (instructions + input) from Chat Completions, which apply.Opts.API selects.
func dialectFor(path string) (bschemas.ModelProvider, string) {
	switch {
	case strings.HasSuffix(path, "/v1/messages"):
		return bschemas.Anthropic, ""
	case strings.HasSuffix(path, "/v1/responses"):
		return bschemas.OpenAI, "responses"
	}
	return bschemas.OpenAI, ""
}
```

In `OnRequest`, replace the `provider :=` line and the `apply.BodyWithModel` call (`plugin.go:253`) with:

```go
	provider, api := dialectFor(pctx.Path)
	models := cgcomponents.ModelSpec{Static: p.static, Incoming: p.incomingModel(pctx, provider)}
	before := len(pctx.Body)
	res := apply.BodyOpts(ctx, p.pipe, p.store, apply.Opts{
		Provider: provider, Body: pctx.Body, API: api, Session: sid, Models: models,
	})
	out, changed := res.Body, res.Changed
```

`incomingModel` builds an OpenAI Chat Completions client; leave it returning nil for `api == "responses"` (add `|| api == "responses"` to its early return by passing `api` in), because `llmclient` speaks only `/v1/chat/completions` — a `model.source: incoming` component then degrades to deterministic, as it already does for Anthropic.

- [ ] **Step 4: Run**

Run: `cd core && GOWORK=off go test -count=1 ./plugins/contextguru/`
Expected: PASS (all old tests and the three new ones).

- [ ] **Step 5: Commit**

```bash
gofmt -l core/plugins/contextguru
git add core/plugins/contextguru
git commit -s -m "feat(context-guru): compact OpenAI Responses requests" \
  -m "Moves the plugin to apply.BodyOpts, which carries the wire envelope, and gates /v1/responses by default." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: Cap `marker_mode` at `summary` (spec §4.1)

**Files:**
- Modify: `core/plugins/contextguru/plugin.go` (`Configure`)
- Test: `core/plugins/contextguru/plugin_test.go`

**Interfaces:**
- Consumes: `cgconfig.(*Config).CapMarkerMode(max string) error` (Plan 1 Task 8).

- [ ] **Step 1: Write the failing test**

```go
// Cortex cannot answer the engine's expand tool (no streamed-frame replacement, spec §4), so a
// compacted body must never carry a reversible marker or the injected expand tool — whatever the
// preset defaults to.
func TestConfigure_NoReversibleMarkersEvenUnderAFullPreset(t *testing.T) {
	p := configured(t, `{"engine": "preset: general"}`)
	pctx := inferencePctx("/v1/chat/completions", chatBody(bigToolOutput()))
	invokeReq(t, p, pctx)
	if bytes.Contains(pctx.Body, []byte("<<cg:")) {
		t.Fatal("compacted body carries a reversible <<cg:…>> marker")
	}
	if bytes.Contains(pctx.Body, []byte("context_guru_expand")) {
		t.Fatal("compacted body carries the expand tool")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd core && GOWORK=off go test ./plugins/contextguru/ -run NoReversibleMarkers -v`
Expected: FAIL — the body carries `<<cg:`. (If it PASSES before the change, `general` did not compact `bigToolOutput()`; enlarge the fixture until the "before" run shows a marker, then continue.)

- [ ] **Step 3: Implement**

In `Configure`, directly after `cfg, err := cgconfig.LoadBytes(engine)` and its error check:

```go
	// marker_mode: full needs the expand loop — re-sending upstream with the stashed content when
	// the model calls context_guru_expand — and Cortex cannot replace a streamed response with
	// that second call's answer yet (spec §4, §9.2). Capped HERE, whatever the preset says,
	// because every offloader defaults to full the moment it is on.
	if err := cfg.CapMarkerMode("summary"); err != nil {
		return fmt.Errorf("context-guru config: %w", err)
	}
```

- [ ] **Step 4: Run**

Run: `cd core && GOWORK=off go test -count=1 ./plugins/contextguru/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add core/plugins/contextguru
git commit -s -m "fix(context-guru): never emit reversible markers Cortex cannot expand" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: Pricing — Cortex's resolver behind the keeper's `Pricer`

**Files:**
- Create: `core/plugins/contextguru/pricer.go`, `core/plugins/contextguru/pricer_test.go`
- Modify: `core/plugins/contextguru/plugin.go` (field `rates pricing.Resolver`, method `SetPricingResolver`, assertion `var _ pricing.ResolverConsumer = (*ContextGuru)(nil)`)

**Interfaces:**
- Consumes: `pricing.Resolver` (`core/cost/pricing/provenance.go:62`): `Resolve(endpoint, model string, promptTotal int) (Rates, Provenance)`; `Rates.For(t Tier) (float64, bool)`; tiers `TierInput, TierCacheWrite, TierCacheRead, TierOutput`; rates are USD per token (`rates.go:88`). `keepalive.Pricer`, `keepalive.Price` (Plan 1 Task 2).
- Produces: `type resolverPricer struct{ r func() pricing.Resolver }` implementing `keepalive.Pricer`.

- [ ] **Step 1: Write the failing test**

```go
// core/plugins/contextguru/pricer_test.go
package contextguru

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/cost/pricing"
)

type fixedResolver struct {
	rates  pricing.Rates
	gotEP  string
	gotTok int
}

func (f *fixedResolver) Resolve(ep, _ string, promptTotal int) (pricing.Rates, pricing.Provenance) {
	f.gotEP, f.gotTok = ep, promptTotal
	return f.rates, pricing.Provenance{}
}
func (f *fixedResolver) CurrencyFor(string, string) string { return "USD" }

func ratesOf(in, cw, cr, out float64) pricing.Rates {
	var r pricing.Rates
	for t, v := range map[pricing.Tier]float64{pricing.TierInput: in, pricing.TierCacheWrite: cw,
		pricing.TierCacheRead: cr, pricing.TierOutput: out} {
		r.Base[t], r.Set[t] = v, true
	}
	return r
}

func TestResolverPricerMapsTiersAndPassesEndpointAndPrefix(t *testing.T) {
	f := &fixedResolver{rates: ratesOf(3e-6, 3.75e-6, 0.3e-6, 15e-6)}
	p := resolverPricer{r: func() pricing.Resolver { return f }}
	price, ok := p.Price(context.Background(), "api.anthropic.com", "claude-sonnet-5", 48_000)
	if !ok {
		t.Fatal("priced model reported unpriced")
	}
	if price.Input != 3e-6 || price.CacheWrite != 3.75e-6 || price.CacheRead != 0.3e-6 || price.Output != 15e-6 {
		t.Fatalf("price = %+v", price)
	}
	if f.gotEP != "api.anthropic.com" || f.gotTok != 48_000 {
		t.Fatalf("Resolve got (%q, %d)", f.gotEP, f.gotTok)
	}
}

func TestResolverPricerUnsetTierIsUnpriced(t *testing.T) {
	r := ratesOf(3e-6, 3.75e-6, 0.3e-6, 15e-6)
	r.Set[pricing.TierCacheRead] = false
	p := resolverPricer{r: func() pricing.Resolver { return &fixedResolver{rates: r} }}
	if _, ok := p.Price(context.Background(), "h", "m", 1); ok {
		t.Fatal("a rate table without cache-read priced a ping")
	}
}

func TestResolverPricerWithNoResolver(t *testing.T) {
	p := resolverPricer{r: func() pricing.Resolver { return nil }}
	if _, ok := p.Price(context.Background(), "h", "m", 1); ok {
		t.Fatal("nil resolver priced a ping")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd core && GOWORK=off go test ./plugins/contextguru/ -run ResolverPricer`
Expected: FAIL — `undefined: resolverPricer`.

- [ ] **Step 3: Implement**

```go
// core/plugins/contextguru/pricer.go
package contextguru

import (
	"context"

	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// resolverPricer prices keep-alive pings from Cortex's own rate table, so the keeper's pre-flight
// ceiling and the cost the plugin logs are the same number (spec §9.1). r is read on every call:
// SetPricingResolver may run after Configure, and a reload hands a new instance a new resolver.
//
// Every tier a ping can bill must be set: an unset rate is unknown, not free, and pricing it as
// zero would leave the ceiling unenforced for that model.
type resolverPricer struct{ r func() pricing.Resolver }

func (p resolverPricer) Price(_ context.Context, endpoint, model string, promptTokens int64) (keepalive.Price, bool) {
	res := p.r()
	if res == nil || model == "" {
		return keepalive.Price{}, false
	}
	rates, _ := res.Resolve(endpoint, model, int(promptTokens))
	var out keepalive.Price
	for _, t := range []struct {
		tier pricing.Tier
		dst  *float64
	}{
		{pricing.TierInput, &out.Input}, {pricing.TierCacheWrite, &out.CacheWrite},
		{pricing.TierCacheRead, &out.CacheRead}, {pricing.TierOutput, &out.Output},
	} {
		v, ok := rates.For(t.tier)
		if !ok {
			return keepalive.Price{}, false
		}
		*t.dst = v
	}
	return out, true
}
```

In `plugin.go`, add field `rates pricing.Resolver` to `ContextGuru`, and:

```go
// SetPricingResolver receives Cortex's rate table (plugins.Deps, before Configure).
func (p *ContextGuru) SetPricingResolver(r pricing.Resolver) { p.rates = r }

var _ pricing.ResolverConsumer = (*ContextGuru)(nil)
```

Then check `plugins.PricingConsumerPlugins()`'s callers (`core/plugins/deps.go:146`): if a caller builds the registry only when a consumer is configured, `context-guru` is now one, which is what we want — no change, but say so in the commit message.

- [ ] **Step 4: Run**

Run: `cd core && GOWORK=off go test -count=1 ./plugins/contextguru/ ./plugins/`
Expected: PASS (`./plugins/` covers `deps_test.go`).

- [ ] **Step 5: Commit**

```bash
gofmt -l core/plugins/contextguru
git add core/plugins/contextguru
git commit -s -m "feat(context-guru): price keep-alive pings from Cortex's rate table" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 5: `cortexDispatcher`

**Files:**
- Create: `core/plugins/contextguru/dispatcher.go`, `core/plugins/contextguru/dispatcher_test.go`

**Interfaces:**
- Consumes: `keepalive.Dispatcher`, `DispatchRequest`, `Key`, `Usage` (Plan 1 Task 3); `credmask.{Mask,Zero,Header}` (Plan 1 Task 1); `keepalivetest.RunDispatcherContract` (Plan 1 Task 3); `sentinelHeader` (`plugin.go:46`).
- Produces: `func newCortexDispatcher(c *http.Client) *cortexDispatcher`; `func (d *cortexDispatcher) hold(key keepalive.Key, scheme, host string, reqHeaders http.Header)`; `Dispatch`; `Release`; `func pingUsage(raw []byte) keepalive.Usage`.

- [ ] **Step 1: Write the failing tests**

```go
// core/plugins/contextguru/dispatcher_test.go
package contextguru

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/keepalive/keepalivetest"
)

func TestCortexDispatcherMeetsTheContract(t *testing.T) {
	keepalivetest.RunDispatcherContract(t, func(t *testing.T, upstreamURL string) keepalivetest.Harness {
		u, err := url.Parse(upstreamURL)
		if err != nil {
			t.Fatal(err)
		}
		d := newCortexDispatcher(http.DefaultClient)
		return keepalivetest.Harness{D: d, Hold: func(k keepalive.Key, h http.Header) {
			d.hold(k, u.Scheme, u.Host, h)
		}}
	})
}

func TestPingUsageReadsBothDialects(t *testing.T) {
	anth := pingUsage([]byte(`{"usage":{"input_tokens":3,"cache_read_input_tokens":48000,` +
		`"cache_creation_input_tokens":0,"output_tokens":1}}`))
	if anth.FreshInput != 3 || anth.CacheRead != 48000 || anth.CacheWrite != 0 || anth.Output != 1 {
		t.Errorf("anthropic usage = %+v", anth)
	}
	// Responses: input_tokens INCLUDES the cached part; fresh is the difference.
	resp := pingUsage([]byte(`{"usage":{"input_tokens":48003,` +
		`"input_tokens_details":{"cached_tokens":48000},"output_tokens":16}}`))
	if resp.FreshInput != 3 || resp.CacheRead != 48000 || resp.Output != 16 {
		t.Errorf("responses usage = %+v", resp)
	}
}

func TestDispatchSetsTheSentinel(t *testing.T) {
	// A ping that transits this proxy must short-circuit OnRequest, not be recorded again.
	var got http.Header
	srv := newHeaderSink(t, &got)
	u, _ := url.Parse(srv.URL)
	d := newCortexDispatcher(http.DefaultClient)
	key := keepalive.Key{Tenant: "", Session: "s"}
	d.hold(key, u.Scheme, u.Host, http.Header{"Authorization": {"Bearer k"}})
	if _, _, err := d.Dispatch(t.Context(), keepalive.DispatchRequest{Key: key, Route: "/v1/messages",
		Body: []byte(`{}`), Headers: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	if got.Get(sentinelHeader) == "" {
		t.Fatalf("ping carried no %s header", sentinelHeader)
	}
}
```

with a small helper in the same file:

```go
func newHeaderSink(t *testing.T, got *http.Header) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}
```

(add `net/http/httptest` to the imports).

- [ ] **Step 2: Run to verify they fail**

Run: `cd core && GOWORK=off go test ./plugins/contextguru/ -run 'CortexDispatcher|PingUsage|Sentinel'`
Expected: FAIL — `undefined: newCortexDispatcher`.

- [ ] **Step 3: Implement**

```go
// core/plugins/contextguru/dispatcher.go
package contextguru

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/rossoctl/context-guru/credmask"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/tidwall/gjson"
)

// authHeaderNames are the slots a provider credential arrives in. Held masked, per thread, only
// while the keeper tracks that thread (keepalive.Record.Hold), and zeroized on Release.
var authHeaderNames = []string{"Authorization", "x-api-key", "x-goog-api-key"}

var errNotHeld = errors.New("context-guru keep-alive: no upstream held for this session")

// cortexDispatcher is the plugin's keepalive.Dispatcher: transport and credential custody only.
// The credential is the one the request carried when it left Cortex's pipeline — after any
// token-exchange or credential-injection plugin ran — because OnFinish reads pctx.Headers then.
type cortexDispatcher struct {
	client *http.Client
	mu     sync.Mutex
	held   map[keepalive.Key]heldTarget
}

type heldTarget struct {
	scheme, host string
	auth         []credmask.Header
}

func newCortexDispatcher(c *http.Client) *cortexDispatcher {
	return &cortexDispatcher{client: c, held: map[keepalive.Key]heldTarget{}}
}

// newPingClient is a standalone client that never routes through HTTPS_PROXY — a ping sent back
// into this proxy would be recorded as agent traffic. The sentinel header is the second guard.
func newPingClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil}}
}

func (d *cortexDispatcher) hold(key keepalive.Key, scheme, host string, reqHeaders http.Header) {
	var auth []credmask.Header
	for _, name := range authHeaderNames {
		for _, v := range reqHeaders.Values(name) {
			b := []byte(v)
			credmask.Mask(b)
			auth = append(auth, credmask.Header{Name: name, Val: b})
		}
	}
	if scheme == "" {
		scheme = "https"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if prev, ok := d.held[key]; ok {
		for _, a := range prev.auth {
			credmask.Zero(a.Val)
		}
	}
	d.held[key] = heldTarget{scheme: scheme, host: host, auth: auth}
}

// Release runs under the keeper's lock: zeroize and forget, nothing else.
func (d *cortexDispatcher) Release(key keepalive.Key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.held[key]; ok {
		for _, a := range h.auth {
			credmask.Zero(a.Val)
		}
		delete(d.held, key)
	}
}

func (d *cortexDispatcher) Dispatch(ctx context.Context, req keepalive.DispatchRequest) (keepalive.Usage, int, error) {
	d.mu.Lock()
	h, ok := d.held[req.Key]
	var auth []credmask.Header
	if ok {
		auth = make([]credmask.Header, len(h.auth))
		for i, a := range h.auth {
			auth[i] = a.Clone()
		}
	}
	d.mu.Unlock()
	defer func() {
		for _, a := range auth {
			credmask.Zero(a.Val)
		}
	}()
	if !ok || h.host == "" {
		return keepalive.Usage{}, 0, errNotHeld
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.scheme+"://"+h.host+req.Route, strings.NewReader(string(req.Body)))
	if err != nil {
		return keepalive.Usage{}, 0, err
	}
	for name, vs := range req.Headers {
		hreq.Header[name] = append([]string(nil), vs...)
	}
	hreq.Header.Set(sentinelHeader, "1")
	for _, a := range auth {
		credmask.Mask(a.Val)
		hreq.Header.Add(a.Name, string(a.Val))
		credmask.Zero(a.Val)
	}
	resp, err := d.client.Do(hreq)
	if err != nil {
		return keepalive.Usage{}, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil && !errors.Is(err, io.EOF) {
		return keepalive.Usage{}, resp.StatusCode, err
	}
	return pingUsage(raw), resp.StatusCode, nil
}

// pingUsage reads a ping's non-streaming response (pings always send stream:false). Anthropic's
// input_tokens excludes the cached tiers; OpenAI Responses' input_tokens includes cached_tokens,
// so fresh is the difference — the same rule the engine's own proxy applies.
func pingUsage(raw []byte) keepalive.Usage {
	u := gjson.GetBytes(raw, "usage")
	if cached := u.Get("input_tokens_details.cached_tokens"); cached.Exists() {
		in, c := u.Get("input_tokens").Int(), cached.Int()
		return keepalive.Usage{FreshInput: max(in-c, 0), CacheRead: c, Output: u.Get("output_tokens").Int()}
	}
	return keepalive.Usage{
		FreshInput:   u.Get("input_tokens").Int(),
		CacheRead:    u.Get("cache_read_input_tokens").Int(),
		CacheWrite:   u.Get("cache_creation_input_tokens").Int(),
		CacheWrite1h: u.Get("cache_creation.ephemeral_1h_input_tokens").Int(),
		Output:       u.Get("output_tokens").Int(),
		StopReason:   gjson.GetBytes(raw, "stop_reason").String(),
	}
}
```

- [ ] **Step 4: Run**

Run: `cd core && GOWORK=off CGO_ENABLED=1 go test -race -count=1 ./plugins/contextguru/`
Expected: PASS, including every contract subtest.

- [ ] **Step 5: Commit**

```bash
gofmt -l core/plugins/contextguru
git add core/plugins/contextguru
git commit -s -m "feat(context-guru): add the keep-alive dispatcher" \
  -m "Transport and credential custody for engine-scheduled pings. Passes keepalivetest.RunDispatcherContract." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 6: Wire the keeper into the plugin lifecycle

**Files:**
- Create: `core/plugins/contextguru/keepalive.go`, `core/plugins/contextguru/keepalive_test.go`
- Modify: `core/plugins/contextguru/plugin.go` (config field, `Configure`, `OnRequest`, new `Init`/`Shutdown`/`OnFinish`, `logEmitter.KeepAlivePing`, interface assertions, `ConfigSchema`)

**Interfaces:**
- Consumes: everything above; `pipeline.Initializer`, `pipeline.Shutdowner`, `pipeline.Finisher`; `pctx.Session.ID` (via `sessionID`), `pctx.StartedAt`, `pctx.StatusCode`, `pctx.Body`, `pctx.Headers`, `pctx.Scheme`, `pctx.Host`, `pctx.Path`, `pctx.Extensions.Inference.{InputTokens,CacheReadTokens,CacheWriteTokens,OutputTokens,PresentKinds,AgentRole}`.
- Produces:

```go
type keepAliveConfig struct {
	Enabled         bool    `json:"enabled"`
	IdleSeconds     int     `json:"idle_seconds"`      // 0 → 280
	MaxPings        int     `json:"max_pings"`         // 0 → 2
	MaxUSDPerPing   float64 `json:"max_usd_per_ping"`  // 0 → keepalive.DefaultMaxUSDPerPing
	MinPrefixTokens int     `json:"min_prefix_tokens"` // 0 → 20000
}
func (c keepAliveConfig) policy() keepalive.Policy
func recordFor(pctx *pipeline.Context, sid string, pol keepalive.Policy, hold func()) (keepalive.Record, bool)
```

The defaults are the engine's shipped ones (K=2, X=280 s, $0.50, the 20k prefix floor quoted in `proxy/keepalive.go`'s comments). Check them against `config.DefaultKeepAlive*` in the engine before committing, and use those constants directly if they are exported.

- [ ] **Step 1: Write the failing tests**

```go
// core/plugins/contextguru/keepalive_test.go
package contextguru

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/cortex/core/pipeline"
)

func finishedPctx(status int, role pipeline.AgentRole) *pipeline.Context {
	p := inferencePctx("/v1/messages", []byte(`{"model":"claude-sonnet-5","messages":[]}`))
	p.StartedAt = time.Unix(1_700_000_000, 0)
	p.StatusCode = status
	p.Headers = http.Header{"Authorization": {"Bearer caller"}, "Anthropic-Version": {"2023-06-01"}}
	p.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-5",
		InputTokens: 3, CacheReadTokens: 48_000, OutputTokens: 120,
		PresentKinds: 1 | 2 | 4 | 8, AgentRole: role}
	return p
}

func TestRecordForMapsAFinishedRequest(t *testing.T) {
	pol := keepAliveConfig{Enabled: true}.policy()
	r, ok := recordFor(finishedPctx(200, pipeline.AgentRoleMain), "s1", pol, func() {})
	if !ok {
		t.Fatal("a finished main-thread request was not recorded")
	}
	if r.Key.Session != "s1" || r.Key.Thread != "" || r.Status != 200 || r.Provider != bschemas.Anthropic {
		t.Fatalf("record = %+v", r)
	}
	if r.Usage.CacheRead != 48_000 || r.Usage.FreshInput != 3 || !r.UsageOK {
		t.Fatalf("usage = %+v ok=%v", r.Usage, r.UsageOK)
	}
	if r.Route != "/v1/messages" || r.Endpoint != "ollama" || r.Hold == nil {
		t.Fatalf("route/endpoint/hold = %q %q %v", r.Route, r.Endpoint, r.Hold != nil)
	}
}

func TestRecordForSkipsSubagentsAndMissingInputs(t *testing.T) {
	pol := keepAliveConfig{Enabled: true}.policy()
	if _, ok := recordFor(finishedPctx(200, pipeline.AgentRoleSubagent), "s1", pol, nil); ok {
		t.Error("a subagent request was recorded under the main thread's key")
	}
	if _, ok := recordFor(finishedPctx(200, pipeline.AgentRoleMain), "", pol, nil); ok {
		t.Error("a request with no session was recorded")
	}
	p := finishedPctx(200, pipeline.AgentRoleMain)
	p.Extensions.Inference = nil
	if _, ok := recordFor(p, "s1", pol, nil); ok {
		t.Error("a request with no inference parse was recorded")
	}
}

func TestPolicyDefaults(t *testing.T) {
	p := keepAliveConfig{Enabled: true}.policy()
	if !p.On() || p.Idle != 280*time.Second || p.MaxPings != 2 || p.MinPrefixTokens != 20_000 {
		t.Fatalf("policy = %+v", p)
	}
	if (keepAliveConfig{}).policy().On() {
		t.Fatal("keep-alive on with enabled: false")
	}
}

func TestKeepAliveOffByDefault(t *testing.T) {
	p := configured(t, `{}`)
	if p.keeper != nil {
		t.Fatal("a keeper was built with no keep_alive block")
	}
}

func TestOnFinishHoldsTheCredentialOnlyForATrackedRequest(t *testing.T) {
	p := configured(t, `{"keep_alive": {"enabled": true, "min_prefix_tokens": 1}}`)
	if err := p.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })
	// Turn 0 is counted but never tracked (the engine's first-request gate).
	for i := 0; i < 2; i++ {
		pctx := finishedPctx(200, pipeline.AgentRoleMain)
		invokeReq(t, p, pctx)
		p.OnFinish(t.Context(), pctx)
	}
	if n := p.keeper.Stats().Live; n != 1 {
		t.Fatalf("live entries = %d, want 1", n)
	}
	if len(p.disp.held) != 1 {
		t.Fatalf("held credentials = %d, want 1", len(p.disp.held))
	}
	// A denied request on the same session retires nothing new and holds nothing.
	bad := finishedPctx(401, pipeline.AgentRoleMain)
	invokeReq(t, p, bad) // Arrive clears the tracked entry
	p.OnFinish(t.Context(), bad)
	if len(p.disp.held) != 0 {
		t.Fatalf("held credentials after a 401 = %d, want 0", len(p.disp.held))
	}
}

func TestSentinelRequestIsNeverRecorded(t *testing.T) {
	p := configured(t, `{"keep_alive": {"enabled": true, "min_prefix_tokens": 1}}`)
	if err := p.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })
	for i := 0; i < 2; i++ {
		pctx := finishedPctx(200, pipeline.AgentRoleMain)
		pctx.Headers.Set(sentinelHeader, "1")
		invokeReq(t, p, pctx)
		p.OnFinish(t.Context(), pctx)
	}
	if n := p.keeper.Stats().Live; n != 0 {
		t.Fatalf("a sentinel request was tracked: %d live", n)
	}
}

func TestShutdownReleasesHeldCredentials(t *testing.T) {
	p := configured(t, `{"keep_alive": {"enabled": true, "min_prefix_tokens": 1}}`)
	if err := p.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		pctx := finishedPctx(200, pipeline.AgentRoleMain)
		invokeReq(t, p, pctx)
		p.OnFinish(t.Context(), pctx)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.disp.held) != 0 {
		t.Fatalf("held credentials after Shutdown = %d, want 0", len(p.disp.held))
	}
}
```

(Reading `p.disp.held` without its lock is safe here: every test drives the keeper synchronously and no ping is due.)

- [ ] **Step 2: Run to verify they fail**

Run: `cd core && GOWORK=off go test ./plugins/contextguru/ -run 'RecordFor|PolicyDefaults|KeepAlive|OnFinish|Sentinel|Shutdown'`
Expected: FAIL — `undefined: keepAliveConfig`, `recordFor`, `p.keeper`.

- [ ] **Step 3: Implement `keepalive.go`**

```go
// core/plugins/contextguru/keepalive.go
package contextguru

import (
	"time"

	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/cortex/core/pipeline"
)

// keepAliveConfig is the plugin's `keep_alive:` block — one static policy, no strategies and no
// per-session overrides (spec §10, Phase 1 table: "simpler than the standalone proxy by design").
// Every value is a knob on a decision the engine makes; none of them is a decision.
type keepAliveConfig struct {
	Enabled         bool    `json:"enabled" description:"Ping an idle session's prompt cache before it expires. Each ping is billed to the caller's own credential."`
	IdleSeconds     int     `json:"idle_seconds" description:"Idle time before the first ping, from the previous request's start." default:"280"`
	MaxPings        int     `json:"max_pings" description:"Most pings in one idle span." default:"2"`
	MaxUSDPerPing   float64 `json:"max_usd_per_ping" description:"Refuse a ping whose projected cost exceeds this (USD)." default:"0.50"`
	MinPrefixTokens int     `json:"min_prefix_tokens" description:"Billed prefix a session must reach before it is pinged." default:"20000"`
}

func (c keepAliveConfig) policy() keepalive.Policy {
	pol := keepalive.Policy{KeepAlive: c.Enabled, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: c.MaxUSDPerPing, MinPrefixTokens: 20_000}
	if c.IdleSeconds > 0 {
		pol.Idle = time.Duration(c.IdleSeconds) * time.Second
	}
	if c.MaxPings > 0 {
		pol.MaxPings = c.MaxPings
	}
	if c.MinPrefixTokens > 0 {
		pol.MinPrefixTokens = c.MinPrefixTokens
	}
	return pol
}

// Bit layout of InferenceExtension.PresentKinds (parsercommon.Kind).
const (
	presentCacheRead  = 2
	presentCacheWrite = 4
)

// recordFor maps a finished request to what the keeper needs. Mapping only: whether the request
// is worth tracking is the keeper's call. The one filter here is about identity, not policy —
// Cortex knows a request is a subagent's but not WHICH subagent, so recording it would replace the
// main thread's entry (engine #423). Subagent caches are therefore not kept warm.
func recordFor(pctx *pipeline.Context, sid string, pol keepalive.Policy, hold func()) (keepalive.Record, bool) {
	inf := pctx.Extensions.Inference
	if sid == "" || inf == nil || inf.AgentRole == pipeline.AgentRoleSubagent {
		return keepalive.Record{}, false
	}
	provider, _ := dialectFor(pctx.Path)
	return keepalive.Record{
		Key:       keepalive.Key{Session: sid},
		StartedAt: pctx.StartedAt,
		Body:      pctx.Body,
		Provider:  provider,
		Endpoint:  pctx.Host,
		Route:     pctx.Path,
		Status:    pctx.StatusCode,
		Usage: keepalive.Usage{FreshInput: int64(inf.InputTokens), CacheRead: int64(inf.CacheReadTokens),
			CacheWrite: int64(inf.CacheWriteTokens), Output: int64(inf.OutputTokens)},
		UsageOK: inf.PresentKinds&(presentCacheRead|presentCacheWrite) != 0,
		Policy:  pol,
		Agent:   pctx.Headers.Get("User-Agent"),
		Header:  pctx.Headers,
		Emitter: logEmitter{},
		Hold:    hold,
	}, true
}
```

- [ ] **Step 4: Implement the lifecycle in `plugin.go`**

1. `contextGuruConfig` gains `KeepAlive keepAliveConfig \`json:"keep_alive"\``. `ConfigSchema` gains a `keep_alive` entry (follow how `model` is described there).
2. `ContextGuru` gains `keeper *keepalive.Keeper`, `disp *cortexDispatcher`, `kaPol keepalive.Policy`.
3. In `Configure`, after the store is built:

```go
	if p.cfg.KeepAlive.Enabled {
		p.kaPol = p.cfg.KeepAlive.policy()
		p.disp = newCortexDispatcher(newPingClient())
		p.keeper = keepalive.New(keepalive.Options{
			Dispatcher: p.disp,
			Prices:     resolverPricer{r: func() pricing.Resolver { return p.rates }},
		})
		slog.Warn("context-guru keep-alive enabled: idle sessions are pinged on the caller's own " +
			"credential, and that spend is logged but does not reach Cortex's cost ledger")
	}
```

4. `Init` and `Shutdown`:

```go
// Init starts the keep-alive sweep. Nothing to start when keep_alive is off.
func (p *ContextGuru) Init(context.Context) error {
	if p.keeper != nil {
		p.keeper.Start()
	}
	return nil
}

// Shutdown stops the sweep and releases every held credential before returning (spec §6.3): on a
// hot-reload this instance is the old one, and its holds must not outlive it.
func (p *ContextGuru) Shutdown(context.Context) error {
	if p.keeper != nil {
		p.keeper.Shutdown()
	}
	return nil
}
```

5. In `OnRequest`, after the sentinel check and after `sid` is known (and only when `p.keeper != nil`), before compaction: `p.keeper.Arrive(keepalive.Key{Session: sid})`. Only for requests that pass `gated`.
6. `OnFinish`:

```go
// OnFinish hands the finished request to the keeper. Here and not in OnResponse: inference-parser
// settles token counts after this plugin's response hook, and by OnFinish they are final and
// pctx.Body still holds the compacted bytes that went upstream (spec §8).
func (p *ContextGuru) OnFinish(_ context.Context, pctx *pipeline.Context) {
	if p.keeper == nil || (pctx.Headers != nil && pctx.Headers.Get(sentinelHeader) != "") ||
		!p.gated(pctx.Path) {
		return
	}
	sid := sessionID(pctx)
	key := keepalive.Key{Session: sid}
	hdr := pctx.Headers
	scheme, host := pctx.Scheme, pctx.Host
	r, ok := recordFor(pctx, sid, p.kaPol, func() { p.disp.hold(key, scheme, host, hdr) })
	if !ok {
		return
	}
	p.keeper.Record(r)
}
```

7. `logEmitter` gains:

```go
// KeepAlivePing logs each ping's cost. Until Cortex has a CostRecorder (spec §9.3) this log line is
// the only record of keep-alive spend in Cortex.
func (logEmitter) KeepAlivePing(r cgcomponents.KeepAliveReport) {
	slog.Info("context-guru keep-alive ping", "session", r.Session, "model", r.Model,
		"ping", r.Pings, "cacheRead", r.CacheRead, "cacheWrite", r.CacheWrite,
		"output", r.Output, "costUSD", r.CostUSD, "status", r.Status, "ms", r.DurationMs)
}
```

8. Interface assertions: add `pipeline.Initializer`, `pipeline.Shutdowner`, `pipeline.Finisher`.

- [ ] **Step 5: Run**

Run: `cd core && GOWORK=off CGO_ENABLED=1 go test -race -count=1 ./plugins/contextguru/ ./pipeline/ ./plugins/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -l core/plugins/contextguru
git add core/plugins/contextguru
git commit -s -m "feat(context-guru): keep idle sessions' prompt caches warm" \
  -m "Opt-in keep_alive block. The engine's keepalive.Keeper decides when to ping; the plugin records finished requests in OnFinish, holds the caller's credential masked per session, and releases everything on Shutdown." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 7: End-to-end check through a real pipeline

**Files:**
- Test: `core/plugins/contextguru/build_test.go`

- [ ] **Step 1: Write the test**

Add a case beside `TestBuild_InChainAfterInferenceParser`: build `[inference-parser, context-guru{keep_alive.enabled: true, min_prefix_tokens: 1}]` with `plugins.Build`, run `Start`, push two Anthropic requests (a JSON body whose upstream response — fed to the pipeline's response pass — carries `cache_read_input_tokens: 48000`) through `RunRequest` → `RunResponse` → `RunFinish` with `pipeline.Outcome{FinalAction: pipeline.OutcomeAllow, StatusCode: 200}`, then assert one live keep-alive entry via the plugin instance, and that `Stop` leaves zero. Look at how `core/listener/forwardproxy/server.go:380` calls `RunFinish` for the exact call sequence.

- [ ] **Step 2: Run**

Run: `cd core && GOWORK=off CGO_ENABLED=1 go test -race -count=1 ./plugins/contextguru/`
Expected: PASS. If the entry count is 0, print `pctx.Extensions.Inference` at `OnFinish`: the usual cause is `PresentKinds` not set because the response fixture lacks a usage block the parser recognizes.

- [ ] **Step 3: Commit**

```bash
git add core/plugins/contextguru
git commit -s -m "test(context-guru): keep-alive through a built pipeline" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 8: Docs, demo, and the packaging proposal

**Files:**
- Modify: `docs/plugin-catalog.md` (the `context-guru` section at line 50), `demos/context-guru/context-guru-tls-bridge.yaml`

- [ ] **Step 1: Catalog entry**

In `docs/plugin-catalog.md`'s `context-guru` section: replace the sentence saying `OnResponse` is a pass-through with one saying the plugin also implements `OnFinish`, `Init` and `Shutdown` for the keep-alive; add `/v1/responses` to the `paths` default; add the five `keep_alive` fields with their defaults; and add these three notes:
- **Ordering:** any plugin that injects or exchanges the real credential must run *before* `context-guru`, or the keep-alive holds the placeholder and its pings are refused (spec §6.4). A refused ping stops that session silently.
- **Spend:** each ping is billed to the caller's credential and logged as `context-guru keep-alive ping` with `costUSD`. It does not appear in `/v1/usage` or the cost ledger yet.
- **Markers:** `marker_mode` is capped at `summary`; content the engine drops is not restorable through Cortex.

- [ ] **Step 2: Demo**

Add `"/v1/responses"` to `paths` in `demos/context-guru/context-guru-tls-bridge.yaml:44-77` (and the matching ConfigMap in `demos/context-guru/k8s/`). Do not enable `keep_alive` in the demo.

- [ ] **Step 3: Packaging proposal — do not apply**

Write, in the PR description (Task 10), the one-line change that would ship the plugin in the laptop build: adding `include_plugin_contextguru` to `ProfileLocal` in `scripts/profile-tags/profiles.go`, its ~16 MiB cost, and that the plugin stays out of `~/.cortex/config.yaml` until a user adds it. The maintainers decide.

- [ ] **Step 4: Spellcheck and commit**

```bash
git add docs/plugin-catalog.md demos/context-guru
git commit -s -m "docs(context-guru): document keep_alive and Responses support" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 9: Replace the dev `replace` with a tagged engine release (blocked)

Blocked until Plan 1 is merged in `rossoctl/context-guru` and a release is tagged. Do not start it before that.

- [ ] **Step 1:** Remove the `replace github.com/rossoctl/context-guru => …` line from the three `go.mod` files.
- [ ] **Step 2:** `cd core && GOWORK=off go get github.com/rossoctl/context-guru@<tag>`, then `go mod tidy` in all three modules.
- [ ] **Step 3:** Run every check in Task 10 Step 1. Then run the engine's contract test against this version: `TestCortexDispatcherMeetsTheContract` (spec §10 Phase 4 — the gate a version bump must pass).
- [ ] **Step 4:** Commit `build(deps): bump context-guru to <tag>` with sign-off and `Assisted-By`.

---

### Task 10: Full verification, then stop

- [ ] **Step 1: Run everything CI runs for the touched modules**

```bash
export PATH=/opt/homebrew/bin:$PATH
cd core && GOWORK=off go vet ./... && GOWORK=off CGO_ENABLED=1 go test -race -count=1 ./... && cd ..
for m in core cmd/cortex cmd/cortex-envoy; do (cd $m && GOWORK=off go mod tidy -diff); done
(cd cmd/cortex && GOWORK=off go build -tags "$(go -C ../../scripts/profile-tags run . full),include_plugin_contextguru" ./...)
gofmt -l core/plugins/contextguru
```

Expected: all PASS; `go mod tidy -diff` prints nothing; `gofmt -l` prints nothing. While the Task 1 `replace` is present, `tidy -diff` is expected to be clean locally only.

- [ ] **Step 2: Stop and report.** Do not push, open a PR, or touch the running Cortex service without the user's go-ahead. Report the commit list and test output. When the user approves a PR: title `Feat: Keep idle sessions' prompt caches warm in the context-guru plugin`, body ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`, and lists the four deviations at the top of this plan plus the Task 8 packaging proposal.
