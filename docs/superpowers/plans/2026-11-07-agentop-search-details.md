# agentop details search Implementation Plan (revision 2: the proxy searches)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On the events pane, a `details` box in the `/` prompt makes Enter, `n` and `N` go to the rows whose detail view shows the query, with the proxy finding the matches in one pass.

**Architecture:** agentop checks the summary rows it holds with `detailHas`. A new route, `GET /v1/sessions/{id}/search?q=`, checks what `summarizeEvent` drops, per phase, and the capture, over memory and the archive, and answers the matching seqs. A row matches when either side matches it.

**Tech Stack:** Go 1.26, `core/sessionapi`, `core/session/archive` (zstd segments, `gjson`), Bubble Tea, `httptest`.

**Spec:** `docs/superpowers/specs/2026-11-07-agentop-search-details-design.md` (revision 2)

## Global Constraints

- Two modules change: `core` and `cmd/agentop`. Run Go commands from each module with `GOWORK=off`. Run `go vet ./...` and `gofmt -l <path>` in each module before each commit.
- The case fold is agentop's `find.Contains` fold: `strings.Map(unicode.ToLower, s)` on both sides, then `strings.Contains`.
- A string of at least `session.InternMinLen` (64) bytes is checked one time for each search, by address and length.
- Route answer, exactly: `{"seqs":[…],"lastSeq":N,"schemaVersion":1}`. Empty `q`: `400` with `{"error":"q is required"}`. Unknown session: `404` with `{"error":"session not found"}`.
- Footer texts, exactly: `[/<q> details: Enter to search]`, `[/<q> details: searching…]`, `[/<q> details k/N]`, `[/<q> details: N matches]`, `[/<q> details: no match]`, `[/<q> details: this Cortex has no details search]`, `[/<q> details: search failed]`. When the proxy matched events older than the held rows, append ` · <n> older (o)`.
- agentop fetches no event to check a row.
- Do not restart the shared proxy (`agentop service restart`, `make dev-install`).
- Commit with `git commit -s`. End each message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Comments, commit messages and docs in ASD-STE100 Simplified Technical English.

## Review Focus

1. **Linked buckets.** The events pane can hold rows whose `SessionID` is a bucket, with its own seq numbers. agentop must search each session id and map seqs per id. Pinned in Task 7 (`TestDetailSearch_LinkedBucketIsSearchedByItsOwnID`).
2. **The open segment.** `Find` reads a segment the writer still appends to. A torn tail must end that segment's read and not fail the search. Pinned in Task 5 (`TestFind_ReadsTheOpenSegment`).
3. **A query with URL characters** (`&`, `#`, `%`, space). It must reach the proxy unchanged. Pinned in Task 7 (`TestSearchSession_EscapesTheQuery`).
4. **A common query** that matches every event. The answer is one int for each event, and agentop's footer counts them. Pinned in Task 6 (`TestSearch_EveryEventMatches`).
5. **A proxy without the archive** (Kubernetes). Memory only, no `raw`. Tests 1 to 6 of Task 6 run with no archive.

## Done in revision 1 (kept)

- **Task 1** (commit `18fed5a`): `detailContent` and `detailHas`. Kept as is.
- **Task 2** (commit `6c96cec`): the per-event scan. Task 7 removes the scan.
- **Task 3** (commit `4354088`): Tab, Enter, `n`/`N`, Esc, the prompt box, the resets, the help line and the README row. Task 7 changes the calls they make.

---

### Task 4: One field list in core

**Files:**
- Create: `core/sessionapi/detail_keys.go`
- Create: `core/sessionapi/detail_keys_test.go`
- Modify: `cmd/agentop/tui/detail_pane.go` (`filterForDetail` near line 427, the `var (` block of the six key lists near line 487, two comments in `restoreReportedZeroSplits`)

**Interfaces:**
- Produces: `func DetailKeys(ext string, response bool) []string` and the unexported `func detailShows(ext string, response bool, key string) bool` in `sessionapi`.

- [ ] **Step 1: Write the failing test**

Create `core/sessionapi/detail_keys_test.go`:

```go
package sessionapi

import (
	"slices"
	"testing"
)

// The detail view hides a response event's request fields, and a request event's response
// fields. The search route reads the same lists, so these pins are for both.
func TestDetailKeys(t *testing.T) {
	for _, tc := range []struct {
		ext      string
		response bool
		has      []string
		hasNot   []string
	}{
		{"inference", false, []string{"messages", "tools", "toolResults", "historyToolCalls"}, []string{"completion", "toolCalls"}},
		{"inference", true, []string{"completion", "toolCalls", "reasoningTokens"}, []string{"messages", "tools"}},
		{"mcp", false, []string{"params"}, []string{"result"}},
		{"mcp", true, []string{"result", "error"}, []string{"params"}},
		{"a2a", false, []string{"parts"}, []string{"artifact"}},
		{"a2a", true, []string{"artifact"}, []string{"parts"}},
	} {
		keys := DetailKeys(tc.ext, tc.response)
		for _, k := range tc.has {
			if !slices.Contains(keys, k) || !detailShows(tc.ext, tc.response, k) {
				t.Errorf("%s response=%v: %q missing", tc.ext, tc.response, k)
			}
		}
		for _, k := range tc.hasNot {
			if slices.Contains(keys, k) || detailShows(tc.ext, tc.response, k) {
				t.Errorf("%s response=%v: %q must not be shown", tc.ext, tc.response, k)
			}
		}
	}
	if DetailKeys("other", false) != nil {
		t.Error("an unknown extension has no list")
	}
	DetailKeys("mcp", false)[0] = "changed"
	if DetailKeys("mcp", false)[0] == "changed" {
		t.Error("DetailKeys must return a copy")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `GOWORK=off go -C core test ./sessionapi -run TestDetailKeys -count=1`
Expected: FAIL to compile with `undefined: DetailKeys`.

- [ ] **Step 3: Move the lists**

Create `core/sessionapi/detail_keys.go`. Copy the six slices from the `var (` block in `cmd/agentop/tui/detail_pane.go` (near line 487) into the map below. Move the two comments in that block (the `reasoningTokens` comment and the `A2A:` comment) with them, unchanged, where the `<…>` lines are:

```go
package sessionapi

import "slices"

// detailKeys is, for each extension, the fields the event detail view shows: on a request event
// at index 0, on a response event at index 1. An event of one phase carries the other phase's
// fields too, and the view hides them. The search route hides them the same way (search.go), so
// a search matches only what the operator can see.
//
// Order is not significant: the detail view sorts keys.
var detailKeys = map[string][2][]string{
	"inference": {
		{"model", "requestedModel", "messages", "temperature", "maxTokens", "topP",
			"stream", "tools", "toolChoice", "toolResults", "historyToolCalls"},
		// <the reasoningTokens comment from detail_pane.go, unchanged>
		{"model", "requestedModel", "completion", "finishReason", "promptTokens",
			"completionTokens", "reasoningTokens", "totalTokens", "toolCalls",
			"cacheWriteTokens", "cacheReadTokens"},
	},
	"mcp": {
		{"method", "rpcId", "params"},
		{"method", "rpcId", "result", "error"},
	},
	// <the A2A comment from detail_pane.go, unchanged>
	"a2a": {
		{"method", "rpcId", "sessionId", "messageId", "taskId", "role", "parts"},
		{"method", "rpcId", "sessionId", "taskId", "finalStatus", "artifact", "errorMessage"},
	},
}

// DetailKeys is the fields of extension ext ("inference", "mcp" or "a2a") that the event detail
// view shows: on a response event when response is true, on a request event otherwise. agentop's
// detail pane and the search route read this one list. Nil for any other ext.
func DetailKeys(ext string, response bool) []string {
	k, ok := detailKeys[ext]
	if !ok {
		return nil
	}
	return slices.Clone(k[phaseIndex(response)])
}

// detailShows reports whether the detail view shows field key of extension ext.
func detailShows(ext string, response bool, key string) bool {
	k, ok := detailKeys[ext]
	return ok && slices.Contains(k[phaseIndex(response)], key)
}

func phaseIndex(response bool) int {
	if response {
		return 1
	}
	return 0
}
```

The file must have no `<` placeholder after this step.

- [ ] **Step 4: Run it to verify it passes**

Run: `GOWORK=off go -C core test ./sessionapi -run TestDetailKeys -count=1`
Expected: PASS.

- [ ] **Step 5: agentop reads the list from core**

In `cmd/agentop/tui/detail_pane.go`, in `filterForDetail`, replace the lines from `keep := inferenceReqKeys` down to the closing `}` of the second `if phase == pipeline.SessionResponse {` block with:

```go
	resp := phase == pipeline.SessionResponse
	keep := sessionapi.DetailKeys("inference", resp)
	mcpKeep := sessionapi.DetailKeys("mcp", resp)
	a2aKeep := sessionapi.DetailKeys("a2a", resp)
```

Delete the `// Field classifications for phase-based filtering.` comment and the whole `var (` block of the six lists. In `restoreReportedZeroSplits`'s comment, replace `inferenceRespKeys' own doc` with `the response list of sessionapi.DetailKeys`, and `inferenceReqKeys not listing it` with `the request list of sessionapi.DetailKeys not listing it`. Add `"github.com/rossoctl/cortex/core/sessionapi"` to the imports if `detail_pane.go` does not import it.

- [ ] **Step 6: Run both modules**

Run: `GOWORK=off go -C core test ./sessionapi -count=1 && GOWORK=off go -C cmd/agentop test ./tui -count=1 && GOWORK=off go -C core vet ./sessionapi && GOWORK=off go -C cmd/agentop vet ./tui && gofmt -l core/sessionapi cmd/agentop/tui`
Expected: `ok` twice, no vet output, no gofmt output. `grep -rn "inferenceReqKeys" cmd/agentop core` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add core/sessionapi/detail_keys.go core/sessionapi/detail_keys_test.go cmd/agentop/tui/detail_pane.go
git commit -s -m "Refactor: Keep the detail view's field lists in core

Move the per-phase field lists of agentop's detail view to
sessionapi.DetailKeys. The detail pane reads them from there, and the
search route will read the same lists.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Task 5: `archive.Find`

**Files:**
- Modify: `core/session/archive/reader.go` (`readRecords` near line 41)
- Create: `core/session/archive/find.go`
- Create: `core/session/archive/find_test.go`

**Interfaces:**
- Produces: `func (a *Archive) Find(ctx context.Context, id string, event func(*model.SessionEvent) bool, text func(string) bool) (seqs []uint64, last uint64, ok bool, err error)` and `func MatchJSON(v string, text func(string) bool) bool`.
- Consumes (tests): `openTest(t, root, clk)`, `newClock()`, `(*Archive).inWriter`, `(*Archive).write(id, *model.SessionEvent, *model.RequestCapture)`, `(*Archive).tick()`, `(*Archive).Close()`.

- [ ] **Step 1: Write the failing tests**

Create `core/session/archive/find_test.go`:

```go
package archive

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/rossoctl/cortex/core/pipeline"
)

// foldText is the search route's text check, in the form Find takes it.
func foldText(q string) func(string) bool {
	q = strings.Map(unicode.ToLower, q)
	return func(s string) bool { return strings.Contains(strings.Map(unicode.ToLower, s), q) }
}

func noEvent(*pipeline.SessionEvent) bool { return false }

// longText is a message content long enough to be a block of its own (blocks start at
// session.InternMinLen bytes).
func longText(s string) string { return s + " " + strings.Repeat("filler words ", 10) }

// reqCapture is a request capture whose body has one user message per text.
func reqCapture(texts ...string) *pipeline.RequestCapture {
	var msgs []string
	for _, t := range texts {
		msgs = append(msgs, `{"role":"user","content":[{"type":"text","text":"`+t+`"}]}`)
	}
	body := `{"model":"m","messages":[` + strings.Join(msgs, ",") + `]}`
	return &pipeline.RequestCapture{Original: []byte(body),
		Header: http.Header{"X-Trace": {"trace-hdr-value"}}, Query: "beta=true"}
}

// findArchive writes the events, each with its capture, and closes the archive.
func findArchive(t *testing.T, caps ...*pipeline.RequestCapture) *Archive {
	t.Helper()
	a := openTest(t, t.TempDir(), newClock())
	a.inWriter(func() {
		for i, c := range caps {
			phase := pipeline.SessionRequest
			if c != nil && c.Response != nil {
				phase = pipeline.SessionResponse
			}
			a.write("s", &pipeline.SessionEvent{Seq: uint64(i + 1), SessionID: "s", Phase: phase}, c)
		}
		a.tick()
	})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return a
}

// Every request re-sends the conversation, so a prompt is one block that each later request
// uses. Each of those events matches.
func TestFind_ABlockMatchesEveryEventThatUsesIt(t *testing.T) {
	first := longText("find the NEEDLE-RAW")
	a := findArchive(t, reqCapture(first), reqCapture(first, longText("next turn")), reqCapture(longText("other")))
	seqs, last, ok, err := a.Find(context.Background(), "s", noEvent, foldText("needle-raw"))
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if !slices.Equal(seqs, []uint64{1, 2}) || last != 3 {
		t.Errorf("seqs %v last %d, want [1 2] and 3", seqs, last)
	}
}

// The detail view shows an image block and a binary body as labels, so their base64 or bytes
// must not match.
func TestFind_SkipsWhatTheDetailShowsAsALabel(t *testing.T) {
	img := `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		strings.Repeat("NEEDLEB64", 20) + `"}}]}`
	imgCap := &pipeline.RequestCapture{Original: []byte(`{"model":"m","messages":[` + img + `]}`)}
	binResp := &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 200,
		Body: []byte("\xff\xfe NEEDLEB64 " + strings.Repeat("x", 100))}}
	textResp := &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 200,
		Body: []byte("data: " + longText("the NEEDLEB64 reply"))}}
	a := findArchive(t, imgCap, binResp, textResp)

	seqs, _, _, err := a.Find(context.Background(), "s", noEvent, foldText("needleb64"))
	if err != nil || !slices.Equal(seqs, []uint64{3}) {
		t.Errorf("seqs %v err %v, want only the text response [3]", seqs, err)
	}
}

