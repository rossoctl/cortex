# Accountant integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cortex hosts `github.com/rossoctl/accountant` to price context savings with the same rules as
the context-guru proxy, writes them to the ledger, brings in keep-alive, and pushes `llm.savings.*` to the
central collector.

**Architecture:** A new package tree `core/accounting` owns all host code (wire parse, cache facts, skip
rules, thread, Tracker host, pricing, savings conversion, metrics). `core/pipeline` gets a small typed
surface (`ReportOps`, the client body, a request-start/end hook set by a pipeline option), so no listener
changes. `inferenceparser` replaces `settle.Avoided` with `accounting.Host.Savings`. `core/accounting` never
imports context-guru.

**Tech Stack:** Go 1.26.5, `github.com/rossoctl/accountant` v0.2.0, `github.com/rossoctl/accountant/metrics`
(pseudo-version), OTel SDK metric + `otlpmetrichttp`, context-guru v0.7.4-beta.1 (plugin only).

**Spec:** `docs/superpowers/specs/2026-11-17-accountant-integration-design.md`. Read it first. The method
documents are in `/Users/davidboaz/Documents/GitHub/accountant` (read tag `v0.2.0` with
`git show v0.2.0:<path>`; `docs/cortex-wiring.md` from `origin/main`). The proxy host code to PORT (read,
never import) is in context-guru main: `git -C /Users/davidboaz/Documents/GitHub/context-guru show
"refs/base/cortex-accountant-check:<path>"` — `proxy/accountant_savings_body.go` (`accParse`),
`proxy/accountant_wire.go` (`accThreadID`, `entryFacts`, `accEndNow`), `proxy/accountant_ping.go`
(`pingFromReport`, `PingSavings`), `proxy/accountant_savings.go`, `proxy/metricsink.go`. PR #1412's code is
on branch `feat/contextguru-keepalive` in this repo.

**One refinement of the spec:** spec §5.3a puts `Accounting.End` in each of the three listeners and the
client-body capture in the listener. `Pipeline.Run` already sees the body before the first plugin and after
the last, in every listener, so this plan puts both there (`pipeline.WithAccountant`). Same behaviour, no
listener changes; `NeedsRequestBody()` returning true makes every listener buffer the request body.

## Global Constraints

- Work only in `/Users/davidboaz/Documents/GitHub/cortex/.worktrees/accountant-integration`, branch
  `feat/accountant-integration` — except Task 4, which works in the accountant worktree it names. Do not push. Do not tag. Do not open a PR.
- Every commit: `git commit -s`, message ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.
  Never `Co-Authored-By`.
- One commit per task, in task order. Each commit builds and its tests pass.
- Go commands per module with `GOWORK=off` (except `core`, where CI uses the workspace; run both ways).
  Before each commit: `gofmt -l <paths>` prints nothing; `go vet ./...` passes in `core`, `cmd/cortex`,
  `cmd/cortex-envoy`, `cmd/agentop`; `go mod tidy -diff` is clean in every module whose `go.mod` changed.
- `core/accounting/...` imports only the standard library, `github.com/rossoctl/accountant[/metrics]`, OTel,
  and `github.com/rossoctl/cortex/core/...`. Never `github.com/rossoctl/context-guru/...`.
- `accountant` version: `v0.2.0`. context-guru (plugin only): `v0.7.4-beta.1`.
- Billing mode: `accountant.BillingFrom(<client request headers before any plugin>)` for every request (Task 5 on; Tasks 2-3 used `BillingAPI` as a placeholder); a ping uses its thread's last mode (`Host.ThreadBilling`).
- `accountant` version: v0.2.0 until Task 5, then the local `feat/billing-from` branch via a dev-only `replace` (v0.3.0 when tagged). The PR cannot merge with that `replace`.
- Accounting fails open: no panic, timeout or error in `core/accounting` may fail, delay past 2s, or alter
  a request or response.
- Hashing (parse, `BeginRequest`, `EndRequest`) never runs on the request goroutine.
- Prose: say "Cortex" for the product (see CLAUDE.md naming rule).

## Review Focus

1. A request a plugin rejects (Reject action) gets `RequestStart` but no `RequestEnd`: no goroutine, no
   Tracker request, no Compute; the handle is dropped. Test in Task 2.
2. Non-inference traffic on an accounting-on pipeline (MCP POST, a GET with no body, a CONNECT tunnel, a
   body `wire.Parse` rejects): no Begin, no Compute, no WARN per request (one DEBUG at most). Test in Task 2.
3. A client that hangs up (499) or an upstream 5xx: `skip.Reason` skips Compute; the pending handle is
   released and the goroutine exits. Test in Task 2.
4. Parallel requests on one session (subagents): `go test -race` over `core/accounting` with 32 concurrent
   sessions × requests passes. Test in Task 2.
5. A pipeline hot reload keeps the same `*accounting.Host` (it is process-scoped, not per pipeline), so
   Tracker state survives; a corrupt or >1h-old snapshot starts fresh with one WARN. Tests in Task 2.

---

## File structure

| Path | Responsibility |
|---|---|
| `core/accounting/wire/parse.go` | Wire JSON → `accountant.Body` + breakpoints (W6.3) |
| `core/accounting/facts/facts.go` | Provider classification; `EntryFacts` table (cites `cached-prefix.md`) |
| `core/accounting/skip.go` | W6.1 skip rules |
| `core/accounting/thread.go` | `accountant.Thread` for a pctx |
| `core/accounting/host.go` | `Host`: Trackers, Start/End/Compute, ping facts, snapshot |
| `core/accounting/price.go` | One pricing function for ledger and metrics |
| `core/accounting/savings.go` | `Result` → `[]event.Saving` + event extras (W7.1) |
| `core/accounting/ping.go` | `Host.Ping`: PingOps, ComputeStandalone, standalone cost record |
| `core/accounting/metrics.go` | Bound pricer + `acmetrics` sink under a mutex |
| `core/accounting/push/push.go` | OTel MeterProvider + OTLP HTTP exporter |
| `core/accounting/boundary_test.go` | No context-guru in `go list -deps` |
| `core/pipeline/accounting.go` | `RequestAccountant`, `WithAccountant`, `ReportOps`, client body, thread setter |
| `core/config/accounting.go` | `AccountingConfig`, `MetricsPushConfig` |
| `docs/accounting-differences.json` | Intended differences from the proxy (accountant testing §10.4) |

