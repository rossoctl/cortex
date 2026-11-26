package contextguru

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/apply"
	cgschema "github.com/rossoctl/context-guru/schema"
	cgstore "github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/cortex/core/pipeline"
)

// Tool deferral (rossoctl/cortex#2150): the ultrahigh preset's toolfilter `defer: auto`. A deferred
// tool stays in the request with defer_loading: true, and the provider loads it through its own
// tool search when the model asks. The decision, the rewrite and the order of the steps are
// context-guru's (tooldefer.Flow). This file is the host's half, a port of the context-guru proxy's
// proxy/tooldefer.go and of inband.RetryRefused:
//   - the state: the tool history, the probe verdicts and the off switches (deferHost);
//   - Flow.Apply on the final body, after the engine's pipeline (deferTools);
//   - the probe, on the request's own credential (startProbe);
//   - the resend of the original request when the provider refuses the deferred one (Resend);
//   - the booking: the accountant reads the body the provider bills (deferOps, SetBilledBody).
//
// Deferral runs only where the listener resends a refused request (pipeline.Context.Resendable).
// Elsewhere the client would see the 400, and Claude Code then turns its tool search off for the
// session. It runs only on the Anthropic Messages wire: there the provider runs the search, so no
// in-band answer is needed (tooldefer.Info.Search stays false).

// deferStartWait bounds how long Init waits for the stored verdicts and history to load, as the
// proxy's startWait. Without the wait, the first request reads "probe unknown", and its whole
// thread defers nothing (rossoctl/context-guru#871, defect 3).
const deferStartWait = 10 * time.Second

// deferHost is the deferral state of one file, for the life of the plugin instances that use it.
// A config reload builds a new instance before the old one shuts down, so the state is shared,
// and counted: two tooldefer.Mem on one file would split the history.
type deferHost struct {
	path   string
	file   *tooldefer.File
	hist   *tooldefer.Mem
	prober *tooldefer.Prober
	gate   *tooldefer.Gate
	ledger tooldefer.Ledger
	stop   func()
	users  int
}

var (
	deferHostsMu sync.Mutex
	deferHosts   = map[string]*deferHost{}
)

// deferFilePath is tool deferral's own file, beside the session-state database.
func deferFilePath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), tooldefer.FileName)
}

// acquireDeferHost returns the state for path, and opens it on first use. nil when the file
// cannot be opened: deferral then stays off, with one WARN.
func acquireDeferHost(path string) *deferHost {
	deferHostsMu.Lock()
	defer deferHostsMu.Unlock()
	if h := deferHosts[path]; h != nil {
		h.users++
		return h
	}
	f, err := tooldefer.OpenFile(path, cgstore.DefaultDiskRetention)
	if err != nil {
		slog.Warn("context-guru: ultrahigh defers no tools: tool deferral's file could not be opened",
			"path", path, "error", err)
		return nil
	}
	h := &deferHost{path: path, file: f, hist: tooldefer.NewMem(f.DB), prober: tooldefer.NewProber(),
		gate: tooldefer.NewGate(), users: 1}
	h.hist.Retention = cgstore.DefaultDiskRetention
	// The verdicts survive a restart with the history, so a thread decided before it keeps its set.
	h.prober.Persist = h.hist.SaveVerdict
	h.stop = h.hist.Open(0, h.prober)
	deferHosts[path] = h
	return h
}

// release drops one user. The last one flushes the history and closes the file, with the
// registry lock held for the whole flush: a config reload's new instance can Init while the
// old instance's Shutdown is still draining (reloadOnce starts the old pipeline's Stop in the
// background and returns), and acquireDeferHost must not build a second Mem on this file while
// the first one's final flush is still writing it (CodeRabbit review finding).
func (h *deferHost) release() {
	if h == nil {
		return
	}
	deferHostsMu.Lock()
	defer deferHostsMu.Unlock()
	h.users--
	if h.users > 0 {
		return
	}
	delete(deferHosts, h.path)
	h.stop()
	_ = h.file.Close()
}

// startDefer builds the deferral state in Init, when the engine's pipeline asks for deferral, and
// waits up to deferStartWait for the stored state to load.
func (p *ContextGuru) startDefer() {
	if !p.deferOn {
		return
	}
	path := deferFilePath(p.statePath)
	if path == "" {
		slog.Warn("context-guru: ultrahigh defers no tools: there is no session-state database, " +
			"so tool deferral has no durable place for its per-thread record")
		return
	}
	h := acquireDeferHost(path)
	if h == nil {
		return
	}
	p.defer_ = h
	ctx, cancel := context.WithTimeout(context.Background(), deferStartWait)
	defer cancel()
	ready := true
	select {
	case <-h.hist.ReadyC():
	case <-ctx.Done():
		ready = false
	}
	slog.Info("context-guru: tool deferral state loaded at start", "path", path, "ready", ready)
}