func TestFind_HeadersQueryAndStatus(t *testing.T) {
	resp := &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 418,
		Header: http.Header{"X-Reply": {"teapot-hdr"}}}}
	a := findArchive(t, reqCapture(longText("hello")), resp)
	for q, want := range map[string][]uint64{
		"trace-hdr": {1}, "x-trace": {1}, "beta=true": {1}, "418": {2}, "teapot-hdr": {2},
	} {
		if seqs, _, _, _ := a.Find(context.Background(), "s", noEvent, foldText(q)); !slices.Equal(seqs, want) {
			t.Errorf("%q: seqs %v, want %v", q, seqs, want)
		}
	}
}

// event decides for the event's own fields, with no capture needed.
func TestFind_EventPredicate(t *testing.T) {
	a := findArchive(t, nil, nil, nil)
	seqs, _, _, _ := a.Find(context.Background(), "s",
		func(e *pipeline.SessionEvent) bool { return e.Seq == 2 }, foldText("absent"))
	if !slices.Equal(seqs, []uint64{2}) {
		t.Errorf("seqs %v, want [2]", seqs)
	}
}

func TestFind_UnknownSession(t *testing.T) {
	a := findArchive(t, nil)
	if _, _, ok, err := a.Find(context.Background(), "other", noEvent, foldText("x")); ok || err != nil {
		t.Errorf("ok %v err %v, want false and nil", ok, err)
	}
}

