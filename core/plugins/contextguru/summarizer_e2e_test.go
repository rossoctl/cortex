package contextguru

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cgstore "github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestSummarizerFiresOnALongSession is #2130's "How to check the fix", in process: a Claude Code
// style session at 95% of its window, turn after turn, through OnRequest and OnFinish with the
// request's own Anthropic model (a fake upstream). After a few turns the store holds the thread's
// billed input (cg:bint:) and a summary checkpoint (cg:sum:).
func TestSummarizerFiresOnALongSession(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		summary := "<summary>" + strings.Repeat("The user and the agent fixed the parser's trailing-newline bug and ran the tests. ", 20) + "</summary>"
		_, _ = io.WriteString(w, `{"model":"claude-sonnet-5","content":[{"type":"text","text":"`+summary+`"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":50,"cache_read_input_tokens":90000,"output_tokens":400}}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	p := configured(t, `{`+offline+`, "windows": {"claude-sonnet-5": 100000}}}`)
	var turns []string
	for i := range 40 {
		turns = append(turns, strconv.Itoa(i)+": "+strings.Repeat("the parser drops the last token when the input ends without a newline. ", 20))
	}
	// turn sends one request of the session with the first n turns of its history, and finishes it
	// at 95% of the window. It returns the body the plugin forwarded.
	turn := func(n int) []byte {
		pctx := anthropicPctx(u.Host, pipeline.AgentRoleMain)
		pctx.Scheme = "http"
		pctx.Headers.Set("Anthropic-Version", "2023-06-01")
		pctx.Body = threadBody(t, turns[:n]...)
		invokeReq(p, pctx)
		pctx.StatusCode = 200
		pctx.Extensions.Inference.InputTokens = 5_000
		pctx.Extensions.Inference.CacheReadTokens = 90_000
		pctx.Extensions.Inference.PresentKinds = 1 | 2 | 4 | 8
		p.OnFinish(t.Context(), pctx)
		return pctx.Body
	}
	key := ""
	summarized := false
	for n := 30; n <= 36 && !summarized; n++ {
		turn(n)
		key, _ = p.cgSessions.get("s1")
		// The summary call runs on its own goroutine, released by OnFinish.
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, summarized = p.store.Get(cgstore.SumPrefix + key); summarized {
				break
			}
		}
	}
	if b, ok := p.store.Get(cgstore.BilledThreadPrefix + key); !ok || string(b) != "95000" {
		t.Errorf("cg:bint: row = %q (present %v), want 95000", b, ok)
	}
	if !summarized {
		t.Fatalf("no cg:sum: row after 7 turns at 95%% of the window; model calls = %d", calls.Load())
	}
	if calls.Load() == 0 {
		t.Fatal("a summary was stored with no model call")
	}

	// The next request carries the summary in place of the history it covers.
	full := threadBody(t, turns[:38]...)
	sent := turn(38)
	if len(sent) >= len(full)/2 {
		t.Fatalf("the request after the summary is %d bytes of %d: want it much smaller", len(sent), len(full))
	}
	t.Logf("request after the summary: %d bytes of %d", len(sent), len(full))
}
