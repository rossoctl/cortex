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
	// THROUGH titleIsBlank, like the other two consumers of "is this named". A raw != "" accepted
	// a whitespace-only title and rendered "    (id)" — a header padded by a title that shows
	// nothing, which is worse than the bare id it would otherwise print.
	if title := m.sessionTitle(id); !titleIsBlank(title) {
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
// THE SESSIONS LIST IS A PICKER TOO, which is what this exists for. The namespaces/pods
// re-harvest was written on the reasoning that "once a session view is up the titles on
// screen are already loaded" — true of a session's own events pane, and false of the list
// you choose a session FROM, which gains a row whenever a new session appears and cannot
// name it without re-reading the transcripts.
//
// KEYED OFF TRAFFIC, not off a wall clock. A session with events has a transcript being
// appended to, so a harvest triggered by its own updates arrives seconds after the title
// becomes readable instead of minutes later. The settle delay is what makes
// this cheap: without it every event on a still-unnamed session would trigger a scan, and
// with it a busy session is harvested once, after it pauses.
//
// Only sessions the metadata does NOT name are considered, so the steady state — every row titled
// — triggers nothing at all.
//
// THE QUIET TEST SPANS TWO CLOCKS and tolerates them disagreeing in the safe direction; the
// reasoning is at the comparison itself.
//
// IT IS NOT FREE, THOUGH, and an earlier version of this comment claimed "one map lookup per row
// per tick", which undersells it: sessionHasTitle goes through sessionTitle, which calls
// sanitizeLabel, which builds a new string. So the steady state allocates once per row per 2s
// tick and always walks the whole list — the all-titled case is the one that cannot exit early,
// because the loop is looking for a row that is not there.
//
// Left as a linear walk deliberately. The rows here are one pod's live sessions, a handful in
// practice against the ~180 in the metadata file, and the alternative — a cached "any untitled"
// flag — is a second piece of state to invalidate on every sessionsLoadedMsg and every harvest
// merge, which is how the events map grew the bugs its own comments now document. The honest
// figure is in the comment; the optimisation waits for a profile that asks for it.
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
		// TWO CLOCKS, NOT ONE, and this subtraction is the only place in the pane where that
		// costs anything. now is the laptop's; UpdatedAt was stamped inside the pod
		// (authlib/session/store.go, sess.UpdatedAt = now) or carried on a streamed event, and
		// the two are reached through a kubectl port-forward with nothing keeping them in step.
		// Kubernetes does not synchronise node clocks, and a laptop that slept is the common
		// way this gets large.
		//
		// A FUTURE UpdatedAt IS A BROKEN CLOCK, NOT A SETTLED SESSION. Pod ahead of client gives
		// a negative delta, which can never reach untitledSettleDelay, so that row's title never
		// arrives — no error, no log, just a permanently blank TITLE cell, and the skew has to
		// exceed only 5s to do it. Treating it as settled instead is the safe direction: the
		// cost of harvesting early is one wasted tree walk that the backoff then widens, against
		// a title that otherwise never comes at all.
		//
		// Clamped rather than corrected, because there is nothing to correct against. Both
		// timestamps on SessionSummary are server-stamped, so the response carries no
		// client-anchored instant to measure the offset from, and inventing one (first-seen-at,
		// per row) would be a second clock model for a pane whose AGE column already tolerates
		// the same skew — relTime renders a negative delta as "just now" and moves on. The
		// asymmetry is the point: a wrong AGE is visibly wrong for one tick, while a wrong
		// settle answer is invisible and permanent.
		if quiet := now.Sub(s.UpdatedAt); quiet < 0 || quiet >= untitledSettleDelay {
			return true
		}
	}
	return false
}

// untitledFresh reports whether some unnamed row on screen was NOT counted against the backoff
// yet — a session that appeared since the last harvest was scored.
//
// ASKED BY THE GATE, WHICH IS THE POINT. untitledMisses is reset for the same reason in the
// harvestedMsg handler, but that reset lands one harvest too late to help the row that caused it:
// the handler runs when a harvest FINISHES, and the gate decides whether one STARTS. So a row
// arriving while an unnameable row held the counter at untitledBackoffCap waited out a 3m penalty
// it had no part in earning, and the reset only took effect afterwards — for the next new row.
// Reading the set here means the arrival is priced on the tick it arrives.
//
// COSTS ONE MAP LOOKUP PER UNNAMED ROW PER TICK, and only while the sessions pane is open. The
// steady state — every row titled — exits on sessionHasTitle without touching the set at all,
// and the loop is over one pod's live sessions. It is the same walk untitledSettled does and the
// same walk the scoring does; see countUntitled, which the scoring shares with this.
//
// DOES NOT MUTATE THE SET. The gate asks a question; the harvest's scoring is what records the
// answer. Updating membership here would consume the freshness before the harvest it authorised
// could be judged, so an arrival would forgive the backoff and then, if the harvest named
// nothing, be counted as a miss for a row that had never been tried — which is exactly the
// ordering the scoring's `fresh` arm exists to avoid.
func (m *model) untitledFresh() bool {
	for _, sess := range m.sessions {
		if m.sessionHasTitle(sess.ID) {
			continue
		}
		if !m.untitledCounted[sess.ID] {
			return true
		}
	}
	return false
}

