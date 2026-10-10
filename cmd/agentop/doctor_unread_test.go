package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// unreadEnv is a setupEnv whose config enables the bridge and points the stats server at
// a test server answering /tls-bridge/unread with body (or 404 when body is "").
func unreadEnv(t *testing.T, goos, body string) *setupEnv {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tls-bridge/unread" || body == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	env := newTestSetupEnv(t)
	env.goos = goos
	cfg := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:47600\n" +
		"stats:\n  address: " + strings.TrimPrefix(srv.URL, "http://") + "\n" +
		"tls_bridge:\n  mode: enabled\n  ca_dir: " + filepath.Join(env.cortexDir, "ca") + "\n"
	if err := os.MkdirAll(env.cortexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.configPath(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return env
}

const helmUnread = `{"osTrustsCA":false,"osTrustCheckedAt":"2026-10-09T14:40:24-04:00","programs":[` +
	`{"program":"/opt/homebrew/Cellar/helm/3.18.3/bin/helm","agent":"/Users/x/.local/share/claude/versions/2.1.295",` +
	`"reason":"os-trust-only","connections":2,"lastHost":"llm-d-incubation.github.io"}],"hosts":[]}`

func TestCheckUnread_ListsEachProgramAndSaysHowToHaveGoProgramsRead(t *testing.T) {
	plainOutput(t)
	env := unreadEnv(t, "darwin", helmUnread)
	var b bytes.Buffer
	checkUnread(env, checklist.New(&b, false))
	out := b.String()
	for _, want := range []string{"helm", "os-trust-only", "llm-d-incubation.github.io", "security add-trusted-cert", "login.keychain-db"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// The advice is one line, because a second one in Advise's reason would lose the
// indent. It says a Go agent goes unpriced as well as unread, as nothing else in doctor
// would, and it promises only what trusting the CA does: Go programs are read on the
// hosts Cortex intercepts, which go's and gh's own hosts are not. Nor is every Go program
// read then: one that relays another system's traffic (gvproxy, listed on any Mac
// running podman) or trusts its own CA list refuses Cortex's certificate instead.
func TestCheckUnread_AdviceSaysAGoAgentIsNotPricedAndPromisesOnlyInterceptedHosts(t *testing.T) {
	plainOutput(t)
	env := unreadEnv(t, "darwin", helmUnread)
	var b bytes.Buffer
	checkUnread(env, checklist.New(&b, false))
	out := b.String()
	var advice string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, markLine("!", "Go tools")) {
			advice = l
		}
	}
	for _, want := range []string{"not read", "Go agent's model calls are not priced", "macOS does not trust"} {
		if !strings.Contains(advice, want) {
			t.Errorf("the advice line lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Go programs' traffic to the hosts it intercepts") {
		t.Errorf("the advice does not say trusting the CA reads only intercepted hosts:\n%s", out)
	}
	for _, want := range []string{"gvproxy", "own CA list", "fail up to three times per Cortex run"} {
		if !strings.Contains(out, want) {
			t.Errorf("the advice does not warn that some Go programs then fail (lacks %q):\n%s", want, out)
		}
	}
}

// A process older than Cortex's CA is remembered on its own, so its line says that
// restarting it is all it takes; a program's own entry says nothing of the sort.
func TestCheckUnread_SaysRestartingAProcessOlderThanTheCAIsEnough(t *testing.T) {
	plainOutput(t)
	env := unreadEnv(t, "linux", `{"programs":[`+
		`{"program":"/usr/bin/python3","pid":600,"reason":"program-refused","failures":1,"connections":1,"lastHost":"example.org"},`+
		`{"program":"/usr/bin/ruby","reason":"program-refused","failures":3,"stopped":true,"connections":3,"lastHost":"example.net"}],"hosts":[]}`)
	var b bytes.Buffer
	checkUnread(env, checklist.New(&b, false))
	out := b.String()
	for _, want := range []string{
		"python3: program-refused (pid 600, started before Cortex's CA — restarting it is enough; last example.org)",
		"ruby: program-refused, not retried until Cortex restarts (last example.net)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A stopped entry for one process is still that process's alone: restarting it is
// enough, so the line must not also say it waits for Cortex to restart.
func TestCheckUnread_AStoppedProcessEntryNamesOnlyTheProcessRestart(t *testing.T) {
	plainOutput(t)
	env := unreadEnv(t, "linux", `{"programs":[`+
		`{"program":"/usr/bin/python3","pid":600,"reason":"program-refused","failures":3,"stopped":true,"connections":3}],"hosts":[]}`)
	var b bytes.Buffer
	checkUnread(env, checklist.New(&b, false))
	out := b.String()
	if !strings.Contains(out, "python3: program-refused (pid 600, started before Cortex's CA — restarting it is enough)") ||
		strings.Contains(out, "until Cortex restarts") {
		t.Errorf("want only the process restart named:\n%s", out)
	}
}

// The host memory holds clients Cortex could not name, so an empty program list is not
// everything read: doctor says how many hosts are passed through, and never fails on it.
func TestCheckUnread_CountsHostsPassedThroughForUnnamedClients(t *testing.T) {
	plainOutput(t)
	for _, c := range []struct {
		hosts, want string
	}{
		{`{"host":"api.example.com","failures":1,"until":"2026-10-09T14:42:00-04:00"}`,
			"1 host passed through for clients Cortex could not name: api.example.com"},
		{`{"host":"a.example","failures":1,"until":"2026-10-09T14:42:00-04:00"},` +
			`{"host":"b.example","failures":1,"until":"2026-10-09T14:42:00-04:00"},` +
			`{"host":"c.example","failures":2,"until":"2026-10-09T14:42:00-04:00"},` +
			`{"host":"d.example","failures":1,"until":"2026-10-09T14:42:00-04:00"}`,
			"4 hosts passed through for clients Cortex could not name: a.example, b.example, c.example and 1 more"},
	} {
		env := unreadEnv(t, "darwin", `{"osTrustsCA":true,"programs":[],"hosts":[`+c.hosts+`]}`)
		var b bytes.Buffer
		checkUnread(env, checklist.New(&b, false))
		out := b.String()
		if !strings.Contains(out, c.want) || strings.Contains(out, "✓") || strings.Contains(out, "✗") {
			t.Errorf("want %q, and no ✓ or ✗ line:\n%s", c.want, out)
		}
	}
}

// Off macOS there is no keychain to point at; the list stays, the advice goes.
func TestCheckUnread_GivesNoKeychainAdviceOffMacOS(t *testing.T) {
	plainOutput(t)
	env := unreadEnv(t, "linux", strings.Replace(helmUnread, `"osTrustsCA":false,`, "", 1))
	var b bytes.Buffer
	checkUnread(env, checklist.New(&b, false))
	if strings.Contains(b.String(), "security") {
		t.Errorf("keychain advice off macOS:\n%s", b.String())
	}
}

// With both lists empty the ✓ claims only what the report shows: nothing is passed
// through for distrusting the CA. Hosts on the passthrough list reach Cortex too and
// are unread by design, so "every program is read" would be false.
func TestCheckUnread_SaysSoWhenNothingIsPassedThroughForTheCA(t *testing.T) {
	plainOutput(t)
	env := unreadEnv(t, "darwin", `{"osTrustsCA":true,"programs":[],"hosts":[]}`)
	var b bytes.Buffer
	checkUnread(env, checklist.New(&b, false))
	if want := markLine("✓", "unread") + "no program is passed through for distrusting Cortex's CA\n"; b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}
}

// A proxy that is not running, or one too old to have the report, gives no line here:
// another check reports a stopped proxy, and an old one has no list to give.
func TestCheckUnread_IsSilentWithNoReport(t *testing.T) {
	plainOutput(t)
	env := unreadEnv(t, "darwin", "")
	var b bytes.Buffer
	checkUnread(env, checklist.New(&b, false))
	if b.Len() != 0 {
		t.Errorf("got output with no report to read:\n%s", b.String())
	}
}
