package script

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/script/internal/worker"
)

// Scripts in these tests are two-line shell wrappers that run this test binary
// again with FAKE_SCRIPT naming a behaviour, so the protocol is exercised for real
// without Python.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_SCRIPT"); mode != "" {
		fakeScript(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeScript(mode string) {
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	hooks := []string{"request"}
	if strings.HasPrefix(mode, "resp-") {
		hooks = []string{"response"}
	}
	send(worker.Handshake{
		Cortex:          worker.Protocol,
		Hooks:           hooks,
		Enforce:         !strings.Contains(mode, "shadow"),
		RewriteResponse: strings.Contains(mode, "rewrite"),
	})
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	calls := 0
	for in.Scan() {
		var msg struct {
			ID       uint64                `json:"id"`
			Request  struct{ Body string } `json:"request"`
			Response struct {
				Body     string   `json:"body"`
				Streamed bool     `json:"streamed"`
				Status   int      `json:"status"`
				Frames   []string `json:"frames"`
			} `json:"response"`
		}
		json.Unmarshal(in.Bytes(), &msg)
		calls++
		r := map[string]any{"id": msg.ID}
		switch strings.TrimSuffix(mode, "-shadow") {
		case "mask":
			r["request"] = map[string]any{"body": strings.ReplaceAll(msg.Request.Body, "secret", "***")}
		case "seen":
			// Reports the body it was given, so a test can see what the script before it left.
			r["invocation"] = map[string]any{"action": "observe", "reason": "seen", "details": map[string]string{"body": msg.Request.Body}}
		case "count":
			r["invocation"] = map[string]any{"action": "observe", "reason": "count", "details": map[string]string{"calls": fmt.Sprint(calls)}}
		case "deny":
			r["deny"] = map[string]any{"reason": "no secrets", "status": 451}
		case "header":
			r["request"] = map[string]any{"set_headers": map[string]string{"X-Script": "yes"}, "remove_headers": []string{"X-Drop"}}
		case "set-auth":
			r["request"] = map[string]any{"set_headers": map[string]string{"Authorization": "Bearer stolen"}}
		case "print":
			fmt.Fprintln(out, "hello from the script")
		case "error":
			r["error"] = "ValueError: boom"
		case "crash":
			os.Exit(1)
		case "garbage":
			fmt.Fprintf(out, "{\"id\": %d, \"deny\": 7}\n", msg.ID)
			out.Flush()
			continue
		case "wrong-direction":
			r["response"] = map[string]any{"body": "x"}
		case "resp-seen":
			r["invocation"] = map[string]any{"action": "observe", "reason": "resp", "details": map[string]string{
				"body": msg.Response.Body, "streamed": fmt.Sprint(msg.Response.Streamed), "status": fmt.Sprint(msg.Response.Status),
				"frames": strings.Join(msg.Response.Frames, "|")}}
		case "resp-rewrite":
			r["response"] = map[string]any{"body": strings.ToUpper(msg.Response.Body)}
		case "resp-edit-undeclared":
			r["response"] = map[string]any{"body": "changed"}
		case "resp-deny":
			r["deny"] = map[string]any{"reason": "bad output"}
		}
		send(r)
	}
}

// home points HOME at a temporary directory and returns ~/.cortex/plugins in it.
func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("SHELL", "") // no login-shell PATH lookup in a test
	dir := filepath.Join(h, ".cortex", "plugins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// script writes an executable wrapper named name into dir that runs mode.
func script(t *testing.T, dir, name, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("#!/bin/sh\nFAKE_SCRIPT=%s exec %q\n", mode, exe)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// configured builds and starts a slot, as a pipeline build would.
func configured(t *testing.T, local bool, raw string) *Script {
	t.Helper()
	s := built(t, local, raw)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

// built configures a slot without starting it: a build the reloader may still refuse.
func built(t *testing.T, local bool, raw string) *Script {
	t.Helper()
	s := New()
	s.SetLocalInstall(local)
	if err := s.Configure(json.RawMessage(raw)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	return s
}

func request(body string) *pipeline.Context {
	return &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "POST",
		Host:      "api.example.com",
		Path:      "/v1/messages",
		Headers:   http.Header{"Authorization": {"Bearer k"}, "X-Drop": {"1"}},
		Body:      []byte(body),
	}
}

func invocations(pctx *pipeline.Context) []pipeline.Invocation {
	if pctx.Extensions.Invocations == nil {
		return nil
	}
	return pctx.Extensions.Invocations.Outbound
}

func TestPluginName(t *testing.T) {
	for in, want := range map[string]string{
		"10-mask.py": "mask", "mask.py": "mask", "mask": "mask", "2-a-b.sh": "a-b", "10-": "", ".py": ".py",
	} {
		if got := pluginName(in); got != want {
			t.Errorf("pluginName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScan_OrderIgnoredAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	for name, mode := range map[string]os.FileMode{
		"20-log.py": 0o700, "10-mask.py": 0o700, "mask.sh": 0o700, // duplicate name: skipped
		"_helper.py": 0o700, ".hidden": 0o700, "x.py~": 0o700, "notes.txt": 0o600,
	} {
		os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), mode)
	}
	os.Mkdir(filepath.Join(dir, "early"), 0o700)
	files, err := scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.name+"="+filepath.Base(f.path))
	}
	if want := "mask=10-mask.py log=20-log.py"; strings.Join(got, " ") != want {
		t.Errorf("scan = %q, want %q", got, want)
	}
	if files, err := scan(filepath.Join(dir, "missing")); err != nil || files != nil {
		t.Errorf("a missing folder: %v, %v", files, err)
	}
}

func TestFingerprint_ChangesWithTheFolder(t *testing.T) {
	dir := t.TempDir()
	if Fingerprint(dir) != "" {
		t.Error("an empty folder has a fingerprint")
	}
	p := filepath.Join(dir, "a.sh")
	os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700)
	one := Fingerprint(dir)
	os.WriteFile(p, []byte("#!/bin/sh\necho\n"), 0o700)
	if two := Fingerprint(dir); one == "" || two == one {
		t.Errorf("fingerprint did not follow an edit: %q -> %q", one, two)
	}
}

func TestSlotDir(t *testing.T) {
	root := home(t)
	if d, _ := SlotDir(""); d != root {
		t.Errorf("SlotDir(\"\") = %q", d)
	}
	if d, _ := SlotDir("early"); d != filepath.Join(root, "early") {
		t.Errorf("SlotDir(early) = %q", d)
	}
	for _, bad := range []string{"/etc", "../x", "a/b", "..", "."} {
		if _, err := SlotDir(bad); err == nil {
			t.Errorf("SlotDir(%q) accepted", bad)
		}
	}
}

func TestConfigure_RejectsBadConfig(t *testing.T) {
	home(t)
	for _, raw := range []string{`{"timeout":"soon"}`, `{"timeout":"-1s"}`, `{"dirr":"x"}`, `{"dir":"../x"}`} {
		s := New()
		s.SetLocalInstall(true)
		if err := s.Configure(json.RawMessage(raw)); err == nil {
			t.Errorf("Configure(%s) accepted", raw)
		}
	}
}

// Off a local install nothing runs, whatever the folder holds, and the slot costs
// the pipeline nothing.
func TestConfigure_NotALocalInstallRunsNothing(t *testing.T) {
	dir := home(t)
	script(t, dir, "deny", "deny")
	s := configured(t, false, "")
	if len(s.loaded) != 0 {
		t.Fatalf("loaded %d scripts off a local install", len(s.loaded))
	}
	if caps := s.Capabilities(); caps.ReadsBody || caps.WritesRequestBody || caps.WritesResponseBody {
		t.Errorf("an empty slot declares %+v", caps)
	}
	if a := s.OnRequest(context.Background(), request("x")); a.Type != pipeline.Continue {
		t.Error("an empty slot rejected a request")
	}
}

func TestCapabilities_NarrowToWhatTheScriptsNeed(t *testing.T) {
	ceiling := New().Capabilities()
	if !ceiling.WritesRequestBody || !ceiling.WritesResponseBody {
		t.Fatalf("ceiling = %+v", ceiling)
	}
	dir := home(t)
	script(t, dir, "mask", "mask")
	caps := configured(t, true, "").Capabilities()
	if !caps.WritesRequestBody || caps.WritesResponseBody {
		t.Errorf("a request script declares %+v; it must not cost responses their streaming", caps)
	}
	if extra := caps.WithinCeiling(ceiling); extra != "" {
		t.Errorf("configured capabilities exceed the ceiling: %s", extra)
	}
}

// Each script sees the request as the one before it left it.
func TestOnRequest_ScriptsChainInFileOrder(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-mask", "mask")
	script(t, dir, "20-seen", "seen")
	s := configured(t, true, "")
	pctx := request(`{"text":"a secret"}`)
	if a := s.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action %+v", a)
	}
	if got := string(pctx.Body); got != `{"text":"a ***"}` {
		t.Errorf("body = %s", got)
	}
	invs := invocations(pctx)
	if len(invs) != 2 || invs[0].Plugin != "mask" || invs[0].Action != pipeline.ActionModify || invs[1].Plugin != "seen" {
		t.Fatalf("invocations = %+v", invs)
	}
	if invs[1].Details["body"] != `{"text":"a ***"}` {
		t.Errorf("the second script saw %q", invs[1].Details["body"])
	}
}

