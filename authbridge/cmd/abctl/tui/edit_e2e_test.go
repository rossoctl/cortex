package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/edit"
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
			"last_success_unix": int64(99999999999),
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
	fetchedMsg := cmd().(edit.FetchedMsg)
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
	updated, _ = mm.Update(editorExitedMsg{err: nil})
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
	appliedMsg := cmd().(edit.AppliedMsg)
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
	polledMsg := cmd().(edit.PolledMsg)
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
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	fetchedMsg := cmd().(edit.FetchedMsg)
	defer os.Remove(fetchedMsg.TempPath)

	// Pretend the user edited.
	editedSubtree := []byte("pipeline:\n  inbound:\n    - name: jwt-validation\n      config:\n        x: 1\n")
	_ = os.WriteFile(fetchedMsg.TempPath, editedSubtree, 0o600)

	updated, _ = mm.Update(fetchedMsg)
	mm = updated.(*model)
	updated, _ = mm.Update(editorExitedMsg{err: nil})
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
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	fetchedMsg := cmd().(edit.FetchedMsg)
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
	updated, _ = mm.Update(editorExitedMsg{err: nil})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDiff {
		t.Fatalf("phase = %v, want editPhaseDiff", mm.editState.phase)
	}
	if len(mm.editState.editedRaw) == 0 || mm.editState.editedRaw[len(mm.editState.editedRaw)-1] != '\n' {
		t.Fatalf("editedRaw should be normalized to end with newline; got %q",
			mm.editState.editedRaw)
	}
}
