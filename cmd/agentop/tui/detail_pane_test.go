package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// tlsHeader returns "" for plaintext events so callers can prepend
// unconditionally. Locks the contract that the header is invisible
// on non-TLS connections.
func TestTLSHeader_NilProducesEmpty(t *testing.T) {
	if got := tlsHeader(nil); got != "" {
		t.Errorf("tlsHeader(nil) = %q, want empty", got)
	}
}

// Full TLS state — the header includes version, cipher, and peer.
func TestTLSHeader_FullState(t *testing.T) {
	got := tlsHeader(&pipeline.EventTLS{
		Version:      "TLS 1.3",
		CipherSuite:  "TLS_AES_128_GCM_SHA256",
		PeerSPIFFEID: "spiffe://rossoctl.local/ns/team1/sa/caller-agent",
	})
	for _, want := range []string{
		"TLS:",
		"version: TLS 1.3",
		"cipher: TLS_AES_128_GCM_SHA256",
		"peer:    spiffe://rossoctl.local/ns/team1/sa/caller-agent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tlsHeader missing %q\ngot:\n%s", want, got)
		}
	}
}

// Partial state — version+cipher only, no peer (e.g. peer cert had
// no SPIFFE URI). The peer line should be absent rather than showing
// "peer: " with an empty value.
func TestTLSHeader_NoPeerSPIFFEID(t *testing.T) {
	got := tlsHeader(&pipeline.EventTLS{
		Version:     "TLS 1.3",
		CipherSuite: "TLS_AES_128_GCM_SHA256",
	})
	if strings.Contains(got, "peer:") {
		t.Errorf("tlsHeader unexpectedly included peer line on no-SPIFFE cert\ngot:\n%s", got)
	}
	if !strings.Contains(got, "version: TLS 1.3") {
		t.Errorf("tlsHeader missing version on partial state\ngot:\n%s", got)
	}
}

// Empty version + cipher but with peer — the version/cipher line
// is omitted, but peer still shows. Tolerates partial wire data
// gracefully.
func TestTLSHeader_PeerOnly(t *testing.T) {
	got := tlsHeader(&pipeline.EventTLS{
		PeerSPIFFEID: "spiffe://test/example",
	})
	if !strings.Contains(got, "spiffe://test/example") {
		t.Errorf("tlsHeader missing peer\ngot:\n%s", got)
	}
	if strings.Contains(got, "version:") || strings.Contains(got, "cipher:") {
		t.Errorf("tlsHeader unexpectedly included version/cipher on peer-only state\ngot:\n%s", got)
	}
}

// tunnelHeader summarizes the folded CONNECT tunnel on a TLS-bridged row:
// the bridged origin (host:port) and any gate invocation that ran on the
// tunnel-open before the bytes were decrypted.
func TestTunnelHeader_OriginAndInvocations(t *testing.T) {
	got := tunnelHeader(&pipeline.SessionEvent{
		Host: "api.anthropic.com:443",
		Invocations: &pipeline.Invocations{
			Outbound: []pipeline.Invocation{
				{Plugin: "jwt-validation", Action: pipeline.ActionSkip, Reason: "no_inbound_identity"},
			},
		},
	})
	for _, want := range []string{
		"CONNECT api.anthropic.com:443",
		"TLS bridge",
		"jwt-validation skip",
		"no_inbound_identity",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tunnelHeader missing %q\ngot:\n%s", want, got)
		}
	}
}

// A tunnel with no gate invocations (pure passthrough that was then bridged)
// still names the origin without trailing invocation lines.
func TestTunnelHeader_NoInvocations(t *testing.T) {
	got := tunnelHeader(&pipeline.SessionEvent{Host: "example.com:8443"})
	if !strings.Contains(got, "CONNECT example.com:8443") {
		t.Errorf("tunnelHeader missing origin\ngot:\n%s", got)
	}
	if strings.Count(got, "\n") != 0 {
		t.Errorf("tunnelHeader with no invocations should be one line\ngot:\n%s", got)
	}
}

