package routerconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const twoServers = `{
	"servers": {
		"ete": {"url": "https://ete.example.com", "key": "sk-ete", "main": "m", "helper": "h"},
		"glm": {"url": "https://glm.example.com:8443", "key": "sk-glm", "main": "m", "helper": "h"}
	},
	"agents": {"claude-code": "glm"}
}`

func TestDecode_AcceptsServersAndAgents(t *testing.T) {
	c, err := Decode(json.RawMessage(twoServers))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Servers) != 2 || c.Agents["claude-code"] != "glm" {
		t.Errorf("decoded %+v", c)
	}
}

func TestDecode_NoAgentsIsAnEmptyMap(t *testing.T) {
	c, err := Decode(json.RawMessage(`{"servers": {"ete": {"url": "https://ete.example.com", "key": "k", "main": "m", "helper": "h"}}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c.Agents == nil {
		t.Error("Agents is nil; an absent agents block routes nothing, as an empty one does")
	}
}

// A default server is the feature this design leaves out on purpose; a config that
// asks for one must be told, not silently route nothing.
func TestDecode_RefusesUnknownFields(t *testing.T) {
	_, err := Decode(json.RawMessage(`{"servers": {"ete": {"url": "https://ete.example.com", "key": "k", "main": "m", "helper": "h"}}, "default": "ete"}`))
	if err == nil || !strings.Contains(err.Error(), `"default"`) {
		t.Fatalf("err = %v, want an unknown-field error naming default", err)
	}
}

func TestValidate_RefusesEachBrokenRule(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
	}{
		{"no servers", `{"servers": {}}`, "at least one server"},
		{"absent servers", `{}`, "at least one server"},
		{"uppercase name", `{"servers": {"ETE": {"url": "https://e.example", "key": "k", "main": "m", "helper": "h"}}}`, `"ETE" is not a server name`},
		{"name with a space", `{"servers": {"e te": {"url": "https://e.example", "key": "k", "main": "m", "helper": "h"}}}`, `"e te" is not a server name`},
		{"ftp", `{"servers": {"e": {"url": "ftp://e.example", "key": "k", "main": "m", "helper": "h"}}}`, "scheme must be http or https"},
		{"no scheme", `{"servers": {"e": {"url": "e.example", "key": "k", "main": "m", "helper": "h"}}}`, "scheme must be http or https"},
		{"no host", `{"servers": {"e": {"url": "https://", "key": "k", "main": "m", "helper": "h"}}}`, "has no host"},
		{"path", `{"servers": {"e": {"url": "https://e.example/v1/../x", "key": "k", "main": "m", "helper": "h"}}}`, "must be plain segments"},
		{"query", `{"servers": {"e": {"url": "https://e.example?x=1", "key": "k", "main": "m", "helper": "h"}}}`, "query or fragment"},
		{"fragment", `{"servers": {"e": {"url": "https://e.example#x", "key": "k", "main": "m", "helper": "h"}}}`, "query or fragment"},
		{"user info", `{"servers": {"e": {"url": "https://u:p@e.example", "key": "k", "main": "m", "helper": "h"}}}`, "user info"},
		{"non-numeric port", `{"servers": {"e": {"url": "https://e.example:abc", "key": "k", "main": "m", "helper": "h"}}}`, "servers.e.url: not a valid URL"},
		{"port zero", `{"servers": {"e": {"url": "https://e.example:0", "key": "k", "main": "m", "helper": "h"}}}`, "port must be a number from 1 to 65535"},
		{"port too high", `{"servers": {"e": {"url": "https://e.example:65536", "key": "k", "main": "m", "helper": "h"}}}`, "port must be a number from 1 to 65535"},
		{"shared host, port and case ignored", `{"servers": {
			"a": {"url": "https://gw.example", "key": "k", "main": "m", "helper": "h"},
			"b": {"url": "http://GW.example:4000", "key": "k", "main": "m", "helper": "h"}}}`, `"a" and "b" are both on gw.example`},
		{"empty key", `{"servers": {"e": {"url": "https://e.example", "key": "", "main": "m", "helper": "h"}}}`, "the key is empty"},
		{"key with a space", `{"servers": {"e": {"url": "https://e.example", "key": "sk 1", "main": "m", "helper": "h"}}}`, "printable ASCII with no spaces"},
		{"agent naming no server", `{"servers": {"e": {"url": "https://e.example", "key": "k", "main": "m", "helper": "h"}}, "agents": {"claude-code": "glm"}}`, `"glm" is not a server listed under servers`},
		{"agent name as a label", `{"servers": {"e": {"url": "https://e.example", "key": "k", "main": "m", "helper": "h"}}, "agents": {"Claude Code": "e"}}`, `"Claude Code" is not an agent name`},
		{"the no-User-Agent bucket", `{"servers": {"e": {"url": "https://e.example", "key": "k", "main": "m", "helper": "h"}}, "agents": {"unknown": "e"}}`, `"unknown" cannot be routed`},
		{"no main", `{"servers": {"glm": {"url": "https://e.example", "key": "k", "helper": "h"}}}`, "servers.glm.main: is empty"},
		{"no helper", `{"servers": {"glm": {"url": "https://e.example", "key": "k", "main": "m"}}}`, "servers.glm.helper: is empty"},
		{"a word with a prefix", `{"servers": {"glm": {"url": "https://e.example", "key": "k", "main": "rits/glm", "helper": "h"}}}`, "has a /"},
		{"a word with a space", `{"servers": {"glm": {"url": "https://e.example", "key": "k", "main": "m", "helper": "nemo tron"}}}`, "whitespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(json.RawMessage(tc.config))
			if err == nil {
				t.Fatal("Decode accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The key is a secret; an error about it must not print it.
func TestCheckKey_NeverRepeatsTheKey(t *testing.T) {
	err := CheckKey("sk-secret value")
	if err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("err = %v; want an error that does not contain the key", err)
	}
}

func TestParseURL_Normalises(t *testing.T) {
	for _, tc := range []struct {
		raw, url, host, hostname string
	}{
		{"https://ete.example.com", "https://ete.example.com", "ete.example.com", "ete.example.com"},
		{"https://ETE.Example.com:443/", "https://ete.example.com", "ete.example.com", "ete.example.com"},
		{"http://localhost:80", "http://localhost", "localhost", "localhost"},
		{"https://glm.example.com:8443", "https://glm.example.com:8443", "glm.example.com:8443", "glm.example.com"},
		{"https://x.example:0443", "https://x.example", "x.example", "x.example"},
		{"http://[::1]:4000", "http://[::1]:4000", "[::1]:4000", "::1"},
		{"https://[::1]:443", "https://[::1]", "[::1]", "::1"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			ep, err := ParseURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseURL: %v", err)
			}
			if ep.URL() != tc.url || ep.Host != tc.host || ep.Hostname != tc.hostname {
				t.Errorf("got URL %q Host %q Hostname %q, want %q %q %q", ep.URL(), ep.Host, ep.Hostname, tc.url, tc.host, tc.hostname)
			}
		})
	}
}

func TestHostname_StripsThePortAndCase(t *testing.T) {
	for in, want := range map[string]string{
		"ete.example.com":      "ete.example.com",
		"ETE.example.com:8443": "ete.example.com",
		"[::1]:4000":           "::1",
		"[::1]":                "::1",
	} {
		if got := Hostname(in); got != want {
			t.Errorf("Hostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaintextRemote(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://gw.example":     false,
		"http://localhost:4000":  false,
		"http://127.0.0.1:4000":  false,
		"http://[::1]:4000":      false,
		"http://10.0.0.5:4000":   true,
		"http://gateway.lan":     true,
		"http://LOCALHOST:4000/": false,
	} {
		ep, err := ParseURL(raw)
		if err != nil {
			t.Fatalf("ParseURL(%q): %v", raw, err)
		}
		if got := ep.PlaintextRemote(); got != want {
			t.Errorf("PlaintextRemote(%q) = %v, want %v", raw, got, want)
		}
	}
}

// No ParseURL error quotes any part of the URL. Errors reach the logs and the
// unauthenticated /reload/status, and a key can land anywhere in a mistyped URL —
// user info, query, fragment, a path segment, or wherever url.Parse files it when
// the "//" is wrong — so redacting shape by shape kept missing one. Each error is
// fixed text naming the problem; this table is every input that once leaked.
func TestParseURL_QuotesNoPartOfTheURL(t *testing.T) {
	const (
		invalid = "not a valid URL"
		scheme  = "the URL's scheme must be http or https"
		noHost  = "the URL has no host"
		user    = "the URL carries user info"
		segment = `the URL's path must be plain segments, with no ".", "..", empty or escaped segment`
		query   = "the URL has a query or fragment"
	)
	for _, tc := range []struct{ url, want string }{
		// User info, query and fragment.
		{"https://sk-SECRET@h.example.com", user},
		{"https://u:sk-SECRET@h.example.com", user},
		{"ftp://u:sk-SECRET@h.example.com", scheme},
		{"https://u:sk-SECRET@", noHost},
		{"https:u:sk-SECRET@h.example.com", noHost},
		{"https://h.example.com/?key=sk-SECRET", query},
		{"https://h.example.com/#sk-SECRET", query},
		{"https://h.example.com/?a=1#sk-SECRET", query},
		{"ftp://h.example.com/?key=sk-SECRET", scheme},
		// A mistyped "//" files the key under the path.
		{"https:/u:sk-SECRET@h.example.com", noHost},
		{"https:///u:sk-SECRET@h.example.com", noHost},
		{"https//u:sk-SECRET@h.example.com", scheme},
		{"sk-SECRET@h.example.com", scheme},
		// url.Parse's own errors quote the bad port, or the escape.
		{"https://u:sk-SECRET/x@h.example.com", invalid},
		{"https://u:sk-SECRET#x@h.example.com", invalid},
		{"https://u:sk-SECRET?x@h.example.com", invalid},
		{"https://h.example.com:sk-SECRET", invalid},
		{"https://[::1]:sk-SECRET", invalid},
		{"https://u:sk-SECRET@h.example.com:abc", invalid},
		{"https://u:sk-SECRET%ZZ@h.example.com", invalid},
		// A key in a path that is not plain segments.
		{"https://h.example.com/sk-SECRET/../x", segment},
		{"https://h.example.com/./sk-SECRET", segment},
		{"https://h.example.com/sk-SECRET//x", segment},
		{"https://h.example.com/sk-SECRET%2Fx", segment},
		// A key with a "/" given as user info: url.Parse takes the key for the host.
		{"https://sk-SECRET/x@h.example.com", segment},
	} {
		_, err := ParseURL(tc.url)
		if err == nil {
			t.Errorf("ParseURL(%q) accepted it", tc.url)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, "sk-SECRET") || strings.Contains(msg, "u:") || strings.Contains(msg, "example.com") || strings.Contains(msg, "::1") {
			t.Errorf("ParseURL(%q) error quotes the URL: %q", tc.url, msg)
		}
		if !strings.Contains(msg, tc.want) {
			t.Errorf("ParseURL(%q) error = %q, want %q", tc.url, msg, tc.want)
		}
	}
}

func TestDecode_ReadsMainAndHelper(t *testing.T) {
	c, err := Decode(json.RawMessage(`{"servers": {
		"glm": {"url": "https://glm.example.com", "key": "k", "main": "glm", "helper": "nemotron"}}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if glm := c.Servers["glm"]; glm.Main != "glm" || glm.Helper != "nemotron" {
		t.Errorf("glm = %+v, want main glm and helper nemotron", glm)
	}
}

// A config written for the family fields is told what replaced them, not merely
// that a field is unknown.
func TestDecode_NamesWhatReplacedTheFamilyFields(t *testing.T) {
	for _, field := range []string{"opus", "sonnet", "haiku"} {
		_, err := Decode(json.RawMessage(`{"servers": {"glm": {"url": "https://e.example", "key": "k", "` + field + `": "glm-5-3"}}}`))
		if err == nil || !strings.Contains(err.Error(), "servers.glm."+field) || !strings.Contains(err.Error(), "replaced by main and helper") {
			t.Errorf("%s: err = %v, want it to say main and helper replaced it", field, err)
		}
	}
}

func TestResolve(t *testing.T) {
	eteInt := []string{"aws/claude-opus-4-7", "aws/us.claude-opus-4-7", "claude-opus-4-8", "aws/claude-opus-5",
		"aws/claude-opus-5-5", "claude-sonnet-5-5", "aws/claude-sonnet-5-5", "claude-haiku-4-5-20251001",
		"aws/claude-haiku-4-5", "claude-sonnet-4-5-20250929", "aws/claude-sonnet-4-5"}
	glm := []string{"rits/zai-org/glm-5-3", "rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4"}
	for _, tc := range []struct {
		word string
		ids  []string
		want string
	}{
		{"opus", eteInt, "aws/claude-opus-5-5"},                                  // 5-5 beats 5, which beats 4-8
		{"OPUS", eteInt, "aws/claude-opus-5-5"},                                  // case ignored
		{"sonnet", eteInt, "claude-sonnet-5-5"},                                  // the shorter of two spellings
		{"haiku", eteInt, "aws/claude-haiku-4-5"},                                // a trailing date is not a version
		{"glm", glm, "rits/zai-org/glm-5-3"},                                     // the prefix is not searched
		{"nemotron", glm, "rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4"}, // a size is not a version
		{"zai", glm, ""},                             // nor is a prefix matched
		{"gemini", eteInt, ""},                       // no match
		{"", eteInt, ""},                             // no word
		{"claude-opus-4", eteInt, "claude-opus-4-8"}, // a longer word narrows it
		{"opus", []string{"claude-opus-4-7", "aws/claude-opus-4-7"}, "claude-opus-4-7"}, // a tie: the shortest
	} {
		if got := Resolve(tc.word, tc.ids); got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.word, got, tc.want)
		}
	}
}

func TestFetchModels(t *testing.T) {
	const key = "sk-secret-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data": [{"id": "rits/zai-org/glm-5-3"}, {"id": ""}, {"id": "rits/nvidia/nemotron"}]}`))
	}))
	defer srv.Close()
	ep, err := ParseURL(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ids, header, err := FetchModels(context.Background(), srv.Client(), ep, Auth{Key: key})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if fmt.Sprint(ids) != "[rits/zai-org/glm-5-3 rits/nvidia/nemotron]" || header != "authorization" {
		t.Errorf("ids = %v, header %q, want the two named models and authorization", ids, header)
	}

	// Neither a refusal nor an unreachable server quotes the URL or the key.
	if _, _, err := FetchModels(context.Background(), srv.Client(), ep, Auth{Key: "sk-wrong"}); err == nil ||
		!strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "sk-wrong") {
		t.Errorf("wrong key: err = %v, want the status and nothing of the URL or key", err)
	}
	srv.Close()
	// The address may be named, as the forward proxy names a failed upstream's; the
	// URL, its path and the key may not.
	if _, _, err := FetchModels(context.Background(), srv.Client(), ep, Auth{Key: key}); err == nil ||
		strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "/v1/models") || strings.Contains(err.Error(), key) {
		t.Errorf("closed server: err = %v, want no URL, path or key in it", err)
	}
}