func TestFind_StopsWhenTheContextEnds(t *testing.T) {
	a := findArchive(t, reqCapture(longText("NEEDLE")), reqCapture(longText("NEEDLE")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seqs, _, _, err := a.Find(ctx, "s", noEvent, foldText("needle"))
	if err == nil || len(seqs) != 0 {
		t.Errorf("seqs %v err %v, want none and the context's error", seqs, err)
	}
}

// The writer still appends to the open segment. Its frame has not ended, which the reader
// reports as a torn tail: that ends the segment's read, not the search.
func TestFind_ReadsTheOpenSegment(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	a.inWriter(func() {
		a.write("s", &pipeline.SessionEvent{Seq: 1, SessionID: "s", Phase: pipeline.SessionRequest},
			reqCapture(longText("open NEEDLE")))
		a.tick()
	})
	seqs, _, ok, err := a.Find(context.Background(), "s", noEvent, foldText("needle"))
	if err != nil || !ok || !slices.Equal(seqs, []uint64{1}) {
		t.Errorf("seqs %v ok %v err %v, want [1]", seqs, ok, err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `GOWORK=off go -C core test ./session/archive -run TestFind -count=1`
Expected: FAIL to compile with `a.Find undefined`.

- [ ] **Step 3: Let `readRecords` hand out block records**

In `core/session/archive/reader.go`, rename `func readRecords(` to `func readRecordsWith(`, and add the parameter `onBlock func(rec *record)` after `blocks map[string]string`. Above it, add:

```go
// readRecords is readRecordsWith with no block callback.
func readRecords(path string, want map[string]struct{}, blocks map[string]string,
	fn func(e *model.SessionEvent, c json.RawMessage) bool) (truncated bool, err error) {
	return readRecordsWith(path, want, blocks, nil, fn)
}
```

In `readRecordsWith`'s doc comment, add one line: `onBlock, when not nil, gets each block record, in file order.` In its body:
- change `if want == nil && bytes.HasPrefix(line, blockPrefix) {` to `if want == nil && onBlock == nil && bytes.HasPrefix(line, blockPrefix) {`
- at the start of `case kindBlock:`, add:

```go
				if onBlock != nil {
					onBlock(&rec)
				}
```

- [ ] **Step 4: Write `Find`**

Create `core/session/archive/find.go`:

```go
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/cortex/core/model"
)

// Find returns, ascending, the seqs of id's archived events that match, and the newest seq it
// read. An event matches when event reports it, or when text reports a piece of its capture: a
// body block, the text between blocks, a header name or value, the query, or a response's
// status. ok is false when the archive holds nothing under id.
//
// ONE READ OF EACH SEGMENT. Every request re-sends the conversation, so its body is mostly
// blocks that an earlier request wrote. text sees each block one time for each segment, and an
// event matches through the blocks it uses. No body is rebuilt.
//
// A block that the detail view shows as a label is not checked: an image or a document content
// block, and a block stored as bytes (a body that is not UTF-8). Their text is base64 or binary,
// which the operator never sees.
//
// Like Page, it runs on the caller's goroutine. It stops, with ctx's error, when ctx ends.
func (a *Archive) Find(ctx context.Context, id string, event func(*model.SessionEvent) bool,
	text func(string) bool) (seqs []uint64, last uint64, ok bool, err error) {
	e, ok := a.entry(id)
	if !ok || len(e.segments) == 0 {
		return nil, 0, false, nil
	}
	for _, seg := range e.segments {
		hit := map[string]bool{}
		_, rerr := readRecordsWith(filepath.Join(e.dir, seg.File), nil, nil, func(rec *record) {
			hit[rec.H] = rec.X == nil && !mediaBlock(rec.V) && MatchJSON(rec.V, text)
		}, func(ev *model.SessionEvent, c json.RawMessage) bool {
			if ctx.Err() != nil {
				return false
			}
			last = max(last, ev.Seq)
			if event(ev) || captureMatches(c, hit, text) {
				seqs = append(seqs, ev.Seq)
			}
			return true
		})
		if errors.Is(rerr, fs.ErrNotExist) {
			continue // pruned or renamed since the snapshot, as in Page
		}
		if rerr != nil {
			return seqs, last, true, rerr
		}
		if cerr := ctx.Err(); cerr != nil {
			return seqs, last, true, cerr
		}
	}
	return seqs, last, true, nil
}

// mediaBlock reports whether v is an image or a document content block.
func mediaBlock(v string) bool {
	t := gjson.Get(v, "type").Str
	return (t == "image" || t == "document") && gjson.Get(v, "source").IsObject()
}

// MatchJSON reports whether text matches v as the detail view shows it: each key and value of v
// when v is JSON, v itself when it is not. A capture block is a value cut out of a body, so most
// are JSON. The search route also uses it for a tool's parameters.
func MatchJSON(v string, text func(string) bool) bool {
	if !gjson.Valid(v) {
		return text(v)
	}
	return jsonMatches(gjson.Parse(v), text)
}

func jsonMatches(r gjson.Result, text func(string) bool) bool {
	switch {
	case r.IsObject() || r.IsArray():
		found, obj := false, r.IsObject()
		r.ForEach(func(k, v gjson.Result) bool {
			found = (obj && text(k.Str)) || jsonMatches(v, text)
			return !found
		})
		return found
	case r.Type == gjson.String:
		return text(r.Str)
	}
	return text(r.Raw) // a number, true, false or null, as the tree writes it
}

// captureMatches reports whether text matches c, an event's capture record: a block it uses, the
// text between its blocks, a header, the query, or a response's status.
func captureMatches(c json.RawMessage, hit map[string]bool, text func(string) bool) bool {
	if c == nil {
		return false
	}
	var f captureRec
	if json.Unmarshal(c, &f) != nil {
		return false
	}
	for _, b := range []*bodyRec{f.O, f.W} {
		if b != nil && bodyMatches(b, hit, text) {
			return true
		}
	}
	if headersMatch(f.H, text) || (f.Q != "" && text(f.Q)) {
		return true
	}
	if r := f.R; r != nil {
		return text(strconv.Itoa(r.C)) || headersMatch(r.H, text) || hit[r.B]
	}
	return false
}

// bodyMatches reports whether a body's skeleton matches: a block it names, or the text between.
func bodyMatches(b *bodyRec, hit map[string]bool, text func(string) bool) bool {
	for i, p := range b.P {
		if i%2 == 1 {
			if hit[p] {
				return true
			}
		} else if p != "" && text(p) {
			return true
		}
	}
	return false
}

// headersMatch reports whether a header name, or its values as the detail view joins them,
// matches.
func headersMatch(h http.Header, text func(string) bool) bool {
	for k, vs := range h {
		if text(k) || text(strings.Join(vs, ", ")) {
			return true
		}
	}
	return false
}
```

Note: `http.Header` keys are canonical (`X-Trace`), and the fold makes `x-trace` match.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOWORK=off go -C core test ./session/archive -run 'TestFind|TestCapture' -count=1 -race`
Expected: PASS. If `TestFind_SkipsWhatTheDetailShowsAsALabel` fails on the binary response, check how `splitCapture` stores a non-UTF-8 response body. If it is not a block with `X`, rule on the smallest change that skips it, and ledger the ruling.

- [ ] **Step 6: Run the package, vet and gofmt**

Run: `GOWORK=off go -C core test ./session/archive -count=1 && GOWORK=off go -C core vet ./session/archive && gofmt -l core/session/archive`
Expected: `ok`, no vet output. gofmt prints only `core/session/archive/zz_measure_test.go` if that throwaway file is still there, and nothing else.

- [ ] **Step 7: Commit**

```bash
git add core/session/archive/reader.go core/session/archive/find.go core/session/archive/find_test.go
git commit -s -m "Feat: Find a session's archived events by text

Add archive.Find. It reads each segment one time, checks each body
block one time, and marks the events whose fields or capture match.
Image and document blocks and binary bodies are not checked, because
the detail view shows them as labels.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Task 6: The search route

**Files:**
- Create: `core/sessionapi/search.go`
- Create: `core/sessionapi/search_test.go`
- Modify: `core/sessionapi/server.go` (the route list near line 224, and `handleIndex` if it lists routes by hand)
- Modify: `docs/session-api.md` (the Endpoints table near line 24, a new section after the timeline section)
- Modify: `core/sessionapi/summary.go` (the `THE RULE` paragraph of `summarizeEvent`'s comment, lines 17-22)

**Interfaces:**
- Consumes: `detailShows` (Task 4), `(*archive.Archive).Find` and `archive.MatchJSON` (Task 5), `session.InternMinLen`, `(*session.Store).View`, `(*session.Store).AppendCapture(sessionID string, event model.SessionEvent, c *model.RequestCapture)`.
- Produces: the route `GET /v1/sessions/{id}/search`, answering `searchResponse{Seqs []uint64 "seqs"; LastSeq uint64 "lastSeq"; SchemaVersion int "schemaVersion"}`.

- [ ] **Step 1: Write the failing tests**

Create `core/sessionapi/search_test.go`:

```go
package sessionapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// search asks base for id's events that match q, and returns the answer, the status and the
// body.
func search(t *testing.T, base, id, q string) (searchResponse, int, string) {
	t.Helper()
	resp, err := http.Get(base + "/v1/sessions/" + url.PathEscape(id) + "/search?q=" + url.QueryEscape(q))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out searchResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}
	return out, resp.StatusCode, string(body)
}

// seqsFor is the seqs search answers, failing the test on any status but 200.
func seqsFor(t *testing.T, base, id, q string) []uint64 {
	t.Helper()
	out, code, body := search(t, base, id, q)
	if code != http.StatusOK {
		t.Fatalf("%q: status %d, body %s", q, code, body)
	}
	return out.Seqs
}

func req(inf *pipeline.InferenceExtension) pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Inference: inf}
}

func resp(inf *pipeline.InferenceExtension) pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse, Inference: inf}
}

// A response event carries its request's messages, and the detail view hides them there.
func TestSearch_PromptMatchesTheRequestNotTheResponse(t *testing.T) {
	ts, store := newTestServer(t)
	inf := &pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "find the NEEDLE-PROMPT"}}}
	store.Append("s1", req(inf))
	store.Append("s1", resp(inf))
	if got := seqsFor(t, ts.URL, "s1", "needle-prompt"); !slices.Equal(got, []uint64{1}) {
		t.Errorf("seqs %v, want [1]", got)
	}
}

func TestSearch_ToolCallsMatchTheResponseNotTheRequest(t *testing.T) {
	ts, store := newTestServer(t)
	inf := &pipeline.InferenceExtension{Model: "m",
		ToolCalls: []pipeline.InferenceToolCall{{Name: "bash", Arguments: `{"cmd":"NEEDLE-ARGS"}`}}}
	store.Append("s1", req(inf))
	store.Append("s1", resp(inf))
	if got := seqsFor(t, ts.URL, "s1", "needle-args"); !slices.Equal(got, []uint64{2}) {
		t.Errorf("seqs %v, want [2]", got)
	}
}

// The detail tree shows key names, so they match where the field is present.
func TestSearch_KeyNames(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "a", ToolUseID: "t1"}}}))
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "b"}}}))
	if got := seqsFor(t, ts.URL, "s1", "toolUseId"); !slices.Equal(got, []uint64{1}) {
		t.Errorf("seqs %v, want [1]", got)
	}
}

func TestSearch_ToolParameters(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m", Tools: []pipeline.InferenceTool{{
		Name: "t", Parameters: `{"type":"object","properties":{"needlefield":{"type":"string"}}}`}}}))
	if got := seqsFor(t, ts.URL, "s1", "needlefield"); !slices.Equal(got, []uint64{1}) {
		t.Errorf("seqs %v, want [1]", got)
	}
}

func TestSearch_MCPAndA2A(t *testing.T) {
	ts, store := newTestServer(t)
	mcp := &pipeline.MCPExtension{Method: "tools/call",
		Params: map[string]any{"path": "/needle-p"}, Result: map[string]any{"text": "needle-r"}}
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, MCP: mcp})
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse, MCP: mcp})
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse,
		A2A: &pipeline.A2AExtension{Artifact: "the needle-a artifact"}})
	for q, want := range map[string][]uint64{"needle-p": {1}, "needle-r": {2}, "needle-a": {3}} {
		if got := seqsFor(t, ts.URL, "s1", q); !slices.Equal(got, want) {
			t.Errorf("%q: seqs %v, want %v", q, got, want)
		}
	}
}

// The fold is agentop's find.Contains fold: rune by rune, with unicode.ToLower.
func TestSearch_CaseFold(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "İSTANBUL and straße"}}}))
	for _, q := range []string{"istanbul", "STRAßE"} {
		if got := seqsFor(t, ts.URL, "s1", q); !slices.Equal(got, []uint64{1}) {
			t.Errorf("%q: seqs %v, want [1]", q, got)
		}
	}
}

// archived starts a server whose store records into an archive, appends each event with its
// capture, and flushes the archive by closing it. The store keeps what it holds.
func archived(t *testing.T, events []pipeline.SessionEvent, caps []*pipeline.RequestCapture) string {
	t.Helper()
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	for i, e := range events {
		store.AppendCapture("s1", e, caps[i])
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(":0", store, WithArchive(a)).server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return ts.URL
}

func bodyCapture(texts ...string) *pipeline.RequestCapture {
	var msgs []string
	for _, s := range texts {
		msgs = append(msgs, `{"role":"user","content":[{"type":"text","text":"`+s+` `+strings.Repeat("filler ", 12)+`"}]}`)
	}
	return &pipeline.RequestCapture{Original: []byte(`{"model":"m","messages":[` + strings.Join(msgs, ",") + `]}`)}
}

// The capture is searched where the archive holds it, and only there.
func TestSearch_ArchiveCapture(t *testing.T) {
	ev := req(&pipeline.InferenceExtension{Model: "m"})
	base := archived(t, []pipeline.SessionEvent{ev, ev, ev},
		[]*pipeline.RequestCapture{bodyCapture("NEEDLE-RAW"), bodyCapture("NEEDLE-RAW", "next"), bodyCapture("other")})
	if got := seqsFor(t, base, "s1", "needle-raw"); !slices.Equal(got, []uint64{1, 2}) {
		t.Errorf("seqs %v, want [1 2]", got)
	}
}

// Memory and the archive both hold the event: it is in the answer one time.
func TestSearch_EachSeqOneTime(t *testing.T) {
	ev := req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "needle-both"}}})
	base := archived(t, []pipeline.SessionEvent{ev}, []*pipeline.RequestCapture{bodyCapture("needle-both")})
	out, _, _ := search(t, base, "s1", "needle-both")
	if !slices.Equal(out.Seqs, []uint64{1}) || out.LastSeq != 1 {
		t.Errorf("answer %+v, want seqs [1] and lastSeq 1", out)
	}
}

func TestSearch_EveryEventMatches(t *testing.T) {
	ts, store := newTestServer(t)
	for range 50 {
		store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
			Messages: []pipeline.InferenceMessage{{Role: "user", Content: "common text"}}}))
	}
	out, _, _ := search(t, ts.URL, "s1", "common")
	if len(out.Seqs) != 50 || out.LastSeq != 50 || out.SchemaVersion != SchemaVersion {
		t.Errorf("answer has %d seqs, lastSeq %d, schemaVersion %d", len(out.Seqs), out.LastSeq, out.SchemaVersion)
	}
}

func TestSearch_AnswerAndErrors(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m"}))

	if _, _, body := search(t, ts.URL, "s1", "absent"); !strings.Contains(body, `"seqs":[]`) {
		t.Errorf("no match: body %s, want an empty seqs list, not null", body)
	}
	if _, code, body := search(t, ts.URL, "s1", ""); code != http.StatusBadRequest || !strings.Contains(body, `"q is required"`) {
		t.Errorf("empty q: %d %s", code, body)
	}
	if _, code, body := search(t, ts.URL, "nope", "x"); code != http.StatusNotFound || !strings.Contains(body, `"session not found"`) {
		t.Errorf("unknown session: %d %s", code, body)
	}
}

// A closed connection stops the pass, and nothing is written.
func TestSearch_StopsWhenTheRequestEnds(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "needle"}}}))
	srv := New(":0", store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/v1/sessions/s1/search?q=needle", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(w, r)
	if w.Body.Len() != 0 {
		t.Errorf("body %q, want nothing after the request ended", w.Body.String())
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `GOWORK=off go -C core test ./sessionapi -run TestSearch_ -count=1`
Expected: FAIL to compile with `undefined: searchResponse`.

- [ ] **Step 3: Write the route**

Create `core/sessionapi/search.go`:

```go
package sessionapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unsafe"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// GET /v1/sessions/{id}/search?q=<text> (#1680) answers the seqs of the session's events whose
// detail view shows q in a part that view=summary drops: the bodies, and the capture where the
// archive holds one. agentop holds the summaries and checks them itself. So between the two, a
// row matches when its detail view shows q. See docs/session-api.md.

// searchResponse is the route's answer. Seqs is ascending, and LastSeq is the newest seq the
// pass read, so a client can tell when the session has grown since.
type searchResponse struct {
	Seqs          []uint64 `json:"seqs"`
	LastSeq       uint64   `json:"lastSeq"`
	SchemaVersion int      `json:"schemaVersion"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	id, q := r.PathValue("id"), r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, `{"error":"q is required"}`, http.StatusBadRequest)
		return
	}
	ctx, m := r.Context(), newMatcher(q)
	var seqs []uint64
	var last uint64
	found := false
	// View copies the events under the lock and releases it, so the check runs outside it.
	if view := s.store.View(id); view != nil {
		found = true
		for i := range view.Events {
			if ctx.Err() != nil {
				return
			}
			e := &view.Events[i]
			last = max(last, e.Seq)
			if m.event(e) {
				seqs = append(seqs, e.Seq)
			}
		}
	}
	if s.archive != nil {
		disk, diskLast, ok, err := s.archive.Find(ctx, id, m.event, m.text)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Part of the history was not read. An answer without it could say "no match"
			// where there is one.
			slog.Debug("sessionapi: archive search failed", "error", err, "sessionID", id)
			http.Error(w, `{"error":"archive read failed"}`, http.StatusInternalServerError)
			return
		}
		found = found || ok
		seqs, last = append(seqs, disk...), max(last, diskLast)
	}
	if !found {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	if seqs == nil {
		seqs = []uint64{}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(searchResponse{Seqs: seqs, LastSeq: last, SchemaVersion: SchemaVersion}); err != nil {
		slog.Debug("sessionapi: search encode failed", "error", err, "sessionID", id)
	}
}

// matcher is one search: q folded, and what each long string gave. The store and the archive
// reader share one copy of a long string between the events that repeat it, so the matcher folds
// each copy one time, keyed by its address and length. A string is not hashed for the key,
// because hashing it costs as much as the check. A key holds its string in memory until the
// search ends.
type matcher struct {
	q    string
	seen map[strKey]bool
	json map[strKey]bool
	keys map[string]bool
}

type strKey struct {
	p *byte
	n int
}

func newMatcher(q string) *matcher {
	return &matcher{q: strings.Map(unicode.ToLower, q),
		seen: map[strKey]bool{}, json: map[strKey]bool{}, keys: map[string]bool{}}
}

// text reports whether s holds the query, folded as agentop's find.Contains folds: rune by rune,
// with unicode.ToLower.
func (m *matcher) text(s string) bool {
	if len(s) < session.InternMinLen {
		return strings.Contains(strings.Map(unicode.ToLower, s), m.q)
	}
	k := strKey{unsafe.StringData(s), len(s)}
	r, ok := m.seen[k]
	if !ok {
		r = strings.Contains(strings.Map(unicode.ToLower, s), m.q)
		m.seen[k] = r
	}
	return r
}

// key reports whether a key name holds the query. The same few names repeat in every event.
func (m *matcher) key(k string) bool {
	r, ok := m.keys[k]
	if !ok {
		r = m.text(k)
		m.keys[k] = r
	}
	return r
}

// field reports whether a string field, as the detail view shows it, holds the query in its key
// or its value. An empty value is omitted from the JSON, so the view shows neither.
func (m *matcher) field(key, v string) bool { return v != "" && (m.key(key) || m.text(v)) }

// event reports whether a field that summarizeEvent drops holds the query, where the detail view
// shows that field for e's phase. agentop checks the rest of the event in the summary it holds.
//
// The key names are the ones the detail view shows: a list's own name, and the names inside each
// entry. The view nests each history tool call under its message as toolCalls, so the name
// historyToolCalls is not matched.
func (m *matcher) event(e *model.SessionEvent) bool {
	resp := e.Phase == model.SessionResponse
	if inf := e.Inference; inf != nil {
		switch {
		case detailShows("inference", resp, "messages") && m.messages(inf.Messages),
			detailShows("inference", resp, "tools") && m.tools(inf.Tools),
			detailShows("inference", resp, "toolResults") && m.results(inf.ToolResults),
			detailShows("inference", resp, "historyToolCalls") && m.calls(inf.HistoryToolCalls),
			detailShows("inference", resp, "toolCalls") && m.calls(inf.ToolCalls):
			return true
		}
	}
	if p := e.MCP; p != nil {
		if detailShows("mcp", resp, "params") && len(p.Params) > 0 && (m.key("params") || m.value(p.Params)) {
			return true
		}
		if detailShows("mcp", resp, "result") && len(p.Result) > 0 && (m.key("result") || m.value(p.Result)) {
			return true
		}
	}
	a := e.A2A
	return a != nil && detailShows("a2a", resp, "artifact") && m.field("artifact", a.Artifact)
}

func (m *matcher) messages(ms []model.InferenceMessage) bool {
	if len(ms) == 0 {
		return false
	}
	if m.key("messages") {
		return true
	}
	for i := range ms {
		x := &ms[i]
		if m.field("role", x.Role) || m.field("content", x.Content) || m.field("toolUseId", x.ToolUseID) ||
			m.field("toolCallNames", x.ToolCallNames) || m.field("toolUseIds", x.ToolUseIDs) ||
			(x.ContentBytes != 0 && m.field("contentBytes", strconv.Itoa(x.ContentBytes))) {
			return true
		}
	}
	return false
}

func (m *matcher) tools(ts []model.InferenceTool) bool {
	if len(ts) == 0 {
		return false
	}
	if m.key("tools") {
		return true
	}
	for i := range ts {
		x := &ts[i]
		if m.field("name", x.Name) || m.field("description", x.Description) ||
			(x.Parameters != "" && (m.key("parameters") || m.schema(string(x.Parameters)))) {
			return true
		}
	}
	return false
}

// calls is a list of tool calls: the response's own, or a request's history, which the detail
// view shows under each message as toolCalls.
func (m *matcher) calls(cs []model.InferenceToolCall) bool {
	if len(cs) == 0 {
		return false
	}
	if m.key("toolCalls") {
		return true
	}
	for i := range cs {
		x := &cs[i]
		if m.field("id", x.ID) || m.field("name", x.Name) || m.field("arguments", x.Arguments) {
			return true
		}
	}
	return false
}

func (m *matcher) results(rs []model.InferenceToolResult) bool {
	if len(rs) == 0 {
		return false
	}
	if m.key("toolResults") {
		return true
	}
	for i := range rs {
		x := &rs[i]
		if m.field("toolUseId", x.ToolUseID) || m.field("content", x.Content) ||
			(x.IsError && (m.key("isError") || m.key("true"))) {
			return true
		}
	}
	return false
}

// schema is a tool's parameters: JSON, which the detail tree shows as keys and values. Each
// distinct schema is walked one time.
func (m *matcher) schema(raw string) bool {
	k := strKey{unsafe.StringData(raw), len(raw)}
	r, ok := m.json[k]
	if !ok {
		r = archive.MatchJSON(raw, m.text)
		m.json[k] = r
	}
	return r
}

// value is a decoded JSON value (MCP params and results): its keys and values, as the detail
// tree writes them.
func (m *matcher) value(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if m.key(k) || m.value(e) {
				return true
			}
		}
		return false
	case []any:
		for _, e := range x {
			if m.value(e) {
				return true
			}
		}
		return false
	case string:
		return m.text(x)
	}
	b, err := json.Marshal(v) // a number, a bool or null, as JSON writes it
	return err == nil && m.text(string(b))
}
```

- [ ] **Step 4: Register the route**

In `core/sessionapi/server.go`, after `mux.HandleFunc("GET /v1/sessions/{id}/timeline", s.handleTimeline)`, add:

```go
	mux.HandleFunc("GET /v1/sessions/{id}/search", s.handleSearch)
```

Read `handleIndex`. If it lists the routes by hand, add a line for this route in the same style.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOWORK=off go -C core test ./sessionapi -run 'TestSearch_|TestDetailKeys' -count=1 -race`
Expected: PASS. `TestDocCoversEveryRegisteredRoute` FAILS until Step 6.

- [ ] **Step 6: Document the route**

In `docs/session-api.md`, in the Endpoints table, after the `GET /v1/sessions/{id}/timeline` row, add:

```
| `GET /v1/sessions/{id}/search` | `application/json` | The seqs of the session's events whose bodies or capture hold `?q=`, as the detail view shows them (see below) |
```

After the `GET /v1/sessions/{id}/timeline` section (before `## DELETE /v1/sessions`), add:

````markdown
## `GET /v1/sessions/{id}/search` — find the events whose bodies hold a text

agentop's events pane uses this for its `details` search. The events list carries
`view=summary`, which drops the bodies, so agentop checks the summaries it holds, and this
route checks what the summary drops.

```sh
curl -s 'localhost:9094/v1/sessions/<id>/search?q=retry%20budget'
```

```json
{"seqs":[12,14,40],"lastSeq":980,"schemaVersion":1}
```

- `seqs` is ascending: every event of the session that matches, in memory and in the archive.
  `lastSeq` is the newest seq the pass read. A client that holds a newer event asks again.
- The match is a case-insensitive substring, folded rune by rune with `unicode.ToLower`, as
  agentop's `/` folds.
- **What it reads**, only where the event detail view shows it for the event's phase
  (`sessionapi.DetailKeys`):
  - request: `inference.messages`, `inference.tools` (a tool's `parameters` as JSON),
    `inference.toolResults`, the history tool calls, `mcp.params`
  - response: `inference.toolCalls`, `mcp.result`, `a2a.artifact`
  - the key names in those parts, as the view shows them
  - where the archive holds a capture: its body blocks (keys and values when a block is JSON,
    its text when not), the text between blocks, the headers, the query, and a response's
    status. Image and document blocks and binary bodies are skipped: the view shows them as
    labels.
- **It does not read** the fields the summary keeps (agentop checks those), the labels agentop
  writes itself in the `raw` part (`forwarded as sent`, `[image … KB]`), or a match that spans a
  key and its value.
- **Errors:** an empty `q` is `400` (`{"error":"q is required"}`). An unknown session is `404`
  with `{"error":"session not found"}`. A proxy that predates the route answers the mux's
  plain-text `404`, which is how a client tells the two apart. An archive read that fails is
  `500`: an answer without part of the history could say "no match" where there is one.
- **Cost:** one pass. A long string that many events share is checked one time. On a 10,767-event
  laptop session, the pass took about 4 s and 133 MB of heap, about the same as one read of the
  archive. The pass stops when the client closes the connection.
````

Replace the cost line's figures with the figures Task 8 measures, if they differ.

- [ ] **Step 7: The summary comment names the reader that exists**

In `core/sessionapi/summary.go`, in `summarizeEvent`'s comment, replace the paragraph that starts `// THE RULE: a field may be dropped only if NEITHER` with:

```go
// THE RULE: a field may be dropped only if NEITHER the events table nor agentop's details
// search reads it. The search is the half that is easy to forget, and forgetting it is silent:
// a search for completion text simply stops matching. agentop checks the summary rows it holds
// with detailHas (cmd/agentop/tui/search_details.go), and the search route (search.go) checks
// only what this function drops. A field this function starts to drop must be added to the
// route, or the search stops seeing it. TestSummarizeEvent_KeepsEverythingTheFilterSearches
// names each field the local check reads, so this cannot drift again.
```

In `core/sessionapi/summary_test.go`, in the comments of `TestSummarizeEvent_KeepsEverythingTheFilterSearches` and the test just above it, replace `eventHaystack` and `eventHaystack / matchEventRow` and `eventHaystack/matchEventRow` with `agentop's detailHas`. Change no code.

- [ ] **Step 8: Run the package, vet and gofmt**

Run: `GOWORK=off go -C core test ./sessionapi ./session/... -count=1 && GOWORK=off go -C core vet ./... && gofmt -l core`
Expected: `ok` for each package, no vet output. gofmt prints only the throwaway `zz_measure_test.go`, if it is still there.

- [ ] **Step 9: Commit**

```bash
git add core/sessionapi docs/session-api.md
git commit -s -m "Feat: Add a session search route to the session API

GET /v1/sessions/{id}/search?q= answers the seqs of the events whose
bodies or capture hold the text, as the event detail view shows them.
It checks memory and the archive in one pass, and a long string that
many events share one time. agentop checks the summary fields itself.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Task 7: agentop asks the proxy

**Files:**
- Create: `cmd/agentop/apiclient/search.go`, `cmd/agentop/apiclient/search_test.go`
- Modify: `cmd/agentop/tui/search_details.go` (replace everything between `detailHas` and `detailsBoxWidth`)
- Modify: `cmd/agentop/tui/app.go` (the five `detail*` model fields, the two scan `case` arms in `Update`)
- Modify: `cmd/agentop/tui/search.go` (Enter, `searchStep`)
- Modify: `cmd/agentop/tui/keys.go` (the Esc check)
- Modify: `cmd/agentop/tui/search_details_test.go` (replace everything after the three `TestDetailHas_` tests)
- Create: `cmd/agentop/tui/search_details_parity_test.go`
- Modify: `cmd/agentop/README.md` (the `/` row's details sentence, the summary note)

**Interfaces:**
- Consumes: `detailHas` (Task 1). The route (Task 6). `nextMatch`, `eventsSurface`, `eventSession`, `keyOf`, `heldRaw`, `setFlash`, `streamDefaultTimeout` (apiclient).
- Produces: `apiclient.ErrNoSearch`, `apiclient.SearchResult{Seqs []uint64; LastSeq uint64}`, `(*apiclient.Client).SearchSession(ctx, id, q string) (SearchResult, error)`. In `tui`: `sendDetailSearch(forward, inclusive bool) tea.Cmd`, `stepDetails(forward bool) tea.Cmd`, `detailSearching() bool`, `cancelDetailSearch()`, `resetDetailSearch()`, `detailSearchStatus() string`, `type detailSearchMsg`.

- [ ] **Step 1: Write the failing client tests**

Create `cmd/agentop/apiclient/search_test.go`:

```go
package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestSearchSession_EscapesTheQuery(t *testing.T) {
	var gotID, gotQ string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotQ = r.PathValue("id"), r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{"seqs":[1,4],"lastSeq":9,"schemaVersion":1}`))
	}))
	defer ts.Close()
	mux := http.NewServeMux()
	mux.Handle("GET /v1/sessions/{id}/search", ts.Config.Handler)
	ts.Config.Handler = mux

	res, err := New(ts.URL).SearchSession(context.Background(), "s/1", "a&b #c%d e")
	if err != nil {
		t.Fatal(err)
	}
	if gotID != "s/1" || gotQ != "a&b #c%d e" {
		t.Errorf("proxy got id %q q %q", gotID, gotQ)
	}
	if !slices.Equal(res.Seqs, []uint64{1, 4}) || res.LastSeq != 9 {
		t.Errorf("result %+v", res)
	}
}

// The route answers an unknown session with a JSON body. A proxy without the route answers
// with the mux's plain text.
func TestSearchSession_TellsTheTwo404sApart(t *testing.T) {
	for _, tc := range []struct {
		body string
		want error
	}{
		{"404 page not found\n", ErrNoSearch},
		{`{"error":"session not found"}` + "\n", ErrNotFound},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, tc.body[:len(tc.body)-1], http.StatusNotFound)
		}))
		_, err := New(ts.URL).SearchSession(context.Background(), "s", "q")
		ts.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("body %q: err %v, want %v", tc.body, err, tc.want)
		}
	}
}
```

Note: `TestSearchSession_EscapesTheQuery` wraps the handler in a mux so `r.PathValue("id")` decodes the escaped id as the real server does.

- [ ] **Step 2: Run them to verify they fail**

Run: `GOWORK=off go -C cmd/agentop test ./apiclient -run TestSearchSession -count=1`
Expected: FAIL to compile with `SearchSession undefined`.

- [ ] **Step 3: Write the client call**

Create `cmd/agentop/apiclient/search.go`:

```go
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ErrNoSearch is the answer of a proxy that has no search route, because it predates it.
var ErrNoSearch = errors.New("apiclient: this Cortex has no details search")

// SearchResult is GET /v1/sessions/{id}/search: the seqs of the session's events whose bodies
// or capture hold the query, ascending, and the newest seq the proxy read.
type SearchResult struct {
	Seqs    []uint64 `json:"seqs"`
	LastSeq uint64   `json:"lastSeq"`
}

// maxSearchBody bounds the answer: one int for each event of a session.
const maxSearchBody = 16 << 20

// SearchSession asks the proxy which of session id's events hold q in what view=summary drops.
// It reads a 404's body itself, which getBody does not do: the route answers an unknown session
// with a JSON body (ErrNotFound), and a proxy without the route with the mux's plain text
// (ErrNoSearch). A long session takes seconds, so the default deadline is the stream's.
func (c *Client) SearchSession(ctx context.Context, id, q string) (SearchResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, streamDefaultTimeout)
		defer cancel()
	}
	path := fmt.Sprintf("/v1/sessions/%s/search?q=%s", url.PathEscape(id), url.QueryEscape(q))
	req, err := http.NewRequestWithContext(ctx, "GET", c.endpoint+path, nil)
	if err != nil {
		return SearchResult{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return SearchResult{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchBody))
	if err != nil {
		return SearchResult{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var out SearchResult
		if err := json.Unmarshal(body, &out); err != nil {
			return SearchResult{}, fmt.Errorf("%s: decode: %w", path, err)
		}
		return out, nil
	case http.StatusNotFound:
		if bytes.Contains(body, []byte(`"session not found"`)) {
			return SearchResult{}, fmt.Errorf("%s: %w", path, ErrNotFound)
		}
		return SearchResult{}, fmt.Errorf("%s: %w", path, ErrNoSearch)
	}
	return SearchResult{}, fmt.Errorf("%s: status %d", path, resp.StatusCode)
}
```

- [ ] **Step 4: Run them to verify they pass**

Run: `GOWORK=off go -C cmd/agentop test ./apiclient -run TestSearchSession -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing tui tests**

In `cmd/agentop/tui/search_details_test.go`, delete everything after `TestDetailHas_AgreesWithThePane` (the scan helpers and tests, `keysModel`, `runAll` and the `TestDetailSearch_` tests from revision 1). Fix the import block to what the code below uses. Then append:

```go
// fakeSearch is a proxy with the search route. answer is the seqs for each session id, last the
// lastSeq it reports. noRoute answers as a proxy without the route. It counts the searches and
// every other request, because the search must fetch no event.
type fakeSearch struct {
	mu       sync.Mutex
	answer   map[string][]uint64
	last     map[string]uint64
	noRoute  bool
	searches int
	other    int
}

func (f *fakeSearch) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

func (f *fakeSearch) counts() (searches, other int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searches, f.other
}

func (f *fakeSearch) client(t *testing.T) *apiclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions/{id}/search", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.searches++
		if f.noRoute {
			http.NotFound(w, r)
			return
		}
		id := r.PathValue("id")
		seqs := f.answer[id]
		if seqs == nil {
			seqs = []uint64{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"seqs": seqs, "lastSeq": f.last[id], "schemaVersion": 1})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.other++
		f.mu.Unlock()
		http.NotFound(w, r)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return apiclient.New(ts.URL)
}

// searchModel is an events pane of n request rows, seqs 1 to n, of session "s", against f, with
// the search prompt production builds.
func searchModel(t *testing.T, n int, f *fakeSearch) *model {
	t.Helper()
	resetSettingsForTest(t)
	events := eventSeq(n, "e")
	for i := range events {
		events[i].Seq = uint64(i + 1)
	}
	if f.last == nil {
		f.last = map[string]uint64{"s": uint64(n)}
	}
	m := newEventsPaneModel(events)
	m.ctx, m.client, m.serverProjects = context.Background(), f.client(t), true
	m.searchInput = newSearchInput()
	m.width, m.height = 200, 40
	m.eventColumns = defaultColumnSelection()
	m.layout()
	m.rebuildEventsTable()
	return m
}

// runAll runs a command and every command its messages lead to, through Update.
func runAll(m *model, cmd tea.Cmd) {
	for i := 0; cmd != nil && i < 100; i++ {
		_, cmd = m.Update(cmd())
	}
}

// detailsQuery opens the prompt on the events pane, turns the box on, and types q.
func detailsQuery(m *model, q string) {
	typeKeys(m, "/")
	press(m, tea.KeyTab)
	typeKeys(m, q)
}

func enter(m *model) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return cmd
}

func key(m *model, r rune) { _, cmd := m.Update(keyRune(r)); runAll(m, cmd) }

func TestDetailSearch_TabTogglesTheBoxOnlyOnEvents(t *testing.T) {
	m := searchModel(t, 3, &fakeSearch{})
	typeKeys(m, "/")
	press(m, tea.KeyTab)
	if !m.searchDetails {
		t.Fatal("Tab on the events prompt did not turn the box on")
	}
	if v := stripANSI(m.searchPromptView()); !strings.Contains(v, "[x] details  (tab)") {
		t.Errorf("prompt = %q, want the box on", v)
	}
	press(m, tea.KeyTab)
	if m.searchDetails {
		t.Error("a second Tab did not turn the box off")
	}
	press(m, tea.KeyEsc)
	// The prompt open on another pane: Tab there changes nothing.
	m.pane, m.searching, m.searchPane = paneSessions, true, paneSessions
	press(m, tea.KeyTab)
	if m.searchDetails {
		t.Error("Tab on the sessions prompt turned the events box on")
	}
	if v := stripANSI(m.searchPromptView()); strings.Contains(v, "details") {
		t.Errorf("prompt = %q, want no box off the events pane", v)
	}
}

// Enter lands on the first match at or after the row where `/` was pressed. n and N step and
// wrap with no new search. No event is fetched.
func TestDetailSearch_EnterLandsAndNSteps(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {3, 7}}}
	m := searchModel(t, 10, f)
	setCursorVisible(&m.eventsTbl, 1)
	detailsQuery(m, "needle")
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("typing moved the cursor to %d, want 1", c)
	}
	if st := m.searchStatus(paneEvents); st != "[/needle details: Enter to search]" {
		t.Errorf("status while typing = %q", st)
	}
	cmd := enter(m)
	if st := m.searchStatus(paneEvents); st != "[/needle details: searching…]" {
		t.Errorf("status while searching = %q", st)
	}
	runAll(m, cmd)
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Fatalf("Enter landed on %d, want 2 (seq 3)", c)
	}
	if st := m.searchStatus(paneEvents); st != "[/needle details 1/2]" {
		t.Errorf("status = %q", st)
	}
	for _, step := range []struct {
		key  rune
		want int
	}{{'n', 6}, {'n', 2}, {'N', 6}} {
		key(m, step.key)
		if c := m.eventsTbl.Cursor(); c != step.want {
			t.Errorf("%c landed on %d, want %d", step.key, c, step.want)
		}
	}
	if s, o := f.counts(); s != 1 || o != 0 {
		t.Errorf("searches %d, other requests %d, want 1 and 0", s, o)
	}
}

// A field the summary keeps matches with no help from the proxy.
func TestDetailSearch_TheSummaryMatchesLocally(t *testing.T) {
	m := searchModel(t, 6, &fakeSearch{})
	m.events["s"][4].Host = "needle-host.example"
	m.rebuildEventsTable()
	detailsQuery(m, "needle-host")
	runAll(m, enter(m))
	if c := m.eventsTbl.Cursor(); c != 4 {
		t.Errorf("cursor %d, want 4", c)
	}
}

func TestDetailSearch_NoMatch(t *testing.T) {
	m := searchModel(t, 4, &fakeSearch{})
	setCursorVisible(&m.eventsTbl, 2)
	detailsQuery(m, "absent")
	runAll(m, enter(m))
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Errorf("cursor %d, want 2", c)
	}
	if st := m.searchStatus(paneEvents); st != "[/absent details: no match]" {
		t.Errorf("status = %q", st)
	}
}

func TestDetailSearch_AProxyWithoutTheRoute(t *testing.T) {
	m := searchModel(t, 4, &fakeSearch{noRoute: true})
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	if st := m.searchStatus(paneEvents); st != "[/needle details: this Cortex has no details search]" {
		t.Errorf("status = %q", st)
	}
}

// An event newer than the answer's lastSeq makes n search again.
func TestDetailSearch_NewEventsSearchAgain(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2}}}
	m := searchModel(t, 4, f)
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	key(m, 'n')
	if s, _ := f.counts(); s != 1 {
		t.Fatalf("searches %d, want 1: nothing new arrived", s)
	}
	more := eventSeq(1, "new")
	more[0].Seq = 5
	m.events["s"] = append(m.events["s"], more...)
	m.rebuildEventsTable()
	f.set(func() { f.last["s"] = 5 })
	key(m, 'n')
	if s, _ := f.counts(); s != 2 {
		t.Errorf("searches %d, want 2 after a new event", s)
	}
}

