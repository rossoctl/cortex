// Package toolprune removes unused tool definitions from outbound inference
// requests.
//
// A Claude Code request carries the full tool manifest on every turn — tens of
// thousands of tokens of JSON schema, billed each time and largely for tools
// the agent will never call in a given deployment. The manifest is assembled by
// the client, so the only place to trim it without touching every client is in
// the proxy.
//
// The verdict is entirely configuration: `remove` names the tools to drop.
// There is no learning, no state and no storage dependency. `abctl tools scan`
// produces a candidate list from local transcripts, but the plugin itself only
// ever does what it was told.
//
// Safety is one-directional. Removing a tool the model needs is the harmful
// failure; carrying a few extra definitions is not. So every error path fails
// open, forwarding the original bytes untouched, and a tool named by a forced
// tool_choice is never removed — the manifest and tool_choice have to agree or
// the request is invalid.
//
// That is a promise about this plugin's own failure modes, not a claim that
// pruning is always safe: whether a provider or gateway accepts a validly
// pruned manifest is outside what the plugin can observe. on_error: observe
// exists to establish that empirically before any request changes.
package toolprune

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// defaultPaths are the inference endpoints the plugin acts on, matched by
// suffix as context-guru does.
var defaultPaths = []string{"/v1/chat/completions", "/v1/completions", "/v1/messages"}

type config struct {
	// Remove names the tools to delete from the manifest. Names not present
	// in a given request are ignored; names the plugin never observes are
	// reported as drift rather than failing.
	Remove []string `json:"remove" description:"Tool names to remove from the outbound manifest."`

	// Paths are the request paths this plugin acts on, matched exactly or by
	// suffix. Defaults to the three inference endpoints.
	Paths []string `json:"paths" description:"Request paths to act on (exact or suffix match)."`

	// There is deliberately no pricing here any more.
	//
	// Rates used to be 12 fields plus a per-model map on this struct, with a
	// second copy of the same idea in litellm-budget-track. They now live in one
	// top-level `pricing:` section resolved by authlib/pricing. An operator who
	// had `pricing:` under this plugin moves it there and gains endpoint scoping,
	// which a per-plugin table could not express: the applicable rate depends on
	// which gateway served the request, not on which plugin is asking.
}

func (c *config) applyDefaults() {
	if len(c.Paths) == 0 {
		c.Paths = append([]string(nil), defaultPaths...)
	}
}

// ToolPrune is the plugin. Counters live in metrics, guarded by its own mutex;
// everything else is read-only after Configure.
type ToolPrune struct {
	cfg    config
	raw    json.RawMessage
	remove map[string]struct{}

	// No rate table here any more. This plugin reduces tokens; pricing the reduction is
	// the cost owner's job, and OnFinish reads the figure back off the published record.
	// Dropping the resolver also removes the nil-interface trap that once silently
	// stopped pruning altogether: an un-injected pricing.Resolver panics on call, and
	// this plugin's fail-open recovery swallowed the panic.

	m         metrics
	driftOnce sync.Once
	// driftChecked records that the stale-list check actually ran, so a test can
	// tell "guard consumed" from "guard consumed without checking anything".
	driftChecked bool
}

func New() *ToolPrune { return &ToolPrune{} }

func init() {
	plugins.RegisterPlugin("tool-prune", func() pipeline.Plugin { return New() })
}

func (p *ToolPrune) Name() string { return "tool-prune" }

func (p *ToolPrune) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		// Request-only: the response is never touched, so SSE relay stays
		// incremental. That distinction is the reason WritesResponseBody
		// exists as a separate capability.
		WritesRequestBody: true,
		RequiresAny:       []string{"inference-parser"},
		Description:       "Removes unused tool definitions from inference requests.",
	}
}

// ConfigSchema implements pipeline.SchemaProvider.
func (p *ToolPrune) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(config{})
}

// RawConfig implements pipeline.RawConfigProvider.
func (p *ToolPrune) RawConfig() json.RawMessage { return p.raw }

func (p *ToolPrune) Configure(raw json.RawMessage) error {
	var c config
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("tool-prune config: %w", err)
		}
	}
	c.applyDefaults()
	p.cfg = c
	p.raw = raw
	p.remove = make(map[string]struct{}, len(c.Remove))
	for _, n := range c.Remove {
		if n != "" {
			p.remove[n] = struct{}{}
		}
	}
	if len(p.remove) == 0 {
		slog.Info("tool-prune: configured with an empty remove list — no-op until names are added",
			"hint", "abctl tools scan")
	}
	return nil
}