// A shadow script's edit is recorded and not applied, and the script after it sees
// the request unchanged.
func TestOnRequest_ShadowEditsAreNotApplied(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-mask", "mask-shadow")
	script(t, dir, "20-seen", "seen")
	s := configured(t, true, "")
	pctx := request("secret")
	s.OnRequest(context.Background(), pctx)
	if string(pctx.Body) != "secret" {
		t.Errorf("a shadow edit was applied: %s", pctx.Body)
	}
	invs := invocations(pctx)
	if len(invs) != 2 || !invs[0].Shadow || invs[0].Action != pipeline.ActionModify {
		t.Fatalf("invocations = %+v", invs)
	}
	if invs[1].Details["body"] != "secret" {
		t.Errorf("the script after a shadow one saw %q", invs[1].Details["body"])
	}
}

func TestOnRequest_Deny(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-deny", "deny")
	script(t, dir, "20-seen", "seen")
	pctx := request("x")
	a := configured(t, true, "").OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation.Status != 451 || a.Violation.Reason != "no secrets" || a.Violation.PluginName != "deny" {
		t.Fatalf("action = %+v %+v", a, a.Violation)
	}
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Action != pipeline.ActionDeny {
		t.Errorf("the chain went on after a deny: %+v", invs)
	}
}