---

### Task 1: Host code (`wire`, `facts`, `skip`, `thread`) and the accountant cases

**Files:**
- Create: `core/accounting/wire/parse.go`, `core/accounting/wire/parse_test.go`, `core/accounting/wire/testdata/*.json`
- Create: `core/accounting/facts/facts.go`, `core/accounting/facts/facts_test.go`
- Create: `core/accounting/skip.go`, `core/accounting/thread.go`, `core/accounting/doc.go`
- Create: `core/accounting/cases_test.go`, `core/accounting/boundary_test.go`
- Modify: `core/go.mod` (require `github.com/rossoctl/accountant v0.2.0` directly), `cmd/*/go.mod` via tidy

**Interfaces:**
- Produces:
  - `wire.Format` (`FormatAnthropic`, `FormatChat`, `FormatResponses`) and `func wire.FormatOf(path string) (Format, bool)` — `/v1/messages`, `/v1/chat/completions`, `/v1/responses` (suffix match, as `core/parse/inference` does).
  - `func wire.Parse(f Format, raw []byte) (accountant.Body, []accountant.Breakpoint, error)`.
  - `facts.Provider` (`ProviderAnthropic`, `ProviderOpenAI`, `ProviderUnknown`), `func facts.Classify(host, path string) Provider`.
  - `func facts.For(p Provider, model string, breakpoints []accountant.Breakpoint) accountant.EntryFacts`.
  - `func accounting.SkipReason(in SkipInput) string` with `type SkipInput struct{ Status int; Rounds int; SessionID string; ServerHeldHistory bool; UnplacedDeferral bool }`; `""` means compute.
  - `func accounting.ThreadOf(pctx *pipeline.Context) accountant.Thread` — `"main"` primary; `pctx.Subagent()` with `Parent: pctx.ParentSubagent()` (empty parent → `"main"`). Task 2 adds the plugin override.

- [ ] **Step 1: Write the failing wire tests.** `TestParse_Golden` table over `testdata/{anthropic_basic,anthropic_cache_control,anthropic_system_array,chat_basic,chat_system_msg,responses_basic}.json`, each with a sibling `.want.json` holding the expected per-section position count, per-position wire sizes, and breakpoints. Assertions: `len(body.Tools/System/Messages)` positions, sizes equal wire byte lengths, two bodies that differ only in `cache_control` give equal position hashes, a position with both 5m and 1h `cache_control` yields one breakpoint with `TTL "1h"`. `TestParse_RejectsNonJSON` → error.
- [ ] **Step 2: Run** `cd core && go test ./accounting/wire/` → FAIL (undefined).
- [ ] **Step 3: Implement `wire.Parse`** by porting `accParse` (context-guru `proxy/accountant_savings_body.go:89`) including its Responses handling. Build with `accountant.NewBody`. Keep `json.RawMessage` slices of the original buffer; do not re-marshal except to strip `cache_control` for hashing as `accParse` does.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Write `facts_test.go`.** `Classify("api.anthropic.com","/v1/messages") == ProviderAnthropic`; `Classify("api.openai.com","/v1/chat/completions") == ProviderOpenAI`; a LiteLLM host (`litellm.local:4000`, `/v1/messages`) classifies by path to `ProviderAnthropic` exactly as the proxy's provider detection does; unknown → `ProviderUnknown`. `For(ProviderAnthropic, "claude-sonnet-4-5", bps)` returns the TTL, `MinCacheable`, `Provider.TTLs`, `ReadGranularity` that `entryFacts` + context-guru `internal/modelinfo` give for that model (copy the numbers; each table row has a comment citing `accountant/docs/cached-prefix.md` section). A breakpoint with `TTL "1h"` sets `TTL` to 1h. Unknown provider → `LifetimeUnknown`, TTL 5m.
- [ ] **Step 6: Implement `facts`** (port `entryFacts` minus `PingStore`; ping facts are applied in Task 2). Run → PASS.
- [ ] **Step 7: Write `skip`/`thread` tests** (`TestSkipReason`: 200 → `""`; 499, 502 → non-empty; `Rounds 2` → `"multi_round"`; empty session → `"no_session"`; reason strings copied from W6.1. `TestThreadOf`: primary → `{ID:"main"}`; subagent `"a1"` with parent `"a0"` → `{ID:"a1",Parent:"a0"}`). Implement. Run → PASS.
- [ ] **Step 8: Write `cases_test.go`.** `TestAccountantCases` iterates `cases.All()` (import `github.com/rossoctl/accountant/test/cases`). For each step: standalone → `accountant.ComputeStandalone`; else `BeginRequest(Begin.Meta, Begin.Client, Begin.ClientBreakpoints)`, `Add` each op, `EndRequest(End.Thread, End.Forwarded, End.Breakpoints, End.Facts.EntryFacts())`, `Compute(Usage)`; compare to `Expected` the way accountant's own case runner does (read it at `v0.2.0:internal/engine/*cases*_test.go`). Cases whose id is in `docs/accounting-differences.json` are skipped with the listed reason. Create the JSON with schema from accountant `docs/testing.md` §10.4, initially `{"host":"cortex","differences":[]}`. Run → PASS (all 160).
- [ ] **Step 9: Write `boundary_test.go`.** `TestNoContextGuruDependency` runs `go list -deps github.com/rossoctl/cortex/core/accounting/...` (via `exec.Command("go", ...)`, skip if `go` missing) and fails on any line with prefix `github.com/rossoctl/context-guru`. Run → PASS.
- [ ] **Step 10: Verify and commit.** gofmt, vet, `go mod tidy -diff` in `core`, `cmd/cortex`, `cmd/cortex-envoy`, `cmd/agentop`, `cmd/cortex-praxis`, `cmd/cortex-cpex` (whichever list accountant). Commit `feat(accounting): Add Cortex's own accountant host code`.

