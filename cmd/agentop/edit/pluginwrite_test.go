package edit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func routeClaudeToGLM() ConfigChange { return change([]string{"agents", "claude-code"}, "glm") }

func TestWritePluginConfig_WritesAndReportsTheReload(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	srv := makeStatusServer(t, func() ReloadStatus { return ReloadStatus{LastSuccess: time.Now()} })

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloaded {
		t.Errorf("outcome = %v, want WriteReloaded", res.Outcome)
	}
	if got, want := readFile(t, path), strings.Replace(withRouter, "claude-code: ete", "claude-code: glm", 1); got != want {
		t.Errorf("file differs:\n%s", Diff([]byte(want), []byte(got)))
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the file's own 0600 kept", st.Mode().Perm())
	}
}

func TestWritePluginConfig_ReportsAReloadTheProxyRefused(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		if calls.Add(1) == 1 {
			return ReloadStatus{ReloadsFailed: 2}
		}
		return ReloadStatus{ReloadsFailed: 3, LastError: `configure "inference-router": servers: at least one server is required`}
	})

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloadFailed || !strings.Contains(res.ReloadError, "at least one server") {
		t.Errorf("result = %+v, want WriteReloadFailed with the proxy's error", res)
	}
}

// The proxy reloads 250ms after a write. When it refuses the write before
// agentop's first poll returns, that first poll already counts the refusal. A
// baseline read at that poll then hides the refusal, and agentop waits out its
// whole deadline. The baseline must come from before the write.
func TestWritePluginConfig_ReportsARefusalThatLandsBeforeTheFirstPoll(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	srv := makeStatusServer(t, func() ReloadStatus {
		if !strings.Contains(readFile(t, path), "claude-code: glm") {
			return ReloadStatus{ReloadsFailed: 2}
		}
		return ReloadStatus{ReloadsFailed: 3, LastError: "refused before the first poll"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := WritePluginConfig(ctx, ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloadFailed || res.ReloadError != "refused before the first poll" {
		t.Errorf("result = %+v, want WriteReloadFailed with the proxy's error", res)
	}
}

func TestWritePluginConfig_WithNoProxyWritesAndSaysSo(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteNotRunning {
		t.Errorf("outcome = %v, want WriteNotRunning", res.Outcome)
	}
	if !strings.Contains(readFile(t, path), "claude-code: glm") {
		t.Error("the file was not written")
	}
}

// The reloader ignores a byte-identical file, so a poll after a no-op write would
// wait out its whole deadline for a reload that never comes.
func TestWritePluginConfig_AnUnchangedValueIsNeitherWrittenNorPolled(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	before, _ := os.Stat(path)
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus { calls.Add(1); return ReloadStatus{LastSuccess: time.Now()} })

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL,
		Changes: []ConfigChange{change([]string{"agents", "claude-code"}, "ete")}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteUnchanged {
		t.Errorf("outcome = %v, want WriteUnchanged", res.Outcome)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("polled /reload/status %d times for a write that did not happen", n)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Error("the file was rewritten")
	}
}

func TestWritePluginConfig_NeverWritesAResultThatWillNotLoad(t *testing.T) {
	src := withRouter + "mtls:\n  mode: sideways\n"
	path := writeFixtureContent(t, src, 0o600)
	_, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err == nil || !strings.Contains(err.Error(), "would not load") {
		t.Fatalf("err = %v, want a load refusal", err)
	}
	if readFile(t, path) != src {
		t.Error("a result that does not load was written")
	}
}

func TestWritePluginConfig_NeverWritesAResultVerifyRefuses(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	refuse := func(*config.Config) error { return errors.New("glm is not a server") }
	_, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}, Verify: refuse})
	if err == nil || !strings.Contains(err.Error(), "glm is not a server") {
		t.Fatalf("err = %v, want Verify's refusal", err)
	}
	if readFile(t, path) != withRouter {
		t.Error("a result Verify refused was written")
	}
}

// Verify sees the config the proxy would load, so it can check the plugin's own block.
func TestWritePluginConfig_VerifySeesTheResult(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	var saw string
	look := func(c *config.Config) error {
		for _, e := range c.Pipeline.Outbound.Plugins {
			if e.Name == "inference-router" {
				saw = string(e.Config)
			}
		}
		return nil
	}
	if _, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}, Verify: look}); err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if !strings.Contains(saw, `"claude-code":"glm"`) {
		t.Errorf("Verify saw %s, want the changed agents block", saw)
	}
}

