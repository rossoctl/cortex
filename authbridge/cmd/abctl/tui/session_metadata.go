package tui

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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

// HarvestFunc reads an agent's transcripts and returns what it learned, keyed by session id.
//
// A function on RunOptions rather than a direct call into authlib/observe/claude, so this
// package keeps knowing nothing about where titles come from: it renders a map. main supplies
// the Claude Code implementation, and a test supplies a stub without needing a transcript tree
// on disk.
//
// Returning the map rather than writing the file is deliberate. The harvester still persists
// to ~/.cortex/session-metadata.json — that is what makes the NEXT launch instant — but the
// running viewer must not have to re-read a file it already has a newer version of in memory.
type HarvestFunc func() (map[string]SessionMetadata, error)

// harvestedMsg carries a finished background harvest.
//
// The result only, with no error field: a harvest that failed is indistinguishable here from one
// that found nothing, because the viewer is already open and there is nowhere to print without
// corrupting the frame. Carrying an error the handler could not act on only made the struct claim
// otherwise. main reports the failures that are knowable before the alt screen goes up; this is
// the part that cannot be reported at all.
type harvestedMsg struct {
	meta map[string]SessionMetadata
}

// harvestCmd runs the harvest off the UI goroutine.
//
// This is the whole reason the startup scan no longer blocks: bubbletea runs a tea.Cmd in its
// own goroutine and delivers the result as a message, so the picker paints immediately and the
// titles land whenever the scan finishes. A full scan of a large ~/.claude is ~0.7s, which is
// dead time in front of an empty screen if done before tea.NewProgram.
//
// Returns nil when no harvester was supplied, which is what a test and `--skip-claude-metadata`
// both produce; bubbletea treats a nil Cmd as nothing to do.
func harvestCmd(h HarvestFunc) tea.Cmd {
	if h == nil {
		return nil
	}
	return func() tea.Msg {
		// The error is dropped HERE, at the one place that could still have reported it, and
		// deliberately: see harvestedMsg. A failed harvest costs the TITLE column and nothing
		// else, which is the same posture LoadSessionMetadata takes on the same file.
		meta, _ := h()
		return harvestedMsg{meta: meta}
	}
}

// untitledSettled reports whether some session on screen has no title yet and has been
// quiet long enough that its transcript is probably complete on disk.
//
// THE SESSIONS LIST IS A PICKER TOO, which is what this exists for. reharvestInterval was
// written for the namespaces/pods panes on the reasoning that "once a session view is
// up the titles on screen are already loaded" — true of a session's own events pane, and
// false of the list you choose a session FROM, which gains a row whenever a new session
// appears and cannot name it without re-reading the transcripts.
//
// KEYED OFF TRAFFIC, not off a wall clock. A session with events has a transcript being
// appended to, so a harvest triggered by its own updates arrives seconds after the title
// becomes readable instead of up to reharvestInterval later. The settle delay is what makes
// this cheap: without it every event on a still-unnamed session would trigger a scan, and
// with it a busy session is harvested once, after it pauses.
//
// Only sessions the metadata does NOT name are considered, so the steady state — every row
// titled — triggers nothing at all and costs one map lookup per row per tick.
func (m *model) untitledSettled(now time.Time) bool {
	for _, s := range m.sessions {
		if m.sessionHasTitle(s.ID) {
			continue
		}
		// A ZERO UpdatedAt IS NOT "QUIET SINCE THE EPOCH". The field is whatever /v1/sessions
		// sent, and a summary that omits it decodes to the zero time — which would otherwise
		// read as settled by ~55 years and trigger a harvest on the first tick, before the
		// transcript of a brand new session is necessarily on disk. Unknown is not settled.
		if s.UpdatedAt.IsZero() {
			continue
		}
		if now.Sub(s.UpdatedAt) >= untitledSettleDelay {
			return true
		}
	}
	return false
}

// sessionHasTitle reports whether this session renders a title, as the TITLE cell would judge it.
//
// THROUGH sessionTitle, not the raw map, so this predicate and the cell can never disagree about
// what "unnamed" means: the cell sanitises (sessionTitle does), and a predicate reading
// m.sessionsData[id].Title directly would be asserting about a different string than the one on
// screen. sanitizeLabel replaces rather than strips, so it cannot change emptiness today — the
// point is that this does not depend on that remaining true.
//
// WHITESPACE COUNTS AS UNNAMED, which the raw comparison got wrong. A title of " " is non-empty
// to Go and blank in the column, so it satisfied the old check and suppressed the harvest for a
// row displaying nothing. The harvester normalises its own output and tests each tier's CLIPPED
// value, so this is defence at the consumer rather than a live upstream bug — but this file
// renders whatever is in that map, including what an older harvester or a hand-edited file left.
func (m *model) sessionHasTitle(id string) bool {
	return strings.TrimSpace(m.sessionTitle(id)) != ""
}

// harvestNamedSomething reports whether a finished harvest named a session this model could not
// name before.
//
// NOT len(meta) > 0. An incremental harvest returns the whole merged map — every session it has
// ever seen, not just what this pass parsed — so a non-empty result says nothing about progress
// and would keep the backoff permanently reset. The question is whether any entry names a session
// that was unnamed here, which is also what the operator would call progress.
func (m *model) harvestNamedSomething(meta map[string]SessionMetadata) bool {
	for id, md := range meta {
		if strings.TrimSpace(sanitizeLabel(md.Title)) == "" {
			continue
		}
		if !m.sessionHasTitle(id) {
			return true
		}
	}
	return false
}
