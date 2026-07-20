package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
)

const editFixtureCMYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config-email-agent
  namespace: team1
data:
  config.yaml: |
    mode: proxy-sidecar
    pipeline:
      inbound:
        - name: jwt-validation
    session:
      enabled: true
`

// editFakeRunner records args + returns canned responses.
type editFakeRunner struct {
	getResponse   []byte
	captured      []string
	applyManifest []byte
}

func (f *editFakeRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	f.captured = append(f.captured, strings.Join(args, " "))
	if len(args) > 0 && args[0] == "get" {
		// Pod-label lookup for ResolveAgentName: kubectl get pod ... -o jsonpath=...
		if len(args) >= 2 && args[1] == "pod" {
			return []byte("email-agent"), nil
		}
		return f.getResponse, nil
	}
	if len(args) > 0 && args[0] == "apply" {
		path := args[len(args)-1]
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f.applyManifest = b
		return []byte("applied"), nil
	}
	return nil, nil
}

// TestEditFlow_HappyPath drives the full state machine with stubs:
// e → fetch → editor → validate → diff → y → apply → poll → done.
//
// Bypasses the real $EDITOR by writing the "edited" content to the
// tempfile directly, then injecting editorExitedMsg{err: nil}.
func TestEditFlow_HappyPath(t *testing.T) {
	runner := &editFakeRunner{getResponse: []byte(editFixtureCMYAML)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"last_success": time.Now().Add(1 * time.Hour).Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	m := newPickerModel(context.Background(), nil, nil)
	m.statusURL = srv.URL
	m.editRunner = runner.run
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	// Press "e".
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	if mm.editState.phase != editPhaseFetching {
		t.Fatalf("phase = %v, want editPhaseFetching", mm.editState.phase)
	}
	if cmd == nil {
		t.Fatal("expected fetch Cmd")
	}

	// Run FetchCmd.
	fetchedMsg := cmd().(genFetchedMsg)
	if fetchedMsg.Err != nil {
		t.Fatalf("Fetch failed: %v", fetchedMsg.Err)
	}
	defer os.Remove(fetchedMsg.TempPath)

	// Bypass the editor: write a modified subtree directly.
	editedSubtree := []byte("pipeline:\n  inbound:\n    - name: jwt-validation\n      config:\n        new_key: new_value\n")
	if err := os.WriteFile(fetchedMsg.TempPath, editedSubtree, 0o600); err != nil {
		t.Fatal(err)
	}

	updated, _ = mm.Update(fetchedMsg)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseEditing {
		t.Fatalf("phase = %v, want editPhaseEditing", mm.editState.phase)
	}

	// Inject editorExitedMsg directly (skips the real ExecProcess).
	updated, _ = mm.Update(editorExitedMsg{gen: mm.editState.generation, err: nil})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDiff {
		t.Fatalf("phase = %v, want editPhaseDiff (validate should pass)", mm.editState.phase)
	}
	if mm.editState.diff == "" {
		t.Fatal("diff should be populated")
	}

	// Confirm with "y".
	updated, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseApplying {
		t.Fatalf("phase = %v, want editPhaseApplying", mm.editState.phase)
	}
	if cmd == nil {
		t.Fatal("expected apply Cmd")
	}

	// Run ApplyCmd.
	appliedMsg := cmd().(genAppliedMsg)
	if appliedMsg.Err != nil {
		t.Fatalf("apply failed: %v", appliedMsg.Err)
	}

	updated, cmd = mm.Update(appliedMsg)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseWaiting {
		t.Fatalf("phase = %v, want editPhaseWaiting", mm.editState.phase)
	}
	if cmd == nil {
		t.Fatal("expected poll Cmd")
	}

	// Run PollCmd.
	polledMsg := cmd().(genPolledMsg)
	if polledMsg.Result.Status != edit.PollSuccess {
		t.Fatalf("poll status = %v, want PollSuccess", polledMsg.Result.Status)
	}

	updated, _ = mm.Update(polledMsg)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDone {
		t.Fatalf("phase = %v, want editPhaseDone", mm.editState.phase)
	}

	// The applied manifest should contain the new content.
	if !strings.Contains(string(runner.applyManifest), "new_key: new_value") {
		t.Fatalf("manifest missing new content:\n%s", runner.applyManifest)
	}
}

// TestEditFlow_NCancelsAtDiff verifies "N" at the confirm prompt
// returns to panePipeline without applying.
func TestEditFlow_NCancelsAtDiff(t *testing.T) {
	runner := &editFakeRunner{getResponse: []byte(editFixtureCMYAML)}
	m := newPickerModel(context.Background(), nil, nil)
	m.editRunner = runner.run
	m.statusURL = "http://stub"
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	fetchedMsg := cmd().(genFetchedMsg)
	defer os.Remove(fetchedMsg.TempPath)

	// Pretend the user edited.
	editedSubtree := []byte("pipeline:\n  inbound:\n    - name: jwt-validation\n      config:\n        x: 1\n")
	_ = os.WriteFile(fetchedMsg.TempPath, editedSubtree, 0o600)

	updated, _ = mm.Update(fetchedMsg)
	mm = updated.(*model)
	updated, _ = mm.Update(editorExitedMsg{gen: mm.editState.generation, err: nil})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDiff {
		t.Fatalf("setup: phase = %v, want editPhaseDiff", mm.editState.phase)
	}

	// Press "N".
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDone {
		t.Fatalf("phase = %v, want editPhaseDone (N should cancel)", mm.editState.phase)
	}
	for _, c := range runner.captured {
		if strings.HasPrefix(c, "apply") {
			t.Fatalf("apply ran despite N: %q", c)
		}
	}
}

// TestEditFlow_NormalizesTrailingNewline verifies that the
// editorExitedMsg handler appends a trailing newline to the user's
// edit if missing — preventing the last line of the new subtree
// from concatenating with the next top-level YAML key.
func TestEditFlow_NormalizesTrailingNewline(t *testing.T) {
	runner := &editFakeRunner{getResponse: []byte(editFixtureCMYAML)}
	m := newPickerModel(context.Background(), nil, nil)
	m.editRunner = runner.run
	m.statusURL = "http://stub"
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	fetchedMsg := cmd().(genFetchedMsg)
	defer os.Remove(fetchedMsg.TempPath)

	// Write an edit deliberately missing a trailing newline.
	edited := []byte("pipeline:\n  inbound:\n    - name: jwt-validation\n      config:\n        x: 1")
	if edited[len(edited)-1] == '\n' {
		t.Fatal("test fixture should be missing trailing newline")
	}
	if err := os.WriteFile(fetchedMsg.TempPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	updated, _ = mm.Update(fetchedMsg)
	mm = updated.(*model)
	updated, _ = mm.Update(editorExitedMsg{gen: mm.editState.generation, err: nil})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDiff {
		t.Fatalf("phase = %v, want editPhaseDiff", mm.editState.phase)
	}
	if len(mm.editState.editedRaw) == 0 || mm.editState.editedRaw[len(mm.editState.editedRaw)-1] != '\n' {
		t.Fatalf("editedRaw should be normalized to end with newline; got %q",
			mm.editState.editedRaw)
	}
}

// TestEditFlow_RollbackOnReloadFailure verifies that when the in-pod
// reload fails (PollFailure), abctl re-applies the original ConfigMap
// content so the on-disk CM matches the still-running previous pipeline.
func TestEditFlow_RollbackOnReloadFailure(t *testing.T) {
	runner := &editFakeRunner{getResponse: []byte(editFixtureCMYAML)}
	m := newPickerModel(context.Background(), nil, nil)
	m.editRunner = runner.run
	m.statusURL = "http://stub"
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	// Drive through fetch → editor → diff → apply.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	fetchedMsg := cmd().(genFetchedMsg)
	if fetchedMsg.Err != nil {
		t.Fatalf("Fetch failed: %v", fetchedMsg.Err)
	}
	defer os.Remove(fetchedMsg.TempPath)

	editedSubtree := []byte("pipeline:\n  inbound:\n    - name: bogus\n")
	if err := os.WriteFile(fetchedMsg.TempPath, editedSubtree, 0o600); err != nil {
		t.Fatal(err)
	}

	updated, _ = mm.Update(fetchedMsg)
	mm = updated.(*model)
	updated, _ = mm.Update(editorExitedMsg{gen: mm.editState.generation, err: nil})
	mm = updated.(*model)
	updated, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm = updated.(*model)
	appliedMsg := cmd().(genAppliedMsg)
	if appliedMsg.Err != nil {
		t.Fatalf("apply failed: %v", appliedMsg.Err)
	}
	updated, _ = mm.Update(appliedMsg)
	mm = updated.(*model)

	// Inject a PollFailure manually with the current generation.
	failure := genPolledMsg{
		gen:       mm.editState.generation,
		PolledMsg: edit.PolledMsg{Result: edit.PollResult{Status: edit.PollFailure, LastError: "unknown plugin: bogus"}},
	}
	updated, cmd = mm.Update(failure)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseRollback {
		t.Fatalf("phase = %v, want editPhaseRollback", mm.editState.phase)
	}
	if cmd == nil {
		t.Fatal("expected RollbackCmd")
	}

	rolledBack := cmd().(genRolledBackMsg)
	if rolledBack.Err != nil {
		t.Fatalf("rollback Apply error: %v", rolledBack.Err)
	}
	updated, _ = mm.Update(rolledBack)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseError {
		t.Fatalf("phase = %v, want editPhaseError after rollback", mm.editState.phase)
	}
	if !strings.Contains(mm.editState.err, "rolled back") {
		t.Fatalf("error should mention rollback; got %q", mm.editState.err)
	}
	// The rollback Apply (the LAST apply call) should have the
	// ORIGINAL pipeline content, not the bogus one.
	if strings.Contains(string(runner.applyManifest), "name: bogus") {
		t.Fatalf("rollback manifest still contains bogus content:\n%s", runner.applyManifest)
	}
	if !strings.Contains(string(runner.applyManifest), "name: jwt-validation") {
		t.Fatalf("rollback manifest missing original content:\n%s", runner.applyManifest)
	}
}

// TestEditFlow_LatePolledMsgAfterAbort verifies that a PolledMsg
// arriving after the user has aborted the edit (editState reset to
// Done, fetched = nil) is dropped rather than panicking on a nil
// dereference of editState.fetched.
func TestEditFlow_LatePolledMsgAfterAbort(t *testing.T) {
	m := newPickerModel(context.Background(), nil, nil)
	m.pane = panePipeline
	// editState.phase is editPhaseDone (zero value), fetched is nil.

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("late PolledMsg should be dropped, but panicked: %v", r)
		}
	}()
	// Late message with a stale gen should also be dropped — the gen
	// guard is the more robust discriminator.
	late := genPolledMsg{
		gen:       42,
		PolledMsg: edit.PolledMsg{Result: edit.PollResult{Status: edit.PollFailure, LastError: "anything"}},
	}
	updated, _ := m.Update(late)
	mm := updated.(*model)
	if mm.editState.phase != editPhaseDone {
		t.Fatalf("phase changed on late PolledMsg: %v", mm.editState.phase)
	}
}

// TestEditFlow_BackgroundedSuccess verifies that pressing Esc during
// Waiting moves the watch to the background and a later PollSuccess
// flashes the result instead of just being dropped.
func TestEditFlow_BackgroundedSuccess(t *testing.T) {
	runner := &editFakeRunner{getResponse: []byte(editFixtureCMYAML)}
	m := newPickerModel(context.Background(), nil, nil)
	m.editRunner = runner.run
	m.statusURL = "http://stub"
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	// Drive through fetch → editor → diff → apply → waiting.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	fetchedMsg := cmd().(genFetchedMsg)
	defer os.Remove(fetchedMsg.TempPath)
	_ = os.WriteFile(fetchedMsg.TempPath,
		[]byte("pipeline:\n  inbound:\n    - name: jwt-validation\n      config:\n        x: 1\n"), 0o600)
	updated, _ = mm.Update(fetchedMsg)
	mm = updated.(*model)
	updated, _ = mm.Update(editorExitedMsg{gen: mm.editState.generation, err: nil})
	mm = updated.(*model)
	updated, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm = updated.(*model)
	updated, _ = mm.Update(cmd().(genAppliedMsg))
	mm = updated.(*model)
	if mm.editState.phase != editPhaseWaiting {
		t.Fatalf("setup: phase = %v, want editPhaseWaiting", mm.editState.phase)
	}

	// Press Esc — should background, not reset.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseBackground {
		t.Fatalf("Esc-during-waiting: phase = %v, want editPhaseBackground", mm.editState.phase)
	}
	if mm.editState.fetched == nil {
		t.Fatal("fetched should remain populated for late PolledMsg handling")
	}

	// Late PollSuccess arrives — should flash and reset to Done.
	success := genPolledMsg{
		gen:       mm.editState.generation,
		PolledMsg: edit.PolledMsg{Result: edit.PollResult{Status: edit.PollSuccess}},
	}
	updated, _ = mm.Update(success)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDone {
		t.Fatalf("after late success: phase = %v, want editPhaseDone", mm.editState.phase)
	}
	if !strings.Contains(mm.flash, "succeeded") {
		t.Fatalf("expected success flash; got %q", mm.flash)
	}
}

// TestEditFlow_StaleMsgFromAbandonedEditDropped verifies that a
// PolledMsg from Edit 1 (whose user Esc'd then started Edit 2 quickly)
// is dropped and does NOT route Edit 1's reload result onto Edit 2.
func TestEditFlow_StaleMsgFromAbandonedEditDropped(t *testing.T) {
	runner := &editFakeRunner{getResponse: []byte(editFixtureCMYAML)}
	m := newPickerModel(context.Background(), nil, nil)
	m.editRunner = runner.run
	m.statusURL = "http://stub"
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	// Edit 1: drive to Waiting, then abort.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	fetchedMsg1 := cmd().(genFetchedMsg)
	defer os.Remove(fetchedMsg1.TempPath)
	gen1 := mm.editState.generation

	_ = os.WriteFile(fetchedMsg1.TempPath,
		[]byte("pipeline:\n  inbound:\n    - name: jwt-validation\n      config:\n        x: 1\n"), 0o600)
	updated, _ = mm.Update(fetchedMsg1)
	mm = updated.(*model)
	updated, _ = mm.Update(editorExitedMsg{gen: gen1, err: nil})
	mm = updated.(*model)
	updated, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm = updated.(*model)
	updated, _ = mm.Update(cmd().(genAppliedMsg))
	mm = updated.(*model)

	// User aborts Edit 1.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(*model)
	// (phase is now editPhaseBackground; Esc on Waiting backgrounds.
	// Move it fully out so we can start Edit 2.)
	mm.editState = editState{phase: editPhaseDone, generation: gen1}

	// Edit 2 starts.
	updated, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm = updated.(*model)
	if mm.editState.generation == gen1 {
		t.Fatal("Edit 2's generation should differ from Edit 1's")
	}
	gen2 := mm.editState.generation

	// Edit 1's stale PollFailure arrives now.
	stale := genPolledMsg{
		gen:       gen1,
		PolledMsg: edit.PolledMsg{Result: edit.PollResult{Status: edit.PollFailure, LastError: "edit 1 reload"}},
	}
	updated, returnedCmd := mm.Update(stale)
	mm = updated.(*model)
	// Edit 2 must remain in Fetching (its only allowed phase post-`e`),
	// not jump to Rollback or Error from Edit 1's stale msg.
	if mm.editState.phase != editPhaseFetching {
		t.Fatalf("phase = %v, want editPhaseFetching (stale msg should be ignored)", mm.editState.phase)
	}
	if mm.editState.generation != gen2 {
		t.Fatalf("Edit 2's gen mutated: %d vs %d", mm.editState.generation, gen2)
	}
	if returnedCmd != nil {
		t.Fatal("stale PolledMsg should not produce a Cmd")
	}
	// Quiet "cmd is unused after Edit 2 dispatch" by referencing it.
	_ = cmd
}

// TestEditFlow_PostSpliceYAMLError verifies the post-splice YAML
// validation: an edit that parses standalone but produces an invalid
// combined inner YAML when spliced back routes to editPhaseError
// before the diff phase, instead of waiting for kubelet sync to
// surface the framework's reload error.
func TestEditFlow_PostSpliceYAMLError(t *testing.T) {
	runner := &editFakeRunner{getResponse: []byte(editFixtureCMYAML)}
	m := newPickerModel(context.Background(), nil, nil)
	m.editRunner = runner.run
	m.statusURL = "http://stub"
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	fetchedMsg := cmd().(genFetchedMsg)
	defer os.Remove(fetchedMsg.TempPath)

	// Edit with leading whitespace on the pipeline: line. The subtree
	// parses standalone, but splicing it back produces broken indent
	// against the surrounding mode:/session: keys.
	bad := []byte("  pipeline:\n    inbound:\n      - name: jwt-validation\n")
	if err := os.WriteFile(fetchedMsg.TempPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	updated, _ = mm.Update(fetchedMsg)
	mm = updated.(*model)
	updated, _ = mm.Update(editorExitedMsg{gen: mm.editState.generation, err: nil})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseError {
		t.Fatalf("phase = %v, want editPhaseError (post-splice YAML should fail)", mm.editState.phase)
	}
	if !strings.Contains(mm.editState.err, "after splice") {
		t.Fatalf("error should mention splice; got %q", mm.editState.err)
	}
}