// Esc while the search runs drops the answer. The cursor has not moved, and Esc does not
// leave the pane.
func TestDetailSearch_EscDropsTheAnswer(t *testing.T) {
	m := searchModel(t, 6, &fakeSearch{answer: map[string][]uint64{"s": {5}}})
	setCursorVisible(&m.eventsTbl, 1)
	detailsQuery(m, "needle")
	cmd := enter(m)
	if !m.detailSearching() {
		t.Fatal("Enter started no search")
	}
	press(m, tea.KeyEsc)
	if m.pane != paneEvents || m.detailSearch != nil {
		t.Fatalf("pane %v search %+v, want the events pane and no search", m.pane, m.detailSearch)
	}
	runAll(m, cmd)
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("cursor %d, want 1", c)
	}
}

func TestDetailSearch_LeavingThePaneDropsTheAnswer(t *testing.T) {
	m := searchModel(t, 6, &fakeSearch{answer: map[string][]uint64{"s": {5}}})
	setCursorVisible(&m.eventsTbl, 1)
	detailsQuery(m, "needle")
	cmd := enter(m)
	m.pane = paneSessions
	runAll(m, cmd)
	if m.detailSearch != nil {
		t.Errorf("search %+v, want none after the pane changed", m.detailSearch)
	}
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("cursor %d, want 1", c)
	}
}

