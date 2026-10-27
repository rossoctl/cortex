// Package script is the script plugin: it runs every executable file in a folder
// as a pipeline step, so a user can customize Cortex with a small Python or shell
// script instead of a Go plugin and a rebuild.
//
// One pipeline entry, `script`, is one slot: the files directly in its folder
// (default ~/.cortex/plugins), in file-name order. Each file is a long-lived worker
// process (see internal/worker) that answers one JSON message per hook. Repeating
// `script` with a different `dir` puts a second slot elsewhere in the chain, which
// is how a Go plugin sits between two scripts.
//
// A script declares, in its handshake, which hooks it implements and whether it
// enforces. It starts in shadow mode — its denies and edits are recorded and not
// applied — so trying a script cannot break a request. A script that errors (a
// crash, a timeout, a malformed reply) or does not start lets the request through
// unless its slot is fail_closed.
//
// Scripts run only on a local install (plugins.Deps.LocalInstall). Anywhere else a
// `script` entry loads nothing, so a config that names one cannot run commands
// in a sidecar.
package script

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/script/internal/worker"
)

// Name is the plugin's registered name.
const Name = "script"

func init() {
	plugins.RegisterPlugin(Name, func() pipeline.Plugin { return New() })
}

// pool outlives every pipeline build, so a reload hands an unchanged script's
// worker — and its state — to the next build.
var pool = worker.NewPool()

type config struct {
	Dir        string `json:"dir" description:"Folder of scripts, relative to ~/.cortex/plugins. Empty: that folder itself." default:""`
	Timeout    string `json:"timeout" description:"How long one script may take to answer one hook." default:"2s"`
	FailClosed bool   `json:"fail_closed" description:"Deny the request when a script errors or did not start, instead of letting it through." default:"false"`
}

// Script is the plugin.
type Script struct {
	cfg        config
	dir        string
	timeout    time.Duration
	local      bool
	configured bool
	// holder is this build's hold on its workers in the pool; zero for a slot that
	// runs nothing.
	holder int
	loaded []*loaded
	// missing names the scripts in the folder that did not start, in file order.
	// A fail_closed slot refuses every request while it is not empty.
	missing []string
	m       metrics
	// stateKey is where this slot keeps a streamed response's frames on the
	// request's context. Per folder: two slots in one chain see the same frames,
	// and a reload's rebuild of a slot carries on with the copy the old build kept.
	stateKey string
}

// loaded is one file whose worker started.
type loaded struct {
	file
	w  *worker.Worker
	hs worker.Handshake
}

type metrics struct {
	calls, errors, denies, edits, shadows atomic.Int64
}

// New returns an unconfigured instance.
func New() *Script { return &Script{} }

func (s *Script) Name() string { return Name }

// Capabilities is the ceiling until Configure, and after it what the slot is
// wherever scripts run: a request writer, empty or not. The response side is
// what still narrows to what the loaded scripts asked for — reading or
// rewriting responses — so a slot of request scripts does not cost every
// streamed response its streaming.
func (s *Script) Capabilities() pipeline.PluginCapabilities {
	caps := pipeline.PluginCapabilities{
		ReadsBody:          true,
		WritesRequestBody:  true,
		WritesResponseBody: true,
		Description:        "Runs the scripts in ~/.cortex/plugins (laptop only)",
	}
	if !s.configured {
		return caps
	}
	caps.ReadsBody, caps.WritesRequestBody, caps.WritesResponseBody = false, false, false
	if s.local {
		// The floor: which scripts load depends on the folder, and a slot that
		// runs them is a request writer whatever their hooks. Holding that back
		// until a script loads would let a slot build before a body reader while
		// its folder is empty, then refuse the chain — and crash-loop the next
		// restart — when the first script is dropped in.
		caps.WritesRequestBody = true
	}
	for _, l := range s.loaded {
		if l.hs.Has("request") {
			caps.ReadsBody, caps.WritesRequestBody = true, true
		}
		if l.hs.Has("response") {
			caps.ReadsResponseBody = true
		}
		if l.hs.RewriteResponse {
			caps.WritesResponseBody = true
		}
	}
	return caps
}