// gated reports whether the request path is one the plugin acts on.
//
// The query string is stripped first. Providers accept query parameters on
// these endpoints — /v1/messages?beta=true is a real request Claude Code makes —
// and a suffix match against the raw target silently misses every one of them,
// which reads as the plugin doing nothing for no visible reason.
func (p *ToolPrune) gated(path string) bool {
	path = pathOnly(path)
	for _, s := range p.cfg.Paths {
		if path == s || strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// pathOnly drops a query string and any trailing slash, so the configured
// suffixes match the endpoint rather than the exact request target.
func pathOnly(target string) string {
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if len(target) > 1 && strings.HasSuffix(target, "/") {
		target = strings.TrimRight(target, "/")
	}
	return target
}

// toolNameAt extracts a tool's name from raw manifest element i, covering both
// dialects: Anthropic puts it at tools.i.name, OpenAI at tools.i.function.name.
func toolNameAt(body []byte, i int) string {
	if n := gjson.GetBytes(body, fmt.Sprintf("tools.%d.name", i)); n.Exists() {
		return n.String()
	}
	return gjson.GetBytes(body, fmt.Sprintf("tools.%d.function.name", i)).String()
}

// forcedToolChoice reports the tool a forced tool_choice names, and whether the
// tool_choice could be interpreted at all.
//
// resolvable is false only when tool_choice is an object from which no name can
// be read. That is the dangerous case: the request forces *some* tool the plugin
// cannot identify, so pruning risks removing it and producing an invalid request.
// Dialects nest this differently — Anthropic tool_choice.name, OpenAI
// tool_choice.function.name, Bedrock Converse tool_choice.tool.name — and an
// unknown shape must not be read as "nothing is forced".
//
// A string form ("auto", "none", "any", "required") forces no *specific* tool, so
// it is resolvable with an empty name: pruning is safe.
func forcedToolChoice(body []byte) (name string, resolvable bool) {
	tc := gjson.GetBytes(body, "tool_choice")
	if !tc.Exists() {
		return "", true
	}
	if !tc.IsObject() {
		return "", true // "auto" / "none" / "any" / "required"
	}
	for _, path := range []string{"name", "function.name", "tool.name"} {
		if n := tc.Get(path); n.Type == gjson.String && n.String() != "" {
			return n.String(), true
		}
	}
	// An object naming nothing we recognise. It may still be a plain
	// {"type":"auto"}, which is safe — accept only that narrow shape.
	if t := tc.Get("type"); t.Type == gjson.String {
		switch t.String() {
		case "auto", "none", "any", "required":
			return "", true
		}
	}
	return "", false
}

// OnRequest prunes the manifest. Every failure path returns Continue with the
// body untouched.
func (p *ToolPrune) OnRequest(_ context.Context, pctx *pipeline.Context) (action pipeline.Action) {
	action = pipeline.Action{Type: pipeline.Continue}
	if len(p.remove) == 0 {
		return action
	}
	// A panic here would fail a request to save tokens. Never worth it.
	defer func() {
		if r := recover(); r != nil {
			// Counted, not just logged. Fail-open means a panicking plugin looks
			// healthy while doing nothing, and a log line scrolls away — the metric
			// is what an operator actually sees. See metrics.recovered.
			p.m.recoveredPanic()
			slog.Warn("tool-prune: recovered, forwarding original body", "panic", r)
			action = pipeline.Action{Type: pipeline.Continue}
		}
	}()

	if !p.gated(pctx.Path) {
		// Distinguish "this is not an HTTP request at all" from "the path did
		// not match". A CONNECT tunnel has no path, and reporting it as a path
		// mismatch sends an operator hunting for a routing problem when the
		// real answer is that TLS is not being decrypted — so the client does
		// not trust the bridge CA and nothing downstream can see the request.
		reason := "path_not_inference"
		if pctx.Path == "" {
			reason = "no_path_tunnelled"
		}
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionSkip,
			Reason: reason,
			Path:   pctx.Path,
		})
		return action
	}
	// inference-parser establishes that this is an inference call at all. Its
	// absence means the chain is misconfigured; RequiresAny catches that at
	// build time, so treat it as a skip rather than an error.
	if pctx.Extensions.Inference == nil {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_inference_extension"})
		return action
	}
	body := pctx.Body
	if len(body) == 0 {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_body"})
		return action
	}
	// gjson parses leniently: on a truncated document it still resolves
	// fields, and sjson then rewrites the fragment into garbage. Refuse to
	// touch anything that is not well-formed JSON to begin with.
	if !gjson.ValidBytes(body) {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "invalid_json"})
		return action
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_tool_manifest"})
		return action
	}

	raw := tools.Array()
	p.noteDrift(pctx.Extensions.Inference.Tools)
	p.m.seen()

	// Resolve indices from the raw bytes rather than from the parsed manifest:
	// inference-parser drops unnamed tools, so manifest position does not
	// reliably map back to array position.
	forced, resolvable := forcedToolChoice(body)
	if !resolvable {
		// tool_choice is an object but names no tool we recognise — e.g. a
		// dialect that nests it differently (Bedrock Converse's
		// {"tool":{"name":X}}). Treating that as "nothing is forced" risks
		// pruning the one tool the request requires, so decline instead. A
		// missed saving is the cheap direction of failure.
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionSkip,
			Reason: "tool_choice_unresolved",
			Path:   pctx.Path,
		})
		return action
	}
	// Tools the conversation already used must stay in the manifest. A provider
	// may reject a tool_use / tool_result block that references a tool the
	// request no longer defines, and enabling the plugin mid-conversation (the
	// config hot-reloads) is exactly when history can cite a tool the scan
	// proposed — the scan only looks at a rolling window, so a tool used earlier
	// in this very session can be on the remove list.
	//
	// Not reproducible against every provider (one gateway accepts it), but the
	// cost of the guard is a few unpruned definitions and the cost of being wrong
	// is a failed request, so it is not a trade worth making.
	used := toolsCitedByHistory(body)

	var victims []int
	var anyNameResolved bool
	names := make([]string, 0, len(raw))
	for i := range raw {
		name := toolNameAt(body, i)
		if name == "" {
			continue
		}
		anyNameResolved = true
		if name == forced {
			// Removing the tool tool_choice forces would make the request
			// invalid. Keep it and prune the rest.
			slog.Debug("tool-prune: keeping tool forced by tool_choice", "tool", name)
			continue
		}
		if _, cited := used[name]; cited {
			slog.Debug("tool-prune: keeping tool cited by conversation history", "tool", name)
			continue
		}
		if _, ok := p.remove[name]; ok {
			victims = append(victims, i)
			names = append(names, name)
		}
	}
	if len(victims) == 0 {
		// Distinguish "the manifest had none of the configured tools" from "no
		// tool name could be read at all" — the latter means an unrecognised
		// dialect (Gemini, Bedrock toolSpec nesting), where the plugin is inert
		// for a reason an operator would want to know about.
		reason := "no_configured_tool_present"
		if len(names) == 0 && !anyNameResolved {
			reason = "names_unresolved"
		}
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: reason, Path: pctx.Path})
		return action
	}

	out := body
	var err error
	if len(victims) == len(raw) {
		// Emptying the array is not safe — OpenAI rejects `tools: []`, and
		// tool_choice without tools. Drop both keys instead.
		if out, err = sjson.DeleteBytes(out, "tools"); err != nil {
			slog.Warn("tool-prune: delete tools failed, forwarding original", "err", err)
			return action
		}
		if gjson.GetBytes(out, "tool_choice").Exists() {
			if out, err = sjson.DeleteBytes(out, "tool_choice"); err != nil {
				slog.Warn("tool-prune: delete tool_choice failed, forwarding original", "err", err)
				return action
			}
		}
	} else {
		// A prompt-cache breakpoint rides on one element (Claude Code marks the
		// last tool). Deleting that element deletes the breakpoint, and losing
		// it turns every subsequent turn into a full cache write — which costs
		// far more than the definitions saved. Carry the marker to the last
		// surviving tool instead.
		victimSet := make(map[int]bool, len(victims))
		for _, v := range victims {
			victimSet[v] = true
		}
		// Last marker wins: if two pruned tools each carried a breakpoint, only
		// one can move to the single last survivor. Claude Code marks exactly one
		// tool, so this is not a shape seen in practice — but a future reader
		// should know the overwrite is deliberate, not an oversight.
		var orphanedCacheControl gjson.Result
		for _, v := range victims {
			if cc := gjson.GetBytes(body, fmt.Sprintf("tools.%d.cache_control", v)); cc.Exists() {
				orphanedCacheControl = cc
			}
		}
		lastSurvivor := -1
		for i := len(raw) - 1; i >= 0; i-- {
			if !victimSet[i] {
				lastSurvivor = i
				break
			}
		}
		if orphanedCacheControl.Exists() && lastSurvivor >= 0 &&
			!gjson.GetBytes(body, fmt.Sprintf("tools.%d.cache_control", lastSurvivor)).Exists() {
			if out, err = sjson.SetRawBytes(out,
				fmt.Sprintf("tools.%d.cache_control", lastSurvivor),
				[]byte(orphanedCacheControl.Raw)); err != nil {
				slog.Warn("tool-prune: could not preserve cache_control, forwarding original", "err", err)
				return action
			}
			slog.Debug("tool-prune: moved cache_control to the last surviving tool", "index", lastSurvivor)
		}
		// Descending, so an earlier deletion never shifts a later index.
		for i := len(victims) - 1; i >= 0; i-- {
			if out, err = sjson.DeleteBytes(out, fmt.Sprintf("tools.%d", victims[i])); err != nil {
				slog.Warn("tool-prune: delete failed, forwarding original", "index", victims[i], "err", err)
				return action
			}
		}
	}
	if len(out) >= len(body) {
		// Nothing shrank: treat as a no-op rather than emitting a rewrite.
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_bytes_removed"})
		return action
	}
	// Post-conditions. The edit is surgical, so verify it actually did what
	// was intended before putting it on the wire: still valid JSON, and
	// exactly the intended number of tools left standing.
	if !gjson.ValidBytes(out) {
		slog.Warn("tool-prune: rewrite produced invalid JSON, forwarding original")
		return action
	}
	want := len(raw) - len(victims)
	if got := len(gjson.GetBytes(out, "tools").Array()); got != want {
		slog.Warn("tool-prune: unexpected tool count after rewrite, forwarding original",
			"got", got, "want", want)
		return action
	}

	removedBytes := len(body) - len(out)
	// Publish the per-request saving so a UI can show it on the row rather than
	// only in an aggregate pane. Emitted here, in OnRequest, because the
	// listener records the response session event before the deferred
	// RunFinish, so anything published from OnFinish arrives too late to appear.
	//
	// Everything except the token tier is known now: inference-parser runs
	// earlier in the chain and has already set the model, so the applicable
	// rates resolve here. The consumer pairs this with the response event
	// (matching on RequestID) to get the prompt token total and which tier the
	// saving came out of, and finishes the arithmetic.
	// Rates come from the process table, scoped to the endpoint this request is
	// headed for — the same model can bill differently on a discounted gateway
	// than on the vendor endpoint, and only the target host distinguishes them.
	//
	// SetBody BEFORE publishing, so the event can report what was actually sent.
	// Under ErrorPolicyObserve it is a no-op on bytes and leaves bodyMutated
	// false — this same code path measures without enforcing.
	pctx.SetBody(out)
	applied := pctx.BodyMutated()
	// The body upstream actually sees: the rewrite when it was applied, the
	// original when it was only measured.
	bodySent := len(out)
	if !applied {
		bodySent = len(body)
	}
	p.publish(pctx, pruneEvent{
		ToolsRemoved:   names,
		BytesRemoved:   removedBytes,
		BodyBytesAfter: bodySent,
		Projected:      !applied,
		Model:          inferenceModel(pctx),
	})
	// Carry the saving to OnFinish, where the response reveals which token tier
	// it came out of. SetState keeps it private to this plugin, unlike
	// Extensions.Custom which is shared.
	pipeline.SetState(pctx, p.Name(), &requestState{bytesRemoved: removedBytes})
	if applied {
		p.m.pruned(names, removedBytes)
	} else {
		p.m.projected(names, removedBytes)
	}
	return action
}

