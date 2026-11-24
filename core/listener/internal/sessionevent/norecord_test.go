package sessionevent

import (
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
)

// A request that names its own session and asks not to be recorded flags its request and
// response rows, but never a tunnel row: a bridged tunnel's open row can be filed later under the
// session its first decrypted request names, which need not be the one the CONNECT named (#1923).
func TestNoRecord_TunnelRowsCarryNoFlag(t *testing.T) {
	h := http.Header{}
	h.Set(model.RecordHeader, "off")
	pctx := &pipeline.Context{Headers: h}
	pctx.ResolveClient()
	pctx.SetAttribution("", "", "X-Claude-Code-Session-Id")
	if !pctx.WithholdsSession() {
		t.Fatal("precondition: the request withholds its own session")
	}
	if !Request(pctx, pipeline.Outbound).NoRecord || !Response(pctx, pipeline.Outbound, 200, nil).NoRecord {
		t.Fatal("request and response rows must carry NoRecord")
	}
	if TunnelOpen(pctx, "").NoRecord || TunnelClose(pctx, "", 200, nil, 0, 0).NoRecord {
		t.Fatal("a tunnel row carries NoRecord")
	}
}