### Task 2: Pipeline surface, `Host`, config, tool-prune ops

**Files:**
- Create: `core/pipeline/accounting.go`, `core/pipeline/accounting_test.go`
- Modify: `core/pipeline/pipeline.go` (`options`, `New`, `Run`, `NeedsRequestBody`), `core/pipeline/context.go` (fields)
- Create: `core/accounting/host.go`, `core/accounting/host_test.go`, `core/accounting/snapshot.go`, `core/accounting/snapshot_test.go`
- Create: `core/config/accounting.go`, `core/config/accounting_test.go`; Modify: `core/config/config.go` (`Config` gains `Accounting *AccountingConfig yaml:"accounting,omitempty"`), the reload restart-field check (find where `mtls.*` is refused: `grep -rn "mtls" core/reloader core/config`)
- Modify: `core/plugins/deps.go` (`Deps.Accounting *accounting.Host`; `AccountingConsumer` injection; pass `pipeline.WithAccountant` when non-nil and on)
- Modify: `cmd/cortex/main.go`, `cmd/cortex-envoy/main.go` (build one `Host` per process, pass via `Deps`, `Close` on shutdown)
- Modify: `core/plugins/toolprune/plugin.go` (report ops), `core/plugins/toolprune/plugin_test.go`

**Interfaces:**
- Consumes: Task 1 `wire`, `facts`, `SkipReason`, `ThreadOf`.
- Produces:
  - `pipeline.RequestAccountant interface { RequestStart(pctx *Context); RequestEnd(pctx *Context) }`
  - `func pipeline.WithAccountant(a RequestAccountant) Option`; `Pipeline.NeedsRequestBody()` returns true when an accountant is set.
  - `func (c *Context) ReportOps(ops ...accountant.Operation)` — stamps `Operator` (current plugin name when empty) and `DryRun` (true under `ErrorPolicyObserve`); appends in call order.
  - `func (c *Context) ReportedOps() []accountant.Operation`, `func (c *Context) ClientBody() []byte`
  - `func (c *Context) SetAccountingThread(id, parent string)`, `func (c *Context) AccountingThread() (id, parent string, ok bool)`
  - `accounting.Options{ Resolver pricing.Resolver; SnapshotPath string; CostRecorders []session.Recorder; Now func() time.Time }`, `func accounting.New(o Options) (*Host, error)`
  - `*Host` implements `pipeline.RequestAccountant`; `func (h *Host) Compute(pctx *pipeline.Context) (accountant.Result, bool)`; `func (h *Host) Close() error` (writes a final snapshot).
  - `config.AccountingConfig{ Enabled string yaml:"enabled" }` (`""`/`"auto"`/`"true"`/`"false"`), `func (a *AccountingConfig) On(hasOperator bool) bool` (nil receiver = auto).
  - `plugins.AccountingConsumer interface { SetAccounting(*accounting.Host) }`

