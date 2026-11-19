# The key a request sends selects its inference server (#1979) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a request sends the key of a configured server, the `inference-router` routes the request to that server, or to the server that a new `key_routes:` entry names. An `agents:` entry still wins.

**Architecture:** At `Configure`, the router keeps a list of (server name, key). For each request, `keyOwner` compares the request's credential headers with each key in constant time, and gives the one server whose key the request sent. `choiceFor` gives the unpinned choice in the order `agents:`, `key_routes:`, key owner. Both places that read `p.agents` today call `choiceFor`. `decide.go` does not change. Each router invocation records `key_of`, and the session summary shows it as `inferenceKeyOf`.

**Tech Stack:** Go 1.26.10. Module `core/` only. `cmd/agentop` imports `routerconfig` and must still vet.

**Spec:** issue rossoctl/cortex#1979 (part B of #1975). Read it with `gh issue view 1979 --repo rossoctl/cortex`. Part A1 (#1977, PR #1983) is merged into the base of this plan. Its plan is `docs/superpowers/plans/2026-11-18-server-by-name-1977.md`.

## Global Constraints

- Order for a request in a session with no pin: `sessions:` entry, the session's pin, `agents:`, `key_routes:`, the server whose key the request sent, not routed. Do not change `decide.go`.
- Compare keys with `crypto/subtle.ConstantTimeCompare`, with no early exit over the servers.
- A key is never in an invocation, a log line, an error, `/config` or `/v1/pipeline`. A log line or an error names servers only.
- A duplicate key (two servers with one value) is valid config. `Configure` logs one WARN that names both servers. A request with a duplicate key matches no server.
- A request with no User-Agent (`agentOf` returns `""`) is not routed by its key.
- `key_routes` is one hop only. With `{team-b: team-a, team-a: team-c}`, team-b's key goes to `team-a`.
- A session pinned "not routed" keeps that pin. A session's history wins over a `key_routes:` entry.
- Comments and docs in ASD-STE100 Simplified Technical English. Commits use `git commit -s` and end with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.
- Per-module Go commands run from `core/` with `GOWORK=off`.

## Decisions made in this plan

1. **Signature `choiceFor(agent, keyOf string) string`, not `choiceFor(pctx, agent)`.** Reason: `OnRequest` must record `key_of` also when the session is pinned elsewhere, so it computes `keyOf := p.keyOwner(pctx.Headers)` one time at the start. `choiceFor` then reads no headers, and the two callers cannot see different headers. `serverFor` gets a third parameter `keyOf string`.
2. **The headers that `keyOwner` reads.** The bearer of each `Authorization` value (`auth.ExtractBearer`), each `X-Api-Key` value, and each value of each header that a server's `auth_header` names, other than `authorization` (that one is the bearer case). It does not read `Api-Key` unless an `auth_header` names it. Reason: the spec names these three sources only.
3. **Duplicate keys.** A server whose key another server also has is removed from the match list at `Configure`. Reason: then a duplicate can never match, and the per-request loop stays simple. The WARN is one line per duplicate group: `inference-router: servers share one key, so a request that sends it selects no server` with attribute `servers` = the sorted names joined by `,`.
4. **`key_of` on every router invocation of the request** that `OnRequest` records after `serverFor`: `not_routed`, `pinned_server_removed`, `redirect_failed`, `would_route`, `routed`, and the records `refusePlan` makes from `details()`. Not on `pctx.Skip` records (`not_redirectable`, `not_an_inference_server`). Reason: those return before any choice. `key_of` is recorded also for a request with no User-Agent, because it names whose key the request sent and holds no key.
5. **`model.RoutedKeyOf`.** The summary reads `key_of` from the LAST `Outbound` invocation with `Plugin == "inference-router"` and `Phase == InvocationPhaseRequest` that has a non-empty `key_of` detail. Reason: it is on every reason, not only `routed`, so `RoutedServer`'s filter does not apply.
6. **`InferenceKeyOf` moves with `InferenceHost`**, as `InferenceServer` does: the store entry, the fold, the fold's JSON (`inferenceKeyOf`, omitempty), and the history merge. Reason: a stale `key_of` beside a new host names the wrong key owner. `hostRuleVersion` does not change: the hosts table is not touched.
7. **Type `model.LenientString`** for `SessionSummary.InferenceKeyOf` and the fold JSON field. Reason: the same type as `InferenceServer`, so an old or odd `session.json` decodes.
8. **The redaction test lives in the router package.** Reason: it needs `redact` and `routerconfig`, and the router package already imports both.
9. **One PR.** The estimate is about 800 counted lines.