func TestOnRequest_ShadowDenyContinues(t *testing.T) {
	dir := home(t)
	script(t, dir, "deny", "deny-shadow")
	pctx := request("x")
	if a := configured(t, true, "").OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("a shadow deny rejected the request")
	}
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Action != pipeline.ActionDeny || !invs[0].Shadow {
		t.Errorf("invocations = %+v", invs)
	}
}

func TestOnRequest_Headers(t *testing.T) {
	dir := home(t)
	script(t, dir, "header", "header")
	pctx := request("x")
	configured(t, true, "").OnRequest(context.Background(), pctx)
	if pctx.Headers.Get("X-Script") != "yes" || pctx.Headers.Get("X-Drop") != "" {
		t.Errorf("headers = %v", pctx.Headers)
	}
}

// A script never sees a credential header, and may not set one.
func TestOnRequest_SecretHeaders(t *testing.T) {
	msg := requestMessage(request("x"))
	if _, ok := msg["request"].(map[string]any)["headers"].(map[string]string)["authorization"]; ok {
		t.Error("authorization reached the script")
	}
	dir := home(t)
	script(t, dir, "auth", "set-auth")
	pctx := request("x")
	configured(t, true, "").OnRequest(context.Background(), pctx)
	if pctx.Headers.Get("Authorization") != "Bearer k" {
		t.Errorf("a script replaced the credential: %v", pctx.Headers)
	}
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Reason != "script_error" {
		t.Errorf("invocations = %+v", invs)
	}
}

func TestOnRequest_OutputLandsOnTheRow(t *testing.T) {
	dir := home(t)
	script(t, dir, "print", "print")
	pctx := request("x")
	configured(t, true, "").OnRequest(context.Background(), pctx)
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Details["output"] != "hello from the script" {
		t.Errorf("invocations = %+v", invs)
	}
}

// A failing script lets the request through, with the failure on its row.
func TestOnRequest_ErrorsFailOpen(t *testing.T) {
	for _, mode := range []string{"error", "crash", "garbage", "wrong-direction"} {
		t.Run(mode, func(t *testing.T) {
			dir := home(t)
			script(t, dir, "s", mode)
			pctx := request("secret")
			if a := configured(t, true, "").OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
				t.Fatalf("a failing script blocked the request: %+v", a.Violation)
			}
			invs := invocations(pctx)
			if len(invs) != 1 || invs[0].Reason != "script_error" || invs[0].Details["error"] == "" {
				t.Errorf("invocations = %+v", invs)
			}
			if string(pctx.Body) != "secret" {
				t.Errorf("body changed: %s", pctx.Body)
			}
		})
	}
}

func TestOnRequest_FailClosed(t *testing.T) {
	dir := home(t)
	script(t, dir, "s", "error")
	a := configured(t, true, `{"fail_closed": true}`).OnRequest(context.Background(), request("x"))
	if a.Type != pipeline.Reject || a.Violation.Code != "plugin.script-failed" {
		t.Fatalf("fail_closed let a failure through: %+v", a)
	}
}

// fail_closed applies to enforcing scripts only: a script in shadow mode is being
// tried, and must not be able to block anything.
func TestOnRequest_FailClosedSparesShadowScripts(t *testing.T) {
	dir := home(t)
	script(t, dir, "s", "error-shadow")
	if a := configured(t, true, `{"fail_closed": true}`).OnRequest(context.Background(), request("x")); a.Type != pipeline.Continue {
		t.Fatal("a shadow script failed closed")
	}
}

// broken writes a script into dir that exits before its handshake.
func broken(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func metric(s *Script, name string) float64 {
	for _, m := range s.Metrics() {
		if m.Name == name {
			return m.Value
		}
	}
	return -1
}

// A fail_closed slot whose script did not start refuses every request: nothing can
// tell which ones that script would have stopped.
func TestOnRequest_FailClosedRefusesWhileAScriptDidNotStart(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-mask", "mask")
	broken(t, dir, "20-guard")
	s := configured(t, true, `{"fail_closed": true}`)
	pctx := request("secret")
	a := s.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation.Code != "plugin.script-failed" || a.Violation.Status != 502 ||
		a.Violation.Reason != "script guard did not start" {
		t.Fatalf("OnRequest = %+v, want a 502 naming guard", a.Violation)
	}
	if string(pctx.Body) != "secret" {
		t.Errorf("a refused request was still edited: %s", pctx.Body)
	}
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Plugin != "guard" || invs[0].Action != pipeline.ActionDeny {
		t.Errorf("invocations = %+v, want one deny row for guard", invs)
	}
	if got := metric(s, "scripts not started"); got != 1 {
		t.Errorf("scripts not started = %v, want 1", got)
	}
}

// Without fail_closed a script that did not start is skipped, as before, and the
// others still run.
func TestOnRequest_AScriptThatDidNotStartFailsOpen(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-mask", "mask")
	broken(t, dir, "20-guard")
	pctx := request("secret")
	if a := configured(t, true, "").OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("a slot without fail_closed refused: %+v", a.Violation)
	}
	if string(pctx.Body) != "***" {
		t.Errorf("body = %s, want the loaded script's edit", pctx.Body)
	}
}

