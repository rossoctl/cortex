package pipeline

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// remodeler is a plugin whose OnRequest runs do; declares sets WritesRequestBody.
func remodeler(declares bool, do func(*Context)) *fnPlugin {
	return &fnPlugin{
		name:      "router",
		caps:      PluginCapabilities{WritesRequestBody: declares, Description: "test"},
		onRequest: do,
	}
}

// modelRequest is an outbound context carrying body, with the extension the
// inference parser would have built for it.
func modelRequest(body, model string) *Context {
	return &Context{
		Direction:  Outbound,
		Body:       []byte(body),
		Extensions: Extensions{Inference: &InferenceExtension{Model: model}},
	}
}

func runModel(t *testing.T, plugin Plugin, pctx *Context, opts ...Option) {
	t.Helper()
	p, err := New([]Plugin{plugin}, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.Run(context.Background(), pctx)
}

// assertNothingChanged fails unless pctx still carries body and model as the
// client sent them, with no rewrite flagged and nothing recorded.
func assertNothingChanged(t *testing.T, pctx *Context, body, model string) {
	t.Helper()
	if string(pctx.Body) != body || pctx.BodyMutated() {
		t.Errorf("Body = %q, BodyMutated = %v; want %q untouched", pctx.Body, pctx.BodyMutated(), body)
	}
	if ext := pctx.Extensions.Inference; ext != nil && (ext.Model != model || ext.RequestedModel != "") {
		t.Errorf("Model = %q, RequestedModel = %q; want %q and none", ext.Model, ext.RequestedModel, model)
	}
	if invs := outbound(pctx); len(invs) != 0 {
		t.Errorf("recorded %+v, want nothing", invs)
	}
}

// The one value changes and every other byte stays as the client sent it —
// spacing, key order, a nested "model" — because a prompt cache matches an exact
// prefix.
func TestSetRequestModel_ChangesTheModelAndNothingElse(t *testing.T) {
	const body = `{ "max_tokens": 10,  "model" : "claude-opus-5-5" , "metadata": {"model": "inner"}, "stream": true}`
	const want = `{ "max_tokens": 10,  "model" : "glm-5.3" , "metadata": {"model": "inner"}, "stream": true}`
	var err error
	pctx := modelRequest(body, "claude-opus-5-5")
	runModel(t, remodeler(true, func(c *Context) { err = c.SetRequestModel("glm-5.3") }), pctx)

	if err != nil {
		t.Fatalf("SetRequestModel: %v", err)
	}
	if string(pctx.Body) != want || !pctx.BodyMutated() {
		t.Errorf("Body = %s, BodyMutated = %v; want %s", pctx.Body, pctx.BodyMutated(), want)
	}
	ext := pctx.Extensions.Inference
	if ext.Model != "glm-5.3" || ext.RequestedModel != "claude-opus-5-5" {
		t.Errorf("Model = %q, RequestedModel = %q; want glm-5.3 and claude-opus-5-5", ext.Model, ext.RequestedModel)
	}
	invs := outbound(pctx)
	if len(invs) != 2 || invs[0].Reason != "body_rewritten" || invs[1].Action != ActionModify ||
		invs[1].Reason != "model_rewritten" || invs[1].Plugin != "router" || invs[1].Shadow ||
		invs[1].Details["from"] != "claude-opus-5-5" || invs[1].Details["to"] != "glm-5.3" {
		t.Errorf("invocations = %+v, want body_rewritten, then router modify/model_rewritten from claude-opus-5-5 to glm-5.3", invs)
	}
}

func TestSetRequestModel_RefusesABodyItCannotRewrite(t *testing.T) {
	for name, body := range map[string]string{
		"no body":           ``,
		"not JSON":          `{"model": "claude-opus-5-5"`,
		"no model":          `{"max_tokens": 10}`,
		"only a nested one": `{"metadata": {"model": "claude-opus-5-5"}}`,
		"not a string":      `{"model": 5}`,
		"not an object":     `[{"model": "claude-opus-5-5"}]`,
		"named twice":       `{"model": "claude-opus-5-5", "model": "claude-haiku-4-5"}`,
		// encoding/json matches a key in any letter case and keeps the last, so the
		// parser and a Go server read claude-haiku-4-5 while sjson changes the first.
		"named twice, differing in case": `{"model": "claude-opus-5-5", "Model": "claude-haiku-4-5"}`,
		// "\x5c" is a backslash: the second key spells its m as a JSON escape, which
		// gjson and encoding/json both decode to "model".
		"named twice, once escaped": "{\"model\": \"claude-opus-5-5\", \"\x5cu006dodel\": \"claude-haiku-4-5\"}",
		// RequestedModel cannot hold an empty name, so a second change would record
		// the first plugin's choice as the client's.
		"an empty model": `{"model": ""}`,
		// sjson finds no "model" here and would append one, making a body that names
		// it twice in different cases.
		"only a different case": `{"Model": "claude-opus-5-5"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			pctx := modelRequest(body, "claude-opus-5-5")
			runModel(t, remodeler(true, func(c *Context) { err = c.SetRequestModel("glm-5.3") }), pctx)
			if err == nil {
				t.Fatal("SetRequestModel accepted it")
			}
			assertNothingChanged(t, pctx, body, "claude-opus-5-5")
		})
	}
}

// RequestModel reads exactly what SetRequestModel would replace, and refuses what
// it would refuse, so a plugin deciding on the model and the rewrite never disagree.
func TestRequestModel_ReadsWhatSetRequestModelWouldReplace(t *testing.T) {
	for body, want := range map[string]string{
		`{"max_tokens": 10, "model": "claude-sonnet-5"}`:             "claude-sonnet-5",
		`{"model": "claude-opus-5-5", "model": "x"}`:                 "",
		`{"metadata": {"model": "inner"}}`:                           "",
		`{"model": "claude-opus-5-5"`:                                "",
		`{"model": "claude-opus-5-5", "Model": "x"}`:                 "",
		"{\"model\": \"claude-opus-5-5\", \"\x5cu006dodel\": \"x\"}": "",
		`{"model": ""}`:                "",
		`{"Model": "claude-opus-5-5"}`: "",
	} {
		got, ok := (&Context{Body: []byte(body)}).RequestModel()
		if got != want || ok != (want != "") {
			t.Errorf("RequestModel(%s) = %q, %v; want %q, %v", body, got, ok, want, want != "")
		}
	}
}

func TestSetRequestModel_RefusedWithoutWritesRequestBody(t *testing.T) {
	const body = `{"model": "claude-opus-5-5"}`
	var err error
	pctx := modelRequest(body, "claude-opus-5-5")
	runModel(t, remodeler(false, func(c *Context) { err = c.SetRequestModel("glm-5.3") }), pctx)
	if err == nil || !strings.Contains(err.Error(), "WritesRequestBody") {
		t.Fatalf("SetRequestModel from an undeclared plugin = %v, want a refusal naming WritesRequestBody", err)
	}
	assertNothingChanged(t, pctx, body, "claude-opus-5-5")
}

// Under observe nothing changes — not the body, not the record the parser built —
// and both rows are shadows, so plugin code looks the same either way.
func TestSetRequestModel_UnderObserveChangesNothing(t *testing.T) {
	const body = `{"model": "claude-opus-5-5"}`
	var err error
	pctx := modelRequest(body, "claude-opus-5-5")
	runModel(t, remodeler(true, func(c *Context) { err = c.SetRequestModel("glm-5.3") }), pctx,
		WithPolicies(ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("SetRequestModel under observe: %v; plugin code must not see a difference", err)
	}
	if string(pctx.Body) != body || pctx.BodyMutated() {
		t.Errorf("Body = %q, BodyMutated = %v; want it untouched", pctx.Body, pctx.BodyMutated())
	}
	if ext := pctx.Extensions.Inference; ext.Model != "claude-opus-5-5" || ext.RequestedModel != "" {
		t.Errorf("Model = %q, RequestedModel = %q; want the parser's record untouched", ext.Model, ext.RequestedModel)
	}
	invs := outbound(pctx)
	if len(invs) != 2 || !invs[0].Shadow || !invs[1].Shadow || invs[1].Reason != "model_rewritten" {
		t.Errorf("invocations = %+v, want shadow body_rewritten and shadow model_rewritten", invs)
	}
}

// Asking for the model the request already names changes nothing and records
// nothing: there is no model change to show.
func TestSetRequestModel_TheSameModelIsANoOp(t *testing.T) {
	const body = `{"model": "claude-opus-5-5"}`
	var err error
	pctx := modelRequest(body, "claude-opus-5-5")
	runModel(t, remodeler(true, func(c *Context) { err = c.SetRequestModel("claude-opus-5-5") }), pctx)
	if err != nil {
		t.Fatalf("SetRequestModel: %v", err)
	}
	assertNothingChanged(t, pctx, body, "claude-opus-5-5")
}

// RequestedModel is the client's name however many changes follow, and empty
// again when the last one returns to it.
func TestSetRequestModel_RequestedModelKeepsTheClientsName(t *testing.T) {
	for _, tc := range []struct {
		name          string
		calls         []string
		model, wanted string
	}{
		{"two changes", []string{"glm-5.3", "glm-5.3-air"}, "glm-5.3-air", "claude-opus-5-5"},
		{"and back", []string{"glm-5.3", "claude-opus-5-5"}, "claude-opus-5-5", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := modelRequest(`{"model": "claude-opus-5-5"}`, "claude-opus-5-5")
			runModel(t, remodeler(true, func(c *Context) {
				for _, m := range tc.calls {
					if err := c.SetRequestModel(m); err != nil {
						t.Errorf("SetRequestModel(%q): %v", m, err)
					}
				}
			}), pctx)
			if ext := pctx.Extensions.Inference; ext.Model != tc.model || ext.RequestedModel != tc.wanted {
				t.Errorf("Model = %q, RequestedModel = %q; want %q and %q", ext.Model, ext.RequestedModel, tc.model, tc.wanted)
			}
		})
	}
}

// With no parser in the chain there is no inference record to update, and the
// helper does not invent one: the parser leaves it nil on purpose for a request it
// could not read.
func TestSetRequestModel_WithoutAParserRewritesTheBodyOnly(t *testing.T) {
	var err error
	pctx := &Context{Direction: Outbound, Body: []byte(`{"model": "claude-opus-5-5"}`)}
	runModel(t, remodeler(true, func(c *Context) { err = c.SetRequestModel("glm-5.3") }), pctx)
	if err != nil {
		t.Fatalf("SetRequestModel: %v", err)
	}
	if string(pctx.Body) != `{"model": "glm-5.3"}` || pctx.Extensions.Inference != nil {
		t.Errorf("Body = %s, Inference = %+v; want the body rewritten and no extension", pctx.Body, pctx.Extensions.Inference)
	}
}

func TestSetRequestModel_RefusedOutsideOnRequest(t *testing.T) {
	const body = `{"model": "claude-opus-5-5"}`
	t.Run("response pass", func(t *testing.T) {
		var err error
		plug := remodeler(true, nil)
		plug.onResponse = func(c *Context) { err = c.SetRequestModel("glm-5.3") }
		pctx := modelRequest(body, "claude-opus-5-5")
		p, _ := New([]Plugin{plug})
		p.Run(context.Background(), pctx)
		p.RunResponse(context.Background(), pctx)
		if err == nil || !strings.Contains(err.Error(), "OnRequest") {
			t.Fatalf("SetRequestModel from OnResponse = %v, want a refusal", err)
		}
		assertNothingChanged(t, pctx, body, "claude-opus-5-5")
	})
	t.Run("OnFinish", func(t *testing.T) {
		var err error
		plug := &finishPlugin{
			fnPlugin: *remodeler(true, nil),
			onFinish: func(c *Context) { err = c.SetRequestModel("glm-5.3") },
		}
		pctx := modelRequest(body, "claude-opus-5-5")
		p, _ := New([]Plugin{plug})
		p.Run(context.Background(), pctx)
		p.RunFinish(context.Background(), pctx, Outcome{})
		if err == nil || !strings.Contains(err.Error(), "OnFinish") {
			t.Fatalf("SetRequestModel from OnFinish = %v, want a refusal", err)
		}
		assertNothingChanged(t, pctx, body, "claude-opus-5-5")
	})
}

// TestRequestModelDoesNotPinTheBody is the memory regression for the way
// requestModel reads the name: gjson parses a copy of the whole body, and its
// Result.String() is a substring of that copy, so a stored model name pinned
// the entire body — one full request copy per inference request, invisible to
// the interner, measured at 95MB on a live proxy. The name must come back
// standalone.
func TestRequestModelDoesNotPinTheBody(t *testing.T) {
	const padMiB = 4
	pad := strings.Repeat("x", padMiB<<20)
	var names []string
	for range 10 {
		name, err := requestModel([]byte(`{"model":"claude-opus-5-5","pad":"` + pad + `"}`))
		if err != nil {
			t.Fatalf("requestModel: %v", err)
		}
		names = append(names, name)
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// 10 bodies x 4MiB = 40MiB pinned under the bug; live heap must stay well
	// under that even with noise from the rest of the package's tests.
	if got := ms.HeapAlloc; got > 20<<20 {
		t.Errorf("heap = %dMiB after keeping 10 model names from %dMiB bodies; the names pin the parsed copies", got>>20, padMiB)
	}
	if names[0] != "claude-opus-5-5" {
		t.Errorf("name = %q", names[0])
	}
}

// TestRequestModelDoesNotCopyTheBody is the allocation regression for the same
// scan: ParseBytes is Parse(string(body)), so reading the top-level keys used to
// copy the whole body once per routed request — ~1GB/hour of churn on a live
// proxy, third behind tool-prune's copies. The scan now runs over an alias of
// the body, so one call on a 4MiB body must allocate well under the body's size.
func TestRequestModelDoesNotCopyTheBody(t *testing.T) {
	pad := strings.Repeat("x", 4<<20)
	body := []byte(`{"model":"claude-opus-5-5","pad":"` + pad + `"}`)
	if _, err := requestModel(body); err != nil { // warm: page in gjson's code
		t.Fatalf("requestModel: %v", err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := requestModel(body); err != nil {
		t.Fatalf("requestModel: %v", err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 64<<10 {
		t.Errorf("requestModel on a %dMiB body allocated %dKiB; the scan copies the body", len(body)>>20, got>>10)
	}
}
