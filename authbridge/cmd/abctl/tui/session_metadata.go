package tui

import (
	"encoding/json"
	"io"
	"os"

	"github.com/rossoctl/cortex/authbridge/authlib/observe/claude"
)

// SessionMetadata is what a coding agent knows about one of its own sessions that Cortex
// does not. Owned by authlib/observe/claude, which is where its documentation lives.
//
// An ALIAS, not a distinct type. The shape is the on-disk contract between the harvester
// and this reader, and authlib cannot import tui, so the definition had to move there —
// but aliasing means map[string]SessionMetadata here and map[string]claude.SessionMetadata
// there are the SAME type, so every consumer in this package keeps its own spelling and
// assigns what the harvester returns with no conversion.
type SessionMetadata = claude.SessionMetadata

// SessionMetadataRel is the harvested metadata file, relative to the user's home.
// Re-exported from authlib/observe/claude so this package's readers and its tests keep one
// spelling; a const cannot be aliased.
const SessionMetadataRel = claude.SessionMetadataRel

// SessionMetadataPath returns ~/.cortex/session-metadata.json.
func SessionMetadataPath() (string, error) { return claude.SessionMetadataPath() }

// LoadSessionMetadata reads the harvested titles, degrading to none rather than
// failing.
//
// It never returns an error, for the reason loadUserConfig does not: this file is a
// convenience cache another command produces, and a missing or corrupt one must not
// keep the viewer from opening — the viewer being the tool you reach for when
// everything else is broken. Every failure yields an empty map, which renders as an
// empty TITLE column: the pane still works, it just cannot name anything.
//
// Absent is not a failure at all. Nobody has this file until they run
// `abctl experimental read-claude-sessions`, so a first run must be silent rather than
// scolded.
//
// Silent on a corrupt file rather than warning, unlike loadUserConfig: that one reports
// a broken settings file because the user WROTE it and their edit is being ignored.
// This file is machine-generated, so the actionable answer is to re-run the harvester,
// and there is nowhere safe to say so — the caller loads it while building the model,
// and once tea.NewProgram takes the alt screen anything written to the terminal
// corrupts the frame instead of reaching anyone.
func LoadSessionMetadata(path string) map[string]SessionMetadata {
	out := map[string]SessionMetadata{}
	if path == "" {
		return out
	}
	// BOUNDED. A stray large file at this path would otherwise be read whole and decoded
	// before the TUI starts, stalling startup with nothing on screen to say why — the load
	// happens while the model is built, before tea.NewProgram, so there is nowhere to report
	// it. 16 MiB is far past any real metadata file: the measured 109-session file is 42 KB.
	const maxMetadataBytes = 16 << 20
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return out
	}
	defer f.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes))
	if err != nil {
		return out
	}
	var m map[string]SessionMetadata
	if err := json.Unmarshal(b, &m); err != nil {
		return out
	}
	if m == nil {
		// A file holding JSON `null` decodes to a nil map. Indistinguishable from empty
		// for lookup, but a nil map returned here would be a second empty-ish value for
		// callers to reason about, so normalise it.
		return out
	}
	return m
}

// loadSessionMetadataForModel resolves the path and loads it, for a model constructor.
//
// Its own function because BOTH constructors need it — New and newPickerModel, which
// namespaces_pane.go's comment requires to mirror each other — and because neither can
// handle an error usefully: a home directory abctl cannot resolve costs a label here,
// nothing more, so the path error collapses into the same empty map every other failure
// yields.
func loadSessionMetadataForModel() map[string]SessionMetadata {
	path, err := SessionMetadataPath()
	if err != nil {
		return map[string]SessionMetadata{}
	}
	return LoadSessionMetadata(path)
}

// sessionLabel names a session for a header: "title (id)", or the bare id when nothing
// names it.
//
// One helper for three headers — the events pane, the event viewer and the usage pane —
// because an operator who picked a row by its title should keep seeing that title after
// pressing Enter. Reading the id back out of a header to check you are in the right place
// is the thing having titles is supposed to end.
//
// The id is kept in every case, never replaced. It is what /v1/sessions is keyed by, what
// a curl or a bug report has to quote, and the only one of the two that is guaranteed
// unique — two sessions in the same directory get the same harvested title, so a title
// alone would make them indistinguishable in a header.
func (m *model) sessionLabel(id string) string {
	if title := m.sessionTitle(id); title != "" {
		return title + " (" + id + ")"
	}
	return id
}
