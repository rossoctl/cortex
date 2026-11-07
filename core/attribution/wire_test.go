package attribution

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// every is an Attribution with every field set.
var every = Attribution{Agent: "a", Proc: "1.2", ClientProc: "3.4", Session: "s",
	Source: pipeline.SessionSourceHeader, Header: "X", ChainSession: "c"}

// What Encode writes Decode reads back exactly, for every shape Resolve returns.
func TestWire_RoundTrips(t *testing.T) {
	for _, a := range []Attribution{
		{Agent: "claude-code", Proc: "100.5", Session: "s1", Header: session.ClaudeCodeSessionHeader,
			Source: pipeline.SessionSourceHeader, ChainSession: "outer"},
		{Agent: "claude-code", Proc: "100.5", ClientProc: "140.7", Session: "s1",
			Header: session.ClaudeCodeSessionHeader, Source: pipeline.SessionSourceHeader},
		{Agent: "bob", Proc: "820.1", Source: pipeline.SessionSourcePending},
		{Source: pipeline.SessionSourcePending},
		{Proc: "700.9", Session: "s3", Source: pipeline.SessionSourceProcess},
		{Proc: "700.9", Session: session.DefaultSessionID},
		{Agent: "claude-code", Session: "s2", Source: pipeline.SessionSourceAffinity},
		every,
	} {
		h := http.Header{}
		Encode(h, a)
		got, ok := Decode(h)
		if !ok || got != a {
			t.Errorf("Decode(Encode(%+v)) = %+v, %v", a, got, ok)
		}
	}
}

// A request that carries none decodes to nothing; a malformed one is refused rather than
// half-read, so the recorder resolves the request itself.
func TestWire_DecodeRefusesWhatItCannotTrust(t *testing.T) {
	if _, ok := Decode(http.Header{}); ok {
		t.Error("an empty header decoded")
	}
	for name, h := range map[string]http.Header{
		"unknown source":      {HeaderSource: {"guess"}, HeaderSession: {"s1"}},
		"active source":       {HeaderSource: {string(pipeline.SessionSourceActive)}, HeaderSession: {"s1"}},
		"connection source":   {HeaderSource: {string(pipeline.SessionSourceConnection)}, HeaderSession: {"s1"}},
		"control in session":  {HeaderSession: {"s1\n"}},
		"oversized session":   {HeaderSession: {strings.Repeat("x", session.MaxSessionIDLen+1)}},
		"process not a key":   {HeaderProc: {"claude"}},
		"process half a key":  {HeaderProc: {"100."}},
		"client not a key":    {HeaderProc: {"1.2"}, HeaderClientProc: {"worker"}},
		"control in agent":    {HeaderAgent: {"cl\x1baude"}, HeaderProc: {"1.2"}},
		"control in named-by": {HeaderSession: {"s1"}, HeaderNamedBy: {"X-Id\r"}},
		"control in ancestor": {HeaderSession: {"s1"}, HeaderChain: {"o\x7f"}},
	} {
		if a, ok := Decode(h); ok {
			t.Errorf("%s: decoded %+v", name, a)
		}
	}
}

// A field the hop leaves empty must not keep the value a client forged for it.
func TestWire_EncodeStripsBeforeItInjects(t *testing.T) {
	h := http.Header{}
	for _, k := range wireHeaders {
		h.Set(k, "forged")
	}
	Encode(h, Attribution{Session: "s1", Source: pipeline.SessionSourceHeader})
	for _, k := range wireHeaders {
		want := ""
		switch k {
		case HeaderSession:
			want = "s1"
		case HeaderSource:
			want = string(pipeline.SessionSourceHeader)
		}
		if got := h.Values(k); len(got) > 1 || h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// Strip removes every header Encode sets, so a header added to Encode alone fails here.
func TestWire_StripCoversEveryEncodedHeader(t *testing.T) {
	h := http.Header{}
	Encode(h, every)
	if len(h) != 7 {
		t.Errorf("Encode set %d headers, want 7: %v", len(h), h)
	}
	Strip(h)
	for k := range h {
		if strings.HasPrefix(k, "X-Cortex-") {
			t.Errorf("Strip left %s", k)
		}
	}
}

// Strip removes every attribution header and nothing else.
func TestWire_StripRemovesOnlyAttribution(t *testing.T) {
	h := http.Header{}
	for _, k := range wireHeaders {
		h.Set(k, "forged")
	}
	h.Set("User-Agent", "curl/8")
	Strip(h)
	if len(h) != 1 || h.Get("User-Agent") != "curl/8" {
		t.Errorf("after Strip: %v, want only User-Agent", h)
	}
}