// Matches older than the rows agentop holds are counted, for `o`.
func TestDetailSearch_CountsOlderMatches(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2, 13}}, last: map[string]uint64{"s": 15}}
	m := searchModel(t, 5, f)
	for i := range m.events["s"] {
		m.events["s"][i].Seq = uint64(i + 11)
	}
	m.rebuildEventsTable()
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	if st := m.searchStatus(paneEvents); st != "[/needle details 1/1] · 1 older (o)" {
		t.Errorf("status = %q", st)
	}
}

// A linked bucket's events keep their own session id and seqs. Each id is searched.
func TestDetailSearch_LinkedBucketIsSearchedByItsOwnID(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"bucket": {1}}, last: map[string]uint64{"s": 4, "bucket": 1}}
	m := searchModel(t, 4, f)
	m.events["s"][2].SessionID, m.events["s"][2].Seq = "bucket", 1
	m.rebuildEventsTable()
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	if e := m.selectedEvent(); e == nil || e.SessionID != "bucket" {
		t.Errorf("cursor on %+v, want the bucket's event", e)
	}
	if s, _ := f.counts(); s != 2 {
		t.Errorf("searches %d, want one for each session id", s)
	}
}

// Opening a row while the box is on gives the detail search the same query.
func TestDetailSearch_OpeningARowCarriesTheQuery(t *testing.T) {
	m := searchModel(t, 5, &fakeSearch{answer: map[string][]uint64{"s": {4}}})
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	press(m, tea.KeyEnter)
	if m.pane != paneDetail || m.search[paneDetail] != "needle" {
		t.Errorf("pane %v detail query %q, want detail and needle", m.pane, m.search[paneDetail])
	}
}

