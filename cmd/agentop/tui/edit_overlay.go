package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
)

// editPhase tracks where the edit state machine currently sits.
type editPhase int

const (
	editPhaseDone editPhase = iota // not editing
	editPhaseFetching
	editPhaseEditing // $EDITOR is running; bubbletea is suspended
	editPhaseValidating
	editPhaseDiff
	editPhaseApplying
	editPhaseWaiting
	editPhaseRollback // re-applying the original ConfigMap after a failed reload
	// editPhaseBackground means: user pressed Esc during Waiting/Rollback;
	// the in-flight Cmd is still running and we want to flash its result
	// in the footer rather than reopen the overlay. Overlay renders nothing
	// in this phase. fetched/applyTime stay populated so the PolledMsg
	// handler can still trigger rollback if the in-pod reload failed.
	editPhaseBackground
	editPhaseError
)

// editState lives on *model when an edit is in flight.
type editState struct {
	phase   editPhase
	fetched *edit.FetchedPipeline
	// store is where this edit reads and writes; statusURL is the base whose
	// /reload/status confirms it landed. Captured together when the cycle
	// begins, and read from here rather than off the model for the rest of it,
	// so one transaction always targets one proxy — an operator who switches
	// pods or presses [l] mid-edit cannot have the apply land somewhere the
	// fetch never came from.
	store     edit.Store
	statusURL string
	tempPath  string
	editedRaw []byte // bytes the user wrote in $EDITOR
	diff      string // colorized output from edit.Diff
	err       string // single-line message in editPhaseError
	applyTime time.Time
	// validationErrs are dependency/claim issues agentop detected before
	// apply by checking the proposed pipeline against the plugin
	// catalog. Empty when validation passed or the catalog isn't loaded.
	// Rendered above the diff in the editPhaseDiff overlay so operators
	// see them before deciding to apply. Non-blocking — the framework's
	// own validateRelationships is still the source of truth at reload.
	validationErrs []edit.ValidationError
	// generation is bumped each time a fresh edit cycle begins (initial
	// `e`, retry from error, restart after abort). Each tea.Cmd captures
	// the value at issue time; handlers drop messages whose captured
	// generation doesn't match the current one. Without this, a late
	// PolledMsg from Edit 1 arriving after the user has Esc'd and
	// started Edit 2 would route Edit 1's reload result onto Edit 2's
	// overlay (same phase, different transaction).
	generation int
}

// describeTarget is store.Describe() with a neutral fallback for a nil store.
//
// Nil happens: the overlay-render tests build an editState from a phase alone,
// and a renderer that panicked on that would make every one of them a
// constructor exercise. The fallback says nothing false about either backend.
func describeTarget(s edit.Store) edit.Target {
	if s == nil {
		return edit.Target{
			Noun:          "config",
			WaitHint:      "waiting for the framework to pick up the change",
			OutOfSyncHint: "inspect the stored config",
		}
	}
	return s.Describe()
}