## Review Focus

1. Two headers that match two different servers (`Authorization: Bearer team-a-key` and `X-Api-Key: team-b-key`): no match, not routed. Test in T3 (`TestKeyOwner`).
2. A captured request (to `api.anthropic.com`) with no `agents:` entry and `team-b`'s key: captured and routed. The capture check at `plugin.go:333` must call `choiceFor`. Test in T3 (`TestRouter_KeySelectsServer_Captured`).
3. A session already pinned to `team-b`, then a reload adds `key_routes: {team-b: team-a}`: the session stays on `team-b`. Test in T3 (`TestRouter_KeyRoutes`).
4. The duplicate-key WARN, the `Configure` errors and the invocations hold no key value. Test in T3 (`TestRouter_DuplicateKeyWarns`, `TestRouter_KeyOfDetail`) and T2 (error text).
5. `key_routes` survives `redact.JSON` and decodes into `routerconfig.Config`. Test in T3 (`TestKeyRoutes_SurvivesRedaction`).

---

### Task T1: Show `inferenceKeyOf` on the session summary

Depends on: none

**Files:**
- Modify: `core/model/inferencehosts.go` (add `RoutedKeyOf` beside `RoutedServer` `:44`)
- Modify: `core/pipeline/model_alias.go:192` (wrapper `RoutedKeyOf`)
- Modify: `core/session/store.go:169-170` and `:563-565` (`entry.inferenceKeyOf`, set with `inferenceHost`)
- Modify: `core/session/summary.go:185-188` and `:302` (`InferenceKeyOf`)
- Modify: `core/session/fold.go:274-276`, `:339`, `:374`, `:410-411`, `:471`, `:504` (`SummaryFold.inferenceKeyOf`, its JSON field)
- Modify: `core/session/history.go:218-219` (merge `InferenceKeyOf` with `InferenceHost`)
- Modify: `docs/session-api.md:78-81` (add `inferenceKeyOf` after `inferenceServer`)
- Test: `core/model/inferencehosts_test.go`, `core/session/hosts_test.go`

**Interfaces:**
- Produces:
  - `func RoutedKeyOf(in *Invocations) string` in `core/model` (rule: Decision 5). `""` for a nil `in` or no match. Use the existing `routerPlugin` literal.
  - `func RoutedKeyOf(in *Invocations) string { return model.RoutedKeyOf(in) }` in `core/pipeline/model_alias.go`.
  - `SessionSummary.InferenceKeyOf model.LenientString` with tag `json:"inferenceKeyOf,omitempty"`.
- Consumes: the detail name `"key_of"` that T3 writes. T1 uses it as a literal.

- [ ] **Step 1: Write the failing tests.**
  - `TestRoutedKeyOf` in `core/model/inferencehosts_test.go`: nil gives `""`. Outbound `[{inference-router, skip, not_routed, request, {key_of: team-b}}]` gives `"team-b"`. `[{inference-router, modify, routed, request, {server: team-a, key_of: team-b}}]` gives `"team-b"`. A `key_of` on another plugin, or on `Phase == response`, gives `""`. Two router entries, the last with no `key_of`: gives the earlier one's value.
  - `TestSummary_InferenceKeyOf` in `core/session/hosts_test.go`, beside `TestSummary_InferenceServer` `:378`: an inference request with a router `key_of: team-b` sets `InferenceKeyOf == "team-b"`. A later inference request with no `key_of` sets it to `""`.
  - Extend `TestSummaryFold_JSONRoundTripKeepsInferenceServer` `:405` to also check that `inferenceKeyOf` survives the JSON round trip.
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./model/ ./session/ -run 'RoutedKeyOf|InferenceKeyOf|InferenceServer'`. Expected: FAIL (undefined).
- [ ] **Step 3:** Implement the Interfaces. At each place that sets `inferenceServer` from `model.RoutedServer(...)`, also set `inferenceKeyOf` from `model.RoutedKeyOf(...)`. Document `inferenceKeyOf` in `docs/session-api.md`: "the name of the server whose key the request that set `inferenceHost` sent, from the router's `key_of` detail. Empty when that key matched no server. It moves with `inferenceHost`, and `inferenceHostFromHistory` covers it too. It never holds a key."
- [ ] **Step 4:** Run `cd core && GOWORK=off go test ./model/ ./session/`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: Show the server whose key a session's request sent`.

