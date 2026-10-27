package script

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The SDK end to end: a real Python script, imported from the SDK the plugin
// installs, answering through the real worker. Skipped where there is no python3,
// so CI's Go jobs need none; the SDK's own tests are pytest's.
func TestPythonScriptThroughTheSDK(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	dir := home(t)
	t.Setenv("PATH", filepath.Dir(py)+string(os.PathListSeparator)+os.Getenv("PATH"))
	src := `#!/usr/bin/env python3
from cortex import on, run

calls = 0

@on.request
def mask(flow):
    global calls
    calls += 1
    print("call", calls)
    flow.replace_text("secret", "***")
    if "rm -rf" in flow.latest_text:
        flow.deny("destructive")

run(enforce=True)
`
	if err := os.WriteFile(filepath.Join(dir, "10-mask.py"), []byte(src), 0o700); err != nil {
		t.Fatal(err)
	}
	s := configured(t, true, "")
	if len(s.loaded) != 1 || s.loaded[0].name != "mask" || !s.loaded[0].hs.Enforce {
		t.Fatalf("loaded %+v", s.loaded)
	}
	if _, err := os.Stat(filepath.Join(dir, "_sdk", "cortex.py")); err != nil {
		t.Fatalf("the SDK was not installed: %v", err)
	}

	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"a secret"}]}]}`
	pctx := request(body)
	if a := s.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action %+v", a.Violation)
	}
	var got struct {
		Messages []struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(pctx.Body, &got); err != nil || got.Messages[0].Content[0].Text != "a ***" {
		t.Fatalf("body = %s (%v)", pctx.Body, err)
	}
	if inv := invocations(pctx)[0]; inv.Plugin != "mask" || inv.Action != pipeline.ActionModify || inv.Details["output"] != "call 1" {
		t.Errorf("invocation = %+v", inv)
	}

	deny := request(strings.Replace(body, "a secret", "rm -rf /", 1))
	if a := s.OnRequest(context.Background(), deny); a.Type != pipeline.Reject || a.Violation.Reason != "destructive" {
		t.Errorf("deny: %+v", a)
	}
	// The script's counter survived between the two calls: one process.
	if out := invocations(deny)[0].Details["output"]; out != "call 2" {
		t.Errorf("second call printed %q, want call 2", out)
	}
}
