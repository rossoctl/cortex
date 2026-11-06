package catalog

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// thinkingFill runs the bundled thinking-fill script through the SDK's real line
// protocol and returns a function that sends one hook message and reads its reply.
func thinkingFill(t *testing.T) func(msg map[string]any) map[string]any {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	sdk, err := filepath.Abs(filepath.Join("..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, filepath.Join("entries", "thinking-fill", "thinking-fill.py"))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+sdk)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	lines := make(chan string, 16)
	go func() {
		s := bufio.NewScanner(outPipe)
		s.Buffer(make([]byte, 1<<20), 1<<24)
		for s.Scan() {
			lines <- s.Text()
		}
		close(lines)
	}()
	next := func() string {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("script exited")
			}
			return l
		case <-time.After(10 * time.Second):
			t.Fatal("no line from the script within 10s")
		}
		return ""
	}
	var hs struct {
		Hooks   []string `json:"hooks"`
		Enforce bool     `json:"enforce"`
	}
	if err := json.Unmarshal([]byte(next()), &hs); err != nil || strings.Join(hs.Hooks, ",") != "request,response" || !hs.Enforce {
		t.Fatalf("handshake = %+v (%v), want hooks request,response and enforce", hs, err)
	}
	id := 0
	return func(msg map[string]any) map[string]any {
		id++
		msg["id"] = id
		b, _ := json.Marshal(msg)
		if _, err := io.WriteString(in, string(b)+"\n"); err != nil {
			t.Fatal(err)
		}
		for {
			l := next()
			var r map[string]any
			if json.Unmarshal([]byte(l), &r) == nil {
				if v, ok := r["id"].(float64); ok && int(v) == id {
					return r
				}
			}
			// a printed line, shown on the request's row: not the reply
		}
	}
}

func gatewayResponse(host, version string) map[string]any {
	h := map[string]any{}
	if version != "" {
		h["x-litellm-version"] = version
	}
	return map[string]any{"hook": "response", "session": map[string]any{"id": "s"},
		"request":  map[string]any{"method": "POST", "host": host, "path": "/v1/messages", "headers": map[string]any{}},
		"response": map[string]any{"status": 200, "headers": h, "body": "{}"}}
}

// A Claude Code request whose history holds one signed empty thinking block, one
// signed block with text, and one empty block with no signature.
func messagesRequest(host, path string) map[string]any {
	body := map[string]any{"model": "claude-opus-5-5", "messages": []any{
		map[string]any{"role": "user", "content": "fix it"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "", "signature": "SIG1"},
			map[string]any{"type": "thinking", "thinking": "plan", "signature": "SIG2"},
			map[string]any{"type": "thinking", "thinking": ""},
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}},
	}}
	b, _ := json.Marshal(body)
	return map[string]any{"hook": "request", "session": map[string]any{"id": "s"},
		"request": map[string]any{"method": "POST", "host": host, "path": path, "headers": map[string]any{}, "body": string(b)}}
}

// thinkingBlocks returns the reply's edited history thinking blocks, or nil when the
// script left the request unchanged.
func thinkingBlocks(t *testing.T, reply map[string]any) []map[string]any {
	t.Helper()
	req, _ := reply["request"].(map[string]any)
	if req == nil {
		return nil
	}
	s, _ := req["body"].(string)
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(s), &body); err != nil {
		t.Fatalf("edited body is not JSON: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(body.Messages[1].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, b := range blocks {
		if b["type"] == "thinking" {
			out = append(out, b)
		}
	}
	return out
}

// Behind an affected LiteLLM version, a signed block with empty text gets text, its
// signature is unchanged, and the other blocks are left as they were.
func TestThinkingFill_FillsSignedEmptyBlocksBehindAnAffectedGateway(t *testing.T) {
	send := thinkingFill(t)
	send(gatewayResponse("gw.example", "1.105.1"))
	got := thinkingBlocks(t, send(messagesRequest("gw.example:443", "/v1/messages?beta=true")))
	if len(got) != 3 {
		t.Fatalf("thinking blocks = %v, want the request edited with all 3 blocks", got)
	}
	if got[0]["thinking"] != "." || got[0]["signature"] != "SIG1" {
		t.Errorf("signed empty block = %v, want text \".\" and the signature unchanged", got[0])
	}
	if got[1]["thinking"] != "plan" || got[1]["signature"] != "SIG2" {
		t.Errorf("block with text = %v, want it unchanged", got[1])
	}
	if got[2]["thinking"] != "" {
		t.Errorf("unsigned empty block = %v, want it unchanged", got[2])
	}
}

// Nothing changes where the gateway is fixed, has not answered yet, is not LiteLLM,
// or the request is not the Messages API.
func TestThinkingFill_LeavesEveryOtherRequestUnchanged(t *testing.T) {
	send := thinkingFill(t)
	for name, c := range map[string]struct {
		version string // "" means the host never answered with the header
		path    string
	}{
		"fixed version":    {"1.105.2", "/v1/messages"},
		"fixed 1.106.1":    {"1.106.1", "/v1/messages"},
		"before the bug":   {"1.99.4", "/v1/messages"},
		"not litellm":      {"", "/v1/messages"},
		"chat completions": {"1.105.1", "/v1/chat/completions"},
	} {
		t.Run(name, func(t *testing.T) {
			host := strings.ReplaceAll(name, " ", "-") + ".example"
			if c.version != "" {
				send(gatewayResponse(host, c.version))
			}
			if got := thinkingBlocks(t, send(messagesRequest(host, c.path))); got != nil {
				t.Errorf("request edited: %v, want it unchanged", got)
			}
		})
	}
}

// A gateway that is upgraded turns the script off for that host with its next response.
func TestThinkingFill_StopsWhenTheGatewayIsUpgraded(t *testing.T) {
	send := thinkingFill(t)
	send(gatewayResponse("gw.example", "1.105.1"))
	if thinkingBlocks(t, send(messagesRequest("gw.example", "/v1/messages"))) == nil {
		t.Fatal("not filled behind 1.105.1")
	}
	send(gatewayResponse("gw.example", "1.108.1"))
	if got := thinkingBlocks(t, send(messagesRequest("gw.example", "/v1/messages"))); got != nil {
		t.Errorf("still filled after the gateway answered as 1.108.1: %v", got)
	}
}

// The version rule covers the whole affected range and nothing either side of it.
func TestThinkingFill_AffectedVersionRange(t *testing.T) {
	send := thinkingFill(t)
	for v, want := range map[string]bool{
		"1.99.4": false, "1.100.0": true, "1.100.0-rc.1": true, "1.104.8": true, "1.105.1": true,
		"1.105.2": false, "1.106.0": true, "1.106.1": false, "1.108.1": false, "v1.105.1": true, "dev": false,
	} {
		host := "v" + strings.NewReplacer(".", "-", "+", "-").Replace(v) + ".example"
		send(gatewayResponse(host, v))
		if got := thinkingBlocks(t, send(messagesRequest(host, "/v1/messages"))) != nil; got != want {
			t.Errorf("version %q: filled = %v, want %v", v, got, want)
		}
	}
}