func TestDetailSearch_BoxOffKeepsTheTableSearch(t *testing.T) {
	f := &fakeSearch{}
	m := searchModel(t, 5, f)
	typeKeys(m, "/e3")
	press(m, tea.KeyEnter)
	key(m, 'n')
	if s, _ := f.counts(); s != 0 || m.detailSearch != nil {
		t.Errorf("box off: searches %d, search %+v, want neither", s, m.detailSearch)
	}
}

func TestDetailSearch_ResetClearsEverything(t *testing.T) {
	m := searchModel(t, 5, &fakeSearch{answer: map[string][]uint64{"s": {3}}})
	detailsQuery(m, "needle")
	cmd := enter(m)
	m.resetDetailSearch()
	if m.searchDetails || m.detailSearch != nil {
		t.Errorf("after reset: box %v search %+v", m.searchDetails, m.detailSearch)
	}
	runAll(m, cmd)
	if m.detailSearch != nil {
		t.Error("an answer from before the reset came back")
	}
}
```

Create `cmd/agentop/tui/search_details_parity_test.go`:

```go
package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// THE RULE, END TO END: "the proxy's answer, or detailHas on the summary row" is "detailHas on
// the full event with its capture", for each event and each query. The proxy is the real
// session API, with a store and an archive. The queries avoid the gaps the spec names.
func TestDetailSearch_ParityWithTheDetailView(t *testing.T) {
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	pad := " " + strings.Repeat("filler ", 12)
	inf := pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "alpha prompt" + pad},
			{Role: "assistant", Content: "beta reply", ToolCallNames: "bash", ToolUseIDs: "t1"},
			{Role: "user", ToolUseIDs: "t1"},
		},
		Tools: []pipeline.InferenceTool{{Name: "bash", Description: "run gamma",
			Parameters: `{"type":"object","properties":{"deltafield":{"type":"string"}}}`}},
		ToolResults:      []pipeline.InferenceToolResult{{ToolUseID: "t1", Content: "epsilon output"}},
		HistoryToolCalls: []pipeline.InferenceToolCall{{ID: "t1", Name: "bash", Arguments: `{"cmd":"zeta"}`}},
	}
	resp := inf
	resp.Completion = "eta answer"
	resp.ToolCalls = []pipeline.InferenceToolCall{{ID: "t2", Name: "read", Arguments: `{"path":"/theta"}`}}
	now := time.Now()
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"iota raw` + pad + `"}]}]}`
	for _, ev := range []struct {
		e pipeline.SessionEvent
		c *pipeline.RequestCapture
	}{
		{pipeline.SessionEvent{At: now, Phase: pipeline.SessionRequest, Inference: &inf},
			&pipeline.RequestCapture{Original: []byte(body), Header: http.Header{"X-Kappa": {"lambda"}}}},
		{pipeline.SessionEvent{At: now, Phase: pipeline.SessionResponse, Inference: &resp},
			&pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 200, Body: []byte("data: mu stream" + pad)}}},
		{pipeline.SessionEvent{At: now, Phase: pipeline.SessionRequest,
			MCP: &pipeline.MCPExtension{Method: "tools/call", Params: map[string]any{"path": "/nu"}}}, nil},
		{pipeline.SessionEvent{At: now, Phase: pipeline.SessionResponse,
			MCP: &pipeline.MCPExtension{Method: "tools/call", Result: map[string]any{"text": "xi"}}}, nil},
	} {
		store.AppendCapture("s1", ev.e, ev.c)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(sessionapi.New(":0", store, sessionapi.WithArchive(a)).Server().Handler)
	t.Cleanup(func() { ts.Close(); store.Close() })

	ctx, c := context.Background(), apiclient.New(ts.URL)
	page, err := c.GetSessionPage(ctx, "s1", 0, 100)
	if err != nil || len(page.Events) != 4 {
		t.Fatalf("page: %d events, err %v", len(page.Events), err)
	}
	for _, q := range []string{"alpha", "beta reply", "gamma", "deltafield", "epsilon", "zeta", "eta answer",
		"theta", "iota", "lambda", "x-kappa", "mu stream", "/nu", "xi", "toolUseId", "role", "bash", "absent"} {
		res, err := c.SearchSession(ctx, "s1", q)
		if err != nil {
			t.Fatal(err)
		}
		for i := range page.Events {
			sum := &page.Events[i]
			full, raw, err := c.GetEventRaw(ctx, "s1", sum.Seq)
			if err != nil {
				t.Fatal(err)
			}
			got := slices.Contains(res.Seqs, sum.Seq) || detailHas(eventRow{event: sum}, q)
			if want := detailHas(eventRow{event: full, raw: raw}, q); got != want {
				t.Errorf("%q, seq %d: proxy or summary = %v, the detail view = %v", q, sum.Seq, got, want)
			}
		}
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `GOWORK=off go -C cmd/agentop test ./tui -run 'TestDetailSearch' -count=1`
Expected: FAIL to compile (`m.detailSearch undefined`, `m.detailSearching undefined`).

- [ ] **Step 7: Replace the scan with the search**

In `cmd/agentop/tui/search_details.go`:
- Replace the header comment's last two sentences (from `The events list carries summaries`) with: `agentop checks the summary rows it holds, and the proxy's search route checks what the summary drops and the capture. A row matches when either side matches it. Nothing is fetched to check a row.`
- Delete everything from `// detailScanBatch is how many events` down to the end of `resetDetailSearch`, and the old `detailSearchStatus`. Keep `detailHas`, `detailsBoxWidth` and `searchPromptView`.
- Fix the imports to what the file then uses, and add this code after `detailHas`:

```go
// detailSearch is one details search: the request in flight, or its answer.
type detailSearch struct {
	gen   uint64
	query string
	state detailSearchState
	// noRoute is an answer from a proxy without the search route.
	noRoute bool
	// hits is the held rows that either side matched, by event key.
	hits map[eventKey]bool
	// last is the newest seq the proxy read, for each session id it was asked about.
	last map[string]uint64
	// older counts the proxy's matches older than the rows agentop holds.
	older int
	// forward and inclusive are the step to take when the answer lands.
	forward, inclusive bool
}

type detailSearchState int

const (
	searchSending detailSearchState = iota
	searchReady
	searchFailed
)

// detailSearchMsg is the answer to one details search.
type detailSearchMsg struct {
	gen   uint64
	hits  map[eventKey]bool
	last  map[string]uint64
	older int
	err   error
}

// sendDetailSearch sends the events pane's committed query, and takes the step when the answer
// lands: from the cursor, the cursor's own row included when inclusive (Enter).
func (m *model) sendDetailSearch(forward, inclusive bool) tea.Cmd {
	m.cancelDetailSearch()
	q := m.search[paneEvents]
	if q == "" || m.client == nil {
		return nil
	}
	m.detailSearch = &detailSearch{gen: m.detailSearchGen, query: q, forward: forward, inclusive: inclusive}
	// The command runs on its own goroutine, and replaceHeldEvent writes the held events while it
	// runs. So it gets a copy of each held row.
	rows := make([]eventRow, 0, len(m.visibleRows))
	for _, r := range m.visibleRows {
		if r.event == nil {
			continue // a subagent block's header row
		}
		e := *r.event
		r.event = &e
		r.raw, _ = m.heldRaw(&e)
		rows = append(rows, r)
	}
	ctx, client, gen, sel := m.ctx, m.client, m.detailSearchGen, m.selectedSess
	return func() tea.Msg { return runDetailSearch(ctx, client, gen, sel, rows, q) }
}

// runDetailSearch asks the proxy about each session id among rows, and checks each row's summary
// itself. A linked bucket's events have their own id and seqs, so each id is asked one time.
func runDetailSearch(ctx context.Context, c *apiclient.Client, gen uint64, sel string, rows []eventRow, q string) detailSearchMsg {
	msg := detailSearchMsg{gen: gen, hits: map[eventKey]bool{}, last: map[string]uint64{}}
	held, oldest := map[string]map[uint64]bool{}, map[string]uint64{}
	for _, r := range rows {
		s := eventSession(sel, r.event)
		if held[s] == nil {
			held[s] = map[uint64]bool{}
			oldest[s] = r.event.Seq
		}
		oldest[s] = min(oldest[s], r.event.Seq)
	}
	found := map[string]map[uint64]bool{}
	for s := range held {
		res, err := c.SearchSession(ctx, s, q)
		if err != nil {
			msg.err = err
			return msg
		}
		found[s] = map[uint64]bool{}
		for _, seq := range res.Seqs {
			found[s][seq] = true
			if seq < oldest[s] {
				msg.older++
			}
		}
		msg.last[s] = res.LastSeq
	}
	for _, r := range rows {
		if found[eventSession(sel, r.event)][r.event.Seq] || detailHas(r, q) {
			msg.hits[keyOf(r.event)] = true
		}
	}
	return msg
}

// applyDetailSearch takes an answer: it drops one for an older search, or one that lands after
// the operator left the pane, and otherwise takes the step it was sent for.
func (m *model) applyDetailSearch(msg detailSearchMsg) {
	d := m.detailSearch
	if d == nil || d.gen != msg.gen || d.state != searchSending {
		return
	}
	if m.pane != paneEvents {
		m.cancelDetailSearch()
		return
	}
	if msg.err != nil {
		d.state, d.noRoute = searchFailed, errors.Is(msg.err, apiclient.ErrNoSearch)
		if !d.noRoute {
			m.setFlash("details search failed: " + msg.err.Error())
		}
		return
	}
	d.hits, d.last, d.older, d.state = msg.hits, msg.last, msg.older, searchReady
	m.stepDetailMatch(d.forward, d.inclusive)
}

// detailMatchRows is the visible rows the answer matched, ascending.
func (m *model) detailMatchRows() []int {
	var out []int
	for i, r := range m.visibleRows {
		if r.event != nil && m.detailSearch.hits[keyOf(r.event)] {
			out = append(out, i)
		}
	}
	return out
}

func (m *model) stepDetailMatch(forward, inclusive bool) {
	if i, ok := nextMatch(m.detailMatchRows(), m.eventsTbl.Cursor(), forward, inclusive); ok {
		eventsSurface{m}.reveal(i)
	}
}

// stepDetails is n and N with the box on: the next match at once, or a new search first when
// there is no answer for the query, the last one failed, or the session has grown since.
func (m *model) stepDetails(forward bool) tea.Cmd {
	d := m.detailSearch
	switch {
	case d == nil || d.query != m.search[paneEvents] || d.state == searchFailed || m.detailSearchStale():
		return m.sendDetailSearch(forward, false)
	case d.state == searchSending:
		return nil
	}
	m.stepDetailMatch(forward, false)
	return nil
}

// detailSearchStale reports whether a held row is newer than what the proxy read for its session.
func (m *model) detailSearchStale() bool {
	for _, r := range m.visibleRows {
		if r.event == nil {
			continue
		}
		last, ok := m.detailSearch.last[eventSession(m.selectedSess, r.event)]
		if !ok || r.event.Seq > last {
			return true
		}
	}
	return false
}

func (m *model) detailSearching() bool {
	return m.detailSearch != nil && m.detailSearch.state == searchSending
}

// cancelDetailSearch drops the search and any answer still to come.
func (m *model) cancelDetailSearch() {
	m.detailSearchGen++
	m.detailSearch = nil
}

// resetDetailSearch clears the box with the events pane's query, when another session opens.
func (m *model) resetDetailSearch() {
	m.cancelDetailSearch()
	m.searchDetails = false
}

// detailSearchStatus is the footer's account of the details search.
func (m *model) detailSearchStatus() string {
	q := m.searchQuery(paneEvents)
	if q == "" {
		return ""
	}
	d := m.detailSearch
	switch {
	case m.searching && m.searchPane == paneEvents:
		return fmt.Sprintf("[/%s details: Enter to search]", q)
	case d == nil || d.query != q:
		return fmt.Sprintf("[/%s details]", q)
	case d.state == searchSending:
		return fmt.Sprintf("[/%s details: searching…]", q)
	case d.noRoute:
		return fmt.Sprintf("[/%s details: this Cortex has no details search]", q)
	case d.state == searchFailed:
		return fmt.Sprintf("[/%s details: search failed]", q)
	}
	older := ""
	if d.older > 0 {
		older = fmt.Sprintf(" · %d older (o)", d.older)
	}
	rows := m.detailMatchRows()
	if len(rows) == 0 {
		return fmt.Sprintf("[/%s details: no match]%s", q, older)
	}
	if k, ok := slices.BinarySearch(rows, m.eventsTbl.Cursor()); ok {
		return fmt.Sprintf("[/%s details %d/%d]%s", q, k+1, len(rows), older)
	}
	return fmt.Sprintf("[/%s details: %d matches]%s", q, len(rows), older)
}
```