// warnDeferConfig says, at Configure, why a pipeline that should defer does not.
func warnDeferConfig(preset string, on bool) {
	if !on && preset != "" && tooldefer.DocOn("preset: "+preset+"\n") {
		slog.Warn("context-guru: preset " + preset + " defers tools, but this engine config turns deferral off: " +
			"a components.toolfilter block replaces the preset's whole toolfilter block, so add `defer: auto` to it")
	}
}

// deferParts is the state for the library's flow.
func (p *ContextGuru) deferParts() tooldefer.Parts {
	h := p.defer_
	env := tooldefer.Env{Store: p.store, Fallback: h.file.Store, History: h.hist, Gate: h.gate, Prober: h.prober}
	return tooldefer.Parts{Env: env, Hist: h.hist, Ledger: &h.ledger}
}

// deferFlow is the library's flow for one request. pctx is nil for a call that starts no probe
// (Retry).
func (p *ContextGuru) deferFlow(pctx *pipeline.Context, provider bschemas.ModelProvider, api string) tooldefer.Flow {
	f := tooldefer.Flow{Pipe: p.pipe, Provider: provider, Wire: cgschema.WireFor(string(provider), api),
		Parts: p.deferParts, ProjectFor: apply.ProjectFor}
	if pctx != nil {
		scheme := pctx.Scheme
		if scheme == "" {
			scheme = "https"
		}
		f.Upstream, f.Route = scheme+"://"+pctx.Host, pctx.Path
		f.StartProbe = func(key tooldefer.ProbeKey) { p.startProbe(pctx, key) }
	}
	return f
}

// deferState is one request's deferral, kept on pctx for Resend and OnFinish.
type deferState struct {
	provider bschemas.ModelProvider
	api      string
	di       *tooldefer.Info
	// resent says Resend sent di.Orig after a refusal of status refused, whose body was errBody.
	// stubbed says it then sent the original once more with a stub for a missing tool.
	resent, stubbed bool
	refused         int
	errBody         []byte
}

const deferStateKey = "context-guru-tooldefer"

// noResendOnce logs, once per process, that a listener cannot defer.
var noResendOnce sync.Once

// deferTools runs Flow.Apply on body, the engine's output, and returns the body to send. pre is the
// body deferral read, nil when it changed nothing. A body can change with pre nil: the library adds
// a deferred stub for a tool the transcript names and `tools` lacks (tooldefer.RepairRefs).
func (p *ContextGuru) deferTools(pctx *pipeline.Context, provider bschemas.ModelProvider, api, sid string, body []byte) (out, pre []byte) {
	if !p.deferOn || p.defer_ == nil || provider != bschemas.Anthropic || pctx.Observing() {
		return body, nil
	}
	if !pctx.Resendable() {
		noResendOnce.Do(func() {
			slog.Warn("context-guru: ultrahigh defers no tools on this listener: it cannot send the original " +
				"request again when the provider refuses a deferred one (only the forward proxy can)")
		})
		return body, nil
	}
	th, _ := requestThread(pctx)
	out, di := p.deferFlow(pctx, provider, api).Apply(sid, th.ID, pctx.Body, body)
	if di == nil {
		return out, nil
	}
	pipeline.SetState(pctx, deferStateKey, &deferState{provider: provider, api: api, di: di})
	// di.Orig is captured inside Flow.Apply AFTER RepairRefs runs (context-guru v0.7.9,
	// tooldefer/flow.go), so on a thread whose transcript names a removed tool, di.Orig already
	// carries the repair stub. accrec.OwnDeferred(out, pre) only credits a tool deferred in out
	// and not already deferred in pre, so using di.Orig here would leave that stub's full
	// definition in the accountant's billed body, though the provider bills none of it. body, the
	// input to Apply, is the correct pre-deferral snapshot (clawgenti review finding) — it is
	// byte-identical to di.Orig except when RepairRefs fired, and Resend keeps using st.di.Orig,
	// which is the body to resend, unaffected by this.
	return out, body
}

// startProbe runs the provider check in the background, on the request's own credential, through
// the incoming-model dispatcher. Its spend is reported on this request (meteredDispatcher). A model
// with no price is never probed, as in the proxy (tooldefer.ProbeAllowed), so it is never deferred.
func (p *ContextGuru) startProbe(pctx *pipeline.Context, key tooldefer.ProbeKey) {
	if p.holds == nil {
		return
	}
	if price, ok := p.holds.d.Price(context.Background(), pctx.Host, key.Model, 0); !ok || price.Input <= 0 {
		return
	}
	m, ok := p.anthropicIncoming(pctx)
	if !ok {
		return
	}
	tooldefer.EnsureProbe(p.defer_.prober, m.D, m.Ref, pctx.Path, key, pctx.Headers.Values("Anthropic-Beta"))
}

