package pipeline

import (
	"strings"
	"testing"
)

// A plugin that reads only responses is ordered against response mutators alone: a
// request mutator before it changes nothing it reads, and a response mutator before
// it runs after it on the reverse response pass, so it would see the upstream's bytes.
func TestNew_ResponseReaderIsOrderedAgainstResponseMutatorsOnly(t *testing.T) {
	reader := &stubPlugin{name: "watch", caps: PluginCapabilities{ReadsResponseBody: true}}
	reqWriter := &stubPlugin{name: "prune", caps: PluginCapabilities{WritesRequestBody: true}}
	respWriter := &stubPlugin{name: "rewrite", caps: PluginCapabilities{WritesResponseBody: true}}
	both := &stubPlugin{name: "both", caps: PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true}}

	for name, chain := range map[string][]Plugin{
		"after a request mutator":   {reqWriter, reader},
		"before a response mutator": {reader, respWriter},
		"before a mutator of both":  {reader, both},
	} {
		if _, err := New(chain); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, chain := range map[string][]Plugin{
		"after a response mutator":            {respWriter, reader},
		"after a mutator of both":             {both, reader},
		"after a request then a response one": {reqWriter, respWriter, reader},
	} {
		_, err := New(chain)
		if err == nil || !strings.Contains(err.Error(), `plugin "watch" reads body after mutator`) {
			t.Errorf("%s: err = %v, want the response reader refused", name, err)
		}
	}
	// ReadsBody is undirected, so it stays refused after a request mutator.
	if _, err := New([]Plugin{reqWriter, &stubPlugin{name: "parser", caps: PluginCapabilities{ReadsBody: true}}}); err == nil {
		t.Error("New accepted a ReadsBody plugin after a request mutator")
	}
}

// A plugin that writes requests but only reads responses is a response reader
// too — the local script slot declares exactly that pair. It may follow a
// request mutator, which changes nothing it reads, but not a response mutator,
// which runs after it on the reverse response pass.
func TestNew_RequestWritingResponseReaderIsOrdered(t *testing.T) {
	mixed := &stubPlugin{name: "slot", caps: PluginCapabilities{WritesRequestBody: true, ReadsResponseBody: true}}
	hooked := &stubPlugin{name: "hooks", caps: PluginCapabilities{ReadsBody: true, WritesRequestBody: true, ReadsResponseBody: true}}
	rewriter := &stubPlugin{name: "rewriter", caps: PluginCapabilities{WritesRequestBody: true, ReadsResponseBody: true, WritesResponseBody: true}}
	reqWriter := &stubPlugin{name: "prune", caps: PluginCapabilities{WritesRequestBody: true}}
	respWriter := &stubPlugin{name: "rewrite", caps: PluginCapabilities{WritesResponseBody: true}}

	for name, chain := range map[string][]Plugin{
		"after a request mutator":       {reqWriter, mixed},
		"a both-hooks reader after one": {reqWriter, hooked},
		"before a response mutator":     {mixed, respWriter},
		"a response rewriter after one": {respWriter, rewriter},
	} {
		if _, err := New(chain); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, tc := range []struct {
		name  string
		chain []Plugin
		want  string
	}{
		{"after a response mutator", []Plugin{respWriter, mixed}, `plugin "slot" reads body after mutator "rewrite"`},
		{"after a request and a response one", []Plugin{reqWriter, respWriter, mixed}, `plugin "slot" reads body after mutator "rewrite"`},
		{"a both-hooks reader after one", []Plugin{respWriter, hooked}, `plugin "hooks" reads body after mutator "rewrite"`},
	} {
		_, err := New(tc.chain)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestPipeline_ResponseReaderBuffersOnlyTheResponse(t *testing.T) {
	p, err := New([]Plugin{&stubPlugin{name: "watch", caps: PluginCapabilities{ReadsResponseBody: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if p.NeedsRequestBody() || !p.NeedsResponseBody() || !p.NeedsBody() {
		t.Errorf("NeedsRequestBody %v NeedsResponseBody %v NeedsBody %v; want false true true",
			p.NeedsRequestBody(), p.NeedsResponseBody(), p.NeedsBody())
	}
	if p.WritesRequestBody() || p.WritesResponseBody() {
		t.Error("a response reader reports a write")
	}
}

func TestWithinCeiling_ResponseReader(t *testing.T) {
	caps := PluginCapabilities{ReadsResponseBody: true}
	if got := caps.WithinCeiling(PluginCapabilities{ReadsBody: true}); got != "" {
		t.Errorf("under a ReadsBody ceiling: %q, want none", got)
	}
	if got := caps.WithinCeiling(PluginCapabilities{WritesResponseBody: true}); got != "" {
		t.Errorf("under a response-writer ceiling: %q, want none", got)
	}
	if got := caps.WithinCeiling(PluginCapabilities{WritesDestination: true}); got != "ReadsResponseBody" {
		t.Errorf("under a ceiling that reads no body: %q, want ReadsResponseBody", got)
	}
}
