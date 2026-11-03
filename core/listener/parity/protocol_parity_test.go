package parity

import (
	"fmt"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	// Registered by importing them, as cost_parity_test.go registers inference-parser.
	_ "github.com/rossoctl/cortex/core/plugins/a2aparser"
	_ "github.com/rossoctl/cortex/core/plugins/mcpparser"
)

// A protocol parse reaches the session row in both directions, on every listener (#1575).
//
// The parsers do not check the direction, and our own configs put a2a-parser on the
// outbound chain (demos/session-budget). But each listener built its rows by hand, and
// the copies kept only the extension expected in their direction: outbound rows had no
// A2A and inbound rows had no MCP. The parser's invocation was recorded, so the row
// showed "a2a-parser observe/matched_message/send" with no method, contextId or parts
// behind it.
//
// The A2A request carries no contextId. With one, an inbound row is filed under that
// session rather than "default", which is correct and which this harness does not read.
//
// The method is checked as an exact value on every leg, not only compared between
// legs. A drop shared by every listener of a direction agrees with itself, and the
// cross-direction cells below were exactly such a drop.
func TestParity_ProtocolParseReachesTheRowInBothDirections(t *testing.T) {
	cases := []struct {
		parser, path, method string
		reqBody, respBody    string
		mcp                  bool
	}{
		{
			parser: "a2a-parser", path: "/a2a", method: "message/send",
			reqBody:  `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"hello peer agent"}]}}}`,
			respBody: `{"jsonrpc":"2.0","id":1,"result":{"kind":"task","status":{"state":"completed"}}}`,
		},
		{
			parser: "mcp-parser", path: "/mcp", method: "tools/call", mcp: true,
			reqBody:  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"Paris"}}}`,
			respBody: `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"sunny"}]}}`,
		},
	}
	directions := []struct {
		dir       pipeline.Direction
		listeners []listenerRun
	}{
		{pipeline.Inbound, inboundListeners},
		{pipeline.Outbound, outboundListeners},
	}
	for _, c := range cases {
		for _, d := range directions {
			for _, phase := range []pipeline.SessionPhase{pipeline.SessionRequest, pipeline.SessionResponse} {
				f := fixture{
					name:           fmt.Sprintf("%s-%s-%s", c.parser, d.dir, phase),
					direction:      d.dir,
					entries:        []config.PluginEntry{{Name: c.parser}},
					method:         "POST",
					path:           c.path,
					reqBody:        []byte(c.reqBody),
					upstreamStatus: 200,
					upstreamBody:   []byte(c.respBody),
				}
				for _, l := range d.listeners {
					obs := l.run(t, f, phase)
					if obs == nil {
						t.Errorf("%s on %s: no %s row", f.name, l.name, phase)
						continue
					}
					got := obs.A2AMethod
					if c.mcp {
						got = obs.MCPMethod
					}
					if got != c.method {
						t.Errorf("%s on %s: the row's parse names method %q, want %q", f.name, l.name, got, c.method)
					}
				}
				assertParity(t, f, phase, d.listeners)
			}
		}
	}
}
