package main

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// Success bodies, one for each protocol.
const (
	anthropicOK = `{"type":"message","content":[{"type":"text","text":"p"}]}`
	chatOK      = `{"choices":[{"message":{"content":"p"}}]}`
	responsesOK = `{"output":[{"type":"message"}]}`
)

// probeSeen is what a fake server records of each request: path, auth headers and body.
type probeSeen struct {
	mu   sync.Mutex
	reqs []probeReq
}

type probeReq struct{ path, bearer, apiKey, body string }

func (s *probeSeen) add(r *http.Request) probeReq {
	b, _ := io.ReadAll(r.Body)
	p := probeReq{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), string(b)}
	s.mu.Lock()
	s.reqs = append(s.reqs, p)
	s.mu.Unlock()
	return p
}

// fakeProbeServer answers with handle, after it records each request.
func fakeProbeServer(t *testing.T, handle func(w http.ResponseWriter, p probeReq, r *http.Request)) (*httptest.Server, *probeSeen) {
	t.Helper()
	seen := &probeSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle(w, seen.add(r), r)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func runProbe(t *testing.T, srv *httptest.Server, headers map[string]string, header string, fixed bool) (map[string]string, string, error) {
	t.Helper()
	s := routerconfig.Server{URL: srv.URL, Key: "k", Headers: headers}
	return probeAPI(context.Background(), srv.Client(), s, "m", header, fixed)
}

func TestProbe_ServesAllThree(t *testing.T) {
	srv, seen := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, _ *http.Request) {
		switch p.path {
		case "/v1/messages":
			io.WriteString(w, anthropicOK)
		case "/v1/chat/completions":
			io.WriteString(w, chatOK)
		case "/v1/responses":
			io.WriteString(w, responsesOK)
		default:
			http.NotFound(w, nil)
		}
	})
	api, auth, err := runProbe(t, srv, nil, "authorization", false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"anthropic-messages": "/v1/messages", "chat-completions": "/v1/chat/completions", "responses": "/v1/responses"}
	if !maps.Equal(api, want) || auth != "" {
		t.Errorf("api %v, auth_header %q", api, auth)
	}
	bodies := map[string]string{}
	for _, r := range seen.reqs {
		bodies[r.path] = r.body
	}
	for path, body := range map[string]string{
		"/v1/messages":         `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`,
		"/v1/chat/completions": `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`,
		"/v1/responses":        `{"model":"m","max_output_tokens":16,"input":"ping"}`,
	} {
		if bodies[path] != body {
			t.Errorf("%s body %s, want %s", path, bodies[path], body)
		}
	}
}

func TestProbe_ChatWithoutV1(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, _ *http.Request) {
		if p.path == "/chat/completions" {
			io.WriteString(w, chatOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "404 page not found")
	})
	api, auth, err := runProbe(t, srv, nil, "authorization", false)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(api, map[string]string{"chat-completions": "/chat/completions"}) || auth != "" {
		t.Errorf("api %v, auth_header %q", api, auth)
	}
}

// Review Focus 3.
func TestProbe_ErrorBodyOn200IsNotServed(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, _ probeReq, _ *http.Request) {
		io.WriteString(w, `{"error":{"message":"bad"}}`)
	})
	_, _, err := runProbe(t, srv, nil, "authorization", false)
	if err == nil || !strings.Contains(err.Error(), "every path answered 200") ||
		!strings.Contains(err.Error(), "The model or the key is wrong, not the path.") {
		t.Errorf("err = %v", err)
	}
}

// bearerRefused refuses Bearer with 401 everywhere, and with x-api-key serves chat
// at /v1/chat/completions only.
func bearerRefused(w http.ResponseWriter, p probeReq, _ *http.Request) {
	switch {
	case p.apiKey != "k":
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid jwt string"}`)
	case p.path == "/v1/chat/completions":
		io.WriteString(w, chatOK)
	default:
		http.NotFound(w, nil)
	}
}

func TestProbe_RefusesBearerTakesAPIKey(t *testing.T) {
	srv, _ := fakeProbeServer(t, bearerRefused)
	api, auth, err := runProbe(t, srv, nil, "authorization", false)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(api, map[string]string{"chat-completions": "/v1/chat/completions"}) || auth != "x-api-key" {
		t.Errorf("api %v, auth_header %q", api, auth)
	}
}