// renderEditOverlay returns the overlay content (rendered into a
// styled box) for the current edit phase. width/height are the
// terminal's full dimensions; the overlay sizes itself to fit
// comfortably inside.
func renderEditOverlay(s editState, width, height int) string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Width(min(width-4, 100))

	target := describeTarget(s.store)

	var b strings.Builder
	switch s.phase {
	case editPhaseFetching:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Fetching " + target.Noun + "…")
	case editPhaseEditing:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString(fmt.Sprintf("Editor open at %s", s.tempPath))
	case editPhaseValidating:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Validating YAML…")
	case editPhaseDiff:
		b.WriteString(styleTitle.Render("Edit pipeline — review diff"))
		b.WriteString("\n\n")
		// Validation banner: render BEFORE the diff so operators see
		// dependency issues at first glance. Non-blocking — apply still
		// works.
		if len(s.validationErrs) > 0 {
			b.WriteString(styleError.Render(fmt.Sprintf(
				"⚠ %d validation issue%s — framework reload will reject:",
				len(s.validationErrs), plural(len(s.validationErrs)))))
			b.WriteString("\n")
			for _, ve := range s.validationErrs {
				b.WriteString(fmt.Sprintf("  • [%s] %s pos %d: %s\n",
					ve.Direction, ve.PluginName, ve.Position, ve.Message))
			}
			b.WriteString("\n")
		}
		b.WriteString(s.diff)
		b.WriteString("\n")
		if len(s.validationErrs) > 0 {
			b.WriteString(styleHint.Render("apply anyway? (y/N)"))
		} else {
			b.WriteString(styleHint.Render("apply this change? (y/N)"))
		}
	case editPhaseApplying:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Applying to " + target.Noun + "…")
	case editPhaseWaiting:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Waiting for hot-reload…")
		b.WriteString("\n")
		b.WriteString(styleHint.Render("(" + target.WaitHint + ")"))
	case editPhaseRollback:
		b.WriteString(styleTitle.Render("Edit pipeline — rolling back"))
		b.WriteString("\n\n")
		b.WriteString("Reload failed. Restoring previous " + target.Noun + "…")
	case editPhaseError:
		b.WriteString(styleTitle.Render("Edit pipeline — error"))
		b.WriteString("\n\n")
		b.WriteString(s.err)
		b.WriteString("\n\n")
		b.WriteString(styleHint.Render("[r] re-edit  [Esc] back to Pipeline"))
	}
	return box.Render(b.String())
}

// handleEditKey is the keymap that takes over while an edit is in flight.
func (m *model) handleEditKey(msg tea.KeyMsg) tea.Cmd {
	switch m.editState.phase {
	case editPhaseDiff:
		switch msg.String() {
		case "y", "Y":
			m.editState.phase = editPhaseApplying
			newSubtree := m.editState.editedRaw
			newInner := edit.Splice(
				m.editState.fetched.InnerYAML,
				m.editState.fetched.PipelineStart,
				m.editState.fetched.PipelineEnd,
				newSubtree,
			)
			payload, err := m.editState.store.Build(m.editState.fetched, newInner)
			if err != nil {
				m.editState.phase = editPhaseError
				m.editState.err = "build payload: " + err.Error()
				return nil
			}
			return withGen(m.editState.generation, edit.ApplyCmd(
				m.ctx, m.editState.store, m.editState.fetched, payload))
		case "n", "N", "esc":
			m.editState = editState{phase: editPhaseDone}
			return nil
		}
		return nil
	case editPhaseError:
		switch msg.String() {
		case "r":
			// If the fetch never completed (tempPath empty), retry the
			// fetch instead of opening $EDITOR on "" (which leaves the
			// user with nothing to edit and a misleading flow). A retry
			// bumps gen so any straggling messages from the failed
			// attempt are dropped.
			if m.editState.tempPath == "" {
				gen := m.editState.generation + 1
				// The store the failed attempt used, not a fresh
				// pipelineStore(): a retry is the same transaction, so
				// re-resolving could silently retarget it.
				store, statusURL := m.editState.store, m.editState.statusURL
				m.editState = editState{phase: editPhaseFetching, generation: gen, store: store, statusURL: statusURL}
				return withGen(gen, edit.FetchCmd(m.ctx, store, m.client, catalogPlugins(m.catalog)))
			}
			m.editState.phase = editPhaseEditing
			return openEditorCmd(m.editState.generation, m.editState.tempPath)
		case "esc":
			m.editState = editState{phase: editPhaseDone}
			return nil
		}
		return nil
	}
	// Other phases: Esc backgrounds (Waiting / Rollback) so the in-flight
	// Cmd's eventual result lands as a footer flash, or cancels outright
	// (Fetching / Editing / Applying — phases where the result is still
	// safe to drop).
	if msg.String() == "esc" {
		switch m.editState.phase {
		case editPhaseWaiting, editPhaseRollback:
			m.editState.phase = editPhaseBackground
			m.setFlash("hot-reload watch moved to background; you'll be notified")
		default:
			m.editState = editState{phase: editPhaseDone}
		}
		return nil
	}
	return nil
}