func (s *Script) ConfigSchema() []pipeline.FieldSchema { return pipeline.SchemaOf(config{}) }

// SetLocalInstall is how plugins.BuildWithDeps tells the plugin it may run scripts.
func (s *Script) SetLocalInstall(local bool) { s.local = local }

// Configure loads the slot's scripts and starts their workers. A script that will
// not start is skipped with a WARN, never failing the build: one broken file must
// not stop the proxy from starting or a config edit from applying. A fail_closed
// slot remembers it and refuses requests until a build in which it starts; see
// notStarted.
func (s *Script) Configure(raw json.RawMessage) error {
	s.configured = true
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s.cfg); err != nil {
			return fmt.Errorf("script: %w", err)
		}
	}
	s.timeout = worker.DefaultCallTimeout
	if s.cfg.Timeout != "" {
		d, err := time.ParseDuration(s.cfg.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("script: timeout %q is not a positive duration", s.cfg.Timeout)
		}
		s.timeout = d
	}
	dir, err := SlotDir(s.cfg.Dir)
	if err != nil {
		return err
	}
	s.dir = dir
	s.stateKey = Name + "#" + dir
	if !s.local {
		slog.Warn("script plugin: not a local install, so no scripts are run", "dir", dir)
		return nil
	}

	files, err := scan(dir)
	if err != nil {
		return fmt.Errorf("script: read %s: %w", dir, err)
	}
	if len(files) == 0 {
		return nil // every local install has a slot; an empty one starts nothing and writes nothing
	}
	sdk, err := installSDK()
	if err != nil {
		slog.Warn("script plugin: cannot write the Python SDK; `import cortex` will fail", "error", err)
	}
	env := worker.Environ(os.Environ(), sdk)
	s.holder = pool.NewHolder()
	// Started together, so a slot of slow starters costs one start timeout, not one
	// per script. Results are kept in file order.
	results := make([]*loaded, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		w := pool.Acquire(s.holder, worker.Config{Path: f.path, Env: env, CallTimeout: s.timeout}, f.hash)
		wg.Add(1)
		go func() {
			defer wg.Done()
			hs, err := w.Start(context.Background())
			if errors.Is(err, worker.ErrBackoff) && w.Handshake().Cortex != 0 {
				// It ran before and crashed; keep it, on the handshake it last sent, and
				// let a later call restart it. Dropping it would make a reload — an edit
				// to any other plugin — silently unload a script that is only waiting
				// to restart.
				hs, err = w.Handshake(), nil
			}
			if err != nil {
				slog.Warn("script plugin: script did not start; it is not loaded", "script", f.name, "error", err)
				return
			}
			results[i] = &loaded{file: f, w: w, hs: hs}
		}()
	}
	wg.Wait()
	for i, l := range results {
		if l == nil {
			s.missing = append(s.missing, files[i].name)
			continue
		}
		s.loaded = append(s.loaded, l)
		slog.Info("script plugin: loaded", "script", l.name, "hooks", l.hs.Hooks, "enforce", l.hs.Enforce)
	}
	return nil
}

// Init runs when this build's pipeline starts: from here its workers are the ones
// traffic uses, and anything an abandoned build left in the pool can go.
func (s *Script) Init(context.Context) error {
	if s.holder != 0 {
		pool.Activate(s.holder)
	}
	return nil
}

// Shutdown releases this build's workers when its pipeline has drained. A worker
// the running build also holds keeps running; an edited or removed script's old
// worker stops here, and not before.
func (s *Script) Shutdown(context.Context) error {
	if s.holder != 0 {
		pool.Release(s.holder)
	}
	return nil
}