func TestWritePluginConfig_AChangeThatCannotBeMadeWritesNothing(t *testing.T) {
	path := writeFixtureContent(t, localConfig, 0o600)
	_, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err == nil || !strings.Contains(err.Error(), "has no inference-router entry") {
		t.Fatalf("err = %v, want the missing-entry error", err)
	}
	if readFile(t, path) != localConfig {
		t.Error("the file changed")
	}
}

// Changes apply in order, each to the result of the one before: add a server, then
// route an agent to it, in one write and one reload.
func TestWritePluginConfig_AppliesChangesInOrder(t *testing.T) {
	path := writeFixtureContent(t, localConfig, 0o600)
	add := change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-ete")
	add.CreatePlugin = true
	use := change([]string{"agents", "claude-code"}, "ete")
	if _, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{add, use}}); err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "              key: sk-ete\n          agents:\n            claude-code: ete\n") {
		t.Errorf("want the server then the agent:\n%s", got)
	}
}

// A refused reload restores the original bytes and mode, and reports RolledBack=true.
func TestWritePluginConfig_OnRefusedReloadRestoresOriginalAndReportsRolledBack(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	before := readFile(t, path)
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		if calls.Add(1) == 1 {
			// First poll: ReloadsFailed=2 becomes the baseline
			return ReloadStatus{ReloadsFailed: 2}
		}
		// Second poll: ReloadsFailed increments to 3, triggering failure
		return ReloadStatus{ReloadsFailed: 3, LastError: `configure "inference-router": servers: at least one server is required`}
	})

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloadFailed {
		t.Errorf("outcome = %v, want WriteReloadFailed", res.Outcome)
	}
	if !res.RolledBack {
		t.Error("RolledBack = false, want true")
	}
	if readFile(t, path) != before {
		t.Error("file was not restored to original")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 kept", st.Mode().Perm())
	}
}

// After a rollback, writing the same change again is NOT reported unchanged.
func TestWritePluginConfig_AfterRollbackWritingSameChangeIsNotUnchanged(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		c := calls.Add(1)
		if c == 1 {
			// First poll of first write: ReloadsFailed=2 becomes baseline
			return ReloadStatus{ReloadsFailed: 2}
		} else if c == 2 {
			// Second poll of first write: ReloadsFailed=3, triggers failure
			return ReloadStatus{ReloadsFailed: 3, LastError: "test refusal"}
		} else if c == 3 {
			// First poll of second write: ReloadsFailed=3 becomes new baseline
			return ReloadStatus{ReloadsFailed: 3}
		}
		// Subsequent polls: accept the reload
		return ReloadStatus{LastSuccess: time.Now()}
	})

	// First write fails and rolls back
	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("first WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloadFailed {
		t.Errorf("first outcome = %v, want WriteReloadFailed", res.Outcome)
	}
	if !res.RolledBack {
		t.Error("first RolledBack = false, want true")
	}

	// Second write of the same change should NOT be reported unchanged
	// (the file was rolled back to original, so the change is new again)
	res2, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("second WritePluginConfig: %v", err)
	}
	if res2.Outcome == WriteUnchanged {
		t.Error("second outcome = WriteUnchanged, want WriteReloaded (file is back to original after rollback)")
	}
	if res2.Outcome != WriteReloaded {
		t.Errorf("second outcome = %v, want WriteReloaded", res2.Outcome)
	}
}