- [ ] **Step 1: Pipeline tests** (`accounting_test.go`, a fake `RequestAccountant` recording calls): `TestRun_CallsStartBeforeFirstPluginAndEndAfterLast` (ClientBody equals the body before a mutator's `SetBody`; `pctx.Body` at End is the mutated one); `TestRun_RejectSkipsEnd` (Review Focus 1); `TestReportOps_StampsOperatorAndDryRun` (enforce → `Operator=="p1"`, `DryRun==false`; observe → `DryRun==true`; explicit `Operator` kept); `TestNeedsRequestBody_TrueWithAccountant`; `TestClientBody_NotAliasedByMutator` (mutator writes a new slice; `ClientBody()` bytes unchanged).
- [ ] **Step 2: Run** `cd core && go test ./pipeline/ -run 'Accountant|ReportOps|ClientBody|NeedsRequestBody'` → FAIL.
- [ ] **Step 3: Implement** `core/pipeline/accounting.go` and the `Run` hook: at the top of `Run`, if an accountant is set, `pctx.clientBody = pctx.Body` and `RequestStart(pctx)`; on the final `Continue` return, `RequestEnd(pctx)`; not on Reject or cancel. Run → PASS. Run the full `./pipeline/` suite → PASS.
- [ ] **Step 4: Host tests** (`host_test.go`): `TestHost_EndThenCompute` (fake pctx with an Anthropic body, one reported rewrite op, inference usage on `pctx.Extensions.Inference`, status 200 → `Compute` returns a `Result` with one op whose `Saved` > 0); `TestHost_NonInferenceIsIgnored` (path `/mcp`, `FormatOf` false → `Compute` returns false, no log at WARN; Review Focus 2); `TestHost_SkipReleasesHandle` (status 499 → false, and `runtime.NumGoroutine` returns to baseline; Review Focus 3); `TestHost_ComputeTimesOut` (End goroutine blocked via a test hook → `Compute` returns false within 2.1s); `TestHost_PanicInEndFailsOpen`; `TestHost_ConcurrentSessions` (32 goroutines × 20 requests, run under `-race`; Review Focus 4); `TestHost_ModelChangeBecomesSettingOp` (`inference.requestedModel` set → a `KindSetting` op with the model change is added).
- [ ] **Step 5: Implement `host.go`.** `RequestStart`: nothing beyond what pipeline stored (keep it cheap). `RequestEnd`: if `wire.FormatOf(pctx.Path)` false or `pctx.SessionID==""`, return. Else snapshot the inputs (client body, `pctx.Body`, `ReportedOps()`, thread from `AccountingThread()` else `ThreadOf`, model, destination host (`if _, h, ok := pctx.RedirectTarget(); ok` use `h`, else `pctx.Host`), request ID (`pctx.RequestID()`), `time.Now()`), store a handle with a `done` channel under pctx state key `"accounting.pending"`, start a goroutine with `recover`: `wire.Parse` both bodies, `BeginRequest(RequestMeta{RequestID, Session, Model, At, Billing{BillingAPI}}, client, clientBPs)`, `Add` each op (plus the setting op), `EndRequest(thread, fwd, fwdBPs, facts.For(...)+ping facts)`, close `done`. `Compute`: load the handle (absent → false); build `SkipInput` from `pctx.StatusCode` etc.; skip → false; wait `done` up to 2s; `Compute(accountant.Usage{Input: inf.InputTokens, CacheRead: inf.CacheReadTokens, CacheWrite: inf.CacheWriteTokens})` under `recover`; latch so a second call returns the same `Result`. Run → PASS with `-race`.
- [ ] **Step 6: Snapshot tests/impl** (`snapshot.go`): `TestSnapshot_RoundTrip`, `TestSnapshot_OlderThanHourIgnored` (mtime/embedded time >1h → fresh, one WARN), `TestSnapshot_CorruptIgnored` (Review Focus 5). File `{ "savedAt": RFC3339, "state": <Trackers.Snapshot bytes as base64> }`, mode 0600, written atomically (temp + rename) every 60s by a goroutine started in `New` when `SnapshotPath != ""`, and in `Close`. Run → PASS.
- [ ] **Step 7: Config tests/impl.** `On`: auto+operator → true; auto+none → false; `"true"` → true; `"false"` → false; other strings → `Validate` error. Reload refuses a change of the resolved on/off (same mechanism as `mtls.*`), test it. `hasOperator` = outbound pipeline names `context-guru` or `tool-prune`.
- [ ] **Step 8: Wiring.** `cmd/cortex/main.go`: build `accounting.New` once when `cfg.Accounting.On(hasOperator)`; `SnapshotPath = ~/.cortex/accountant-state.json` only on a local install (use the same predicate `config.ArchiveRunsOnLocalInstall` uses); collect `CostRecorders` at each `sessions.AddRecorder` of the ledger writer and the usage aggregator; set `Deps.Accounting`; `defer host.Close()`. Same in `cmd/cortex-envoy/main.go` without snapshot. `plugins.BuildWithDeps` appends `pipeline.WithAccountant(deps.Accounting)` for the outbound build only, and injects `AccountingConsumer`. Add `TestHost_SurvivesReload` (two pipeline builds share one Host; Tracker state from build 1 is seen in build 2; Review Focus 5).
- [ ] **Step 9: tool-prune ops.** Test `TestPrune_ReportsOneOpPerRemovedTool`: two removed tools → two `KindRewrite` ops, `At` = tools section at each tool's client-body index, `SizeBefore.Bytes` = the declaration's wire bytes, `SizeAfter.Bytes == 0`, `ChangeID == "tool-prune:<name>:" + hex(sha256(decl)[:6])`, `DryRun` = !applied; a second identical request reports the same ChangeIDs. Implement in `plugin.go` beside the existing removal loop (~:447-490; `SetBody` at ~:469, `pruneEvent` publish at ~:478), calling `pctx.ReportOps` only after `SetBody` returned true, or with `DryRun:true` when not applied. Existing `pruneEvent` unchanged. Run toolprune tests → PASS.
- [ ] **Step 10: Debug-only output.** `inferenceparser.settleCost` calls `h.Compute(pctx)` (via `AccountingConsumer`) and logs the result at DEBUG (`"accounting: computed"`, ops count, saved tokens per tier). No figure is written. Test `TestSettleCost_CallsAccountingOnce`.
- [ ] **Step 11: Verify and commit.** Full `go test ./...` in `core`, `cmd/cortex`, `cmd/cortex-envoy` (with `-race` on `./accounting/... ./pipeline/...`), vet, gofmt, tidy. Commit `feat(accounting): Host the accountant Tracker and report tool-prune's changes`.

### Task 3: context-guru plugin reports ops, spends and its thread

**Files:**
- Modify: `core/plugins/contextguru/plugin.go` (`~:262` call, `cgModel` `~:116-134`), `core/plugins/contextguru/plugin_test.go`
- Modify: `core/llmclient/client.go`, `core/llmclient/client_test.go` (take `ChatUsage` from PR #1412: `git show feat/contextguru-keepalive:core/llmclient/client.go`)

**Interfaces:**
- Consumes: `pctx.ReportOps`, `pctx.SetAccountingThread` (Task 2).
- Produces: `llmclient` response `Usage *ChatUsage` with `PromptTokens`, `CompletionTokens`, `CachedTokens()`.

- [ ] **Step 1: Tests.** `TestPlugin_ReportsEngineOps` (a body the engine compacts → `pctx.ReportedOps()` equals `res.Trace.Ops`, operators prefixed `context-guru.`); `TestPlugin_SetsThread` (`AccountingThread()` returns `res.Trace.Thread`'s ID, primary → `"main"` by `ThreadOf`'s rule); `TestPlugin_ModelCallIsSpendOp` (fake llm server returns usage `{prompt_tokens:100, completion_tokens:20, prompt_tokens_details:{cached_tokens:40}}` → one `KindSpend` op, `Call.Model` = called model, `Call.Usage.Input==60`, `CacheRead==40`, `Output==20`, `ChangeID==""`, `Detached==false`); `TestPlugin_ObserveModeOpsAreDryRun`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** Switch `apply.BodyWithModel` to `apply.BodyOpts` with the same inputs, keeping current behaviour; `pctx.ReportOps(res.Trace.Ops...)` only when the write applied (else rely on DryRun stamping); `pctx.SetAccountingThread(res.Trace.Thread.ID, parent)` where `parent, _ := res.Trace.Thread.InheritsFrom()` — `thread.Result` is in context-guru's `internal/thread`, so read its exported fields inline and never name the type in a variable, field or signature; `Inherits` is fork origin, not exactly `accountant.Thread.Parent`, so add a differences entry in Task 8; `cgModel` reads `ChatUsage` and reports the spend op via a pctx captured at call time (the call is in-request). Run → PASS.
- [ ] **Step 4: Verify and commit.** `go test ./plugins/contextguru/ ./llmclient/`, boundary test still passes. Commit `feat(contextguru): Report context operations and model-call spend to the accountant`.

### Task 4: `BillingFrom` in the accountant repository

**Repository:** `/Users/davidboaz/Documents/GitHub/accountant` — NOT Cortex. Create a worktree there:
`git -C /Users/davidboaz/Documents/GitHub/accountant fetch origin main:refs/base/billing-from` then
`git -C /Users/davidboaz/Documents/GitHub/accountant worktree add .worktrees/billing-from -b feat/billing-from refs/base/billing-from`.
Follow that repository's CLAUDE.md (style, DCO, Assisted-By, its doc and case rules). Do not push, do not tag.

**Files (in the accountant worktree):**
- Modify: `accountant.go` (export `BillingFrom`), Create: `internal/engine/billing.go`, `internal/engine/billing_test.go`
- Modify: `docs/design.md` §7.1 (remove "Not built yet"; state the rule and the prefixes), `docs/cortex-wiring.md` W6.6 if it says the function does not exist, README "Status" if relevant

**Interfaces:**
- Produces: `func accountant.BillingFrom(h http.Header) accountant.Billing` (stdlib `net/http` header type only; the module must stay stdlib-only, `boundary_test.go`).

- [ ] **Step 1: Test table** `TestBillingFrom`: `x-api-key: sk-ant-api03-…` → `api`; `x-api-key` with any value → `api`; `Authorization: Bearer sk-ant-api03-…` → `api`; `Authorization: Bearer sk-ant-oat01-…` → `subscription`; `Authorization: Bearer sk-proj-…` and `sk-…` (OpenAI key) → `api`; `Authorization: Bearer <three base64url segments>` (a JWT, as a ChatGPT login sends) → `subscription`; `Authorization: Basic …` → `unknown`; no credential → `unknown`; header names case-insensitive. `TestBillingFrom_NeverReturnsCredential`: the returned `Billing` marshals to JSON with no substring of any input token.
- [ ] **Step 2: Run** `go test ./internal/engine/ -run BillingFrom` → FAIL. **Step 3: Implement** (prefix checks only; never log). **Step 4: Run** `go vet ./... && go test ./...` → PASS (including `boundary_test.go`).
- [ ] **Step 5: Docs** as listed. **Step 6: Commit** in the accountant worktree: `feat: Add BillingFrom, the billing mode from the client's credential type`.

### Task 5: Cortex reads the billing mode with `BillingFrom`

**Files:**
- Modify: `core/go.mod`, `cmd/cortex/go.mod`, `cmd/cortex-envoy/go.mod`, `cmd/agentop/go.mod` (whichever require accountant): add
  `// DEV ONLY — remove when accountant v0.3.0 is tagged.` and
  `replace github.com/rossoctl/accountant => /Users/davidboaz/Documents/GitHub/accountant/.worktrees/billing-from`
- Modify: `core/accounting/host.go` (RequestStart reads the mode; meta uses it; per-thread last mode), `core/accounting/host_test.go`
- Create: `core/accounting/billing_contract_test.go`
- Modify: `docs/accounting-differences.json` (the proxy hard-codes `BillingAPI`)

**Interfaces:**
- Consumes: `accountant.BillingFrom` (Task 4); `pipeline.RequestAccountant.RequestStart` (Task 2) runs before the first plugin.
- Produces: `func (h *Host) ThreadBilling(session, thread string) accountant.Billing` (last mode seen for that thread; `unknown` if none). `RequestMeta.Billing` on every request is the client's mode.

- [ ] **Step 1: Tests.** `TestHost_BillingFromClientHeaderBeforeRewrite`: a request with `Authorization: Bearer sk-ant-oat01-x` and a test plugin that replaces `Authorization` with `Bearer sk-ant-api03-y` → the Tracker's `RequestMeta.Billing.Mode == "subscription"` (assert through a test hook or the Result's request meta). `TestHost_ThreadBillingIsLastRequests`. `TestBillingContract` feeds the same header table as accountant's `TestBillingFrom` through `RequestStart` and expects the same modes.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement**: in `RequestStart`, read `pctx` request headers (the field the listeners fill before `Run`; find it in `core/pipeline/context.go`) and store `accountant.BillingFrom(h)` under a pctx state key; `RequestEnd` uses it in `RequestMeta.Billing` and records it per thread. Never store or log the header value. **Step 4: Run** → PASS, `-race` on `./accounting/...`.
- [ ] **Step 5: Commit** `feat(accounting): Take the billing mode from the client's credential type`.

### Task 6: Keep-alive from PR #1412, accounted through `accountant`

**Files:**
- Modify: `core/go.mod`, `cmd/cortex/go.mod`, `cmd/cortex-envoy/go.mod` (context-guru `v0.7.4-beta.1`, no `replace`)
- Take from `feat/contextguru-keepalive`: `core/plugins/contextguru/{keepalive.go,keepalive_test.go,keepalive_substitute_test.go,dispatcher.go,dispatcher_test.go,windows.go,windows_test.go,build_test.go}`, the keep-alive parts of `plugin.go`, `scripts/dev/verify-contextguru-keepalive.sh`, the keep-alive section of `docs/context-guru-plugin.md`. Plus any file they need to build (`incoming_anthropic.go`, `mask.go` only if required).
- Do NOT take: `core/cost/spend/`, `Deps.Costs`, `spend.LogRecorder`, `saving.go`, `unique.go`, `spend.go`, the `avoided.go` context-guru path, the keep-alive pricer (`82cd72b4`).
- Create: `core/accounting/ping.go`, `core/accounting/ping_test.go`

**Interfaces:**
- Consumes: `Host` (Task 2), `price.go` does not exist yet → ping record uses `settle.SettleInput` for its cost.
- Produces: `func (h *Host) Ping(session string, client *model.EventClient, host string, p accountant.Ping) (accountant.Result, bool)`.

- [ ] **Step 1: Ping tests** (`ping_test.go`): `TestPing_RecordsSpendToCostRecorders` (fake `session.Recorder`s receive one `SessionEvent` whose cost record has `Source=="keep-alive"`, priced from the ping usage; the store is never touched); `TestPing_RefusesZeroWindowNon2xxZeroUsage`; `TestPing_FactsAppliedToNextRequest` (after a ping on thread `main`, the next request's `EntryFacts` has `LastPing` set, then cleared); `TestPing_RescueOnNextRequest` (ping then a request whose cache read covers the pinged prefix → `Result.CacheEffects` has a `rescue` with `ChangeID` prefix `ka:main:`); `TestPing_ManualKeepsNoFacts`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement `ping.go`** (port `PingSavings` minus dash): refusal rules, `accountant.PingOps(p, h.ThreadBilling(session, thread))`, `ComputeStandalone`, store `PingFacts` per `session+thread` (concurrency-safe map; applied with `PingFacts.Apply` in `RequestEnd`'s goroutine then deleted), build a `model.SessionEvent` the way `recordOutboundResponseEvent` builds its `Plugins["cost/event"]` (read `core/listener/forwardproxy/server.go:~1105`), and call each `CostRecorders[i].Record(session, ev)`. Never call the session store. Run → PASS.
- [ ] **Step 4: Bring in the keeper.** Check out the listed files from `feat/contextguru-keepalive`, bump context-guru, `go mod tidy`. Replace every `spend.Charge`/`RecordCost` use with: the plugin's `KeepAliveEmitter.KeepAlivePing(r)` converts `r` to `accountant.Ping` (field table: wiring W4.6; reference `pingFromReport`) and calls `h.Ping(...)`. The conversion lives in `core/plugins/contextguru/ping.go`.
- [ ] **Step 5: Fix `TestCortexSubstitutesCacheAwareSummarizerForAPing`** (fails against v0.7.4-beta.1 with `SummarySubstituted = 0, want 1`). Use superpowers:systematic-debugging; find which context-guru change moved the behaviour (`git -C <cg> log keeper-export..v0.7.4-beta.1 -- keepalive/substitute.go` or diff the API), adapt the test or the wiring. If it is a context-guru bug, skip the test with `t.Skip("context-guru#<n>: ...")` and record it in the task report — do not change context-guru.
- [ ] **Step 6: Verify and commit.** Plugin tests, `scripts/dev/verify-contextguru-keepalive.sh` if it runs offline (else note it), boundary test, tidy in all three modules. Commit `feat(contextguru): Keep idle sessions' caches warm, accounted through the accountant`.

### Task 7: The 1h cache-write tier

**Files:**
- Modify: `core/model/extensions.go:~300` (add `CacheWrite1hTokens int json:"cacheWrite1hTokens,omitempty"`, a subset of `CacheWriteTokens`), `core/parse/inference/anthropic.go` (parse `usage.cache_creation.ephemeral_1h_input_tokens`)
- Modify: `core/cost/pricing/rates.go` (`TierCacheWrite1h`; `Rates.For` falls back), `core/cost/pricing/inference.go` (`Usage.CacheWrite1h`), `core/cost/settle/settle.go` `modelledCost` (price 1h writes at the 1h rate), tests beside each.
- Modify: `core/accounting/host.go` (`Usage.CacheWrite1h` from the extension)

- [ ] **Step 1: Tests.** `TestAnthropic_ParsesCacheWrite1h` (`cache_creation_input_tokens:1000`, `cache_creation:{ephemeral_5m_input_tokens:400, ephemeral_1h_input_tokens:600}` → `CacheWriteTokens 1000`, `CacheWrite1hTokens 600`); `TestRates_CacheWrite1hFallback` (Anthropic rates with no explicit 1h → 1h rate = input × 2.0, provenance unchanged; non-Anthropic → 5m rate, `estimated` true); `TestModelledCost_Prices1hWrites` (600 1h + 400 5m at Sonnet rates → `400*cw5m + 600*cw1h`).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** Run → PASS; the settle and pricing suites pass except figures that intentionally change (update those tests' expectations, citing this task in a comment).
- [ ] **Step 4: Commit** `fix(pricing): Price 1-hour cache writes at the 1-hour rate`.

### Task 8: Pricing function, shadow figure, cross-host harness, differences file

**Files:**
- Create: `core/accounting/price.go`, `core/accounting/price_test.go`, `core/accounting/savings.go`, `core/accounting/savings_test.go`
- Modify: `core/plugins/inferenceparser/cost.go` (shadow: compute both, log difference)
- Take and adapt: `scripts/dev/compare-contextguru-savings.py`, `scripts/dev/integration-contextguru-litellm.py` from `feat/contextguru-keepalive`
- Modify: `docs/accounting-differences.json`

**Interfaces:**
- Produces:
  - `func USD(r pricing.Resolver, endpoint, model string, promptTotal int, t pricing.Tier, tokens int64) (usd float64, estimated bool)` — `estimated` = provenance `ProvBundled` or `ProvNone`.
  - `func TierUSD(r pricing.Resolver, endpoint, model string, promptTotal int, tt accountant.TierTokens) (float64, bool)` — maps `Input→TierInput`, `CacheWrite5m→TierCacheWrite`, `CacheWrite1h→TierCacheWrite1h`, `CacheRead→TierCacheRead`.
  - `type Extras struct{ UnattributedBreakUSD float64; Thread string; OpsVersion int }` (`Result.Version` is `int`; `event.Saving.OpsVersion` is `int` too)
  - `func Savings(res accountant.Result, price func(model, host string, tt accountant.TierTokens) (float64, bool), reqModel, reqHost string) ([]event.Saving, Extras)` — one `Saving` per operator, W7.1: `USD` = priced Saved of its ops + rescue effects on its ChangeIDs; `PenaltyUSD` = priced Caused + break effects on its ChangeIDs (`Net()` cost part); spend ops priced with `Call.Model`/`Call.Host`; `Tier` = tier with largest Saved; `Projected` = all its ops DryRun; `Estimated` = any op/effect/rate estimated; `TokensAvoided` = sum of Saved tiers; `InBaseline` effects skipped; breaks with no ChangeID → `UnattributedBreakUSD`.
  - `func (h *Host) Savings(pctx *pipeline.Context) ([]event.Saving, Extras, bool)` — `Compute` then `Savings` with `TierUSD` bound to the request's endpoint and prompt total.
  - Basis (spec §5.7a): `event.Saving.Basis` = `"usd"` for mode `api`, `"subscriptionEquivalent"` for `subscription`, `"unknown"` for `unknown`; every non-`usd` saving has `Estimated: true`. `event.Saving.Units` (usage-limit units, float) = Saved tokens × weights for a `subscriptionEquivalent` saving; `PenaltyUnits` likewise for Caused. `USD`/`PenaltyUSD` are still filled (the API-equivalent figure).
  - `config.SubscriptionWeights{ Input, CacheWrite5m, CacheWrite1h, CacheRead, Output float64 }` under `accounting.subscription_weights`, defaults `1, 1.25, 2, 0.1, 5`; `func Units(w config.SubscriptionWeights, tt accountant.TierTokens, output int) float64`. Validation: every weight ≥ 0.

- [ ] **Step 1: Tests.** `price_test.go`: one token per tier at a configured table → exact rates; bundled table → `estimated`. `savings_test.go` with hand-built `Result`s: two operators split correctly; a break with ChangeID charged to its operator; a break without → unattributed; a rescue credited to `keeper`; DryRun-only operator → `Projected`; a spend op priced with its own model; `InBaseline` ignored; negative net allowed (USD < PenaltyUSD both kept, no floor); a subscription result → `Basis "subscriptionEquivalent"`, `Estimated`, `Units` = tokens×default weights (e.g. 1000 cache-read tokens → 100 units); custom weights from config applied; `unknown` → `Basis "unknown"`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** Run → PASS.
- [ ] **Step 4: Shadow.** `settleCost`: compute `old := settle.Avoided(...)` and `new, extras, ok := h.Savings(pctx)`; publish `old` as today; log at INFO `"accounting: shadow"` with session, per-component old/new USD and the difference, only when they differ by more than $0.000001. Add `TestSettleCost_ShadowWritesOldFigure`.
- [ ] **Step 5: Harness.** Bring both scripts; change `compare-contextguru-savings.py` check P1 to compare the Cortex row's accountant tokens per operator against the proxy's dashboard tokens per component; keep X1 and K1. Do not run it in CI. If a LiteLLM key and both binaries are available, run it and save `checks.json` under `/tmp/accounting-harness/`; otherwise write in the task report that it was not run.
- [ ] **Step 6: Differences.** Run Cortex locally against captured traffic if available (otherwise the harness output); for each shadow difference, add an entry to `docs/accounting-differences.json` with `{"id", "what", "why", "spec"}`; expected entries are the design §W9 list (lower penalty, per-position tier, `context_guru_expand` cost, break from read extent, negative keep-alive windows). Commit `feat(accounting): Price accountant results and compare them with the current figure`.

### Task 9: Switch the ledger and agentop

**Files:**
- Modify: `core/cost/event/event.go` (`Saving.PenaltyUSD` and `Saving.Operator`, `Saving.OpsVersion`; `Event.UnattributedBreakUSD`, `Event.Thread`; `TotalPenaltyMicros()`); take `PenaltyUSD` doc from #1412 but **no floor** — `TotalAvoidedUSD` stays gross
- Modify: `core/cost/usage/usage.go:~217` (`PenaltyMicros`, and per spec §5.7a `AvoidedUnits`, `PenaltyUnits` as int64 milli-units, `AvoidedUnknownMicros`; only `Basis=="usd"` enters `AvoidedMicros`/`PenaltyMicros`), `core/cost/ledger/row.go`, `core/cost/ledger/writer.go:~588`, `core/session/summary.go:~96,379`, `core/session/fold.go:~397` (`penaltyMicros`), `/v1/usage` response
- Modify: `core/plugins/inferenceparser/cost.go` (publish accountant savings; remove shadow)
- Modify: `cmd/agentop/tui/prune_saving.go` (take `rowSavingFor` from #1412, list = every component in the record, net = `USD-PenaltyUSD` without floor), `cmd/agentop/cmd_cost.go:~696`, `spend.go`, `spend_drawer.go`, `sessions_pane.go` (show saved, penalty, net in money for `usd`; a usage-limit column for subscription savings with the API-equivalent dollars beside it, labelled "API equivalent"; `unknown` shown beside totals, never added)
- Delete: tool-prune path of `core/cost/settle/avoided.go` (whole file if nothing remains), `pruneEvent`'s `bytesRemoved/bodyBytesAfter/projected` reliance in settle (keep the event fields agentop still reads), `pricing.EstimateTokensFromBytes` if unused; their tests.
- Modify: docs `docs/pricing.md` (Cost records, avoided), `docs/tool-prune-plugin.md:152,340`, `docs/laptop-token-savings.md`, `docs/session-api.md:44`

- [ ] **Step 1: Tests.** Event: `TestTotalPenaltyMicros`, `TestTotalAvoidedUSD_CountsOnlyUSDBasis` (a `subscriptionEquivalent` and an `unknown` saving add 0); usage: `TestAggregator_SumsPenalty` and the old `TotalsAreInvariantToAvoidedCost` still holds for penalty; ledger: `TestRow_PenaltyMicrosRoundTrip` and an old row without the field reads 0; session list `penaltyMicros` present when nonzero; agentop: `TestRowSaving_NetCanBeNegative`, `TestRowSaving_AnyComponent`; inferenceparser: `TestSettleCost_PublishesAccountantSavings` (a tool-prune request → `Avoided[0].Component=="tool-prune"`, USD from `TierUSD`).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** and delete the old path. Update tests that pinned old figures (list in spec research: settle `TestAvoided_*`, toolprune `TestMetrics_AttributesSavingToTheRightTier`, `TestPrune_SettlementCalibrates...`, forwardproxy `chainedwriters_test.go:118,278-301`, ledger/usage/event tests) to the accountant figure, or delete those that tested removed code.
- [ ] **Step 4: Verify** all modules' tests, vet, gofmt, tidy (`go mod tidy -diff` everywhere — deletions can drop deps). Commit `feat(accounting): Record accountant savings in the ledger, /v1/usage and agentop`.

### Task 10: Metrics push

**Files:**
- Create: `core/accounting/metrics.go`, `core/accounting/metrics_test.go`, `core/accounting/push/push.go`, `core/accounting/push/push_test.go`
- Modify: `core/config/accounting.go` (`MetricsPushConfig`), `core/config/config.go` (`MetricsPush *MetricsPushConfig yaml:"metrics_push,omitempty"`), `cmd/cortex/main.go`, `cmd/cortex-envoy/main.go`, `core/go.mod` (OTel `sdk/metric`, `exporters/otlp/otlpmetric/otlpmetrichttp`, `github.com/rossoctl/accountant/metrics` at the pseudo-version in context-guru's **v0.7.4-beta.1 tag** `go.mod` — read it with `git -C <cg> show v0.7.4-beta.1:go.mod`, not main)
- Modify: docs `docs/laptop-service.md`, `docs/pricing.md`, CLAUDE.md (port table unchanged; add `metrics_push` and `accounting` to config notes)

**Interfaces:**
- Produces:
  - `config.MetricsPushConfig{ Enabled *bool yaml:"enabled"; URL string yaml:"url"; Token string yaml:"token"; Interval time.Duration yaml:"interval" }`; `func (m *MetricsPushConfig) Resolve(env func(string) string) MetricsPushConfig` — env `METRICS_PUSH`, `METRICS_PUSH_URL`, `METRICS_PUSH_TOKEN`, `METRICS_PUSH_INTERVAL` override; default interval 30s; `Validate`: https required except loopback.
  - `func push.Start(cfg config.MetricsPushConfig, version, machineIDPath string) (metric.MeterProvider, func(context.Context) error, error)` — nil provider when no URL or disabled.
  - `func accounting.NewSink(mp metric.MeterProvider, r pricing.Resolver) *Sink`; `func (s *Sink) Observe(meta accountant.RequestMeta, u accountant.Usage, res accountant.Result, endpoint string, promptTotal int)`; `Host` gains `Options.Sink *Sink` and calls it after each `Compute` and `Ping` (pings with empty `RequestID`).

- [ ] **Step 1: Tests.** `metrics_test.go` with an SDK `ManualReader`: a `Result` with a saved op gives `llm.savings.saved.tokens{kind,tier,component="mask"}` and `llm.savings.saved.usd` equal to `TierUSD` for the same request (the ledger and metrics agree to the micro); two requests on different endpoints observed concurrently each priced with their own endpoint (mutex binding); a panic in the pricer does not escape. `push_test.go`: an `httptest` OTLP receiver at `/v1/metrics` gets a request with `Authorization: Bearer t`, resource `service.instance.id` (stable across two `Start`s with the same file), `service.version`, `service.name=cortex`; series names and label keys ⊆ the central allow-list (copy from context-guru `central_e2e_test.go:183` with a citing comment: `__name__, machine, proxy, preset, service_version, model, provider, tier` after collector mapping — assert the pushed label keys are `kind,tier,component,effect,cause,direction,reason,check,result` only); http non-loopback URL → `Validate` error; no URL → nil provider and one INFO line; receiver down → no error returned from `Observe`, one WARN per minute at most, `llm.proxy.export_failures` incremented.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** (sink: `meter := mp.Meter("github.com/rossoctl/cortex/core/accounting")`, then `acmetrics.New(meter, boundPricer, acmetrics.WithComponents(<operator names without prefix>))`; `boundPricer.Price(model, at, tier)` reads endpoint and prompt total set under `Sink.mu`; machine ID: random 16-byte hex, file 0600). Wire in `main.go`; shutdown flushes with a 5s timeout. Run → PASS.
- [ ] **Step 4: Docs, verify, commit.** Document both config blocks and the env vars; full verification across modules. Commit `feat(accounting): Push llm.savings metrics to the central collector`.

---

## After the last task

- Run the whole-branch review (per the execution skill).
- Write the PR description draft to `/tmp/accounting-pr-body.md` (summary, the 8 commits, harness result or "not run", differences list, #1412 superseded). Do not open the PR; the user decides.