### Task T2: Add `key_routes:` to the router config

Depends on: none

**Files:**
- Modify: `core/plugins/inferencerouter/routerconfig/config.go:34-41` (field), `:197-204` (`applyDefaults`), `:248-266` (`Validate`)
- Test: `core/plugins/inferencerouter/routerconfig/config_test.go`

**Interfaces:**
- Produces: `Config.KeyRoutes map[string]string` with tag `json:"key_routes" description:"Servers whose key a request sends, by server name, to the server the request goes to. One hop only. An agents: entry wins over it."`. Default: an empty map.

- [ ] **Step 1: Write the failing test** `TestValidate_KeyRoutes`, table-driven, on a config with servers `team-a`, `team-b`:
  - `{team-b: team-a}` is valid. `{team-a: team-a}` is valid.
  - `{nope: team-a}` gives the error `key_routes: "nope" is not a server listed under servers`.
  - `{team-b: nope}` gives the error `key_routes.team-b: "nope" is not a server listed under servers`.
  - A config with no `key_routes` decodes (`Decode`) to a non-nil empty map.
  - Two bad entries: the error names the first in sorted order.
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./plugins/inferencerouter/routerconfig/ -run KeyRoutes`. Expected: FAIL.
- [ ] **Step 3:** Implement. In `Validate`, check `key_routes` after `sessions`, in sorted key order, with the two error texts above.
- [ ] **Step 4:** Run `cd core && GOWORK=off go test ./plugins/inferencerouter/routerconfig/`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: Add key_routes to the inference-router config`.

### Task T3: Route by the key a request sends

Depends on: T2

**Files:**
- Modify: `core/plugins/inferencerouter/plugin.go` (`Router` struct `:206-212`, `Configure` `:273-305`, `OnRequest` `:322-371`, `:484`, `serverFor` `:560-563`, package doc `:13-19`)
- Create: `core/plugins/inferencerouter/keys.go` (`keyOwner`, `buildKeys`)
- Test: `core/plugins/inferencerouter/keys_test.go` (create), `core/plugins/inferencerouter/plugin_test.go`, `core/plugins/inferencerouter/reload_test.go`