// A timed-out reload leaves the file with the new bytes (state is uncertain). The
// deadline is the caller's ctx when that is shorter than LocalPollDeadline, so the
// test waits a fraction of a second rather than the real 30s.
func TestWritePluginConfig_OnTimeoutLeavesNewBytes(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	original := readFile(t, path)
	srv := makeStatusServer(t, func() ReloadStatus {
		// Never change status, so poll times out
		return ReloadStatus{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := WritePluginConfig(ctx, ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloadTimedOut {
		t.Errorf("outcome = %v, want WriteReloadTimedOut", res.Outcome)
	}
	after := readFile(t, path)
	if after == original {
		t.Error("file was restored; on timeout, new bytes should remain")
	}
	if !strings.Contains(after, "claude-code: glm") {
		t.Error("file should still have the new bytes after timeout")
	}
}

// refuseOnSecondPoll is a /reload/status that takes its baseline on the first
// poll and reports a refused reload on the second, after running during — the
// moment a person's own edit or a failing disk would land.
func refuseOnSecondPoll(t *testing.T, during func()) string {
	t.Helper()
	var calls atomic.Int32
	return makeStatusServer(t, func() ReloadStatus {
		if calls.Add(1) == 1 {
			return ReloadStatus{ReloadsFailed: 2}
		}
		during()
		return ReloadStatus{ReloadsFailed: 3, LastError: `configure "inference-router": servers: at least one server is required`}
	}).URL
}

// A person who edits the file while the proxy reloads agentop's write keeps the
// edit: the rollback puts back only what agentop wrote. The error says the file was
// left as found and why the proxy refused, and quotes nothing from the file, which
// holds API keys.
func TestWritePluginConfig_ARefusalLeavesAFileEditedDuringTheReloadAsFound(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	edited := strings.Replace(withRouter, "# only agents listed here are routed", "# edited by hand meanwhile", 1)
	stats := refuseOnSecondPoll(t, func() {
		if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
			t.Error(err)
		}
	})

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: stats, Changes: []ConfigChange{routeClaudeToGLM()}})
	if got := readFile(t, path); got != edited {
		t.Errorf("the edit made during the reload was overwritten:\n%s", Diff([]byte(edited), []byte(got)))
	}
	if err == nil {
		t.Fatalf("err = nil, result %+v; want an error saying the file was left as found", res)
	}
	for _, want := range []string{"changed while", "left as found", "at least one server is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
	for _, leak := range []string{"sk-ete", "edited by hand", "claude-code: glm"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("err = %q quotes the file (%q)", err, leak)
		}
	}
	if res.Outcome != WriteReloadFailed || res.RolledBack || !strings.Contains(res.ReloadError, "at least one server") {
		t.Errorf("result = %+v, want WriteReloadFailed with the proxy's error, not rolled back", res)
	}
}

// When the file cannot be put back after a refusal, the error still says why the
// proxy refused it, and the result still says what the proxy did.
func TestWritePluginConfig_AFailedRestoreStillSaysWhyTheProxyRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write into a read-only directory")
	}
	path := writeFixtureContent(t, withRouter, 0o600)
	dir := filepath.Dir(path)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	stats := refuseOnSecondPoll(t, func() {
		// The restore writes a sibling temp file, which a read-only directory refuses.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Error(err)
		}
	})

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: stats, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err == nil {
		t.Fatalf("err = nil, result %+v; want the failed restore reported", res)
	}
	for _, want := range []string{"at least one server is required", "by hand"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "sk-ete") {
		t.Errorf("err = %q quotes the file", err)
	}
	if res.Outcome != WriteReloadFailed || res.RolledBack || res.ReloadError == "" {
		t.Errorf("result = %+v, want WriteReloadFailed with the proxy's error, not rolled back", res)
	}
	if !strings.Contains(readFile(t, path), "claude-code: glm") {
		t.Error("the file does not hold the change, yet the restore was reported failed")
	}
}

// shortenPollSchedule makes PollUntilReloaded's unreachable path take milliseconds
// rather than its real ~16s of backoff.
func shortenPollSchedule(t *testing.T) {
	t.Helper()
	interval, max := pollInterval, pollMaxBackoff
	pollInterval, pollMaxBackoff = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { pollInterval, pollMaxBackoff = interval, max })
}

// A proxy that answers once and then stops answering has not refused anything,
// and the result must not say it did. The file is still put back: whether the
// proxy took the change is unknown, and one that went away may next start from it.
func TestWritePluginConfig_TellsAProxyThatStoppedAnsweringFromARefusal(t *testing.T) {
	shortenPollSchedule(t)
	path := writeFixtureContent(t, withRouter, 0o600)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(ReloadStatus{ReloadsFailed: 2})
			return
		}
		http.Error(w, "gone", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteStatusUnreachable || !strings.Contains(res.ReloadError, "unreachable") {
		t.Errorf("result = %+v: want WriteStatusUnreachable, not a refusal, with the poller's error", res)
	}
	if !res.RolledBack || readFile(t, path) != withRouter {
		t.Errorf("result = %+v; want the file put back as it was", res)
	}
}

// A refusal is still a refusal with the schedule shortened: the two outcomes differ.
func TestWritePluginConfig_ARefusalIsStillARefusal(t *testing.T) {
	shortenPollSchedule(t)
	path := writeFixtureContent(t, withRouter, 0o600)
	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: refuseOnSecondPoll(t, func() {}), Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloadFailed || !res.RolledBack {
		t.Errorf("result = %+v, want WriteReloadFailed, rolled back", res)
	}
}
