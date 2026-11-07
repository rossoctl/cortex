// Package parsefixture runs a parser plugin over saved request and response pairs
// and compares the result with a golden file. The mcp-parser, a2a-parser and
// inference-parser fixture tests use it.
package parsefixture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

var update = flag.Bool("update", false, "rewrite expected.json in each case directory from the parser output")

// responseTypes maps a response file to the Content-Type the upstream sent with it.
var responseTypes = map[string]string{
	"response.sse":  "text/event-stream",
	"response.json": "application/json",
	"response.html": "text/html",
}

// Parser is a parser plugin. The listeners call OnRequest, then OnResponseFrame.
type Parser interface {
	OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action
	OnResponseFrame(ctx context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action
}

// Run runs the parser over each case directory that pattern matches and that has a
// request.headers file. Each case is parsed twice: with the whole response body, and
// frame by frame. The two results must be equal, and they must agree with expected.json
// in the case directory. Run with -update to write expected.json.
// newParser gives a parser for one run. result gives the extension to compare. It can
// fail t when the extension is wrong in a way that the golden file does not show.
// prefix is the event key of the extension, for example "inference".
func Run(t *testing.T, pattern, prefix string, newParser func(t *testing.T) Parser, result func(t *testing.T, x *pipeline.Extensions) any) {
	t.Helper()
	heads, err := filepath.Glob(filepath.Join(pattern, "request.headers"))
	if err != nil || len(heads) == 0 {
		t.Fatalf("no cases under %s: %v", pattern, err)
	}
	for _, head := range heads {
		dir := filepath.Dir(head)
		t.Run(strings.TrimPrefix(filepath.ToSlash(dir), "testdata/"), func(t *testing.T) {
			buffered := runCase(t, dir, false, newParser(t), result)
			streamed := runCase(t, dir, true, newParser(t), result)
			if !bytes.Equal(buffered, streamed) {
				t.Fatalf("buffered and streamed results differ\nbuffered:\n%s\nstreamed:\n%s", buffered, streamed)
			}
			checkFieldPaths(t, prefix, buffered)
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
// The path is from a plugin package directory, where the fixture tests run.
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

// runCase sends one case through OnRequest and then the response hooks.
// With frames false it gives the whole body to OnResponseFrame in one call with last=true.
// That is the buffered path of a listener. RunResponse skips OnResponse for a StreamingResponder.
// With frames true it gives an SSE body to OnResponseFrame one event at a time, the way a listener does.
// It returns the extension that result gives, as indented JSON. A request that the parser does not claim gives null.
func runCase(t *testing.T, dir string, frames bool, p Parser, result func(t *testing.T, x *pipeline.Extensions) any) []byte {
	t.Helper()
	head, err := os.ReadFile(filepath.Join(dir, "request.headers"))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(endHead(head))))
	if err != nil {
		t.Fatalf("request.headers: %v", err)
	}
	body := readBody(t, filepath.Join(dir, "request.json"))
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
		respFile, contentType = "response.json", "application/json"
	}
	resp := readBody(t, filepath.Join(dir, respFile))

	ctx := context.Background()
	pctx := &pipeline.Context{Method: req.Method, Host: req.Host, Path: req.URL.Path, Headers: req.Header, Body: body}
	p.OnRequest(ctx, pctx)

	pctx.ResponseHeaders = http.Header{"Content-Type": []string{contentType}}
	// An optional response.headers file holds the response headers of the case.
	if raw, err := os.ReadFile(filepath.Join(dir, "response.headers")); err == nil {
		h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(endHead(raw)))).ReadMIMEHeader()
		if err != nil {
			t.Fatalf("response.headers: %v", err)
		}
		pctx.ResponseHeaders = http.Header(h)
	}
	switch {
	case !frames:
		pctx.ResponseBody = resp
		p.OnResponseFrame(ctx, pctx, resp, true)
	case contentType == "text/event-stream":
		for _, f := range sseFrames(resp) {
			p.OnResponseFrame(ctx, pctx, f, false)
		}
		p.OnResponseFrame(ctx, pctx, nil, true)
	default:
		p.OnResponseFrame(ctx, pctx, resp, true)
	}

	out, err := json.MarshalIndent(result(t, &pctx.Extensions), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// readBody returns the bytes of the body file at path, and no bytes when the file does not exist.
// check-json rejects an empty .json file, so a case with an empty body has no body file.
func readBody(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return b
}

// endHead returns head with one blank line at the end, the line that ends an HTTP head.
// end-of-file-fixer removes that line from a .headers file, so the file can have it or not.
func endHead(head []byte) []byte {
	return append(bytes.TrimRight(head, "\n"), "\n\n"...)
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