// A loopback host is one machine serving many things, so there a server is its host and
// port; anywhere else the port is ignored, as before. A port the scheme defaults is no
// port on either side.
func TestKey(t *testing.T) {
	for hostport, want := range map[string]string{
		"localhost:11434":      "localhost:11434",
		"LOCALHOST:11434":      "localhost:11434",
		"127.0.0.1:8000":       "127.0.0.1:8000",
		"[::1]:8080":           "[::1]:8080",
		"localhost":            "localhost",
		"localhost:80":         "localhost",
		"localhost:443":        "localhost",
		"glm.example.com:8443": "glm.example.com",
		"GW.example":           "gw.example",
	} {
		if got := Key(hostport); got != want {
			t.Errorf("Key(%q) = %q, want %q", hostport, got, want)
		}
	}
}

// Two model servers on one machine are two servers; one address named twice is still
// refused.
func TestDecode_LocalServersAreTheirPorts(t *testing.T) {
	if _, err := Decode(json.RawMessage(`{"servers": {
		"ollama": {"url": "http://localhost:11434", "key": "k", "main": "m", "helper": "h"},
		"vllm":   {"url": "http://localhost:8000", "key": "k", "main": "m", "helper": "h"},
		"llama":  {"url": "http://127.0.0.1:8080", "key": "k", "main": "m", "helper": "h"}}}`)); err != nil {
		t.Errorf("Decode refused three local servers on their own ports: %v", err)
	}
	_, err := Decode(json.RawMessage(`{"servers": {
		"a": {"url": "http://localhost:11434", "key": "k", "main": "m", "helper": "h"},
		"b": {"url": "http://LOCALHOST:11434", "key": "k", "main": "m", "helper": "h"}}}`))
	if err == nil || !strings.Contains(err.Error(), `"a" and "b" are both on localhost:11434`) {
		t.Errorf("err = %v, want the two servers on localhost:11434 refused", err)
	}
}