func TestProbe_SameModelErrorEverywhere(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, _ probeReq, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"message":"model not available"}}`)
	})
	_, _, err := runProbe(t, srv, nil, "authorization", false)
	if err == nil || !strings.Contains(err.Error(), "every path answered 403") || !strings.Contains(err.Error(), "model not available") {
		t.Errorf("err = %v", err)
	}
}

func TestProbe_FixedHeaderNoRetry(t *testing.T) {
	srv, seen := fakeProbeServer(t, bearerRefused)
	if _, _, err := runProbe(t, srv, nil, "authorization", true); err == nil || !strings.Contains(err.Error(), "every path answered 401") {
		t.Errorf("a fixed header that the server refuses: err = %v", err)
	}
	for _, r := range seen.reqs {
		if r.apiKey != "" {
			t.Errorf("%s was sent with X-Api-Key", r.path)
		}
	}
}

func TestProbe_SendsTheServersHeaders(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, r *http.Request) {
		switch {
		case r.Header.Get("User-Agent") != "bob-shell":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "<html>denied</html>")
		case p.path == "/v1/chat/completions":
			io.WriteString(w, chatOK)
		default:
			http.NotFound(w, nil)
		}
	})
	api, _, err := runProbe(t, srv, map[string]string{"User-Agent": "bob-shell"}, "authorization", false)
	if err != nil {
		t.Fatal(err)
	}
	if api["chat-completions"] != "/v1/chat/completions" {
		t.Errorf("api %v", api)
	}
}

// Anthropic is served only with Bearer and chat only with x-api-key: neither is the
// protocol's usual header, and they differ, so no one auth_header fits (decision 14).
func TestProbe_DifferentHeaderPerProtocol(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, _ *http.Request) {
		switch {
		case p.path == "/v1/messages" && p.bearer == "Bearer k":
			io.WriteString(w, anthropicOK)
		case p.path == "/v1/chat/completions" && p.apiKey == "k":
			io.WriteString(w, chatOK)
		case p.path == "/v1/messages" || p.path == "/v1/chat/completions":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, nil)
		}
	})
	_, _, err := runProbe(t, srv, nil, "authorization", false)
	if err == nil || err.Error() != "the server takes a different auth header for each protocol; give --auth-header and --api" {
		t.Errorf("err = %v", err)
	}
}

// Each protocol works only with its usual header, so the headers differ: decision 14
// writes no auth_header.
func TestProbe_EachUsualButDifferent(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, _ *http.Request) {
		switch {
		case p.path == "/v1/messages" && p.apiKey == "k":
			io.WriteString(w, anthropicOK)
		case p.path == "/v1/chat/completions" && p.bearer == "Bearer k":
			io.WriteString(w, chatOK)
		case p.path == "/v1/messages" || p.path == "/v1/chat/completions":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, nil)
		}
	})
	api, auth, err := runProbe(t, srv, nil, "authorization", false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"anthropic-messages": "/v1/messages", "chat-completions": "/v1/chat/completions"}
	if !maps.Equal(api, want) || auth != "" {
		t.Errorf("api %v, auth_header %q", api, auth)
	}
}

// Anthropic works with its usual x-api-key and chat with the same x-api-key, which
// is not its usual header: the one shared header is written.
func TestProbe_UsualAndSharedHeader(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, _ *http.Request) {
		switch {
		case p.apiKey != "k":
			w.WriteHeader(http.StatusUnauthorized)
		case p.path == "/v1/messages":
			io.WriteString(w, anthropicOK)
		case p.path == "/v1/chat/completions":
			io.WriteString(w, chatOK)
		default:
			http.NotFound(w, nil)
		}
	})
	api, auth, err := runProbe(t, srv, nil, "authorization", false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"anthropic-messages": "/v1/messages", "chat-completions": "/v1/chat/completions"}
	if !maps.Equal(api, want) || auth != "x-api-key" {
		t.Errorf("api %v, auth_header %q", api, auth)
	}
}

// Different replies on different paths say the path is wrong, not the model or key.
func TestProbe_MixedRepliesNoPath(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, _ *http.Request) {
		if strings.HasPrefix(p.path, "/v1/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.NotFound(w, nil)
	})
	_, _, err := runProbe(t, srv, nil, "authorization", false)
	if err == nil || err.Error() != "no path tried served a protocol; give the protocols with --api <protocol>=<path>" {
		t.Errorf("err = %v", err)
	}
}

// A reply that repeats the key, or holds control characters, is printed without them.
func TestProbe_ReplyHidesKeyAndControls(t *testing.T) {
	const key = "sk-secret-123" // "k" is in the error's own words, so a longer key
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, _ probeReq, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "bad key "+key+"\x1b[31m\r\nred\x07")
	})
	s := routerconfig.Server{URL: srv.URL, Key: key}
	_, _, err := probeAPI(context.Background(), srv.Client(), s, "m", "authorization", true)
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	if strings.Contains(msg, key) || strings.ContainsAny(msg, "\x1b\r\n\x07") {
		t.Errorf("err = %q", msg)
	}
	if !strings.Contains(msg, "bad key <key> [31m red") {
		t.Errorf("err = %q, want the reply with <key>", msg)
	}
}

// A server that takes both headers gets each protocol's usual one, so no auth_header
// is written, whatever header the model list worked with.
func TestProbe_UsualHeaderFirst(t *testing.T) {
	srv, seen := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, _ *http.Request) {
		switch p.path {
		case "/v1/messages":
			io.WriteString(w, anthropicOK)
		case "/v1/chat/completions":
			io.WriteString(w, chatOK)
		case "/v1/responses":
			io.WriteString(w, responsesOK)
		default:
			http.NotFound(w, nil)
		}
	})
	for _, h := range []string{"authorization", "x-api-key"} {
		_, auth, err := runProbe(t, srv, nil, h, false)
		if err != nil || auth != "" {
			t.Errorf("model list with %s: auth = %q, err = %v; want no auth_header", h, auth, err)
		}
	}
	for _, r := range seen.reqs {
		if (r.path == "/v1/messages") != (r.apiKey != "") {
			t.Errorf("%s sent bearer=%q x-api-key=%q; want its usual header only", r.path, r.bearer, r.apiKey)
		}
	}
}

// A path that drops the connection is one path that did not serve; the probe goes on.
func TestProbe_DroppedPathIsNotTheEnd(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, _ *http.Request) {
		switch p.path {
		case "/v1/messages":
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		case "/messages":
			io.WriteString(w, anthropicOK)
		case "/v1/chat/completions":
			io.WriteString(w, chatOK)
		default:
			http.NotFound(w, nil)
		}
	})
	api, _, err := runProbe(t, srv, nil, "", false)
	want := map[string]string{"anthropic-messages": "/messages", "chat-completions": "/v1/chat/completions"}
	if err != nil || !maps.Equal(api, want) {
		t.Errorf("api = %v, err = %v; want %v", api, err, want)
	}
}

// hang answers only when the request is cancelled.
func hang(r *http.Request) { <-r.Context().Done() }

func TestProbe_HungProtocolLeavesTimeForTheNext(t *testing.T) {
	old := probeProtocolTime
	probeProtocolTime = 100 * time.Millisecond
	t.Cleanup(func() { probeProtocolTime = old })
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, r *http.Request) {
		switch {
		case strings.HasSuffix(p.path, "messages"):
			hang(r) // a Bob-like server: no anthropic path answers
		case p.path == "/v1/chat/completions":
			io.WriteString(w, chatOK)
		default:
			http.NotFound(w, nil)
		}
	})
	// A deadline, so the old code fails here and does not hang the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	api, _, err := probeAPI(ctx, srv.Client(), routerconfig.Server{URL: srv.URL, Key: "k"}, "m", "", false)
	want := map[string]string{"chat-completions": "/v1/chat/completions"}
	if err != nil || !maps.Equal(api, want) {
		t.Errorf("api = %v, err = %v; want %v", api, err, want)
	}
}

func TestProbe_EndedContextKeepsWhatWasFound(t *testing.T) {
	srv, _ := fakeProbeServer(t, func(w http.ResponseWriter, p probeReq, r *http.Request) {
		if p.path == "/v1/messages" {
			io.WriteString(w, anthropicOK)
			return
		}
		hang(r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	api, _, err := probeAPI(ctx, srv.Client(), routerconfig.Server{URL: srv.URL, Key: "k"}, "m", "", false)
	want := map[string]string{"anthropic-messages": "/v1/messages"}
	if err != nil || !maps.Equal(api, want) {
		t.Errorf("api = %v, err = %v; want %v", api, err, want)
	}
}
