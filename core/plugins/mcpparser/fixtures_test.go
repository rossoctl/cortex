package mcpparser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

var update = flag.Bool("update", false, "rewrite testdata/*/expected.json from the parser output")

// responseTypes maps a response file to the Content-Type the upstream sent with it.
var responseTypes = map[string]string{
	"response.sse":  "text/event-stream",
	"response.json": "application/json",
}

// TestFixtures runs the parser over the hand-built cases in testdata/<case>.
// testdata/README.md tells which spec the cases follow.
// Each case is parsed twice: with the whole response body, and frame by frame.
// The two results must be equal, and they must agree with expected.json.
func TestFixtures(t *testing.T) {
	heads, err := filepath.Glob(filepath.Join("testdata", "*", "request.headers"))
	if err != nil || len(heads) == 0 {
		t.Fatalf("no cases under testdata: %v", err)
	}
	for _, head := range heads {
		dir := filepath.Dir(head)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			buffered := runFixture(t, dir, false)
			streamed := runFixture(t, dir, true)
			if !bytes.Equal(buffered, streamed) {
				t.Fatalf("buffered and streamed results differ\nbuffered:\n%s\nstreamed:\n%s", buffered, streamed)
			}
			checkFieldPaths(t, "mcp", buffered)
			golden := filepath.Join(dir, "expected.json")
			if *update {
				if err := os.WriteFile(golden, buffered, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to write it)", err)
			}
			if !bytes.Equal(buffered, want) {
				t.Errorf("result differs from %s (run with -update to rewrite it)\ngot:\n%s", golden, buffered)
			}
		})
	}
}

// parsedRequestFields is the field list that core/pipeline's TestParsedRequestFields keeps.
const parsedRequestFields = "../../pipeline/testdata/parsed-request.fields"

// checkFieldPaths fails t when a key path of the JSON object doc is not in parsedRequestFields.
// prefix is the event key of the extension, for example "inference".
// The check does not go below a field of kind any. It reads the keys of a map field as "{}".
func checkFieldPaths(t *testing.T, prefix string, doc []byte) {
	t.Helper()
	raw, err := os.ReadFile(parsedRequestFields)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		path, kind, _ := strings.Cut(line, " ")
		kinds[path] = kind
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				p := path + "." + k
				if kinds[path] == "map" {
					p = path + "{}"
				}
				kind, ok := kinds[p]
				if !ok {
					t.Errorf("%s is not in %s", p, parsedRequestFields)
				} else if kind != "any" {
					walk(p, child)
				}
			}
		case []any:
			for _, child := range v {
				walk(path+"[]", child)
			}
		}
	}
	walk(prefix, v)
}

// runFixture sends one case through OnRequest and then the response hooks.
// The parser has the default config, so /mcp is the MCP path.
// With frames false it gives the whole body to OnResponse.
// OnResponse is a defence-in-depth path, not the live path: RunResponse skips a StreamingResponder.
// The live path for a JSON body is one OnResponseFrame call with last=true.
// With frames true it gives an SSE body to OnResponseFrame one event at a time, the way a listener does.
// It returns the MCP extension as indented JSON. A request that the parser does not claim gives null.
func runFixture(t *testing.T, dir string, frames bool) []byte {
	t.Helper()
	head, err := os.ReadFile(filepath.Join(dir, "request.headers"))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		t.Fatalf("request.headers: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var respFile, contentType string
	for name, ct := range responseTypes {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			// Map order is random, so a second response file would make the case flaky.
			if respFile != "" {
				t.Fatalf("more than one response file: %s and %s", respFile, name)
			}
			respFile, contentType = name, ct
		}
	}
	if respFile == "" {
		t.Fatal("no response.sse or response.json")
	}
	resp, err := os.ReadFile(filepath.Join(dir, respFile))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	p := NewMCPParser()
	if err := p.Configure(nil); err != nil {
		t.Fatal(err)
	}
	pctx := &pipeline.Context{Method: req.Method, Host: req.Host, Path: req.URL.Path, Headers: req.Header, Body: body}
	p.OnRequest(ctx, pctx)

	pctx.ResponseHeaders = http.Header{"Content-Type": []string{contentType}}
	// An optional response.headers file holds the response headers of the case.
	if raw, err := os.ReadFile(filepath.Join(dir, "response.headers")); err == nil {
		h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
		if err != nil {
			t.Fatalf("response.headers: %v", err)
		}
		pctx.ResponseHeaders = http.Header(h)
	}
	switch {
	case !frames:
		pctx.ResponseBody = resp
		p.OnResponse(ctx, pctx)
	case contentType == "text/event-stream":
		for _, f := range sseFrames(resp) {
			p.OnResponseFrame(ctx, pctx, f, false)
		}
		p.OnResponseFrame(ctx, pctx, nil, true)
	default:
		p.OnResponseFrame(ctx, pctx, resp, true)
	}

	out, err := json.MarshalIndent(pctx.Extensions.MCP, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// sseFrames splits an LF-framed SSE body into the data payload of each event.
// It skips an event with no data line, as the listeners do.
func sseFrames(body []byte) [][]byte {
	var frames [][]byte
	for _, event := range bytes.Split(body, []byte("\n\n")) {
		var data [][]byte
		for _, line := range bytes.Split(event, []byte("\n")) {
			if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				data = append(data, bytes.TrimPrefix(v, []byte(" ")))
			}
		}
		if len(data) > 0 {
			frames = append(frames, bytes.Join(data, []byte("\n")))
		}
	}
	return frames
}
