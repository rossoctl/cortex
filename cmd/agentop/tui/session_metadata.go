package tui

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/observe/claude"
	"github.com/rossoctl/cortex/core/session"
)

// SessionMetadata is what a coding agent knows about one of its own sessions that Cortex
// does not. Owned by core/observe/claude, which is where its documentation lives.
//
// An ALIAS, not a distinct type. The shape is the on-disk contract between the harvester
// and this reader, and core cannot import tui, so the definition had to move there —
// but aliasing means map[string]SessionMetadata here and map[string]claude.SessionMetadata
// there are the SAME type, so every consumer in this package keeps its own spelling and
// assigns what the harvester returns with no conversion.
type SessionMetadata = claude.SessionMetadata

// SessionMetadataRel is the harvested metadata file, relative to the user's home.
// Re-exported from core/observe/claude so this package's readers and its tests keep one
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
// everything else is broken. Every failure yields an empty map: the pane still works, it
// just cannot name anything FROM HERE.
//
// AND THE COLUMN NO LONGER GOES BLANK WITH IT. This used to say a failure "renders as an
// empty TITLE column", which stopped being true when sessionTitleFor gained the fallback
// to the title /v1/sessions serves: a session the proxy has named still renders one
// through a total load failure. Degrading to none is therefore less visible than it was,
// which is the right direction and worth stating so the silence stays justified.
//
// Absent is not a failure at all. Nobody has this file until they run
// `agentop experimental read-claude-sessions`, so a first run must be silent rather than
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
	//
	// READ ONE PAST THE CAP, and bail on over-cap rather than decoding. Without the +1 the file
	// was read to exactly the cap and the truncated buffer handed to Unmarshal, and what that
	// produced depended on where the cut landed. Measured both ways:
	//
	//   - Cut mid-token, the common case: Unmarshal fails with "unexpected end of JSON input"
	//     and the result is this same empty map. No behaviour change from the fix.
	//   - Cut where the prefix is INDEPENDENTLY VALID JSON — trailing whitespace past the cap
	//     does it — the prefix decoded and this function returned a PARTIAL map as though it
	//     were the whole file, while claude.ReadMetadata refused the identical bytes. A
	//     16,777,238-byte file loaded 1 entry before the fix and 0 after.
	//
	// The second is what the +1 is for: not the volume of titles lost, but that a partial read
	// was indistinguishable from a complete one.
	//
	// Still returns the empty map, because this function has nowhere to report anything and
	// says so above — but claude.ReadMetadata applies the SAME cap and DOES distinguish it,
	// carrying ErrMetadataTooLarge, and `agentop observe`'s pre-flight calls that before the alt
	// screen goes up. So the operator gets the one line naming the remedy from there; what
	// this bail-out buys is that the two readers agree on which files are too large, instead
	// of one silently reading a prefix the other refuses whole.
	const maxMetadataBytes = 16 << 20
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return out
	}
	defer f.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes+1))
	if err != nil {
		return out
	}
	if len(b) > maxMetadataBytes {
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
// handle an error usefully: a home directory agentop cannot resolve costs a label here,
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
	// THROUGH titleIsBlank, like every other consumer of "is this named". A raw != "" accepted
	// a whitespace-only title and rendered "    (id)" — a header padded by a title that shows
	// nothing, which is worse than the bare id it would otherwise print.
	//
	// sessionTitleFor, not sessionTitle, so a header names a session on whichever source can — the
	// same precedence the TITLE column applies. A row an operator selected BY its served title must
	// not lose it on Enter; that inconsistency is exactly what this helper's doc above rules out.
	// blankSanitized, not titleIsBlank: sessionTitleFor returns a sanitised string on every path, so
	// re-sanitising it here would allocate a second copy of it to reach the same answer.
	if title := m.sessionTitleFor(id, m.servedTitle(id)); !blankSanitized(title) {
		return title + " (" + id + ")"
	}
	return id
}

// servedTitle returns the title /v1/sessions published for this session, or "" if it listed none.
//
// A LINEAR WALK, deliberately, as several others in this package already are: m.sessions is one
// pod's live sessions — a handful in practice — and the three headers this feeds each render ONE
// selected session per frame. An id-keyed map would be a second structure to keep in step with the
// slice, and the slice is rebuilt wholesale on every poll, so the sync is the cost, not the lookup.
//
// "" for a session the server does not list is the honest answer and the one sessionTitleFor wants:
// it means nothing served a title, which is indistinguishable here from serving an empty one.
func (m *model) servedTitle(id string) string {
	for _, s := range m.sessions {
		if s.ID == id {
			return s.Title
		}
	}
	return ""
}

// HarvestFunc reads an agent's transcripts and returns what it learned, keyed by session id.
//
// A function on RunOptions rather than a direct call into core/observe/claude, so this
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