func (p *ToolPrune) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// requestState carries the per-request byte saving from OnRequest to OnFinish.
type requestState struct{ bytesRemoved int }

// OnFinish converts the request's byte saving into tokens and attributes it to
// the token tier it actually came out of.
//
// Two things make a single "tokens saved" number wrong, which is why this is
// per-tier. First, the ratio: rather than bundling a tokenizer or assuming
// bytes-per-token, it is calibrated on this request — prompt tokens over request
// bytes, both post-pruning, so the two sides are consistent. Second, and larger:
// providers price prompt tiers very differently. Anthropic charges 1.25x the
// input rate for a cache write and 0.1x for a cache read, so identical saved
// bytes are worth more than 12x more on a cache miss than on a hit. Reporting
// one blended figure would hide a factor of twelve.
//
// The tool manifest sits inside the cached prefix — Claude Code puts
// cache_control on the tool block — so on a cache-miss request the saving comes
// out of cache writes, and on a hit out of cache reads. That is the assumption
// this attribution rests on; it is stated here because it is the one thing that
// would need revisiting for a client that lays out its prompt differently.
func (p *ToolPrune) OnFinish(_ context.Context, pctx *pipeline.Context) {
	st := pipeline.GetState[requestState](pctx, p.Name())
	if st == nil || st.bytesRemoved <= 0 {
		return
	}
	// The saving is priced by the cost owner, not here.
	//
	// This function used to do it: estimate tokens from the byte delta, pick the tier,
	// resolve rates, multiply. That was the third copy of the same arithmetic — abctl had
	// one and litellm-budget-track had another — and this plugin's job is reducing
	// tokens, not accounting for money. It now reads the figure attributed to it and
	// aggregates, so the pane and the ledger cannot disagree about what was saved.
	//
	// Nothing to report is the normal case for a request with no inference in it.
	sv, ok := savingFor(pctx, p.Name())
	if !ok {
		return
	}
	// An unrecognized tier means the record and this build disagree about the vocabulary.
	// Passed as nil rather than defaulted: naming it input would misattribute the tokens
	// by up to 12.5x and render as a real input saving.
	var tier *pricing.Tier
	if t, ok := pricing.TierFromString(sv.Tier); ok {
		tier = &t
	}
	prov := pricing.ProvNone
	if sv.Provenance != "" {
		if pv, ok := pricing.ProvenanceFromString(sv.Provenance); ok {
			prov = pv
		}
	}
	// Projected travels with the figure: in observe mode the bytes went upstream and were
	// billed, so this is money that WAS spent and must not join a realized total.
	p.m.observeSaving(float64(sv.TokensAvoided), tier, sv.USD, prov, modelOf(pctx), sv.Projected)
}