**Interfaces:**
- Consumes: `routerconfig.Config.KeyRoutes` (T2).
- Produces (package-private):
  - `type serverKey struct{ name, key string }`.
  - Router fields `keys []serverKey` (sorted by name, duplicates removed, Decision 3), `keyHeaders []string` (canonical names from each server's `auth_header`, without `Authorization`, sorted, unique), `keyRoutes map[string]string`.
  - `func buildKeys(servers map[string]routerconfig.Server) (keys []serverKey, headers []string)`. Logs the duplicate WARN (Decision 3).
  - `func (p *Router) keyOwner(h http.Header) string`: the one server whose key the request sent, `""` for none or more than one. Collect the candidate values (Decision 2), skip empty values, compare each with each `p.keys` entry by `subtle.ConstantTimeCompare` and do not exit early. Return a name only when exactly one distinct server matched.
  - `func (p *Router) choiceFor(agent, keyOf string) string`: `""` when `agent == ""`. Else `p.agents[agent]` when set. Else, when `keyOf != ""`, `p.keyRoutes[keyOf]` when set, else `keyOf`. Else `""`.
  - `serverFor(pctx *pipeline.Context, captured bool, keyOf string) (string, pinning)`: `choice := p.choiceFor(agent, keyOf)`.
  - Detail `"key_of"` = `keyOf` on the invocations of Decision 4, set only when `keyOf != ""`.

- [ ] **Step 1: Write the failing tests.** Use `sharedConfig` (`plugin_test.go:1204`: `team-a` and `team-b` on `https://gw.example.com/litellm`, keys `team-a-key`, `team-b-key`) and `routerConfig` (`:30`). Make a request with `request` (`:65`) and replace its `Authorization` header to send a server key.
  - `TestKeyOwner` (keys_test.go), table-driven on a Router from `sharedConfig("")` plus a server `hdr` with `auth_header: x-team-key` and key `hdr-key`: `Bearer team-b-key` gives `team-b`. `X-Api-Key: team-b-key` gives `team-b`. `X-Team-Key: hdr-key` gives `hdr`. `Bearer team-a-key` with `X-Api-Key: team-b-key` gives `""`. `Bearer team-b-key` with `X-Api-Key: team-b-key` gives `team-b`. `Bearer client-key` gives `""`. A subscription token `Bearer sk-ant-oat01-xyz` gives `""`. `X-Team-Key: team-b-key` gives `team-b` (any read header matches any key). No headers gives `""`.
  - `TestRouter_KeySelectsServer`: no `agents:` entry, request to `gw.example.com` with `Bearer team-b-key`, session `s1`, Claude Code UA. `assertRouted(t, pctx, "gw.example.com", "team-b-key")`. `pinOf` gives `team-b`. Record `routed` with `server: team-b`, `key_of: team-b`. Sub-tests: the same with `X-Api-Key: team-b-key` (and no `Authorization`), and with a server's `auth_header`.
  - `TestRouter_KeySelectsServer_Captured`: request to `api.anthropic.com` with `Bearer team-b-key`, no `agents:` entry. Routed to `gw.example.com` with `team-b-key`, and the record has `captured`.
  - `TestRouter_KeyRoutes`: config `sharedConfig("")` plus `"key_routes": {"team-b": "team-a"}`. A new session sending `team-b-key` routes with `team-a-key`, record `server: team-a`, `key_of: team-b`. A session pinned to `team-b` before the reload (use `reloadBuild` from `reload_test.go:20`) stays on `team-b` after a reload that adds the entry.
  - `TestRouter_KeyRoutesOneHop`: `{"team-b": "team-a", "team-a": "ete"}`. `team-b-key` goes to `team-a`.
  - `TestRouter_AgentsWinOverKey`: `agents: {claude-code: ete}` and `key_routes: {team-b: team-a}`. `team-b-key` goes to `ete` with `ete-key`, and the record has `key_of: team-b`.
  - `TestRouter_KeyChoiceSideRequest`: with `claudeCode` (`:824`) as a main-role one-shot (no tools), no `agents:` entry, `team-b-key`: routed to `team-b`, pin `none`. Same result as the same test with `agents: {claude-code: team-b}`.
  - `TestRouter_KeyNotRouted`: `client-key`, a subscription token, and a config where `team-a` and `team-b` have one key: `assertRecord(... ActionSkip, "not_routed", ...)` and the request is untouched (`assertUntouched` `:117`, after you set the expected header). A not-routed record for a duplicate key has no `key_of`.
  - `TestRouter_NoUserAgentNotRoutedByKey`: UA `""`, `team-b-key`: not routed, and the record has `key_of: team-b`.
  - `TestRouter_DuplicateKeyWarns`: capture slog as `plugin_test.go:496` does. `Configure` of a config where `team-a` and `team-b` share key `same-key` logs one WARN that contains `team-a,team-b` and does not contain `same-key`. `Configure` returns no error.
  - `TestRouter_KeyOfDetail`: for each of `routed`, `not_routed` and `would_route` (`pipeline.WithPolicies(pipeline.ErrorPolicyObserve)`, as the test at `plugin_test.go:487` builds it), no invocation detail value equals any server key. The record has `key_of` when the key matched.
  - `TestKeyRoutes_SurvivesRedaction`: `redact.JSON` of a `sharedConfig` with `key_routes: {team-b: team-a}`. `json.Unmarshal` into `routerconfig.Config` gives no error and `KeyRoutes == {team-b: team-a}`. The output contains neither `team-a-key` nor `team-b-key`.
  - In `reload_test.go`, `TestReload_RefusesUnknownKeyRoute`: `plugins.BuildWithDeps` with `key_routes: {team-b: nope}` gives an error that contains `key_routes.team-b`.
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./plugins/inferencerouter/`. Expected: FAIL (undefined symbols).
- [ ] **Step 3: Implement.**
  - `Configure`: call `buildKeys(c.Servers)`, set `p.keys`, `p.keyHeaders`, `p.keyRoutes = c.KeyRoutes` in the same assignment as the other tables.
  - `OnRequest`: after the `Redirectable` check, `keyOf := p.keyOwner(pctx.Headers)` and `agent := agentOf(pctx)`. At `:333` replace `p.agents[agentOf(pctx)] == ""` with `p.choiceFor(agent, keyOf) == ""`. Pass `keyOf` to `serverFor`. Add `key_of` to the `not_routed` map and to `details()` when `keyOf != ""`.
  - Package doc: replace "agents by name to a server" with the order of the Global Constraints, in one short paragraph. Say that a request that sends a server's key and has no `agents:` entry is routed to that server.
- [ ] **Step 4:** Run `cd core && GOWORK=off go test -race ./plugins/inferencerouter/...`. Expected: PASS, with every earlier test unchanged.
- [ ] **Step 5:** Commit: `feat: Route an inference request by the key it sends`.

### Task T4: Document key routing

Depends on: none

**Files:**
- Modify: `docs/inference-router-plugin.md` (`:22-24` field list, the order near `:71`, and a new section before `## Servers that serve another protocol` `:261`)

Facts for this task (all from this plan, so no code is needed):
- New field `key_routes` (map): server name to server name. Both must name a listed server. An entry whose two names are the same is valid and does nothing. One hop only: with `{team-b: team-a, team-a: team-c}`, team-b's key goes to `team-a`. It hot-reloads as `agents` does. An unknown server name is refused at load and on reload.
- Order for a request with no `sessions:` entry and no pin: `agents:`, then `key_routes:` for the server whose key the request sent, then that server, then not routed. The history rules (1 to 4 near `:71`) apply to a key choice as they do to an `agents:` choice: "its agent's current server" now means this choice.
- Key matching: the router reads the bearer of `Authorization`, the value of `X-Api-Key`, and the value of each header that a server's `auth_header` names. It compares each value with each server's key in constant time. A request matches a server only when all its matching headers name that one server. Two headers that match two servers select no server.
- Duplicate key: two servers with the same key are valid. The proxy logs one WARN that names both servers, and a request with that key selects no server.
- A request with no User-Agent (agent `unknown`) is not routed by its key.
- Recording: each router invocation has `key_of: <server name>` when the request's key matched a server, also when the session is pinned elsewhere. The session summary shows it as `inferenceKeyOf`. Neither holds a key.
- Change in behavior: a session of an agent with no `agents:` entry that calls a configured server with that server's key is now routed and pinned to that server. It gets model substitution, limit fitting and the server's `headers:`. A session pinned "not routed" before the upgrade keeps that pin.
- Sessions that exist now: a session's pin and its history win over a `key_routes:` entry. So a change to `key_routes:` moves only new sessions.
- Example: two servers on one URL with two team keys, and `key_routes` that sends team B's terminals to team A:

```yaml
servers:
  team-a: {url: https://gw.example.com/litellm, key: ${TEAM_A_KEY}, main: opus, helper: haiku}
  team-b: {url: https://gw.example.com/litellm, key: ${TEAM_B_KEY}, main: opus, helper: haiku}
key_routes:
  team-b: team-a   # terminals that send team B's key use team A's server and key
```

- [ ] **Step 1:** Write the field entry beside `agents` and `sessions`, the order change, and a section `## Choosing a server by the key a request sends` with the facts above and the example.
- [ ] **Step 2:** Run `pre-commit run check-doc-links --files docs/inference-router-plugin.md`. Expected: Passed.
- [ ] **Step 3:** Commit: `docs: Explain how the key a request sends selects its server`.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Let the key a request sends select its inference server` | none | T1, T2, T3, T4 | 800 |

Estimates: T1 120, T2 80, T3 500, T4 100.

## For a human after review

Needs: T1 to T4 merged.

1. In the PR body, and so in the release notes, state the change in behavior: "An agent with no `agents:` entry that calls a configured server with that server's key is now routed and pinned to that server. It gets model substitution, limit fitting and the server's `headers:`."
2. Close #1979 and mark part B done on #1975. Parts C1 and C2 (agentop commands and pickers) are next. Until C1 lands, `agentop server remove` of a server that a `key_routes:` entry names is refused by the config check, with the generic message.