func TestDecode_Sessions(t *testing.T) {
	base := `{"servers":{"ete":{"url":"https://ete.example.com","key":"k","main":"m","helper":"h"}}`
	for _, tc := range []struct {
		name    string
		extra   string
		wantErr string
	}{
		{"a session to a server", `,"sessions":{"8e298f11-8bf1-4f40-a27f-ffef5876a5b9":"ete"}`, ""},
		{"a session to empty is allowed", `,"sessions":{"8e298f11-8bf1-4f40-a27f-ffef5876a5b9":""}`, ""},
		{"no sessions is an empty map", ``, ""},
		{"a session to an unknown server is refused", `,"sessions":{"s1":"nope"}`,
			`sessions.s1: "nope" is not a server listed under servers`},
		{"the default bucket is refused", `,"sessions":{"default":"ete"}`,
			`is a synthetic session`},
		{"a pending id is refused", `,"sessions":{"pending:claude-code":"ete"}`,
			`is a synthetic session`},
		{"an empty id is refused", `,"sessions":{"":"ete"}`,
			`the session id is empty`},
		{"an id with whitespace is refused", `,"sessions":{"a b":"ete"}`,
			`the session id has whitespace or a control character`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Decode(json.RawMessage(base + tc.extra + `}`))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				if tc.name == "no sessions is an empty map" && c.Sessions == nil {
					t.Error("Sessions is nil; an absent sessions block moves nothing, as an empty one does")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Decode error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
func TestCheckAPI(t *testing.T) {
	for _, api := range []map[string]string{
		nil,
		{"chat-completions": "/v1/chat/completions"},
		{"chat-completions": "/chat/completions", "anthropic-messages": "/anthropic/v1/messages"},
		{"responses": "/v1/responses"},
	} {
		if err := CheckAPI(api); err != nil {
			t.Errorf("CheckAPI(%v) = %v, want nil", api, err)
		}
	}
	for api, want := range map[string]string{
		`{"chat": "/v1/chat/completions"}`:                     `unknown protocol "chat"; valid protocols: anthropic-messages, chat-completions, responses`,
		`{"chat-completions": "v1/chat/completions"}`:          `api.chat-completions: path "v1/chat/completions" must be absolute, with no query, fragment or ".." segment`,
		`{"chat-completions": "/v1/chat/completions?x=1"}`:     `api.chat-completions: path "/v1/chat/completions?x=1" must be absolute`,
		`{"chat-completions": "/v1/../chat/completions"}`:      `".." segment`,
		`{"chat-completions": "/v1/chat#x"}`:                   `must be absolute`,
		`{"anthropic-messages": "x", "chat-completions": "y"}`: `api.anthropic-messages:`, // sorted: first key first
	} {
		var m map[string]string
		if err := json.Unmarshal([]byte(api), &m); err != nil {
			t.Fatal(err)
		}
		err := CheckAPI(m)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckAPI(%s) = %v, want %q", api, err, want)
		}
	}
}

func TestCheckAuthHeader(t *testing.T) {
	for _, name := range []string{"", "authorization", "x-api-key", "api-key", "Authorization"} {
		if err := CheckAuthHeader(name); err != nil {
			t.Errorf("CheckAuthHeader(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"Host", "Connection", "content-length", "Transfer-Encoding", "TE", "Proxy-Authorization", "bad header", "x:y"} {
		if err := CheckAuthHeader(name); err == nil {
			t.Errorf("CheckAuthHeader(%q) = nil, want an error", name)
		}
	}
}

func TestCheckHeaders(t *testing.T) {
	if err := CheckHeaders(map[string]string{"User-Agent": "bob-shell", "X-Team": "a b"}, ""); err != nil {
		t.Errorf("CheckHeaders = %v, want nil", err)
	}
	for name, want := range map[string]string{
		"Authorization":     `headers: "Authorization" cannot be set here: Cortex sets it`,
		"X-Api-Key":         `headers: "X-Api-Key" cannot be set here: Cortex sets it`,
		"api-key":           `Cortex sets it`,
		"Content-Length":    `headers: "Content-Length" cannot be set here: it is a hop-by-hop or framing header`,
		"Transfer-Encoding": `it is a hop-by-hop or framing header`,
		"host":              `it is a hop-by-hop or framing header`,
		"X-Key":             `Cortex sets it`, // the server's own auth_header
		"bad name":          `not a valid header name`,
	} {
		err := CheckHeaders(map[string]string{name: "v"}, "x-key")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckHeaders(%q) = %v, want %q", name, err, want)
		}
	}
	for _, v := range []string{"a\nb", "a\rb", "a\x00b"} {
		if err := CheckHeaders(map[string]string{"User-Agent": v}, ""); err == nil {
			t.Errorf("CheckHeaders(value %q) = nil, want an error", v)
		}
	}
}

func TestDecode_APIAuthHeaderHeaders(t *testing.T) {
	c, err := Decode(json.RawMessage(`{"servers": {"bob": {"url": "https://bob.example.com", "key": "k", "main": "m", "helper": "h",
		"api": {"chat-completions": "/v1/chat/completions"}, "auth_header": "x-api-key", "headers": {"User-Agent": "bob-shell"}}}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	s := c.Servers["bob"]
	if s.API["chat-completions"] != "/v1/chat/completions" || s.AuthHeader != "x-api-key" || s.Headers["User-Agent"] != "bob-shell" {
		t.Errorf("decoded %+v", s)
	}
	for field, want := range map[string]string{
		`"api": {"chat": "/x"}`:                    "servers.bob.api: unknown protocol",
		`"auth_header": "Host"`:                    "servers.bob.auth_header:",
		`"headers": {"Authorization": "Bearer x"}`: "servers.bob.headers: \"Authorization\" cannot be set here",
	} {
		_, err := Decode(json.RawMessage(`{"servers": {"bob": {"url": "https://bob.example.com", "key": "k", "main": "m", "helper": "h", ` + field + `}}}`))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Decode(%s) = %v, want %q", field, err, want)
		}
	}
}

func TestParseURL_Path(t *testing.T) {
	ep, err := ParseURL("https://api.us-east.bob.ibm.com/inference")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	if ep.Path != "/inference" || ep.Host != "api.us-east.bob.ibm.com" ||
		ep.URL() != "https://api.us-east.bob.ibm.com/inference" || ep.Key() != "api.us-east.bob.ibm.com" {
		t.Errorf("got %+v URL %q Key %q", ep, ep.URL(), ep.Key())
	}
	for raw, want := range map[string]string{
		"https://h.example/inference/": "/inference",
		"https://h.example/a/b":        "/a/b",
		"https://h.example/":           "",
		"https://h.example":            "",
	} {
		ep, err := ParseURL(raw)
		if err != nil || ep.Path != want {
			t.Errorf("ParseURL(%q) = Path %q, err %v; want %q", raw, ep.Path, err, want)
		}
	}
	for raw, want := range map[string]string{
		"https://h.example/a/../b":        `the URL's path must be plain segments, with no ".", "..", empty or escaped segment`,
		"https://h.example/./a":           `the URL's path must be plain segments, with no ".", "..", empty or escaped segment`,
		"https://h.example/a//b":          `the URL's path must be plain segments, with no ".", "..", empty or escaped segment`,
		"https://h.example/a%2Fb":         `the URL's path must be plain segments, with no ".", "..", empty or escaped segment`,
		"https://h.example/inference?x=1": "the URL has a query or fragment",
	} {
		if _, err := ParseURL(raw); err == nil || err.Error() != want {
			t.Errorf("ParseURL(%q) err = %v, want %q", raw, err, want)
		}
	}
}

// modelsRecorder is a fake server that records each request as "<path> <auth>",
// where <auth> is "Bearer" or "x-api-key", and answers with answer.
func modelsRecorder(t *testing.T, seen *[]string, answer func(path, auth string, r *http.Request) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := ""
		switch {
		case strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "):
			auth = "Bearer"
		case r.Header.Get("X-Api-Key") != "":
			auth = "x-api-key"
		}
		*seen = append(*seen, r.URL.Path+" "+auth)
		status, body := answer(r.URL.Path, auth, r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchModels_TriesInOrder(t *testing.T) {
	var seen []string
	srv := modelsRecorder(t, &seen, func(path, auth string, _ *http.Request) (int, string) {
		switch {
		case auth == "Bearer":
			return 401, ""
		case path == "/v1/models":
			return 403, ""
		case path == "/v1/model/info":
			return 200, `{"data":[{"model_name":"a"},{"model_name":"a"},{"model_name":""},{"model_name":"b"}]}`
		}
		return 404, ""
	})
	ep, _ := ParseURL(srv.URL)
	ids, header, err := FetchModels(context.Background(), srv.Client(), ep, Auth{Key: "k"})
	if err != nil || fmt.Sprint(ids) != "[a b]" || header != "x-api-key" {
		t.Errorf("got %v %q %v, want [a b] x-api-key", ids, header, err)
	}
	if got := strings.Join(seen, ", "); got != "/v1/models Bearer, /v1/models x-api-key, /v1/model/info x-api-key" {
		t.Errorf("requests = %s", got)
	}
}

func TestFetchModels_FallsBackToModelInfoWithPrefix(t *testing.T) {
	var seen []string
	srv := modelsRecorder(t, &seen, func(path, _ string, r *http.Request) (int, string) {
		switch {
		case path == "/inference/v1/models":
			return 403, ""
		case path == "/inference/v1/model/info" && r.Header.Get("X-Api-Key") == "k" && r.Header.Get("User-Agent") == "bob-shell":
			return 200, `{"data":[{"model_name":"premium-ide"},{"model_name":"fast"}]}`
		}
		return 403, ""
	})
	ep, err := ParseURL(srv.URL + "/inference")
	if err != nil {
		t.Fatal(err)
	}
	ids, header, err := FetchModels(context.Background(), srv.Client(), ep,
		Auth{Key: "k", Header: "x-api-key", Headers: http.Header{"User-Agent": {"bob-shell"}}})
	if err != nil || fmt.Sprint(ids) != "[premium-ide fast]" || header != "x-api-key" {
		t.Errorf("got %v %q %v (requests %v)", ids, header, err, seen)
	}
}

func TestFetchModels_KeepsAConfiguredHeader(t *testing.T) {
	var seen []string
	srv := modelsRecorder(t, &seen, func(string, string, *http.Request) (int, string) { return 401, "" })
	ep, _ := ParseURL(srv.URL)
	_, _, err := FetchModels(context.Background(), srv.Client(), ep, Auth{Key: "k", Header: "x-api-key"})
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Join(seen, ", ") != "/v1/models x-api-key" {
		t.Errorf("fixed header: err %v, requests %v", err, seen)
	}
	seen = nil
	_, _, err = FetchModels(context.Background(), srv.Client(), ep, Auth{Key: "k"})
	if err == nil || err.Error() != "the server answered 401, 401 to its model list" ||
		strings.Join(seen, ", ") != "/v1/models Bearer, /v1/models x-api-key" {
		t.Errorf("no header: err %v, requests %v", err, seen)
	}
}

func TestFetchModels_BadBodyStops(t *testing.T) {
	var seen []string
	srv := modelsRecorder(t, &seen, func(string, string, *http.Request) (int, string) { return 200, "not json" })
	ep, _ := ParseURL(srv.URL)
	_, _, err := FetchModels(context.Background(), srv.Client(), ep, Auth{Key: "k"})
	if err == nil || err.Error() != "the server's model list is not an OpenAI-style list of models" || len(seen) != 1 {
		t.Errorf("err %v, requests %v", err, seen)
	}
}

func TestSetAuth(t *testing.T) {
	for name, want := range map[string]string{"": "Authorization: Bearer k", "Authorization": "Authorization: Bearer k", "x-api-key": "X-Api-Key: k"} {
		h := http.Header{}
		SetAuth(h, name, "k")
		var b strings.Builder
		_ = h.Write(&b)
		if got := strings.TrimSpace(b.String()); got != want {
			t.Errorf("SetAuth(%q) = %q, want %q", name, got, want)
		}
	}
}