// The proxy writes the cost record under both the current and the legacy key, so an agentop
// older than the rename keeps showing cost. This agentop reads the current one, so the detail
// view must show ONE record — two identical objects under two names give an operator no way
// to tell which is authoritative.
func TestFilterForDetail_ShowsOneCostRecord(t *testing.T) {
	record := `{"cost_usd":0.25,"settled":true}`
	wire := []byte(`{"phase":"response","plugins":{` +
		`"` + event.Key + `":` + record + `,` +
		`"` + event.PluginName + `":` + record + `,` +
		`"tool-prune":{"bytesRemoved":900}}}`)

	var got map[string]any
	if err := json.Unmarshal(filterForDetail(wire, pipeline.SessionResponse), &got); err != nil {
		t.Fatal(err)
	}
	pl, ok := got["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("no plugins block survived: %v", got)
	}
	if _, ok := pl[event.Key]; !ok {
		t.Errorf("the current cost key was dropped: %v", pl)
	}
	if _, ok := pl[event.PluginName]; ok {
		t.Errorf("the legacy duplicate is still rendered: %v", pl)
	}
	// Unrelated plugin events are untouched — this filters a duplicate, not a category.
	if _, ok := pl["tool-prune"]; !ok {
		t.Errorf("an unrelated plugin event was dropped: %v", pl)
	}
}

// Tool results are request-side, like messages: shown on the request row, which is where an
// operator looks for what a tool returned, and hidden on the response row, which carries the
// same array by reference.
func TestFilterForDetail_ToolResultsOnTheRequestRowOnly(t *testing.T) {
	wire := []byte(`{"inference":{"model":"m","toolResults":[{"toolUseId":"toolu_1","content":"Exit code 1","isError":true}]}}`)
	for _, c := range []struct {
		phase pipeline.SessionPhase
		want  bool
	}{{pipeline.SessionRequest, true}, {pipeline.SessionResponse, false}} {
		var got map[string]any
		if err := json.Unmarshal(filterForDetail(wire, c.phase), &got); err != nil {
			t.Fatal(err)
		}
		inf, _ := got["inference"].(map[string]any)
		if _, ok := inf["toolResults"]; ok != c.want {
			t.Errorf("phase %s: toolResults shown = %v, want %v (%v)", c.phase, ok, c.want, inf)
		}
	}
}

// A proxy older than the rename writes ONLY the legacy key, and its cost must still render.
func TestFilterForDetail_KeepsALoneLegacyRecord(t *testing.T) {
	wire := []byte(`{"phase":"response","plugins":{"` + event.PluginName + `":{"cost_usd":0.25}}}`)
	var got map[string]any
	if err := json.Unmarshal(filterForDetail(wire, pipeline.SessionResponse), &got); err != nil {
		t.Fatal(err)
	}
	pl, _ := got["plugins"].(map[string]any)
	if _, ok := pl[event.PluginName]; !ok {
		t.Errorf("a lone legacy record was dropped, so an older proxy shows no cost: %v", pl)
	}
}

func TestRedirectHeader_EmptyWithoutARedirect(t *testing.T) {
	if got := redirectHeader(&pipeline.SessionEvent{Host: "ete-litellm.example.com"}); got != "" {
		t.Errorf("redirectHeader = %q for an event nothing redirected, want empty", got)
	}
}

func TestRedirectHeader_NamesBothHosts(t *testing.T) {
	got := redirectHeader(&pipeline.SessionEvent{
		Host:          "glm-litellm.example.com",
		RequestedHost: "ete-litellm.example.com",
	})
	want := "redirected:  ete-litellm.example.com → glm-litellm.example.com"
	if got != want {
		t.Errorf("redirectHeader = %q, want %q", got, want)
	}
}

func TestModelHeader_EmptyUnlessAPluginChangedTheModel(t *testing.T) {
	for _, e := range []*pipeline.SessionEvent{
		{Host: "glm-litellm.example.com"},
		{Inference: &pipeline.InferenceExtension{Model: "claude-opus-5-5"}},
	} {
		if got := modelHeader(e); got != "" {
			t.Errorf("modelHeader = %q for %+v, want empty", got, e)
		}
	}
}

func TestModelHeader_NamesBothModels(t *testing.T) {
	got := modelHeader(&pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{
		Model: "glm-5.3", RequestedModel: "claude-opus-5-5",
	}})
	if want := "model:       claude-opus-5-5 → glm-5.3"; got != want {
		t.Errorf("modelHeader = %q, want %q", got, want)
	}
}

// Both headers print what the client sent — the model it asked for, the host it
// named — and what the upstream echoed, so a control character in either must not
// reach the terminal: an escape sequence there could recolour or rewrite the pane.
// Each becomes U+FFFD, as everywhere else agentop prints a caller-supplied label.
func TestRewriteHeader_NeutralisesControlCharacters(t *testing.T) {
	got := rewriteHeader(&pipeline.SessionEvent{
		Host:          "glm\x1b[2Jlitellm.example.com",
		RequestedHost: "ete\x07litellm.example.com",
		Inference:     &pipeline.InferenceExtension{Model: "glm\u202e5.3", RequestedModel: "claude\x1b]0;x\x07opus"},
	})
	want := "redirected:  ete\uFFFDlitellm.example.com → glm\uFFFD[2Jlitellm.example.com\n" +
		"model:       claude\uFFFD]0;x\uFFFDopus → glm\uFFFD5.3"
	if got != want {
		t.Errorf("rewriteHeader = %q, want %q", got, want)
	}
}