// awaitsHarvest reports whether s is a row the title harvest could still be waiting on: one the
// proxy holds in memory. A row only the session archive holds is history — its title came from
// the metadata cache while it was live, or there is none to find — and with thirty days of such
// rows listed, counting them would hold the gate open on rows nothing can name. A session that
// resumes is resident again, and counts again.
func awaitsHarvest(s session.SessionSummary) bool { return s.Resident == nil || *s.Resident }

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
// Only sessions the metadata does NOT name are considered, plus named sessions whose transcript
// moved since the harvest that named them (see transcriptMoved). A fully-harvested list whose
// transcripts are unchanged triggers nothing at all.
//
// "FULLY HARVESTED" IS NOT "EVERY ROW NAMED ON SCREEN", and this comment used to conflate the two.
// sessionHasTitle asks about the HARVEST only — sessionTitleFor's served-title fallback is
// deliberately invisible to it (see sessionTitle's doc for why) — so a row showing a proxy-derived
// title still counts as untitled here and keeps triggering settled harvests. For an agent with no
// Claude Code transcript tree on this disk those harvests can never succeed, so that is not a
// transient state on the way to quiet: it is the permanent one, and the allocation noted below is
// paid for as long as the pane is open. Bounded by untitledBackoffCap (~3m between attempts) rather
// than by ever being satisfied. That periodic scan is the accepted price of not letting a served
// title stop the search for the richer harvested one; it is not a leak, but it is not free either.
//
// THE QUIET TEST SPANS TWO CLOCKS and tolerates them disagreeing in the safe direction; the
// reasoning is at the comparison itself.
//
// IT IS NOT FREE, THOUGH, and an earlier version of this comment claimed "one map lookup per row
// per tick", which undersells it: sessionHasTitle goes through sessionTitle, which calls
// sanitizeLabel, which builds a new string. So the steady state allocates once per resident row
// per 2s tick and always walks the whole list — the all-titled case is the one that cannot exit
// early, because the loop is looking for a row that is not there.
//
// Left as a linear walk deliberately. The rows that cost anything here are one pod's live
// sessions, a handful in practice against the ~180 in the metadata file: with a session archive
// the list also holds every session the archive keeps — ~2,000 over thirty days — but
// awaitsHarvest skips those archive-only rows before the allocating sessionHasTitle call, so each
// of them costs a pointer check. And the alternative — a cached "any untitled" flag — is a second
// piece of state to invalidate on every sessionsLoadedMsg and every harvest merge, which is how
// the events map grew the bugs its own comments now document. The honest figure is in the
// comment; the optimisation waits for a profile that asks for it.
func (m *model) untitledSettled(now time.Time) bool {
	for _, s := range m.sessions {
		if !awaitsHarvest(s) || (m.sessionHasTitle(s.ID) && !m.transcriptMoved(s.ID)) {
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
		// (core/session/store.go, sess.UpdatedAt = now) or carried on a streamed event, and
		// the two are reached through a kubectl port-forward with nothing keeping them in step.
		// Kubernetes does not synchronise node clocks, and a laptop that slept is the common
		// way this gets large.
		//
		// A FUTURE UpdatedAt IS A BROKEN CLOCK, NOT A SETTLED SESSION. Pod ahead of client gives
		// a negative delta, which can never reach untitledSettleDelay, so the harvested title for
		// that row never arrives — no error, no log, and the skew has to exceed only 5s to do it.
		// The row is left on whatever the proxy served, or blank if it served nothing; either way
		// it is stuck there. Treating it as settled instead is the safe direction: the
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

// transcriptMoved reports whether the transcript a harvested title came from changed after the
// harvest that read it. A NAMED row needs this to be read again: the title it has can be the cwd
// fallback or an early prompt — a session started by /clear and left idle is named by its cwd —
// and the transcript can since have grown a better one. Without it, a named row kept its first
// title for as long as agentop ran.
//
// One Stat per named live row per tick, on the rows awaitsHarvest admits. A harvest that only
// refreshes a named row names nothing NEW, so it counts as a miss and widens the backoff: an active
// session is re-read at most once per untitledBackoff, not once per pause.
func (m *model) transcriptMoved(id string) bool {
	meta := m.sessionsData[id]
	if meta.LogFile == "" {
		return false
	}
	st, err := os.Stat(meta.LogFile)
	return err == nil && st.ModTime().After(meta.LogModTime)
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
// COSTS ONE MAP LOOKUP PER UNNAMED ROW PER TICK, and only while the sessions pane is open. A
// harvest-named row exits on sessionHasTitle without touching the set at all, and the loop is over
// one pod's live sessions. Note that "unnamed" here means UNHARVESTED, not blank on screen: a row
// wearing a served title from sessionTitleFor reaches the lookup, and on an agent whose transcripts
// this machine does not have it reaches it on every tick indefinitely — see untitledSettled,
// where the same asymmetry is spelled out. It is the same walk untitledSettled does and the
// same walk the scoring does; see countUntitled, which the scoring shares with this.
//
// DOES NOT MUTATE THE SET. The gate asks a question; the harvest's scoring is what records the
// answer. Updating membership here would consume the freshness before the harvest it authorised
// could be judged, so an arrival would forgive the backoff and then, if the harvest named
// nothing, be counted as a miss for a row that had never been tried — which is exactly the
// ordering the scoring's `fresh` arm exists to avoid.
func (m *model) untitledFresh() bool {
	for _, sess := range m.sessions {
		if !awaitsHarvest(sess) || m.sessionHasTitle(sess.ID) {
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
		if !awaitsHarvest(sess) || m.sessionHasTitle(sess.ID) {
			continue
		}
		counted[sess.ID] = true
		if !m.untitledCounted[sess.ID] {
			fresh = true
		}
	}
	return counted, fresh
}

// sessionHasTitle reports whether the HARVEST has named this session.
//
// NOT "does the row render a title" — it deliberately says less than that. A row the harvest has
// not named can still display the title the proxy served (see sessionTitleFor), and this predicate
// answers false for it on purpose, so the harvest keeps looking for the title it would prefer.
// Every backoff predicate in this file is built on that distinction; do not widen this to mean
// "something is on screen".
//
// THROUGH sessionTitle, not the raw map, so this asks about the same sanitised string the harvest
// path renders: a predicate reading m.sessionsData[id].Title directly would judge a different
// string. sanitizeLabel replaces rather than strips, so it cannot change emptiness today — the
// point is that this does not depend on that remaining true.
//
// WHITESPACE COUNTS AS UNNAMED, which the raw comparison got wrong. A title of " " is non-empty
// to Go and blank in the column, so it satisfied the old check and suppressed the harvest for a
// row displaying nothing. The harvester normalises its own output and tests each tier's CLIPPED
// value, so this is defence at the consumer rather than a live upstream bug — but this file
// renders whatever is in that map, including what an older harvester or a hand-edited file left.
func (m *model) sessionHasTitle(id string) bool {
	// blankSanitized: sessionTitle sanitises, so titleIsBlank would do it again on a string already
	// through it — per row per tick on the render path.
	return !blankSanitized(m.sessionTitle(id))
}

// titleIsBlank reports whether a title string would render as an empty TITLE cell.
//
// THE ONE DEFINITION OF "UNNAMED", extracted because its callers ask that question about strings
// reached different ways — what the model already holds, what a harvest just returned and is not in
// the model yet, what the proxy served — and an inline copy in any of them is the drift
// sessionHasTitle's comment exists to prevent. Deliberately not enumerated here: the list went
// stale the first time a caller was added, and the callers are one grep away.
//
// THE CELL REACHES THIS NOW, through sessionTitleFor, which is where the fallback decides whether
// the harvested title is worth keeping. So a harvested " " no longer survives to the screen: it
// answers blank here and the cell shows the served title instead, or "" when there is none. What
// remains NOT a blankness test is sessionTitleCell's own `title == ""` fast path, which only skips
// truncating an empty string and is reached after this predicate has already had its say.
//
// SANITISES BEFORE TRIMMING, in that order, because that is the order the display applies them: it
// renders sessionTitleFor, which is sanitizeLabel'd on both paths, and nothing trims afterwards.
// sanitizeLabel REPLACES control and BIDI runes with U+FFFD rather than stripping them, so a "\t" or
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
//
// IDEMPOTENT IS NOT FREE, THOUGH, which is why blankSanitized exists beside this. sanitizeLabel
// builds a new string with b.Grow(len(s)) on the UNTRUNCATED input, so a caller already holding one
// and calling this anyway allocates a second full copy to reach the same answer. On the render path
// that is per row per rebuild, at whatever length the producer sent. Callers holding a sanitised
// string should say so rather than pay for the round trip.
func titleIsBlank(title string) bool {
	return blankSanitized(sanitizeLabel(title))
}

// blankSanitized is titleIsBlank for a string that sanitizeLabel has ALREADY been applied to.
//
// The trim half of the predicate, split out so the sanitise half is not paid twice. Every caller of
// titleIsBlank that passes the output of sessionTitle or sessionTitleFor is in that position, and
// so is sessionTitleFor itself, once it holds the sanitised served string it is about to return.
//
// WHY THE SPLIT IS SAFE HERE AND NOT A GENERAL LICENCE: the two halves are not interchangeable.
// titleIsBlank's doc above spends a paragraph on why the ORDER matters — sanitise before trim, so
// a "\t" answers not-blank because the cell paints a glyph for it. This helper is the second half
// only, so handing it a RAW title reintroduces the exact disagreement-with-the-screen that the
// ordering prevents: a raw "\t" would trim to "" and read blank, and the row would re-harvest
// forever while displaying a glyph. Call it only where the sanitising demonstrably already ran.
func blankSanitized(sanitized string) bool {
	return strings.TrimSpace(sanitized) == ""
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
		if !awaitsHarvest(sess) || m.sessionHasTitle(sess.ID) {
			continue
		}
		if !titleIsBlank(meta[sess.ID].Title) {
			return true
		}
	}
	return false
}
