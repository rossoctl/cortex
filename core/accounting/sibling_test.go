package accounting

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting/wire"
	"github.com/rossoctl/cortex/core/pipeline"
)

// threadSetter is a request plugin that names the accounting thread, the way a subagent's
// request is keyed.
type threadSetter struct{ id string }

func (s *threadSetter) Name() string { return "thread" }
func (s *threadSetter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (s *threadSetter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.SetAccountingThread(s.id, "")
	return pipeline.Action{Type: pipeline.Continue}
}
func (s *threadSetter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// sharedSystem is the system prompt every subagent of the session sends, with a cache mark: about
// 1,800 tokens at 4 bytes per token.
var sharedSystem = `[{"type":"text","text":"` + strings.Repeat("you are a helpful agent. ", 288) +
	`","cache_control":{"type":"ephemeral"}}]`

// subagentBody is one subagent's body: the shared system prompt, then a turn of its own (about
// 400 tokens), with a cache mark on it.
func subagentBody(task string) []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":10,"system":` + sharedSystem +
		`,"messages":[{"role":"user","content":[{"type":"text","text":"` + task + strings.Repeat(" lorem ipsum", 133) +
		`","cache_control":{"type":"ephemeral"}}]}]}`)
}

func threadRequest(t *testing.T, h *Host, thread string, body []byte, usage pipeline.InferenceExtension) *pipeline.Context {
	t.Helper()
	p, err := pipeline.New([]pipeline.Plugin{&threadSetter{id: thread}}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	return runOn(t, p, "s1", "/v1/messages", body, 200, usage)
}

func parsed(t *testing.T, body []byte) (accountant.Body, []accountant.Breakpoint) {
	t.Helper()
	b, bps, err := wire.Parse(wire.FormatAnthropic, body)
	if err != nil {
		t.Fatal(err)
	}
	return b, bps
}

func rescuedTokens(res accountant.Result) int {
	n := 0
	for _, e := range res.CacheEffects {
		if e.Kind == accountant.EffectRescue {
			n += e.Caused.CacheRead
		}
	}
	return n
}

// siblingScenario runs subagents a and b of session s1, which share a 1,800-token system prefix.
// Both start at +0. A ping on a at +4m reads a's whole prompt. If bAgain, b reads the shared
// prefix again at bAt. At +8m a reads its whole prompt. It returns a's last Result and the
// EntryFacts it was ended with.
func siblingScenario(t *testing.T, bAgain bool, bAt time.Duration) (accountant.Result, accountant.EntryFacts) {
	t.Helper()
	now := pingT0
	h := clockHost(t, &now)
	seen := factsSeen(h)
	bodyA, bodyB := subagentBody("task A"), subagentBody("task B")

	if _, ok := h.Compute(threadRequest(t, h, "a", bodyA, pipeline.InferenceExtension{InputTokens: 3, CacheWriteTokens: 2200})); !ok {
		t.Fatal("a, first request: no Result")
	}
	if _, ok := h.Compute(threadRequest(t, h, "b", bodyB, pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800, CacheWriteTokens: 400})); !ok {
		t.Fatal("b, first request: no Result")
	}
	ping := accountant.Ping{
		Session: "s1", ThreadID: "a", Model: "claude-sonnet-4-5", Host: "api.anthropic.com",
		Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 1, CacheRead: 2200}, Output: 1},
		At:    pingT0.Add(4 * time.Minute), Window: pingT0,
	}
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", ping, okInfo()); !ok {
		t.Fatal("ping refused")
	}
	if bAgain {
		now = pingT0.Add(bAt)
		if _, ok := h.Compute(threadRequest(t, h, "b", bodyB, pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 2200})); !ok {
			t.Fatal("b, second request: no Result")
		}
	}
	now = pingT0.Add(8 * time.Minute)
	res, ok := h.Compute(threadRequest(t, h, "a", bodyA, pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 2200}))
	if !ok {
		t.Fatal("a, second request: no Result")
	}
	got := seen()
	last := got[len(got)-1]
	if last.LastPing.IsZero() {
		t.Fatal("setup: a's second request did not get the ping's facts")
	}
	return res, last
}

// TestSharedWarm_SiblingPrefixLeftOutOfRescue: b read the shared prefix at +6m, so b kept it
// warm, and the ping on a rescued only a's own turn (about 400 tokens), not the whole 2,200-token
// read (rossoctl/cortex#2138).
func TestSharedWarm_SiblingPrefixLeftOutOfRescue(t *testing.T) {
	res, f := siblingScenario(t, true, 6*time.Minute)
	fwdA, _ := parsed(t, subagentBody("task A"))
	fwdB, bpsB := parsed(t, subagentBody("task B"))
	want := accountant.SharedWarmFrom(fwdA, []accountant.Sibling{{Body: fwdB, Breakpoints: bpsB}})
	if want == nil || want.Section != accountant.SectionSystem {
		t.Fatalf("setup: SharedWarmFrom = %+v, want a system position", want)
	}
	if f.SharedWarm == nil || *f.SharedWarm != *want {
		t.Errorf("EntryFacts.SharedWarm = %v, want %+v (b kept that prefix warm)", f.SharedWarm, *want)
	}
	if n := rescuedTokens(res); n < 300 || n > 500 {
		t.Errorf("rescue = %d tokens, want about 400 (a's own turn only); checks %+v", n, res.Checks)
	}
	for _, c := range res.Checks {
		if c.Check == accountant.CheckSharedWarm {
			t.Errorf("shared_warm check failed: %+v", c)
		}
	}
}

