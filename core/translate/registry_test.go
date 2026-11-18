package translate

import (
	"reflect"
	"testing"
)

func TestNames(t *testing.T) {
	want := []string{"anthropic-messages", "chat-completions", "responses"}
	if got := Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	for _, n := range want {
		p, ok := Lookup(n)
		if !ok || p.Name() != n {
			t.Errorf("Lookup(%q) = %v, %v", n, p, ok)
		}
	}
	if _, ok := Lookup("gemini"); ok {
		t.Error(`Lookup("gemini") found a protocol`)
	}
}

func TestTarget(t *testing.T) {
	for _, tc := range []struct {
		served []string
		want   string
		ok     bool
	}{
		{[]string{"responses"}, "", false},
		{[]string{"responses", "chat-completions"}, "chat-completions", true},
		{[]string{"anthropic-messages", "responses"}, "", false},
		{nil, "", false},
		{[]string{"unknown", "chat-completions"}, "chat-completions", true},
	} {
		got, ok := Target(tc.served)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Target(%v) = %q, %v; want %q, %v", tc.served, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDecodes(t *testing.T) {
	for name, want := range map[string]bool{Anthropic: true, Chat: false, Responses: false, "unknown": false} {
		if got := Decodes(name); got != want {
			t.Errorf("Decodes(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestErrorBody(t *testing.T) {
	for _, tc := range []struct{ protocol, want string }{
		{Anthropic, `{"type":"error","error":{"type":"api_error","message":"m"}}`},
		{Chat, `{"error":{"message":"m","type":"api_error","code":null}}`},
		{Responses, `{"error":{"message":"m","type":"api_error","code":null}}`},
		{"unknown", ``},
	} {
		if got := string(ErrorBody(tc.protocol, 502, "api_error", "m")); got != tc.want {
			t.Errorf("ErrorBody(%q) = %s, want %s", tc.protocol, got, tc.want)
		}
	}
}

func TestRequest_NoDirection(t *testing.T) {
	if _, err := Request(Chat, Anthropic, []byte(`{}`), nil); err == nil {
		t.Error("Request(chat, anthropic) gave no error; neither direction exists")
	}
}