// savingFor pulls this plugin's entry out of the published cost record.
//
// Reads the record rather than recomputing, and reads it by COMPONENT so a pipeline with
// several body-shrinking plugins attributes each one's saving to itself.
func savingFor(pctx *pipeline.Context, component string) (costevent.Saving, bool) {
	if pctx == nil || len(pctx.Extensions.Custom) == 0 {
		return costevent.Saving{}, false
	}
	ev, ok := pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix].(costevent.Event)
	if !ok {
		return costevent.Saving{}, false
	}
	for _, s := range ev.Avoided {
		if s.Component == component {
			return s, true
		}
	}
	return costevent.Saving{}, false
}

// modelOf names the model for the unpriced-model tally, which is what tells an operator
// WHICH pricing entry to add.
func modelOf(pctx *pipeline.Context) string {
	if pctx.Extensions.Inference == nil {
		return ""
	}
	return pctx.Extensions.Inference.Model
}

// tierOf picks the tier the pruned manifest belonged to, delegating the rule to
// authlib/pricing so the UI that renders the saving and the plugin that measures
// it cannot disagree about which tier it came from.
func tierOf(inf *pipeline.InferenceExtension) pricing.Tier {
	return pricing.PromptTier(pricing.UsageFromInference(inf))
}

// noteDrift logs, once, any configured name absent from the first manifest the
// plugin actually sees. A stale list costs savings rather than correctness, so
// it surfaces as a warning instead of a failure.
func (p *ToolPrune) noteDrift(observed []pipeline.InferenceTool) {
	// Check the precondition BEFORE consuming the Once. sync.Once marks itself
	// done however the closure returns, so an early return on an empty manifest
	// used to disable this warning permanently — and an empty first manifest is
	// the norm on the dialects the plugin already knows it cannot read names
	// from (Gemini functionDeclarations, Bedrock toolSpec nesting), which is a
	// live path here. The result was that a stale remove list stayed silent in
	// exactly the deployments most likely to have one.
	if len(observed) == 0 {
		return
	}
	p.driftOnce.Do(func() {
		p.driftChecked = true
		present := make(map[string]struct{}, len(observed))
		for _, t := range observed {
			present[t.Name] = struct{}{}
		}
		var missing []string
		for _, n := range p.cfg.Remove {
			if _, ok := present[n]; !ok {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			slog.Warn("tool-prune: configured tools not present in the observed manifest — list may be stale",
				"missing", strings.Join(missing, ","),
				"hint", "re-run abctl tools scan")
		}
	})
}

// Metrics implements pipeline.MetricsProvider.
func (p *ToolPrune) Metrics() []pipeline.Metric { return p.m.snapshot() }