// TestSharedWarm_SiblingPastTTLIsNotASibling: b's last request is at +0, 8 minutes before a's,
// past the 5m TTL. Only the ping kept the prefix alive, so the whole read is rescued.
func TestSharedWarm_SiblingPastTTLIsNotASibling(t *testing.T) {
	res, f := siblingScenario(t, false, 0)
	if f.SharedWarm != nil {
		t.Errorf("EntryFacts.SharedWarm = %+v, want nil (b is past the TTL)", *f.SharedWarm)
	}
	if n := rescuedTokens(res); n != 2200 {
		t.Errorf("rescue = %d tokens, want 2200", n)
	}
}

func TestSiblingStore_ChoosesSiblings(t *testing.T) {
	fwdA, bpsA := parsed(t, subagentBody("task A"))
	fwdB, bpsB := parsed(t, subagentBody("task B"))
	const ttl = 5 * time.Minute

	for name, c := range map[string]struct {
		thread string        // the thread that sent b's body
		at     time.Duration // when it sent it, after pingT0
		want   bool          // a's request at pingT0+5m sees a shared prefix
	}{
		"another thread, inside the TTL": {"b", 2 * time.Minute, true},
		"another thread, at the TTL":     {"b", 0, true},
		"another thread, past the TTL":   {"b", -time.Second, false},
		"another thread, later":          {"b", 6 * time.Minute, false},
		"another session's thread":       {"", 2 * time.Minute, false},
		"the same thread":                {"a", 2 * time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			var s siblingStore
			session := "s1"
			if c.thread == "" {
				session, c.thread = "s2", "b"
			}
			s.sharedWarm(session, c.thread, pingT0.Add(c.at), ttl, fwdB, bpsB, false)
			got := s.sharedWarm("s1", "a", pingT0.Add(ttl), ttl, fwdA, bpsA, true)
			if (got != nil) != c.want {
				t.Errorf("sharedWarm = %+v, want a position: %v", got, c.want)
			}
		})
	}
}

// Without want the bodies are not compared, but the request is still recorded for its siblings.
func TestSiblingStore_RecordsWithoutWant(t *testing.T) {
	fwdA, bpsA := parsed(t, subagentBody("task A"))
	fwdB, bpsB := parsed(t, subagentBody("task B"))
	var s siblingStore
	s.sharedWarm("s1", "b", pingT0, time.Minute, fwdB, bpsB, false)
	if got := s.sharedWarm("s1", "a", pingT0, time.Minute, fwdA, bpsA, false); got != nil {
		t.Errorf("sharedWarm without want = %+v, want nil", *got)
	}
	if got := s.sharedWarm("s1", "c", pingT0, time.Minute, fwdA, bpsA, true); got == nil {
		t.Error("a and b were not recorded without want")
	}
}

// Records end on their own goroutines, in any order: an earlier request that ends later does not
// replace the thread's later one.
func TestSiblingStore_KeepsTheLaterRequest(t *testing.T) {
	fwdA, bpsA := parsed(t, subagentBody("task A"))
	fwdB, bpsB := parsed(t, subagentBody("task B"))
	var s siblingStore
	s.sharedWarm("s1", "b", pingT0.Add(4*time.Minute), 5*time.Minute, fwdB, bpsB, false)
	s.sharedWarm("s1", "b", pingT0, 5*time.Minute, fwdB, bpsB, false)
	if got := s.sharedWarm("s1", "a", pingT0.Add(8*time.Minute), 5*time.Minute, fwdA, bpsA, true); got == nil {
		t.Error("the earlier request replaced b's later one")
	}
}

func TestSiblingStore_IsBounded(t *testing.T) {
	fwd, bps := parsed(t, subagentBody("task A"))
	var s siblingStore
	for i := range siblingsCap + 1 {
		s.sharedWarm(fmt.Sprintf("s%d", i/10), fmt.Sprintf("t%d", i%10), pingT0.Add(time.Duration(i)*time.Second),
			time.Minute, fwd, bps, false)
	}
	if n := s.len(); n != siblingsTrimTo {
		t.Errorf("entries = %d, want %d after a trim", n, siblingsTrimTo)
	}
	// The trim drops the oldest: the first session is gone and the last one is kept.
	if _, ok := s.sessions["s0"]; ok {
		t.Error("the oldest session survived the trim")
	}
	if _, ok := s.sessions[fmt.Sprintf("s%d", siblingsCap/10)]; !ok {
		t.Error("the newest session was trimmed")
	}

	var one siblingStore
	for i := range siblingThreadsCap + 1 {
		one.sharedWarm("s1", fmt.Sprintf("t%d", i), pingT0.Add(time.Duration(i)*time.Second), time.Minute, fwd, bps, false)
	}
	if n := len(one.sessions["s1"]); n != siblingThreadsCap || one.len() != siblingThreadsCap {
		t.Errorf("threads in s1 = %d (count %d), want %d", n, one.len(), siblingThreadsCap)
	}
	if _, ok := one.sessions["s1"]["t0"]; ok {
		t.Error("the session's oldest thread survived its cap")
	}
}

// Run with -race: the store is used from every record's goroutine.
func TestSiblingStore_Concurrent(t *testing.T) {
	fwd, bps := parsed(t, subagentBody("task A"))
	var s siblingStore
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			for j := range 50 {
				s.sharedWarm(fmt.Sprintf("s%d", i%4), fmt.Sprintf("t%d", i), pingT0.Add(time.Duration(j)*time.Second),
					time.Minute, fwd, bps, j%2 == 0)
			}
		})
	}
	wg.Wait()
	if n := s.len(); n != 32 {
		t.Errorf("entries = %d, want 32", n)
	}
}
