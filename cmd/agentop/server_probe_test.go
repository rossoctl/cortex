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
	if !maps.Equal(api, want) || auth != "authorization" {
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
	if _, _, err := runProbe(t, srv, nil, "authorization", true); err == nil {
		t.Error("a fixed header that the server refuses gave no error")
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