// What a router changed is one block: where the request went, then what it was
// sent for, each line only when it applies.
func TestRewriteHeader_StacksTheRedirectAndTheModel(t *testing.T) {
	e := &pipeline.SessionEvent{
		Host:          "glm-litellm.example.com",
		RequestedHost: "ete-litellm.example.com",
		Inference:     &pipeline.InferenceExtension{Model: "glm-5.3", RequestedModel: "claude-opus-5-5"},
	}
	want := "redirected:  ete-litellm.example.com → glm-litellm.example.com\n" +
		"model:       claude-opus-5-5 → glm-5.3"
	if got := rewriteHeader(e); got != want {
		t.Errorf("rewriteHeader = %q, want %q", got, want)
	}
	e.RequestedHost = ""
	if got := rewriteHeader(e); got != "model:       claude-opus-5-5 → glm-5.3" {
		t.Errorf("rewriteHeader with no redirect = %q, want the model line alone", got)
	}
	if got := rewriteHeader(&pipeline.SessionEvent{Host: "x"}); got != "" {
		t.Errorf("rewriteHeader = %q for an event nothing changed, want empty", got)
	}
}

// The JSON below the header keeps requestedModel on both phases, beside model.
func TestFilterForDetail_KeepsRequestedModel(t *testing.T) {
	wire := []byte(`{"inference":{"model":"glm-5.3","requestedModel":"claude-opus-5-5","completion":"hi"}}`)
	for _, phase := range []pipeline.SessionPhase{pipeline.SessionRequest, pipeline.SessionResponse} {
		var got map[string]any
		if err := json.Unmarshal(filterForDetail(wire, phase), &got); err != nil {
			t.Fatal(err)
		}
		inf, _ := got["inference"].(map[string]any)
		if inf["requestedModel"] != "claude-opus-5-5" {
			t.Errorf("phase %s: inference = %v, want requestedModel kept", phase, inf)
		}
	}
}

// The upgrade line is absent on every request that asked for no upgrade — nearly
// all of them — and says what happens next when one did, since "why did my agent's
// WebSocket fail?" is the question it exists to answer.
func TestUpgradeHeader(t *testing.T) {
	for name, e := range map[string]*pipeline.SessionEvent{
		"no upgrade":       {Host: "api.anthropic.com"},
		"empty value only": {Host: "api.anthropic.com", UpgradeStripped: ""},
	} {
		if got := upgradeHeader(e); got != "" {
			t.Errorf("%s: upgradeHeader = %q, want empty", name, got)
		}
	}

	got := upgradeHeader(&pipeline.SessionEvent{
		Host:            "chatgpt.com",
		UpgradeStripped: "websocket",
	})
	if !strings.Contains(got, "websocket") {
		t.Errorf("upgradeHeader = %q, want it to name the protocol", got)
	}
	if !strings.Contains(got, "not forwarded") || !strings.Contains(got, "HTTPS") {
		t.Errorf("upgradeHeader = %q, want it to say the upgrade was not forwarded and the client retries over HTTPS", got)
	}
	// The request row cannot tell a 405 from the origin from the proxy's own 426, so
	// the line must not name either status: one of them would be false.
	if strings.Contains(got, "405") || strings.Contains(got, "426") {
		t.Errorf("upgradeHeader = %q, names a status the request row cannot know", got)
	}
}

// The value reaches a terminal from an unauthenticated API, so it passes
// sanitizeLabel like every other caller-supplied label agentop prints — belt and
// braces over the sanitise at record time.
func TestUpgradeHeader_SanitisesTheProtocol(t *testing.T) {
	got := upgradeHeader(&pipeline.SessionEvent{UpgradeStripped: "web\x1b[2Jsocket"})
	if strings.ContainsAny(got, "\x1b") {
		t.Errorf("upgradeHeader = %q, still carries an escape sequence", got)
	}
}

// And it reaches the composed block, not just its own helper — the wiring a new
// header is silently missing otherwise.
func TestRewriteHeader_IncludesTheUpgradeLine(t *testing.T) {
	got := rewriteHeader(&pipeline.SessionEvent{
		Host:            "chatgpt.com",
		UpgradeStripped: "websocket",
	})
	if !strings.Contains(got, "websocket") {
		t.Errorf("rewriteHeader = %q, want the upgrade line composed in", got)
	}
}
