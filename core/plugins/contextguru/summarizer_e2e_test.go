package contextguru

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
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
	lastKey := ""
	turn := func(n int) []byte {
		pctx := anthropicPctx(u.Host, pipeline.AgentRoleMain)
		pctx.Scheme = "http"
		pctx.Headers.Set("Anthropic-Version", "2023-06-01")
		pctx.Body = withTools(t, threadBody(t, turns[:n]...))
		invokeReq(p, pctx)
		pctx.StatusCode = 200
		pctx.Extensions.Inference.InputTokens = 5_000
		pctx.Extensions.Inference.CacheReadTokens = 90_000
		pctx.Extensions.Inference.PresentKinds = 1 | 2 | 4 | 8
		p.OnFinish(t.Context(), pctx)
		lastKey = engineKey(pctx)
		return pctx.Body
	}
	// The summary call runs on its own goroutine, which OnFinish releases. The test learns that a
	// turn started one from the engine's own counter, and waits for the store write that ends it.
	stored := &sumNotifyStore{Store: p.store, sum: make(chan struct{})}
	p.store = stored
	key := ""
	summarized := false
	for n := 30; n <= 36 && !summarized; n++ {
		before, _, _, _ := offload.CacheAwareAsyncStats()
		turn(n)
		key = lastKey
		if started, _, _, _ := offload.CacheAwareAsyncStats(); started > before {
			select {
			case <-stored.sum:
				summarized = true
			case <-t.Context().Done():
				t.Fatal("the summary call never stored a checkpoint")
			}
		}
	}
	if b, ok := p.store.Get(cgstore.BilledThreadPrefix + key); !ok || string(b) != "95000" {
		t.Errorf("cg:bint: row = %q (present %v), want 95000", b, ok)
	}
	if _, ok := p.store.Get(cgstore.SumPrefix + key); !summarized || !ok {
		t.Fatalf("no cg:sum: row after 7 turns at 95%% of the window; model calls = %d", calls.Load())
	}
	if calls.Load() == 0 {
		t.Fatal("a summary was stored with no model call")
	}

	// The next request carries the summary in place of the history it covers.
	full := withTools(t, threadBody(t, turns[:38]...))
	sent := turn(38)
	if len(sent) >= len(full)/2 {
		t.Fatalf("the request after the summary is %d bytes of %d: want it much smaller", len(sent), len(full))
	}
	t.Logf("request after the summary: %d bytes of %d", len(sent), len(full))
}

// withTools adds agentTools to body: an agent's turns declare tools, and stashSent stashes only those.
func withTools(t *testing.T, body []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	var tools map[string]any
	if err := json.Unmarshal([]byte("{"+agentTools+"}"), &tools); err != nil {
		t.Fatal(err)
	}
	m["tools"] = tools["tools"]
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sumNotifyStore is the engine store with one test hook: it closes sum when the first summary
// checkpoint (cg:sum:) is written, so the test waits on that event rather than on the clock.
type sumNotifyStore struct {
	cgstore.Store
	sum  chan struct{}
	once sync.Once
}

func (s *sumNotifyStore) Put(key string, val []byte) {
	s.Store.Put(key, val)
	if strings.HasPrefix(key, cgstore.SumPrefix) {
		s.once.Do(func() { close(s.sum) })
	}
}