- [ ] **Step 8: The model and the call sites**

In `cmd/agentop/tui/app.go`, replace the five fields `searchDetails` to `detailCheckedQ` and their comment with:

```go
	// searchDetails is the events pane's `details` box: with it on, Enter, n and N go to the
	// rows whose detail view shows the query. detailSearch is that search, in flight or
	// answered, and detailSearchGen numbers each one, so an answer for an older one is
	// dropped. See search_details.go.
	searchDetails   bool
	detailSearch    *detailSearch
	detailSearchGen uint64
```

Run `gofmt -w cmd/agentop/tui/app.go`. In `Update`, replace the `case detailScanFetchedMsg:` and `case detailScanNextMsg:` arms with:

```go
	case detailSearchMsg:
		m.applyDetailSearch(msg)
		return m, nil
```

In `cmd/agentop/tui/search.go`: in `searchKey`'s Enter case, replace `return m.startDetailScan(true, true)` with `return m.sendDetailSearch(true, true)`. In `searchStep`, replace `return m.startDetailScan(forward, false)` with `return m.stepDetails(forward)`.

In `cmd/agentop/tui/keys.go`, replace the Esc check with:

```go
	// Esc while a details search runs drops its answer. Esc again backs out.
	if msg.String() == "esc" && m.pane == paneEvents && m.detailSearching() {
		m.cancelDetailSearch()
		return nil
	}
```

- [ ] **Step 9: Run the tui tests to verify they pass**

Run: `GOWORK=off go -C cmd/agentop test ./tui -run 'TestDetailSearch|TestDetailHas|TestSearch' -count=1 -race`
Expected: PASS. If the parity test fails for one query and event, read both sides' text for it before you change code. A difference that is one of the spec's named gaps is a test defect: change the query, and ledger it. Any other difference is a defect in the route or in `detailHas`.

- [ ] **Step 10: The README**

In `cmd/agentop/README.md`, in the `/` row, replace the sentence that starts ``agentop fetches one event at a time from the cursor`` and the two after it (to `Press `o` first for older ones`) with:

```
The proxy finds the matches in one pass over memory and the archive (`GET /v1/sessions/{id}/search`), and agentop checks the summary rows it holds itself, so nothing is fetched. The footer counts the matches, and also the matches in events older than the ones agentop holds: press `o` to load those. A Cortex that predates the search says so in the footer
```

In "How a timeline is fetched", replace `or from the events pane with the `details` box on, which fetches one event at a time and keeps none of them.` with `or from the events pane with the `details` box on, which asks the proxy and fetches no event.`

- [ ] **Step 11: Run the module, vet and gofmt**

Run `GOWORK=off go -C cmd/agentop test ./... -count=1` in the background (it takes more than 2 minutes), and read the result. Then: `GOWORK=off go -C cmd/agentop vet ./... && gofmt -l cmd/agentop`
Expected: `ok` for every package, no vet output, gofmt prints only the throwaway `zz_measure_test.go` if it is still there.

- [ ] **Step 12: Commit**

```bash
git add cmd/agentop/apiclient/search.go cmd/agentop/apiclient/search_test.go cmd/agentop/tui cmd/agentop/README.md
git commit -s -m "Feat: Ask the proxy for details matches in agentop

The details search sends one request for each session id among the
held rows, and checks the summary rows itself. Enter, n and N then
move with no fetch, and the footer counts the matches. n searches
again when new events arrived. A proxy without the route says so.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Task 8: Measure, clean up, review, open the PR

**Files:**
- Throwaway, never committed: `core/sessionapi/zz_measure_test.go`
- Delete: `core/session/archive/zz_measure_test.go`, `cmd/agentop/tui/zz_measure_test.go`, `/tmp/arch-measure`, `/tmp/arch-root`
- Modify only if the review finds a problem.

- [ ] **Step 1: Measure the real pass on the large session**

Copy the session into an archive root of its own, so `archive.Open` reads a copy and never the live archive:

```bash
mkdir -p /tmp/arch-root/data
cp -R ~/.cortex/sessions/data/7bff7270f623a17a3b3a424cd94e8a76 /tmp/arch-root/data/
```

Create `core/sessionapi/zz_measure_test.go`:

```go
package sessionapi

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/session/archive"
)

// Throwaway: not committed. The route's pass over one archived session, with the real matcher.
func TestZZMeasureSearch(t *testing.T) {
	root, id := os.Getenv("MEASURE_ROOT"), os.Getenv("MEASURE_SESSION")
	if root == "" {
		t.Skip()
	}
	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, q := range []string{"zz-no-such-text-zz", "worktree"} {
		runtime.GC()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)
		peak := base.HeapInuse
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(10 * time.Millisecond):
					var s runtime.MemStats
					runtime.ReadMemStats(&s)
					peak = max(peak, s.HeapInuse)
				}
			}
		}()
		m := newMatcher(q)
		start := time.Now()
		seqs, last, ok, err := a.Find(context.Background(), id, m.event, m.text)
		close(stop)
		t.Logf("q=%q ok=%v err=%v seqs=%d last=%d time=%s peakHeap=%dMB",
			q, ok, err, len(seqs), last, time.Since(start).Round(time.Millisecond), peak>>20)
	}
}
```

Run: `MEASURE_ROOT=/tmp/arch-root MEASURE_SESSION=1c22ef81-848f-4f1f-8eec-6610a1c8a200 GOWORK=off go -C core test ./sessionapi -run TestZZMeasureSearch -count=1 -v -timeout 580s 2>&1 | grep 'q='`
Expected: `ok=true err=<nil>`, `last=10767`, and for each query a time and a peak heap. Record both lines for the PR body. If the time is above 30 s or the peak heap above 400 MB, stop and report: the spec's cost claim does not hold.

Put the measured figures into the cost line of `docs/session-api.md` (Task 6, Step 6) if they differ from "about 4 s and 133 MB", and commit that as `Docs: Record the measured cost of the session search`.

- [ ] **Step 2: Delete the throwaways**

```bash
rm core/sessionapi/zz_measure_test.go core/session/archive/zz_measure_test.go cmd/agentop/tui/zz_measure_test.go
rm -r /tmp/arch-measure /tmp/arch-root
git status --short
```

Expected: `git status --short` shows no `zz_measure` file. The two `/tmp` copies held the operator's real conversations, so they must go.

- [ ] **Step 3: The whole branch**

Run in the background, and read each result:
- `GOWORK=off go -C core test ./... -count=1`
- `GOWORK=off go -C cmd/agentop test ./... -count=1`

Then: `GOWORK=off go -C core vet ./... && GOWORK=off go -C cmd/agentop vet ./... && gofmt -l core cmd/agentop && (cd core && GOWORK=off go mod tidy -diff) && (cd cmd/agentop && GOWORK=off go mod tidy -diff)`
Expected: every package `ok`, no output from vet, gofmt or tidy.

Then: `pre-commit run --files $(git diff --name-only $(git merge-base refs/base/search-bodies-1680 HEAD) HEAD)`
Expected: every hook passes.

- [ ] **Step 4: Final review**

Run the executing-plans final review: the review package, then a reviewer on the most capable model with the spec, this plan, the Review Focus above, and the ledger's `Ruling:` lines. Fix Critical and Important findings, each with a test that fails first. Ledger the minors.

- [ ] **Step 5: Push and open the PR**

```bash
git fetch -q https://github.com/rossoctl/cortex.git main:refs/base/search-bodies-1680
git merge-base --is-ancestor refs/base/search-bodies-1680 HEAD || git merge refs/base/search-bodies-1680
git push -u origin docs/search-bodies-1680
gh pr create -R rossoctl/cortex --title "Feat: Search event details from the agentop events pane" --body-file /tmp/pr-1680.md
```

Write `/tmp/pr-1680.md` first, in Simplified Technical English, with these parts:
1. **Problem:** the issue's problem, in two lines, and `Fixes #1680`.
2. **What changes:** the `details` box (Tab), Enter/`n`/`N`, the footer, the new route, the split between agentop's summary check and the proxy's check.
3. **Why the proxy searches:** the measurement table from the spec, and the two figures from Step 1.
4. **Gaps:** the spec's list of what does not match.
5. **Upgrade note:** an older proxy answers 404, and agentop says so. The proxy restart cuts attached sessions.
6. **Out of scope:** the spec's list.
7. **Rulings and deferred minors:** from the ledger.
8. The line `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.

If a merge in this step brought changes, run Step 3 again before you push.
