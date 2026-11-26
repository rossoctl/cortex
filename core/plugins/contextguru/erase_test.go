package contextguru

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	cgstore "github.com/rossoctl/context-guru/store"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// parserStandIn satisfies the plugin's RequiresAny ("inference-parser") so a real pipeline builds.
type parserStandIn struct{}

func (parserStandIn) Name() string                              { return "inference-parser" }
func (parserStandIn) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (parserStandIn) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (parserStandIn) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// eraseRoute serves the real session API with p, wrapped as production wraps it, in the outbound
// pipeline and session id in the store, and sends DELETE route.
func eraseRoute(t *testing.T, p *ContextGuru, id, route string) {
	t.Helper()
	pipe, err := pipeline.New([]pipeline.Plugin{parserStandIn{}, pipeline.WrapConfigured(p, nil)})
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	store.Append(id, pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	srv := sessionapi.New(":0", store, sessionapi.WithClearAllowed(true),
		sessionapi.WithPipelines(nil, pipeline.NewHolder(pipe)))
	ts := httptest.NewServer(srv.Server().Handler)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+route, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE %s = %d, want 200", route, resp.StatusCode)
	}
}

// stateRows counts the session_state rows of one key.
func stateRows(t *testing.T, p *ContextGuru, key string) int {
	t.Helper()
	var n int
	if err := p.stateDB.QueryRow(`SELECT count(*) FROM session_state WHERE key = ?`, []byte(key)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// #1962: both erase routes remove the plugin's saved session state, and a new instance (a restart)
// loads nothing back. Before the fix the row stayed in context-guru-state.db for 30 days.
func TestErase_RoutesRemoveSavedState(t *testing.T) {
	for _, route := range []string{"/v1/sessions/s-1962", "/v1/sessions"} {
		t.Run(route, func(t *testing.T) {
			const id = "s-1962"
			key := cgstore.SumPrefix + id
			p := freshPlugin(t)
			p.store.Put(key, []byte("SUMMARY OF THE USER'S CONVERSATION"))
			p.stateDisk.Load().Flush()
			if stateRows(t, p, key) != 1 {
				t.Fatal("setup: the summary is not on disk")
			}
			p.store.Put(cgstore.BilledPrefix+id, []byte("1")) // still pending at the erase

			eraseRoute(t, p, id, route)
			if err := p.Shutdown(context.Background()); err != nil { // the last flush
				t.Fatal(err)
			}
			for _, k := range []string{key, cgstore.BilledPrefix + id} {
				if n := stateRows(t, p, k); n != 0 {
					t.Errorf("after DELETE %s, session_state has %d row(s) for %q", route, n, k)
				}
			}
			second := freshPlugin(t)
			t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
			mem := second.store.(*cgstore.Memory)
			mem.LoadSession(id)
			if got, ok := mem.Get(key); ok {
				t.Errorf("after DELETE %s, a new instance loads the summary back: %q", route, got)
			}
		})
	}
}

// The erase reaches an instance that a config reload left draining: its memory would otherwise
// flush the session back at its Shutdown.
func TestErase_ReachesADrainingInstance(t *testing.T) {
	const id = "s-drain"
	key := cgstore.SumPrefix + id
	old := freshPlugin(t)
	old.store.Put(key, []byte("PENDING IN THE OLD INSTANCE")) // not flushed yet
	current := freshPlugin(t)
	t.Cleanup(func() { _ = current.Shutdown(context.Background()) })

	if err := current.EraseSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, ok := old.store.Get(key); ok {
		t.Error("the draining instance still holds the session in memory")
	}
	if err := old.Shutdown(context.Background()); err != nil { // its last flush
		t.Fatal(err)
	}
	if n := stateRows(t, current, key); n != 0 {
		t.Errorf("the draining instance's last flush wrote the erased session back (%d rows)", n)
	}
}

// The forwarded-body stash holds content too: the erase removes the session's threads and keeps
// another session's entry.
func TestErase_DropsTheForwardedBody(t *testing.T) {
	p := freshPlugin(t)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	p.stash.put("s1", bschemas.Anthropic, []byte("pre"), []byte("BODY"))
	p.stash.put("s1\x00sub", bschemas.Anthropic, []byte("pre"), []byte("SUB BODY"))
	p.stash.put("s2", bschemas.Anthropic, []byte("pre"), []byte("OTHER"))

	if err := p.EraseSession(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if p.stash.get("s1") != nil || p.stash.get("s1\x00sub") != nil {
		t.Error("the erased session's forwarded body is still stashed")
	}
	if string(p.stash.get("s2")) != "OTHER" {
		t.Error("another session's forwarded body went")
	}
	if want := len("pre") + len("OTHER"); p.stash.bytes != want {
		t.Errorf("stash bytes = %d, want %d", p.stash.bytes, want)
	}

	if err := p.EraseAllSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.stash.get("s2") != nil || p.stash.bytes != 0 {
		t.Error("EraseAllSessions left a forwarded body")
	}
}