// countUntitled returns the set of on-screen rows that have no title, and whether any of them is
// one untitledCounted has not seen.
//
// ONE WALK SHARED BY THE GATE'S QUESTION AND THE SCORING'S BOOKKEEPING, because they must agree
// about what "unnamed and not yet counted" means. untitledFresh answers the gate from the same
// predicate this builds the set from; a second inline copy of the loop is how the two would drift
// into disagreeing, which would show up as either a forgiven backoff that never gets recorded or a
// recorded row that never got forgiven.
func (m *model) countUntitled() (counted map[string]bool, fresh bool) {
	counted = make(map[string]bool, len(m.sessions))
	for _, sess := range m.sessions {
		if m.sessionHasTitle(sess.ID) {
			continue
		}
		counted[sess.ID] = true
		if !m.untitledCounted[sess.ID] {
			fresh = true
		}
	}
	return counted, fresh
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
	return !titleIsBlank(m.sessionTitle(id))
}

// titleIsBlank reports whether a title string would render as an empty TITLE cell.
//
// THE ONE DEFINITION OF "UNNAMED" AMONG THE PREDICATES, extracted because three callers ask that
// question about different strings — sessionHasTitle about what the model already holds,
// sessionLabel about the same for a header, and harvestNamedSomething about what a harvest just
// returned, which is not in the model yet and so cannot be reached through sessionTitle. An
// inline copy in any of them is the drift sessionHasTitle's comment exists to prevent.
//
// THE CELL DOES NOT CALL THIS, and the claim that it does was overstated. sessionTitleCell tests
// a raw title == "" as a fast path to skip truncating an empty string; it does not judge
// blankness, and a " " title falls through it and is returned as " ". So the two AGREE in
// behaviour on every input — verified across "", " ", "   ", "\t" and ordinary prose — but by
// construction rather than by sharing this function. If that fast path ever becomes a real
// blankness test, it should route through here.
//
// SANITISES BEFORE TRIMMING, in that order, because that is the order the cell applies them: it
// renders sessionTitle, which is sanitizeLabel'd, and nothing trims afterwards. sanitizeLabel
// REPLACES control and BIDI runes with U+FFFD rather than stripping them, so a title of "\t" or
// "\n" is NOT blank here — the cell shows "�", a visible glyph, and a predicate calling that row
// unnamed would re-harvest forever for a row that is already displaying something.
//
// Reversing the two — sanitizeLabel(TrimSpace(title)) — is the tempting reading, since it makes
// "\t" answer "blank" the way a human skimming the source expects. It is wrong for this
// predicate: TrimSpace would strip the tab before sanitizeLabel could turn it into the glyph the
// cell actually paints, so the predicate would disagree with the screen. That disagreement is the
// one thing this helper exists to prevent. (Only tab/newline-class runes differ between the two
// orders; NUL and the BIDI controls are not whitespace, so TrimSpace never reaches them.)
//
// Sanitising a string sessionTitle already sanitised is a no-op, not a second pass with different
// meaning: sanitizeLabel is idempotent — U+FFFD matches none of its cases and falls through — so
// the caller does not have to know which of the two paths got there first.
func titleIsBlank(title string) bool {
	return strings.TrimSpace(sanitizeLabel(title)) == ""
}

// harvestNamedSomething reports whether a finished harvest named a session that is ON SCREEN and
// was unnamed.
//
// ITERATES m.sessions, NOT THE RESULT MAP, and that distinction is the whole function. An
// incremental harvest returns the whole merged map — every session it has ever seen, roughly 180
// entries on a developer laptop against the handful a pod is currently serving. Walking the result
// and asking "is this id unnamed here?" therefore answers yes on the first historical session the
// model has no metadata for, on every single call, which pins the backoff at zero and defeats it
// just as surely as len(meta) > 0 would. An earlier version did exactly that.
//
// So the question is asked from the screen inward: for each row the viewer is showing and cannot
// name, does this result name it? That is also what an operator would call progress — a title
// arriving for a session nobody is looking at is not why the backoff exists.
func (m *model) harvestNamedSomething(meta map[string]SessionMetadata) bool {
	for _, sess := range m.sessions {
		if m.sessionHasTitle(sess.ID) {
			continue
		}
		if !titleIsBlank(meta[sess.ID].Title) {
			return true
		}
	}
	return false
}
