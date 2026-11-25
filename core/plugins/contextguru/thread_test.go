package contextguru

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	cgstore "github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/pipeline"
)

// threadBody is an Anthropic request whose history is the given user turns, with an assistant
// reply between each pair, so a later body that repeats an earlier one's turns continues it.
func threadBody(t *testing.T, turns ...string) []byte {
	t.Helper()
	var msgs []map[string]any
	for i, u := range turns {
		if i > 0 {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok " + turns[i-1]})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": u})
	}
	b, err := json.Marshal(map[string]any{"model": "claude-sonnet-4-5", "system": "you are a coding agent", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// engineThread runs one request through OnRequest and returns the engine thread it resolved.
func engineThread(t *testing.T, p *ContextGuru, pctx *pipeline.Context) (string, bool) {
	t.Helper()
	invokeReq(p, pctx)
	th := pipeline.GetState[cgThread](pctx, cgThreadStateKey)
	if th == nil {
		return "", false
	}
	return th.id, true
}

// The plugin sets apply.Opts.ThreadOf, so a session's requests split into threads: the primary
// thread for the main conversation and its turns, a thread of its own for an unrelated one-shot
// call, and "a:<id>" for a named agent. Before, every request was the primary thread.
func TestOnRequest_SplitsASessionIntoThreads(t *testing.T) {
	p := configured(t, `{}`)

	main1 := inferencePctx("/v1/messages", threadBody(t, "fix the failing test in parser.go"))
	if id, ok := engineThread(t, p, main1); !ok || id != thread.Primary {
		t.Fatalf("first request: thread %q (resolved %v), want the primary thread", id, ok)
	}
	if _, _, set := main1.AccountingThread(); set {
		t.Fatal("primary thread: the plugin must leave the accounting default (main) in place")
	}

	oneShot := inferencePctx("/v1/messages", threadBody(t, "write a five word title for this chat"))
	id, _ := engineThread(t, p, oneShot)
	if !strings.HasPrefix(id, "p:") {
		t.Fatalf("unrelated one-shot call: thread %q, want a prefix-continuity thread (p:...)", id)
	}
	if acct, _, set := oneShot.AccountingThread(); !set || acct != id {
		t.Fatalf("one-shot call: accounting thread %q (set %v), want the engine thread %q", acct, set, id)
	}

	main2 := inferencePctx("/v1/messages", threadBody(t, "fix the failing test in parser.go", "now run the tests"))
	if id, _ := engineThread(t, p, main2); id != thread.Primary {
		t.Fatalf("next main turn: thread %q, want the primary thread it continues", id)
	}

	sub := inferencePctx("/v1/messages", threadBody(t, "search the repo for parser tests"))
	sub.Headers.Set(pipeline.ClaudeCodeAgentIDHeader, "agent-7")
	if id, _ := engineThread(t, p, sub); id != agentThreadPrefix+"agent-7" {
		t.Fatalf("named agent: thread %q, want %q (the keeper's key for it)", id, agentThreadPrefix+"agent-7")
	}
	if _, _, set := sub.AccountingThread(); set {
		t.Fatal("named agent: the plugin must leave the accounting default (subagent id, parent) in place")
	}
}

// A primary thread and a named agent keep core/accounting.ThreadOf's rule; a prefix-continuity
// thread is named with the engine's id.
func TestAccountingThread(t *testing.T) {
	for _, tc := range []struct {
		name   string
		res    thread.Result
		wantID string
		wantOK bool
	}{
		{"primary", thread.Result{ID: thread.Primary, Source: thread.SourceSession}, "", false},
		{"primary by prefix", thread.Result{ID: thread.Primary, Source: thread.SourcePrefix}, "", false},
		{"named agent", thread.Result{ID: "a:agent-7", Source: thread.SourceHeader}, "", false},
		{"prefix thread", thread.Result{ID: "p:0123456789ab", Source: thread.SourcePrefix}, "p:0123456789ab", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, parent, ok := accountingThread(tc.res)
			if id != tc.wantID || ok != tc.wantOK || parent != "" {
				t.Fatalf("accountingThread = (%q, %q, %v), want (%q, \"\", %v)", id, parent, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// OnFinish records the per-thread billed input under the thread OnRequest resolved, so the
// engine reads it back on that thread's next request (Ctx.PrevBilledInputThread).
func TestFinishEngine_WritesTheResolvedThread(t *testing.T) {
	p := configured(t, `{}`)
	pctx := inferencePctx("/v1/messages", threadBody(t, "write a five word title for this chat"))
	invokeReq(p, inferencePctx("/v1/messages", threadBody(t, "fix the failing test in parser.go")))
	id, ok := engineThread(t, p, pctx)
	if !ok || id == thread.Primary {
		t.Fatalf("setup: thread %q (resolved %v), want a prefix-continuity thread", id, ok)
	}
	pctx.StatusCode = 200
	pctx.Extensions.Inference = &pipeline.InferenceExtension{InputTokens: 100, CacheReadTokens: 900}
	p.finishEngine(pctx)

	key, _ := p.cgSessions.get("s1")
	b, found := p.store.Get(cgstore.BilledThreadPrefix + thread.Key(key, id))
	if !found || string(b) != "1000" {
		t.Fatalf("per-thread billed input = %q (found %v), want 1000", b, found)
	}
}

// A new thread that continues the primary thread by content (an edited last turn, a rewind)
// gets the primary thread as its accountant parent, under the accountant's name for it: "main".
func TestOnRequest_AThreadThatContinuesThePrimaryHasParentMain(t *testing.T) {
	p := configured(t, `{}`)
	long := strings.Repeat("the parser drops the last token when the input ends without a newline. ", 40)
	invokeReq(p, inferencePctx("/v1/messages", threadBody(t, "fix the parser", long, "now run the tests", long+" again")))
	// The same history with its last turn edited: it shares every leading message but the last.
	fork := inferencePctx("/v1/messages", threadBody(t, "fix the parser", long, "now run the tests", "run only the parser tests"))
	id, _ := engineThread(t, p, fork)
	if id == thread.Primary {
		t.Fatal("an edited last turn must start a new thread, which inherits from the primary one")
	}
	acct, parent, set := fork.AccountingThread()
	if !set || acct != id || parent != "main" {
		t.Fatalf("accounting thread = (%q, %q, %v), want (%q, \"main\", true)", acct, parent, set, id)
	}
}

// gatesOf runs one request through OnRequest with a DEBUG logger and returns the gates the
// engine's cache_aware_summarizer reported on it (apply's logDecisions line).
func gatesOf(t *testing.T, p *ContextGuru, pctx *pipeline.Context) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	invokeReq(p, pctx)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "component=cache_aware_summarizer") {
			return line
		}
	}
	t.Fatalf("no decision line for cache_aware_summarizer in:\n%s", buf.String())
	return ""
}

// #2130 gap 1: the summarizer's size gate needs the thread's billed input from the previous turn
// (Ctx.PrevBilledInputThread). OnFinish writes it, so on the next turn the gate can resolve the
// fill against the window. Without OnFinish, the gate stays closed with "window_not_exact".
func TestOnFinish_OpensTheSummarizerSizeGateOnTheNextTurn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		finish     bool
		wantClosed bool
	}{
		{"no finish: the gate cannot resolve the fill", false, true},
		{"finish: the gate resolves the fill", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := configured(t, `{`+offline+`, "windows": {"claude-sonnet-5": 100000}}}`)
			// A long history, so the summarizer has a span to summarize and reaches its size gate.
			var turns []string
			for i := range 30 {
				turns = append(turns, strconv.Itoa(i)+": "+strings.Repeat("the parser drops the last token when the input ends without a newline. ", 20))
			}
			turn1 := inferencePctx("/v1/messages", threadBody(t, turns[:29]...))
			turn1.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-5"}
			gatesOf(t, p, turn1)
			if tc.finish {
				fin := finishedPctx(200, pipeline.AgentRoleMain)
				turn1.StatusCode, turn1.Headers, turn1.StartedAt = fin.StatusCode, fin.Headers, fin.StartedAt
				turn1.Extensions.Inference = fin.Extensions.Inference
				turn1.Extensions.Inference.InputTokens = 95_000 - turn1.Extensions.Inference.CacheReadTokens
				p.OnFinish(t.Context(), turn1)
			}
			turn2 := inferencePctx("/v1/messages", threadBody(t, turns...))
			turn2.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-5"}
			line := gatesOf(t, p, turn2)
			if closed := strings.Contains(line, "window_not_exact"); closed != tc.wantClosed {
				t.Fatalf("window_not_exact on turn 2 = %v, want %v\n%s", closed, tc.wantClosed, line)
			}
			// 95,000 of a 100,000 window is past the shipped 0.9 fraction: with the fill known, the
			// size gate opens too, and the summarizer goes on to its model (none in this test).
			if !tc.wantClosed && strings.Contains(line, "below_request_trigger") {
				t.Fatalf("turn 2 is 95%% full but the size gate stayed closed:\n%s", line)
			}
		})
	}
}

// bodyPctx is a finished 200 main-agent request (finishedPctx) with body as its messages.
func bodyPctx(body []byte) *pipeline.Context {
	p := finishedPctx(200, pipeline.AgentRoleMain)
	p.Body = body
	return p
}

// A session's one-shot call (here a title request, with a first message of its own) is its own
// keeper entry. Before, every request without an agent id was the main thread's, so the call
// replaced the main thread's last turn in the keeper.
func TestAOneShotCallHasItsOwnKeeperEntry(t *testing.T) {
	p := keepAlivePlugin(t)
	for _, b := range [][]byte{
		threadBody(t, "fix the parser"),
		threadBody(t, "fix the parser", "now run the tests"),
	} {
		pctx := bodyPctx(b)
		invokeReq(p, pctx)
		p.OnFinish(t.Context(), pctx)
	}
	for _, b := range [][]byte{ // turn 0 of a thread is counted but never tracked
		threadBody(t, "write a five word title for this chat"),
		threadBody(t, "write a five word title for this chat", "shorter"),
	} {
		pctx := bodyPctx(b)
		invokeReq(p, pctx)
		key, ok := keeperKey(pctx, sessionID(pctx))
		if !ok || !strings.HasPrefix(key.Thread, "p:") {
			t.Fatalf("one-shot call: keeper key %+v (ok %v), want its own p: thread", key, ok)
		}
		p.OnFinish(t.Context(), pctx)
	}
	if n := p.keeper.Stats().Live; n != 2 {
		t.Fatalf("live entries = %d, want the main thread's and the one-shot thread's", n)
	}
}

// A Codex subagent (Thread-Id with X-Codex-Parent-Thread-Id) is its own thread: in the engine, in
// the keeper and in the accountant.
func TestACodexSubagentIsItsOwnThread(t *testing.T) {
	p := configured(t, `{}`)
	pctx := inferencePctx("/v1/messages", threadBody(t, "search the repo for parser tests"))
	pctx.Headers.Set("User-Agent", "codex_cli_rs/0.172.0")
	pctx.Headers.Set("Thread-Id", "t-sub")
	pctx.Headers.Set("X-Codex-Parent-Thread-Id", "t-main")
	id, _ := engineThread(t, p, pctx)
	if id != agentThreadPrefix+"t-sub" {
		t.Fatalf("engine thread %q, want %q", id, agentThreadPrefix+"t-sub")
	}
	if key, _ := keeperKey(pctx, sessionID(pctx)); key.Thread != id {
		t.Fatalf("keeper thread %q, want the engine's %q", key.Thread, id)
	}
	if got := accounting.ThreadOf(pctx); got.ID != "t-sub" || got.Parent != "main" {
		t.Fatalf("accountant thread %+v, want {t-sub main}", got)
	}
}

// OnRequest removes every x-context-guru-* header, on an inference path and on any other path, and
// leaves the others. context-guru reads them first: x-context-guru-request-kind still marks the
// client's compaction request.
func TestOnRequest_StripsContextGuruHeaders(t *testing.T) {
	p := configured(t, `{}`)
	for _, path := range []string{"/v1/messages", "/v1/models"} {
		pctx := inferencePctx(path, threadBody(t, "fix the parser"))
		pctx.Headers.Set("x-context-guru-request-kind", "compaction")
		pctx.Headers.Set("X-Context-Guru-Client", "pi")
		pctx.Headers.Set("X-Context-Guru-LLM", "a guessed sentinel")
		pctx.Headers.Set("Anthropic-Version", "2023-06-01")
		invokeReq(p, pctx)
		for k := range pctx.Headers {
			if strings.HasPrefix(strings.ToLower(k), "x-context-guru-") {
				t.Errorf("%s: header %s was forwarded", path, k)
			}
		}
		if pctx.Headers.Get("Anthropic-Version") == "" {
			t.Errorf("%s: an unrelated header was removed", path)
		}
	}
}

// The plugin's own calls keep their sentinel: OnRequest skips them before the strip.
func TestOnRequest_KeepsTheSentinelOnItsOwnCalls(t *testing.T) {
	p := configured(t, `{}`)
	pctx := inferencePctx("/v1/messages", threadBody(t, "fix the parser"))
	pctx.Headers.Set(sentinelHeader, p.sentinel)
	invokeReq(p, pctx)
	if pctx.Headers.Get(sentinelHeader) != p.sentinel {
		t.Fatal("the plugin's own call lost its sentinel")
	}
}