// The refusal lasts until a build in which the script starts — an edit triggers
// one — or from which its file is gone. An edit that breaks a loaded script
// refuses from that build on.
func TestConfigure_ARebuildClearsAScriptThatDidNotStart(t *testing.T) {
	dir := home(t)
	slot := func() pipeline.ActionType {
		return configured(t, true, `{"fail_closed": true}`).OnRequest(context.Background(), request("x")).Type
	}
	broken(t, dir, "guard")
	if slot() != pipeline.Reject {
		t.Fatal("the broken script did not refuse")
	}
	script(t, dir, "guard", "count") // fixed
	if slot() != pipeline.Continue {
		t.Fatal("a rebuild with the script fixed still refused")
	}
	broken(t, dir, "guard") // broken again by an edit
	if slot() != pipeline.Reject {
		t.Fatal("an edit that broke a loaded script let requests through")
	}
	if err := os.Remove(filepath.Join(dir, "guard")); err != nil {
		t.Fatal(err)
	}
	if slot() != pipeline.Continue {
		t.Fatal("a slot whose broken script was deleted still refused")
	}
}

// A script that never sends its handshake is given up on at the start timeout, and
// counts as not started.
func TestOnRequest_FailClosedRefusesAScriptThatNeverAnswers(t *testing.T) {
	dir := home(t)
	if err := os.WriteFile(filepath.Join(dir, "guard"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s := configured(t, true, `{"fail_closed": true}`)
	if d := time.Since(start); d > worker.DefaultStartTimeout+2*time.Second {
		t.Errorf("Configure took %v", d)
	}
	if a := s.OnRequest(context.Background(), request("x")); a.Type != pipeline.Reject {
		t.Fatal("a script that never answered let requests through")
	}
}

// Under on_error: observe the refusal is recorded, as a shadow row, and the request
// goes on.
func TestRun_ObservePolicyDoesNotRefuseForAScriptThatDidNotStart(t *testing.T) {
	dir := home(t)
	broken(t, dir, "guard")
	s := configured(t, true, `{"fail_closed": true}`)
	p, err := pipeline.New([]pipeline.Plugin{s}, pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	if err != nil {
		t.Fatal(err)
	}
	pctx := request("x")
	if a := p.Run(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("an observed slot refused: %+v", a.Violation)
	}
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Plugin != "guard" || !invs[0].Shadow {
		t.Errorf("invocations = %+v, want one shadow row for guard", invs)
	}
}

// A rebuild with an unchanged script keeps its process, and with it the script's state.
func TestConfigure_RebuildKeepsScriptState(t *testing.T) {
	dir := home(t)
	script(t, dir, "count", "count")
	first := configured(t, true, "")
	first.OnRequest(context.Background(), request("x"))
	second := built(t, true, "")
	second.Init(context.Background())
	first.Shutdown(context.Background())
	pctx := request("x")
	second.OnRequest(context.Background(), pctx)
	if got := invocations(pctx)[0].Details["calls"]; got != "2" {
		t.Errorf("the rebuilt slot's script has seen %s calls, want 2", got)
	}
}

// The slot's timeout bounds a call to a script that never answers.
func TestOnRequest_Timeout(t *testing.T) {
	dir := home(t)
	exe, _ := os.Executable()
	// Answers the handshake, then never replies.
	body := fmt.Sprintf("#!/bin/sh\nprintf '{\"cortex\":1,\"hooks\":[\"request\"],\"enforce\":true}\\n'\nexec sleep 3600\n# %s\n", exe)
	os.WriteFile(filepath.Join(dir, "hang"), []byte(body), 0o700)
	s := configured(t, true, `{"timeout": "150ms"}`)
	start := time.Now()
	pctx := request("x")
	if a := s.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatal("a hung script blocked the request")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

// A rebuild while a crashed script waits out its backoff keeps the script loaded.
func TestConfigure_KeepsAScriptThatIsBackingOff(t *testing.T) {
	dir := home(t)
	script(t, dir, "s", "crash")
	first := configured(t, true, "")
	first.OnRequest(context.Background(), request("x")) // crashes: now backing off
	second := built(t, true, "")
	if len(second.loaded) != 1 {
		t.Fatalf("a rebuild during backoff loaded %d scripts, want 1", len(second.loaded))
	}
}

// Editing a script builds a new worker beside the old one, and the running pipeline
// keeps the old one until that pipeline is shut down. A reload that is refused after
// its plugins were configured therefore leaves the running slot working.
func TestConfigure_AnEditDoesNotBreakTheRunningSlot(t *testing.T) {
	dir := home(t)
	script(t, dir, "count", "count")
	running := configured(t, true, "")
	running.OnRequest(context.Background(), request("x"))

	exe, _ := os.Executable()
	os.WriteFile(filepath.Join(dir, "count"), []byte(fmt.Sprintf("#!/bin/sh\n# edited\nFAKE_SCRIPT=count exec %q\n", exe)), 0o700)
	refused := built(t, true, "") // configured, never started: the reload was refused
	_ = refused

	pctx := request("x")
	running.OnRequest(context.Background(), pctx)
	if inv := invocations(pctx)[0]; inv.Reason != "count" || inv.Details["calls"] != "2" {
		t.Fatalf("the running slot broke when a reload was refused: %+v", inv)
	}
}

// The workers of a build that never started are stopped when a later build starts.
func TestInit_SweepsARefusedBuildsWorkers(t *testing.T) {
	dir := home(t)
	script(t, dir, "count", "count")
	running := configured(t, true, "")
	exe, _ := os.Executable()
	os.WriteFile(filepath.Join(dir, "count"), []byte(fmt.Sprintf("#!/bin/sh\n# v2\nFAKE_SCRIPT=count exec %q\n", exe)), 0o700)
	refused := built(t, true, "")
	orphan := refused.loaded[0].w
	os.WriteFile(filepath.Join(dir, "count"), []byte(fmt.Sprintf("#!/bin/sh\n# v3\nFAKE_SCRIPT=count exec %q\n", exe)), 0o700)
	next := built(t, true, "")
	next.Init(context.Background())
	running.Shutdown(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := orphan.Call(context.Background(), map[string]any{})
		if err != nil && strings.Contains(err.Error(), "stopped") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a refused build's worker was never stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if a := next.OnRequest(context.Background(), request("x")); a.Type != pipeline.Continue {
		t.Fatal("the build that started does not work")
	}
}

// An empty slot — every local install has one — starts nothing and writes nothing.
func TestConfigure_AnEmptySlotDoesNothing(t *testing.T) {
	dir := home(t)
	s := configured(t, true, "")
	if s.holder != 0 || len(s.loaded) != 0 {
		t.Errorf("an empty slot took a pool holder or loaded scripts: %+v", s)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("an empty slot wrote into the folder: %v", entries)
	}
}

// Under on_error: observe the pipeline mutes a body write. The row must say shadow,
// not claim an edit the wire never carried.
func TestOnRequest_ObservePolicyEditIsRecordedAsShadow(t *testing.T) {
	dir := home(t)
	script(t, dir, "mask", "mask")
	s := configured(t, true, "")
	p, err := pipeline.New([]pipeline.Plugin{s}, pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	if err != nil {
		t.Fatal(err)
	}
	pctx := request("a secret")
	p.Run(context.Background(), pctx)
	if string(pctx.Body) != "a secret" {
		t.Fatalf("an observed slot changed the body: %s", pctx.Body)
	}
	invs := invocations(pctx)
	if len(invs) != 1 || invs[0].Plugin != "mask" || !invs[0].Shadow {
		t.Errorf("invocations = %+v, want one shadow row for mask", invs)
	}
}

func TestDenyStatus(t *testing.T) {
	for in, want := range map[int]int{403: 403, 451: 451, 503: 503, 0: 0, 200: 0, 302: 0, 4030: 0, -1: 0} {
		if got := denyStatus(in); got != want {
			t.Errorf("denyStatus(%d) = %d, want %d", in, got, want)
		}
	}
}

// A response reader declares no response rewriting, so it costs no streaming.
func TestCapabilities_FollowTheHooks(t *testing.T) {
	dir := home(t)
	script(t, dir, "seen", "resp-seen")
	caps := configured(t, true, "").Capabilities()
	if caps.WritesResponseBody || caps.ReadsBody || !caps.ReadsResponseBody {
		t.Errorf("a response reader declares %+v", caps)
	}
}

// Under on_error: observe nothing the slot does takes effect, so neither of its
// refusals — a script's deny, a fail_closed failure — may be recorded as a denial.
// Each must leave one row, under the file's name, marked shadow, and the request's
// outcome must not say it was denied.
func TestRun_ObservePolicyRefusalsAreShadow(t *testing.T) {
	for _, tc := range []struct{ mode, raw string }{
		{"deny", ""},
		{"error", `{"fail_closed": true}`},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir := home(t)
			script(t, dir, "s", tc.mode)
			s := configured(t, true, tc.raw)
			p, err := pipeline.New([]pipeline.Plugin{s}, pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
			if err != nil {
				t.Fatal(err)
			}
			pctx := request("x")
			if a := p.Run(context.Background(), pctx); a.Type != pipeline.Continue {
				t.Fatalf("an observed slot rejected the request: %+v", a.Violation)
			}
			invs := invocations(pctx)
			if len(invs) != 1 || invs[0].Plugin != "s" || !invs[0].Shadow {
				t.Fatalf("invocations = %+v, want one shadow row for s", invs)
			}
			if out := pipeline.OutcomeFromContext(pctx); out.FinalAction == pipeline.OutcomeDeny {
				t.Errorf("the outcome says denied for a forwarded request: %+v", out)
			}
		})
	}
}

// writer is a request-body writer, the shape of tool-prune, which the preset puts
// ahead of the auto-appended slot.
type writer struct{}

func (writer) Name() string { return "writer" }
func (writer) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesRequestBody: true}
}
func (writer) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (writer) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Whatever its scripts' hooks, a slot can follow a request writer: the preset puts
// it there, and a slot the pipeline refuses there stops the proxy from starting.
func TestCapabilities_ASlotCanFollowARequestWriter(t *testing.T) {
	for _, mode := range []string{"mask", "resp-seen", "resp-rewrite"} {
		t.Run(mode, func(t *testing.T) {
			dir := home(t)
			script(t, dir, "s", mode)
			s := configured(t, true, "")
			if _, err := pipeline.New([]pipeline.Plugin{writer{}, s}); err != nil {
				t.Fatalf("a slot running a %s script cannot follow a request writer: %v", mode, err)
			}
		})
	}
}

// Under on_error: observe a plugin's header writes still apply — only its
// rejections, body writes and redirects are dropped (see pipeline.PolicyAt). A
// script's header edit therefore applies, and its body edit is recorded as shadow.
func TestRun_ObservePolicyAppliesHeadersOnly(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-header", "header")
	script(t, dir, "20-mask", "mask")
	s := configured(t, true, "")
	p, err := pipeline.New([]pipeline.Plugin{s}, pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	if err != nil {
		t.Fatal(err)
	}
	pctx := request("a secret")
	p.Run(context.Background(), pctx)
	if pctx.Headers.Get("X-Script") != "yes" {
		t.Errorf("a header edit did not apply under observe: %v", pctx.Headers)
	}
	if string(pctx.Body) != "a secret" {
		t.Errorf("a body edit applied under observe: %s", pctx.Body)
	}
	invs := invocations(pctx)
	if len(invs) != 2 || invs[0].Shadow || !invs[1].Shadow {
		t.Errorf("invocations = %+v, want the header edit applied and the body edit shadow", invs)
	}
}

// reader is a body reader, the shape of the parsers, which the preset puts ahead
// of the auto-appended slot.
type reader struct{}

func (reader) Name() string { return "reader" }
func (reader) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (reader) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (reader) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// A slot is a request writer wherever scripts run, empty or not: what the folder
// holds must not decide whether the proxy starts. A slot listed before a body
// reader is refused while the folder is still empty — not by the first script
// dropped into it, which is how a running proxy used to become one that would
// not restart.
func TestCapabilities_AnEmptyLocalSlotBeforeABodyReaderIsRefused(t *testing.T) {
	home(t)
	s := built(t, true, "")
	if _, err := pipeline.New([]pipeline.Plugin{s, reader{}}); err == nil {
		t.Fatal("an empty local slot before a body reader built; the first script dropped into the folder would refuse the chain and crash-loop the next restart")
	}
}

// Where no scripts run the slot declares nothing, so its placement stays free.
func TestCapabilities_ANonLocalSlotBuildsBeforeABodyReader(t *testing.T) {
	home(t)
	s := built(t, false, "")
	if _, err := pipeline.New([]pipeline.Plugin{s, reader{}}); err != nil {
		t.Fatalf("a non-local slot before a body reader was refused: %v", err)
	}
}

// The floor an empty slot declares: a request writer, and nothing that would
// cost a streamed response its streaming.
func TestCapabilities_AnEmptyLocalSlotDeclaresTheRequestWriterFloor(t *testing.T) {
	home(t)
	caps := built(t, true, "").Capabilities()
	if !caps.WritesRequestBody {
		t.Errorf("an empty local slot declares %+v; which scripts load depends on the folder, so the build must not", caps)
	}
	if caps.ReadsBody || caps.WritesResponseBody {
		t.Errorf("an empty local slot declares %+v; it must not cost responses their streaming", caps)
	}
}

func response(body string) *pipeline.Context {
	pctx := request(`{"q":1}`)
	pctx.StatusCode = 200
	pctx.ResponseHeaders = http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"s=1"}}
	pctx.ResponseBody = []byte(body)
	return pctx
}

// A buffered response reaches the script whole, as one last frame — the dispatch
// every listener makes to a StreamingResponder.
func TestResponse_BufferedIsReadWhole(t *testing.T) {
	dir := home(t)
	script(t, dir, "seen", "resp-seen")
	s := configured(t, true, "")
	pctx := response(`{"answer":"hi"}`)
	if a := s.OnResponseFrame(context.Background(), pctx, pctx.ResponseBody, true); a.Type != pipeline.Continue {
		t.Fatalf("action %+v", a)
	}
	inv := invocations(pctx)[0]
	if inv.Details["body"] != `{"answer":"hi"}` || inv.Details["streamed"] != "false" || inv.Details["status"] != "200" {
		t.Errorf("the script saw %+v", inv.Details)
	}
	if caps := s.Capabilities(); !caps.ReadsResponseBody || caps.WritesResponseBody {
		t.Errorf("a response reader declares %+v; it must not cost streaming", caps)
	}
}

// A streamed response is read after it is delivered: the frames, and nothing the
// script answers can change it.
func TestResponse_StreamedIsReadAfterDelivery(t *testing.T) {
	dir := home(t)
	script(t, dir, "seen", "resp-seen")
	s := configured(t, true, "")
	pctx := response("")
	for _, f := range []string{`{"delta":"he"}`, `{"delta":"llo"}`} {
		s.OnResponseFrame(context.Background(), pctx, []byte(f), false)
	}
	pctx.MarkResponseDelivered()
	s.OnResponseFrame(context.Background(), pctx, nil, true)
	inv := invocations(pctx)[0]
	if inv.Details["streamed"] != "true" || inv.Details["frames"] != `{"delta":"he"}|{"delta":"llo"}` {
		t.Errorf("the script saw %+v", inv.Details)
	}
}

func TestResponse_RewriteApplies(t *testing.T) {
	dir := home(t)
	script(t, dir, "up", "resp-rewrite")
	s := configured(t, true, "")
	if !s.Capabilities().WritesResponseBody {
		t.Fatal("a rewriting script did not narrow the slot to WritesResponseBody")
	}
	pctx := response(`{"answer":"hi"}`)
	s.OnResponseFrame(context.Background(), pctx, pctx.ResponseBody, true)
	if got := string(pctx.ResponseBody); got != `{"ANSWER":"HI"}` || !pctx.ResponseBodyMutated() {
		t.Errorf("response body = %s (mutated %v)", got, pctx.ResponseBodyMutated())
	}
	invs := invocations(pctx)
	if len(invs) != 1 || invs[0].Plugin != "up" || invs[0].Action != pipeline.ActionModify {
		t.Errorf("invocations = %+v", invs)
	}
}

// A response edit from a script that never said it rewrites responses is refused,
// as is any edit to a response the client already has.
func TestResponse_UndeclaredOrLateEditsAreRefused(t *testing.T) {
	dir := home(t)
	script(t, dir, "x", "resp-edit-undeclared")
	pctx := response("orig")
	configured(t, true, "").OnResponseFrame(context.Background(), pctx, pctx.ResponseBody, true)
	if string(pctx.ResponseBody) != "orig" || invocations(pctx)[0].Reason != "script_error" {
		t.Errorf("undeclared edit: body %s, invocations %+v", pctx.ResponseBody, invocations(pctx))
	}

	dir = home(t)
	script(t, dir, "up", "resp-rewrite")
	s := configured(t, true, "")
	pctx = response("")
	s.OnResponseFrame(context.Background(), pctx, []byte("frame"), false)
	pctx.MarkResponseDelivered()
	s.OnResponseFrame(context.Background(), pctx, nil, true)
	if pctx.ResponseBodyMutated() || invocations(pctx)[0].Reason != "script_error" {
		t.Errorf("late edit: invocations %+v", invocations(pctx))
	}
}

func TestResponse_DenyBlocksABufferedResponseOnly(t *testing.T) {
	dir := home(t)
	script(t, dir, "d", "resp-deny")
	s := configured(t, true, "")
	pctx := response("x")
	if a := s.OnResponseFrame(context.Background(), pctx, pctx.ResponseBody, true); a.Type != pipeline.Reject {
		t.Fatalf("a buffered response was not blocked: %+v", a)
	}
	pctx = response("")
	pctx.MarkResponseDelivered()
	if a := s.OnResponseFrame(context.Background(), pctx, nil, true); a.Type != pipeline.Continue {
		t.Fatal("a delivered response was 'blocked'")
	}
	if inv := invocations(pctx)[0]; inv.Action != pipeline.ActionDeny || !inv.Late {
		t.Errorf("a late deny is recorded as %+v, want a late deny", inv)
	}
}

// Response hooks run in reverse file order, like the pipeline's own response pass.
func TestResponse_RunsInReverseOrder(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-a", "resp-seen")
	script(t, dir, "20-b", "resp-seen")
	pctx := response("x")
	configured(t, true, "").OnResponseFrame(context.Background(), pctx, pctx.ResponseBody, true)
	invs := invocations(pctx)
	if len(invs) != 2 || invs[0].Plugin != "b" || invs[1].Plugin != "a" {
		t.Errorf("order = %+v", invs)
	}
}

// lastFrame is the shape of the parsers: a streaming reader ahead of the slot,
// which the response pass reaches only after the slot's scripts are done.
type lastFrame struct{ got bool }

func (lastFrame) Name() string { return "lastFrame" }
func (lastFrame) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (lastFrame) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (lastFrame) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (l *lastFrame) OnResponseFrame(_ context.Context, _ *pipeline.Context, _ []byte, last bool) pipeline.Action {
	if last {
		l.got = true
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// A slow response script must leave the tail of the dispatch deadline for the
// plugins after the slot. The listener gives one final response dispatch a
// shared deadline, the slot runs first on the reverse pass, and the parsers
// after it turn the folded state into the charge — a script that spends the
// whole deadline leaves them refused with pipeline.cancelled, which sends the
// agent a 499 for a response that arrived complete and drops the settle.
func TestResponse_SlowScriptsLeaveTheDeadlineForTheParsersAfterTheSlot(t *testing.T) {
	dir := home(t)
	// Answers the handshake, then never replies.
	body := "#!/bin/sh\nprintf '{\"cortex\":1,\"hooks\":[\"response\"],\"enforce\":true}\\n'\nexec sleep 3600\n"
	if err := os.WriteFile(filepath.Join(dir, "hang"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	// A timeout larger than the dispatch deadline, so only the reserve can stop it.
	s := configured(t, true, `{"timeout": "30s"}`)
	rec := &lastFrame{}
	p, err := pipeline.New([]pipeline.Plugin{rec, s})
	if err != nil {
		t.Fatal(err)
	}
	pctx := response(`{"answer":"hi"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a := p.RunResponseFrame(ctx, pctx, pctx.ResponseBody, true)
	if a.Type != pipeline.Continue {
		t.Fatalf("action %+v", a)
	}
	if !rec.got {
		t.Fatal("the parser after the slot never saw its last frame")
	}
	if d, ok := ctx.Deadline(); !ok || time.Until(d) <= 0 {
		t.Fatal("the scripts spent the whole dispatch deadline")
	}
}

// Once the reserve is all that remains of the deadline, the scripts are skipped
// at once rather than called into it — recorded as a script error, like any
// other failure, so the parsers after the slot still run.
func TestResponse_AnExhaustedDeadlineSkipsTheScripts(t *testing.T) {
	dir := home(t)
	script(t, dir, "seen", "resp-seen")
	s := configured(t, true, "")
	rec := &lastFrame{}
	p, err := pipeline.New([]pipeline.Plugin{rec, s})
	if err != nil {
		t.Fatal(err)
	}
	pctx := response(`{"answer":"hi"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	a := p.RunResponseFrame(ctx, pctx, pctx.ResponseBody, true)
	if a.Type != pipeline.Continue {
		t.Fatalf("action %+v", a)
	}
	if !rec.got {
		t.Fatal("the parser after the slot never saw its last frame")
	}
	if inv := invocations(pctx)[0]; inv.Reason != "script_error" || inv.Shadow {
		t.Errorf("the skipped script is recorded as %+v", inv)
	}
}

// Set-Cookie is a credential too, and never reaches a script.
func TestResponse_HeadersHideCookies(t *testing.T) {
	if _, ok := flatten(response("").ResponseHeaders)["set-cookie"]; ok {
		t.Error("set-cookie reached the script")
	}
}

// Two slots in one chain each keep their own copy of a streamed response: the
// pipeline hands every frame to both, and each slot's scripts see it once.
func TestResponse_StreamedCopyIsPerSlot(t *testing.T) {
	dir := home(t)
	script(t, dir, "a", "resp-seen")
	if err := os.MkdirAll(filepath.Join(dir, "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	script(t, filepath.Join(dir, "b"), "b", "resp-seen")
	first, second := configured(t, true, ""), configured(t, true, `{"dir":"b"}`)
	pctx := response("")
	// The response pass runs in reverse chain order: the later slot first.
	for _, f := range []string{"f1", "f2"} {
		second.OnResponseFrame(context.Background(), pctx, []byte(f), false)
		first.OnResponseFrame(context.Background(), pctx, []byte(f), false)
	}
	pctx.MarkResponseDelivered()
	second.OnResponseFrame(context.Background(), pctx, nil, true)
	first.OnResponseFrame(context.Background(), pctx, nil, true)
	invs := invocations(pctx)
	if len(invs) != 2 {
		t.Fatalf("invocations = %+v", invs)
	}
	for _, inv := range invs {
		if inv.Details["frames"] != "f1|f2" {
			t.Errorf("script %s saw frames %q, want f1|f2", inv.Plugin, inv.Details["frames"])
		}
	}
}

// A slot that only reads responses is ordered against a response-rewriting slot
// ahead of it: the reverse response pass would run the reader first, so its
// hooks would see the upstream's bytes instead of the client's.
func TestResponse_AReadingSlotAfterARewritingSlotIsRefused(t *testing.T) {
	dir := home(t)
	script(t, dir, "a", "resp-rewrite")
	if err := os.MkdirAll(filepath.Join(dir, "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	script(t, filepath.Join(dir, "b"), "b", "resp-seen")
	first, second := configured(t, true, ""), configured(t, true, `{"dir":"b"}`)
	if _, err := pipeline.New([]pipeline.Plugin{first, second}); err == nil {
		t.Fatal("a response-reading slot after a response-rewriting one built; on the reverse response pass its hooks would see the upstream's bytes, not the client's")
	}
}

// A reload that lands mid-stream hands the rest of the stream to the new build of the
// slot, which carries on with the copy the old build kept.
func TestResponse_StreamedCopySurvivesAReload(t *testing.T) {
	dir := home(t)
	script(t, dir, "a", "resp-seen")
	old := configured(t, true, "")
	pctx := response("")
	old.OnResponseFrame(context.Background(), pctx, []byte("f1"), false)
	rebuilt := configured(t, true, "")
	rebuilt.OnResponseFrame(context.Background(), pctx, []byte("f2"), false)
	pctx.MarkResponseDelivered()
	rebuilt.OnResponseFrame(context.Background(), pctx, nil, true)
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Details["frames"] != "f1|f2" {
		t.Errorf("invocations = %+v, want one that saw f1|f2", invs)
	}
}
