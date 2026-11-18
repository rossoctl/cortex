package plugins

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/pipeline"
)

// accountingPlugin records the Host it was handed and whether it had it at Configure.
type accountingPlugin struct {
	host           *accounting.Host
	hadAtConfigure bool
}

func (p *accountingPlugin) Name() string { return "test-accounting" }
func (p *accountingPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Description: "test"}
}
func (p *accountingPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *accountingPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *accountingPlugin) SetAccounting(h *accounting.Host) { p.host = h }
func (p *accountingPlugin) Configure(json.RawMessage) error {
	p.hadAtConfigure = p.host != nil
	return nil
}

func TestBuildWithDeps_Accounting(t *testing.T) {
	p := &accountingPlugin{}
	RegisterPlugin("test-accounting", func() pipeline.Plugin { return p })
	t.Cleanup(func() { UnregisterPlugin("test-accounting") })
	h, err := accounting.New(accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	with, err := BuildWithDeps(entriesFor("test-accounting"), Deps{Accounting: h})
	if err != nil {
		t.Fatal(err)
	}
	if p.host != h || !p.hadAtConfigure {
		t.Errorf("SetAccounting: host %p (want %p), before Configure %v", p.host, h, p.hadAtConfigure)
	}
	if !with.NeedsRequestBody() {
		t.Error("a build with an accountant does not buffer the request body: WithAccountant was not passed")
	}

	*p = accountingPlugin{}
	without, err := BuildWithDeps(entriesFor("test-accounting"), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if p.host != nil {
		t.Error("SetAccounting was called with no Host")
	}
	if without.NeedsRequestBody() {
		t.Error("a build with no accountant buffers the request body")
	}
}
