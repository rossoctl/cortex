package inferencerouter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/storage"
	"github.com/rossoctl/cortex/core/storage/filestore"
)

// openStore opens a plugin store at path, as cmd/cortex does on a local install.
func openStore(t *testing.T, path string) *filestore.Store {
	t.Helper()
	st, err := filestore.Open(path)
	if err != nil {
		t.Fatalf("filestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// buildWithStore is build with st injected before Configure, as plugins.BuildWithDeps
// does, and the router's clock set to now when it is non-nil.
func buildWithStore(t *testing.T, config string, st storage.Store, now func() time.Time) *pipeline.Pipeline {
	t.Helper()
	r := New()
	r.SetStore(st)
	if now != nil {
		r.now = now
	}
	if err := r.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p, err := pipeline.New([]pipeline.Plugin{r})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

// storedPin is the record the store holds for session, and whether there is one.
func storedPin(t *testing.T, st storage.Store, session string) (pinRecord, bool) {
	t.Helper()
	raw, err := st.Get(context.Background(), pinPrefix+session)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if raw == "" {
		return pinRecord{}, false
	}
	var rec pinRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("stored pin %q does not decode: %v", raw, err)
	}
	return rec, true
}

// The live case: a session started on glm, the proxy restarted, and its next turn —
// a continuation, which the router would leave unrouted — went to ete and failed. With
// a store, the pin outlives the process, and the restarted router keeps the session
// on glm.
func TestRouter_WithAStoreAPinSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin-state.json")
	before := openStore(t, path)
	shared := newStore(t)
	p := buildWithStore(t, routerConfig(`"claude-code": "glm"`), before, nil)
	run(t, p, claudeCode(request(shared, eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 0))
	if _, inMemory := pinOf(t, shared, "s1"); inMemory {
		t.Error("the pin was kept in memory as well as in the store")
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	// A new process: a fresh router, a fresh process store, and the store reopened.
	after := openStore(t, path)
	restarted := buildWithStore(t, routerConfig(`"claude-code": "glm"`), after, nil)
	pctx := claudeCode(request(newStore(t), eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 3)
	run(t, restarted, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinExisting})
}

// Every request renews its session's pin, and a store write per request would rewrite
// the file every interval while any session is busy. The stored pin is renewed once
// it is a day old: its TTL then runs between 29 and 30 days from the session's last
// request, rather than exactly 30.
func TestRouter_AStoredPinIsRenewedAtMostOnceADay(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "plugin-state.json"))
	now := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)
	p := buildWithStore(t, routerConfig(`"claude-code": "glm"`), st, func() time.Time { return now })
	send := func() {
		run(t, p, claudeCode(request(newStore(t), eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 0))
	}

	send()
	first, ok := storedPin(t, st, "s1")
	if !ok || first.Agent != "claude-code" || first.Server != "glm" || first.Renewed != now.Unix() {
		t.Fatalf("stored pin = %+v (found %v), want claude-code on glm renewed now", first, ok)
	}
	now = now.Add(23 * time.Hour)
	send()
	if rec, _ := storedPin(t, st, "s1"); rec.Renewed != first.Renewed {
		t.Errorf("renewed at %d within a day of %d, want it left alone", rec.Renewed, first.Renewed)
	}
	now = now.Add(2 * time.Hour)
	send()
	if rec, _ := storedPin(t, st, "s1"); rec.Renewed != now.Unix() {
		t.Errorf("renewed = %d past a day, want %d", rec.Renewed, now.Unix())
	}
}

// A stored value that is not a pin record says nothing about the session, which is
// decided as an unpinned one and pinned over it.
func TestRouter_AStoredValueThatIsNoPinIsIgnored(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "plugin-state.json"))
	if err := st.Set(context.Background(), pinPrefix+"s1", "not a pin", 0); err != nil {
		t.Fatal(err)
	}
	p := buildWithStore(t, routerConfig(`"claude-code": "glm"`), st, nil)
	pctx := claudeCode(request(newStore(t), eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 0)
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	if rec, ok := storedPin(t, st, "s1"); !ok || rec.Server != "glm" {
		t.Errorf("stored pin = %+v (found %v), want glm", rec, ok)
	}
}

// The origin is renewed wherever the pin is, so it never expires under a pin its
// session keeps alive: a foreign origin lost that way would reseed with the target
// and let the old server's reasoning through. An override naming the pin renews it
// too, as every request does.
func TestRouter_TheOriginIsRenewedWithThePin(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"its pin", routerConfig(`"claude-code": "glm"`)},
		{"an override naming its pin", sessionsConfig(`"claude-code": "glm"`, `"s1": "glm"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			now := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			run(t, buildWithStore(t, routerConfig(`"claude-code": "glm"`), st, clock),
				claudeCode(request(newStore(t), eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 0))
			first, _ := storedPin(t, st, "s1")

			now = now.Add(25 * time.Hour)
			run(t, buildWithStore(t, tc.config, st, clock),
				claudeCode(request(newStore(t), eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 0))
			if rec, _ := storedPin(t, st, "s1"); rec.Renewed == first.Renewed {
				t.Errorf("the pin was not renewed past a day")
			}
			if !slices.Contains(st.expired, originPrefix+"s1") || st.ttls[originPrefix+"s1"] != pinTTL {
				t.Errorf("origin not renewed with the pin: expired %v, ttl %v", st.expired, st.ttls[originPrefix+"s1"])
			}
		})
	}
}