// deferOps reports a deferral to the accountant, as the proxy does (proxy.go's ToolDefer hook and
// accrec.Record): the names list deferral adds to the messages is one tooldefer op, and the tools
// change is read against the body the provider bills, with each deferred definition a stub
// (accountant R56). The accountant gets that body through SetBilledBody. in is this plugin's input,
// pre the body deferral read, out the body sent.
func deferOps(pctx *pipeline.Context, in, pre, out []byte) []accountant.Operation {
	ops := accwire.For(pre).MessageOps(accwire.OpToolDefer, pre, out)
	billed, noResult := accrec.BilledForwarded(out, pre)
	pctx.SetBilledBody(func(forwarded []byte) ([]byte, bool) {
		b, why := accrec.BilledForwarded(forwarded, pre)
		return b, why != ""
	})
	if noResult != "" {
		// The accountant books no figure for this request (deferral_unplaced).
		return append(ops, envelopeOps(in, out)...)
	}
	return append(ops, deferEnvelopeOps(in, billed, pre)...)
}

// deferEnvelopeOps is envelopeOps from in to billed, with deferral's edits given to the tooldefer
// operator, so its saving does not land on toolschema (rossoctl/context-guru#908). It is a port of
// accrec.EnvelopeOpsFor on this plugin's own envelope ops: each op keeps its ChangeID, and only the
// operator changes.
func deferEnvelopeOps(in, billed, pre []byte) []accountant.Operation {
	ops := envelopeOps(in, billed)
	own := accrec.OwnDeferred(billed, pre)
	atBilled := map[string]int{}
	for i, t := range gjson.GetBytes(billed, "tools").Array() {
		atBilled[t.Get("name").String()] = i
	}
	inTools := gjson.GetBytes(in, "tools").Array()
	schemaOp, deferOp := accwire.Operator("toolschema"), accwire.Operator(accwire.OpToolDefer)
	for i, o := range ops {
		if o.Operator != schemaOp || o.At.Section != accountant.SectionTools {
			continue
		}
		switch {
		case strings.HasPrefix(o.ChangeID, "cg:toolschema:decl-rewritten:") && o.At.Index < len(inTools):
			name := inTools[o.At.Index].Get("name").String()
			if at, ok := atBilled[name]; ok && own[name] && accrec.IsStub(billed, at) {
				ops[i].Operator = deferOp
			}
		case strings.HasPrefix(o.ChangeID, "cg:toolschema:decl-added:") &&
			gjson.GetBytes(billed, "tools."+strconv.Itoa(o.At.Index)+".name").String() == tooldefer.SearchName:
			ops[i].Operator = deferOp
		}
	}
	return ops
}

// Resend implements pipeline.Resender. It is inband.RetryRefused, split over the listener's
// rounds. On a 400 or 422 to a request deferral changed, it sends the request without deferral,
// once. When that original is refused too because its transcript names a tool `tools` lacks, it
// sends it once more with a deferred stub for that tool (rossoctl/context-guru#871). The client
// never sees deferral's own 400: Claude Code would turn its tool search off for the session.
//
// It returns false at once for every request this plugin did not defer, which is most of them:
// deferState is set only on a changed, deferred request (deferTools). But implementing Resender
// at all makes this plugin a Resender in EVERY chain it runs in, at every preset, not only
// ultrahigh: resenderOf is a type assertion on the method's presence, with no way to say "only
// when configured for deferral". pipeline.Pipeline.HasResenders() then reports true for that
// chain, and a forward-proxy request on it reads up to pipeline.ResendPeekLimit (64 KiB) of
// every non-2xx answer's body before relaying it — a quiet latency cost for a slow-streaming or
// open-ended error body, on every host that pipeline chain is used for, whether or not the
// request is Anthropic Messages or ultrahigh (clawgenti review finding). For an ordinary error
// body this costs nothing measurable; a follow-up could add a pctx-level opt-in that
// resendIfAsked checks before peeking, so an unrelated chain pays nothing at all.
func (p *ContextGuru) Resend(_ context.Context, pctx *pipeline.Context, status int, body []byte) bool {
	st := pipeline.GetState[deferState](pctx, deferStateKey)
	if st == nil || st.di == nil {
		return false
	}
	switch {
	case !st.resent && (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity):
		if !pctx.SetBody(st.di.Orig) {
			return false
		}
		st.resent, st.refused, st.errBody = true, status, bytes.Clone(body)
		pctx.MarkResend("tool_defer_refused")
		return true
	case st.resent && !st.stubbed && status == http.StatusBadRequest:
		fixed, ok := tooldefer.RepairFromError(body, pctx.Body)
		if !ok || !pctx.SetBody(fixed) {
			return false
		}
		st.stubbed = true
		pctx.MarkResend("tool_defer_stub")
		return true
	}
	return false
}

// finishDefer books a resend (Flow.Retry): when the original went through, deferral caused the
// refusal, so the library turns deferral off for this model and forgets the probe verdict. A
// request sent again is recorded with no saving (upstream_rounds), so nothing else is booked.
func (p *ContextGuru) finishDefer(pctx *pipeline.Context) {
	st := pipeline.GetState[deferState](pctx, deferStateKey)
	if st == nil || !st.resent || p.defer_ == nil {
		return
	}
	ok := !st.stubbed && pctx.StatusCode > 0 && pctx.StatusCode < 400
	p.deferFlow(nil, st.provider, st.api).Retry(st.di, st.refused, st.errBody, ok)
}
