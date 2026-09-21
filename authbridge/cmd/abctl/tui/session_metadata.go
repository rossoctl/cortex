package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SessionMetadata is what a coding agent knows about one of its own sessions that
// Cortex does not.
//
// Cortex buckets traffic by session id and nothing more: `/v1/sessions` returns a
// UUID, and a UUID does not tell an operator which session it is. The agent that
// produced the traffic already knows — Claude Code writes a per-session transcript
// carrying an AI-generated title and the directory it ran in — so this is the shape
// that carries those facts back, keyed by the same id Cortex uses.
//
// Every field is optional. A harvester that cannot determine one must leave it empty
// rather than invent a value, and a consumer must render an entry that has only some
// of them: the fields come from an agent's own on-disk layout, which is not a
// contract anyone here controls.
//
// The tags here are the package's first `json:` tags — every other tagged type in
// tui is YAML, because those are abctl's own settings. This one is written as JSON to
// ~/.cortex/session-metadata.json. Key names stay camelCase to match settings.go's
// sortColumn / sortDesc rather than introducing a second convention.
type SessionMetadata struct {
	// Title is a human-readable name for the session. From Claude Code this is the
	// model-generated title when the transcript carries one, and otherwise the
	// working directory it ran in — so it may well be a path rather than a phrase,
	// and a consumer that assumes prose will be wrong most of the time (measured: 2
	// of 109 local sessions had a real title).
	Title string `json:"title,omitempty"`
	// AgentType names the agent that produced the session, e.g. "Claude Code". A
	// plain display string, not an enum: the set of agents Cortex fronts is open,
	// and a consumer showing this has no decision to make on it.
	AgentType string `json:"agentType,omitempty"`
	// AgentConfigDir is the directory the metadata was harvested from — the parent
	// of the agent's own per-session layout. Recorded per entry rather than once per
	// file so a future harvester can merge two agents, or two config dirs of one
	// agent, into the same map without the entries becoming ambiguous.
	AgentConfigDir string `json:"agentConfigDir,omitempty"`
	// LogFile is the transcript this entry was read from, as a path. The one field
	// that lets a reader go and check: a title that looks wrong is answerable by
	// opening the file it came from.
	LogFile string `json:"logFile,omitempty"`
}

// SessionMetadataRel is the harvested metadata file, relative to the user's home.
//
// Exported so `abctl experimental read-claude-sessions`, which writes it from package
// main, names the same path this package reads. Two constants for one path is the kind
// of drift that only shows up as an empty column nobody can explain.
//
// Follows yankDirRel and cmd_claudecode.go's cortexCfgRel / stateRel, so there is one
// ~/.cortex tree rather than a new dotfile per feature.
const SessionMetadataRel = ".cortex/session-metadata.json"

// SessionMetadataPath returns ~/.cortex/session-metadata.json.
func SessionMetadataPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	if home == "" {
		// A separate branch, not folded into the one above: %w on a nil error renders as
		// "%!w(<nil>)", which would make this path unreadable to whoever hits it. Same
		// shape as yankDir and userConfigPath.
		return "", errors.New("cannot determine your home directory: it is empty")
	}
	return filepath.Join(home, SessionMetadataRel), nil
}

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
