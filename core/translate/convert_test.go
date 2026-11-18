package translate

import (
	"net/http"
	"testing"
)

const historyRequest = `{"model":"claude-x","max_tokens":10,"stream":true,"system":"be brief",` +
	`"messages":[{"role":"user","content":"hi"}]}`

// TestRequest_RemovesSourceHeaders checks rule R3: the source protocol
// removes its own headers, and other headers stay.
func TestRequest_RemovesSourceHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Version", "2023-06-01")
	h.Set("Anthropic-Beta", "x")
	h.Set("X-Keep", "1")
	if _, err := Request(Anthropic, Chat, []byte(historyRequest), h); err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h.Get("X-Keep") != "1" {
		t.Errorf("headers after Request = %v, want only X-Keep", h)
	}
}

// TestRequest_ReqHasNoHistory checks decision 5: Converted.Req keeps what
// the reply encoder needs, and not the conversation.
func TestRequest_ReqHasNoHistory(t *testing.T) {
	c, err := Request(Anthropic, Chat, []byte(historyRequest), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Req.Model != "claude-x" {
		t.Errorf("Req.Model = %q, want claude-x", c.Req.Model)
	}
	if c.Req.Messages != nil || c.Req.System != nil {
		t.Errorf("Req keeps history: system %v, messages %v", c.Req.System, c.Req.Messages)
	}
	if !c.Stream || c.Req.Stream != c.Stream {
		t.Errorf("Req.Stream = %v, Stream = %v, want both true", c.Req.Stream, c.Stream)
	}
}