// SlotDir is the folder a slot's dir config names: ~/.cortex/plugins, or a folder
// directly inside it.
func SlotDir(dir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("script: cannot find the home directory: %w", err)
	}
	root := filepath.Join(home, ".cortex", "plugins")
	if dir == "" {
		return root, nil
	}
	if filepath.IsAbs(dir) || strings.ContainsAny(dir, `/\`) || dir == "." || dir == ".." {
		return "", fmt.Errorf("script: dir %q must be the name of a folder inside %s", dir, root)
	}
	return filepath.Join(root, dir), nil
}

// OnRequest runs every loaded script's request hook, in order. Each sees the
// request as the scripts before it left it, except that a shadow script's edits
// are not applied and so are not seen.
func (s *Script) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if s.cfg.FailClosed && len(s.missing) > 0 {
		if a := s.notStarted(pctx); a.Type != pipeline.Continue {
			return a
		}
	}
	var msg map[string]any
	for _, l := range s.loaded {
		if !l.hs.Has("request") {
			continue
		}
		// Rebuilt only after an applied edit: the next script must see the body the
		// one before it left, and an unedited request need not be encoded twice.
		if msg == nil {
			msg = requestMessage(pctx)
		}
		msg["plugin"] = l.name
		a, stop, edited := s.run(ctx, pctx, l, msg, false)
		if stop {
			return a
		}
		if edited {
			msg = nil
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse is never called: the pipeline sends a StreamingResponder's responses
// to OnResponseFrame instead, buffered or streamed. It exists for the Plugin
// interface, and runs the hooks should a caller use it directly.
func (s *Script) OnResponse(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	return s.responseHooks(ctx, pctx, false, nil)
}

// streamState carries one response's frames to its last one.
type streamState struct {
	frames [][]byte
	size   int
	cut    bool
}

// maxStreamCopy bounds what the plugin keeps of a streamed response for its
// scripts: the forward proxy's own cap on a buffered body.
const maxStreamCopy = 10 << 20

// OnResponseFrame is how responses reach scripts. A buffered response arrives as
// one last frame with the whole body; a streamed one as each SSE event's payload,
// then an empty last frame — by then every byte is with the client, so the
// scripts read it and cannot change or block it. A script that rewrites
// responses narrows this plugin to WritesResponseBody, which sends streamed
// responses down the buffered path, where its edits apply.
func (s *Script) OnResponseFrame(ctx context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if !s.hasResponseHooks() {
		return pipeline.Action{Type: pipeline.Continue}
	}
	// The listeners mark a response delivered before its last frame only when they
	// relayed it as it arrived. A buffered one — JSON, or SSE held back for a
	// response writer — reaches its last frame unsent, with pctx.ResponseBody whole,
	// so the scripts get the body and their edits and denies apply.
	if last && !pctx.ResponseDelivered() {
		pipeline.SetState[streamState](pctx, s.stateKey, nil)
		return s.responseHooks(ctx, pctx, false, nil)
	}
	st := pipeline.GetState[streamState](pctx, s.stateKey)
	if st == nil {
		st = &streamState{}
		pipeline.SetState(pctx, s.stateKey, st)
	}
	if !last {
		if !st.cut && st.size+len(frame) <= maxStreamCopy {
			st.frames = append(st.frames, append([]byte(nil), frame...))
			st.size += len(frame)
		} else {
			st.cut = true
		}
		return pipeline.Action{Type: pipeline.Continue}
	}
	pipeline.SetState[streamState](pctx, s.stateKey, nil)
	return s.responseHooks(ctx, pctx, true, st)
}

func (s *Script) hasResponseHooks() bool {
	for _, l := range s.loaded {
		if l.hs.Has("response") {
			return true
		}
	}
	return false
}

// finalReserve is the part of a response dispatch's deadline that response
// hooks may not spend. Every final response dispatch — the listener's teardown
// context — is one budget shared with the plugins after the slot, which the
// reverse pass reaches only once the slot's scripts are done: the parsers turn
// the folded state into the charge, and a script that spends the whole deadline
// leaves them refused with pipeline.cancelled, which sends the agent a 499 for
// a response that arrived complete and drops the settle. So each call is
// narrowed to what remains minus this reserve, and once the reserve is all that
// is left the script is skipped and recorded like any other script failure. A
// context with no deadline is left alone: nothing after the slot waits on it.
const finalReserve = time.Second

// errReserveSpent names a script skipped to keep finalReserve for the plugins
// after the slot.
var errReserveSpent = errors.New("skipped to leave the response deadline's reserve for the plugins after the slot")

// responseBudget narrows ctx to the part of its deadline one response-hook call
// may spend. ok is false when the reserve is all that remains.
func responseBudget(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}, true
	}
	budget := time.Until(deadline) - finalReserve
	if budget <= 0 {
		return nil, nil, false
	}
	narrowed, cancel := context.WithTimeout(ctx, budget)
	return narrowed, cancel, true
}

// responseHooks runs the response hooks. st is the copied stream for a response
// already delivered, nil for one that is still the pipeline's to change.
func (s *Script) responseHooks(ctx context.Context, pctx *pipeline.Context, streamed bool, st *streamState) pipeline.Action {
	var msg map[string]any
	for i := len(s.loaded) - 1; i >= 0; i-- {
		l := s.loaded[i]
		if !l.hs.Has("response") {
			continue
		}
		callCtx, cancel, ok := responseBudget(ctx)
		if !ok {
			a, stop := s.failed(pctx, l, errReserveSpent, nil)
			if stop {
				if streamed {
					// The client already has the response; record the refusal, which the
					// pipeline marks late, and let it stand.
					return pipeline.Action{Type: pipeline.Continue}
				}
				return a
			}
			continue
		}
		// Built once, and again only after an applied edit, which the next script
		// must see. The request goes along without its body: a response hook rarely
		// reads it, and on a long conversation it is the largest thing in the message.
		if msg == nil {
			msg = responseMessage(pctx, streamed, st)
		}
		msg["plugin"] = l.name
		a, stop, edited := s.run(callCtx, pctx, l, msg, true)
		cancel()
		if stop {
			if streamed {
				// The client already has the response; record the refusal, which the
				// pipeline marks late, and let it stand.
				return pipeline.Action{Type: pipeline.Continue}
			}
			return a
		}
		if edited {
			msg = nil
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}

func responseMessage(pctx *pipeline.Context, streamed bool, st *streamState) map[string]any {
	msg := requestMessage(pctx)
	msg["hook"] = "response"
	delete(msg["request"].(map[string]any), "body")
	delete(msg["request"].(map[string]any), "body_base64")
	resp := map[string]any{
		"status":   pctx.StatusCode,
		"headers":  flatten(pctx.ResponseHeaders),
		"streamed": streamed,
	}
	if streamed {
		resp["frames"] = frameStrings(st.frames)
		if st.cut {
			resp["truncated"] = true
		}
	} else {
		putBody(resp, pctx.ResponseBody)
	}
	msg["response"] = resp
	return msg
}

func frameStrings(frames [][]byte) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = string(f)
	}
	return out
}

// run calls one script and applies its reply. stop is true when the chain ends
// here; edited is true when the script's edit reached the request or response.
func (s *Script) run(ctx context.Context, pctx *pipeline.Context, l *loaded, msg map[string]any, response bool) (a pipeline.Action, stop, edited bool) {
	s.m.calls.Add(1)
	r, err := l.w.Call(ctx, msg)
	if err != nil {
		a, stop = s.failed(pctx, l, err, nil)
		return a, stop, false
	}
	rep, err := parseReply(r.Line)
	if err == nil && rep.Error != "" {
		err = fmt.Errorf("%s", rep.Error)
	}
	if err != nil {
		a, stop = s.failed(pctx, l, err, r.Logs)
		return a, stop, false
	}
	return s.apply(pctx, l, rep, r.Logs, response)
}

func (s *Script) apply(pctx *pipeline.Context, l *loaded, rep reply, logs []string, response bool) (pipeline.Action, bool, bool) {
	inv := pipeline.Invocation{Plugin: l.name, Action: pipeline.ActionObserve, Reason: "script"}
	if rep.Invocation != nil {
		if a := pipeline.InvocationAction(rep.Invocation.Action); validAction(a) {
			inv.Action = a
		}
		if rep.Invocation.Reason != "" {
			inv.Reason = rep.Invocation.Reason
		}
		inv.Details = rep.Invocation.Details
	}
	inv.Details = withLogs(inv.Details, logs)
	shadow := !l.hs.Enforce
	fail := func(err error) (pipeline.Action, bool, bool) {
		a, stop := s.failed(pctx, l, err, logs)
		return a, stop, false
	}

	if rep.Deny != nil {
		code := rep.Deny.Code
		if code == "" {
			code = "policy.forbidden"
		}
		reason := rep.Deny.Reason
		if reason == "" {
			reason = "denied by script " + l.name
		}
		inv.Action, inv.Reason = pipeline.ActionDeny, code
		inv.Details = withDetail(inv.Details, "deny_reason", reason)
		s.m.denies.Add(1)
		if shadow || pctx.Observing() {
			s.m.shadows.Add(1)
			inv.Shadow = true
			pctx.Record(inv)
			return pipeline.Action{}, false, false
		}
		pctx.Record(inv)
		v := &pipeline.Violation{Code: code, Reason: reason, Status: denyStatus(rep.Deny.Status), PluginName: l.name}
		return pipeline.Action{Type: pipeline.Reject, Violation: v}, true, false
	}

	if response && rep.Request.changed() {
		return fail(fmt.Errorf("a response hook may not edit the request"))
	}
	if !response && rep.Response.changed() {
		return fail(fmt.Errorf("a request hook may not edit the response"))
	}
	e := rep.Request
	if response {
		e = rep.Response
		if e.changed() && !l.hs.RewriteResponse {
			return fail(fmt.Errorf("edited a response without declaring rewrite_response"))
		}
		if e.changed() && pctx.ResponseDelivered() {
			return fail(fmt.Errorf("edited a response the client already has"))
		}
	}
	if !e.changed() {
		pctx.Record(inv)
		return pipeline.Action{}, false, false
	}
	if rep.Invocation == nil || inv.Action == pipeline.ActionObserve {
		inv.Action, inv.Reason = pipeline.ActionModify, "script_edit"
	}
	if shadow {
		s.m.shadows.Add(1)
		inv.Shadow = true
		pctx.Record(inv)
		return pipeline.Action{}, false, false
	}
	before := invocationCount(pctx)
	applied, err := s.edit(pctx, e, response)
	if err != nil {
		return fail(err)
	}
	// SetBody records its own body_rewritten row under the pipeline entry's name —
	// "script", for every file in the slot. The row has to name the file, and one
	// write must not show as two, so that row is dropped in favour of inv.
	truncateInvocations(pctx, before)
	if !applied {
		s.m.shadows.Add(1)
		inv.Shadow = true
	} else {
		s.m.edits.Add(1)
	}
	pctx.Record(inv)
	return pipeline.Action{}, false, applied
}

func invocationList(pctx *pipeline.Context) *[]pipeline.Invocation {
	invs := pctx.Extensions.Invocations
	if invs == nil {
		return nil
	}
	if pctx.Direction == pipeline.Inbound {
		return &invs.Inbound
	}
	return &invs.Outbound
}

func invocationCount(pctx *pipeline.Context) int {
	if l := invocationList(pctx); l != nil {
		return len(*l)
	}
	return 0
}

func truncateInvocations(pctx *pipeline.Context, n int) {
	if l := invocationList(pctx); l != nil && len(*l) > n {
		*l = (*l)[:n]
	}
}

// edit applies a script's edits, and reports whether they reached the request or
// response: false when the pipeline muted the body write (on_error: observe). A
// body is written last, so a refused header leaves nothing half-applied.
func (s *Script) edit(pctx *pipeline.Context, e *edits, response bool) (bool, error) {
	body, hasBody, err := e.body()
	if err != nil {
		return false, err
	}
	if response {
		if len(e.SetHeaders) > 0 || len(e.RemoveHeaders) > 0 {
			return false, fmt.Errorf("response headers cannot be changed by any plugin")
		}
		return hasBody && pctx.SetResponseBody(body), nil
	}
	if err := applyHeaders(pctx.Headers, e); err != nil {
		return false, err
	}
	if hasBody {
		return pctx.SetBody(body), nil
	}
	return true, nil
}

// failed records a script error. The request goes on unless the slot fails closed
// and the script enforces: a shadow script must never block anything.
func (s *Script) failed(pctx *pipeline.Context, l *loaded, err error, logs []string) (pipeline.Action, bool) {
	s.m.errors.Add(1)
	slog.Warn("script plugin: script failed", "script", l.name, "error", err)
	inv := pipeline.Invocation{
		Plugin:  l.name,
		Action:  pipeline.ActionObserve,
		Reason:  "script_error",
		Details: withLogs(map[string]string{"error": err.Error()}, logs),
		Shadow:  !l.hs.Enforce || pctx.Observing(),
	}
	if s.cfg.FailClosed && !inv.Shadow {
		inv.Action = pipeline.ActionDeny
		pctx.Record(inv)
		v := &pipeline.Violation{Code: "plugin.script-failed", Reason: "script " + l.name + " failed", Status: 502, PluginName: l.name}
		return pipeline.Action{Type: pipeline.Reject, Violation: v}, true
	}
	pctx.Record(inv)
	return pipeline.Action{}, false
}

// notStarted refuses a request in a fail_closed slot while one of its scripts did
// not start. The slot cannot tell which requests that script would have acted on,
// so it refuses them all: fail_closed means a request the script never saw does
// not go through. Under on_error: observe the refusal is recorded as shadow, as
// failed records one.
func (s *Script) notStarted(pctx *pipeline.Context) pipeline.Action {
	name := s.missing[0]
	inv := pipeline.Invocation{
		Plugin:  name,
		Action:  pipeline.ActionObserve,
		Reason:  "script_not_started",
		Details: map[string]string{"error": "did not start; see the Cortex log", "not_started": strings.Join(s.missing, ",")},
		Shadow:  pctx.Observing(),
	}
	if inv.Shadow {
		pctx.Record(inv)
		return pipeline.Action{}
	}
	inv.Action = pipeline.ActionDeny
	pctx.Record(inv)
	v := &pipeline.Violation{Code: "plugin.script-failed", Reason: "script " + name + " did not start", Status: 502, PluginName: name}
	return pipeline.Action{Type: pipeline.Reject, Violation: v}
}

// denyStatus is a script's deny status when it is an HTTP error, and otherwise 0,
// which lets the violation's code choose one. A 200 would present the refusal to
// the agent as an answer, and a status outside 100-999 panics net/http.
func denyStatus(status int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return 0
}

func validAction(a pipeline.InvocationAction) bool {
	return slices.Contains([]pipeline.InvocationAction{
		pipeline.ActionAllow, pipeline.ActionDeny, pipeline.ActionSkip, pipeline.ActionModify, pipeline.ActionObserve,
	}, a)
}

func withDetail(d map[string]string, k, v string) map[string]string {
	if d == nil {
		d = map[string]string{}
	}
	d[k] = v
	return d
}

// withLogs puts what the script printed on its row, where the user reads it.
func withLogs(d map[string]string, logs []string) map[string]string {
	if len(logs) == 0 {
		return d
	}
	return withDetail(d, "output", strings.Join(logs, "\n"))
}

// Metrics reports the slot's counters on /v1/pipeline.
func (s *Script) Metrics() []pipeline.Metric {
	return []pipeline.Metric{
		{Name: "scripts loaded", Value: float64(len(s.loaded))},
		{Name: "scripts not started", Value: float64(len(s.missing)), Note: "a fail_closed slot refuses every request while this is not 0"},
		{Name: "calls", Value: float64(s.m.calls.Load())},
		{Name: "errors", Value: float64(s.m.errors.Load())},
		{Name: "denies", Value: float64(s.m.denies.Load())},
		{Name: "edits", Value: float64(s.m.edits.Load())},
		{Name: "shadow decisions", Value: float64(s.m.shadows.Load()), Note: "denies and edits not applied"},
	}
}
