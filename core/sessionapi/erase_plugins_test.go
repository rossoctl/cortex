package sessionapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// erasingPlugin keeps "session data of its own" and records each erase (pipeline.SessionEraser).
type erasingPlugin struct {
	fakePlugin
	erased []string
	all    int
	err    error
}

func (e *erasingPlugin) EraseSession(_ context.Context, id string) error {
	e.erased = append(e.erased, id)
	return e.err
}

func (e *erasingPlugin) EraseAllSessions(context.Context) error {
	e.all++
	return e.err
}

// eraseServer is a server whose outbound pipeline holds the given plugins, with session s1 in the
// store.
func eraseServer(t *testing.T, plugins ...pipeline.Plugin) *httptest.Server {
	t.Helper()
	pipe, err := pipeline.New(plugins)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	srv := New(":0", store, WithClearAllowed(true), WithPipelines(nil, pipeline.NewHolder(pipe)))
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func deleteJSON(t *testing.T, url string) (int, clearResponse) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body clearResponse
	if resp.StatusCode != http.StatusNotFound {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, body
}

// Both erase routes reach each plugin that keeps session data of its own (#1962), and a plugin
// that keeps none is left alone.
func TestErase_CallsPluginErasers(t *testing.T) {
	eraser := &erasingPlugin{fakePlugin: fakePlugin{name: "keeps-state"}}
	plain := &fakePlugin{name: "no-state"}
	ts := eraseServer(t, plain, eraser)

	if code, _ := deleteJSON(t, ts.URL+"/v1/sessions/s1"); code != http.StatusOK {
		t.Fatalf("DELETE /v1/sessions/s1 = %d, want 200", code)
	}
	if len(eraser.erased) != 1 || eraser.erased[0] != "s1" {
		t.Errorf("EraseSession calls = %v, want [s1]", eraser.erased)
	}
	if code, _ := deleteJSON(t, ts.URL+"/v1/sessions"); code != http.StatusOK {
		t.Fatalf("DELETE /v1/sessions = %d, want 200", code)
	}
	if eraser.all != 1 {
		t.Errorf("EraseAllSessions calls = %d, want 1", eraser.all)
	}
}

// A session that only a plugin still holds (after a restart, with the archive off) is erased
// there too, though the route answers 404 for the store and the archive.
func TestErase_PluginErasesAnIdTheStoreDoesNotHold(t *testing.T) {
	eraser := &erasingPlugin{fakePlugin: fakePlugin{name: "keeps-state"}}
	ts := eraseServer(t, eraser)
	if code, _ := deleteJSON(t, ts.URL+"/v1/sessions/only-in-the-plugin"); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if len(eraser.erased) != 1 || eraser.erased[0] != "only-in-the-plugin" {
		t.Errorf("EraseSession calls = %v, want [only-in-the-plugin]", eraser.erased)
	}
}

// A plugin that fails is named in pluginErrors with a 500. The store's half of the erase stands,
// and a later plugin is still asked.
func TestErase_PluginErrorIsReported(t *testing.T) {
	failing := &erasingPlugin{fakePlugin: fakePlugin{name: "broken"}, err: errors.New("disk full")}
	after := &erasingPlugin{fakePlugin: fakePlugin{name: "after"}}
	ts := eraseServer(t, failing, after)

	code, body := deleteJSON(t, ts.URL+"/v1/sessions/s1")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if body.Sessions != 1 {
		t.Errorf("sessions = %d, want 1: the store's half must stand", body.Sessions)
	}
	if len(body.PluginErrors) != 1 || body.PluginErrors[0] != "broken: disk full" {
		t.Errorf("pluginErrors = %v, want [broken: disk full]", body.PluginErrors)
	}
	if len(after.erased) != 1 {
		t.Error("a plugin after the failing one was not asked")
	}

	code, body = deleteJSON(t, ts.URL+"/v1/sessions")
	if code != http.StatusInternalServerError || len(body.PluginErrors) != 1 {
		t.Errorf("DELETE /v1/sessions = %d, pluginErrors %v; want 500 and one error", code, body.PluginErrors)
	}
}
