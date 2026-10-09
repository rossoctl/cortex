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
		{"path", `{"servers": {"e": {"url": "https://e.example/v1", "key": "k", "main": "m", "helper": "h"}}}`, "has a path"},
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
		path    = "the URL has a path"
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
		// A key as a path segment.
		{"https://h.example.com/sk-SECRET", path},
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
	ids, err := FetchModels(context.Background(), srv.Client(), ep, key)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if fmt.Sprint(ids) != "[rits/zai-org/glm-5-3 rits/nvidia/nemotron]" {
		t.Errorf("ids = %v, want the two named models", ids)
	}

	// Neither a refusal nor an unreachable server quotes the URL or the key.
	if _, err := FetchModels(context.Background(), srv.Client(), ep, "sk-wrong"); err == nil ||
		!strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "sk-wrong") {
		t.Errorf("wrong key: err = %v, want the status and nothing of the URL or key", err)
	}
	srv.Close()
	// The address may be named, as the forward proxy names a failed upstream's; the
	// URL, its path and the key may not.
	if _, err := FetchModels(context.Background(), srv.Client(), ep, key); err == nil ||
		strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "/v1/models") || strings.Contains(err.Error(), key) {
		t.Errorf("closed server: err = %v, want no URL, path or key in it", err)
	}
}
