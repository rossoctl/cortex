package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
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
	phase     editPhase
	fetched   *edit.FetchedPipeline
	tempPath  string
	editedRaw []byte // bytes the user wrote in $EDITOR
	diff      string // colorized output from edit.Diff
	err       string // single-line message in editPhaseError
	applyTime time.Time
	// validationErrs are dependency/claim issues abctl detected before
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

// renderEditOverlay returns the overlay content (rendered into a
// styled box) for the current edit phase. width/height are the
// terminal's full dimensions; the overlay sizes itself to fit
// comfortably inside.
func renderEditOverlay(s editState, width, height int) string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Width(min(width-4, 100))

	var b strings.Builder
	switch s.phase {
	case editPhaseFetching:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Fetching ConfigMap…")
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
		b.WriteString("Applying to ConfigMap…")
	case editPhaseWaiting:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Waiting for hot-reload…")
		b.WriteString("\n")
		b.WriteString(styleHint.Render("(this can take up to 120s while kubelet syncs the ConfigMap)"))
	case editPhaseRollback:
		b.WriteString(styleTitle.Render("Edit pipeline — rolling back"))
		b.WriteString("\n\n")
		b.WriteString("Reload failed. Restoring previous ConfigMap…")
	case editPhaseError:
		b.WriteString(styleTitle.Render("Edit pipeline — error"))
		b.WriteString("\n\n")
		b.WriteString(s.err)
		b.WriteString("\n\n")
		b.WriteString(styleHint.Render("[r] re-edit  [Esc] back to Pipeline"))
	}
	return box.Render(b.String())
}
