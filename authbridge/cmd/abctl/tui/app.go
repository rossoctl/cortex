package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/cluster"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
)

// Pane identifiers.
type paneID int

const (
	paneNamespaces paneID = iota
	panePods
	paneSessions
	paneEvents
	paneDetail
	panePipeline
	panePluginDetail
	paneCatalog
	paneUsage
)

// lastPaneID is the highest valid paneID. Kept adjacent to the iota block so
// adding a pane means updating one line here, and TestPaneKeysCoverAllPanes then
// fails until that pane is documented in paneKeys — which is how paneUsage
// shipped reachable by `u` but named in no footer and no help overlay.
const lastPaneID = paneUsage

// paneNone is the explicit "no previous pane recorded" sentinel for
// model.previousPane. Using paneNamespaces (the zero value) as a
// sentinel would conflict with a future feature that wanted to open
// the catalog from the picker. -1 is unambiguous.
const paneNone paneID = -1

// Connection state for the SSE stream.
type connPhase int

const (
	connConnecting connPhase = iota
	connOpen
	connReconnecting
	connFailed
)

type connStateInfo struct {
	phase     connPhase
	attempt   int
	nextRetry time.Time
	err       error
}

// flashDuration is how long a one-shot status message (e.g. yank
// confirmation) stays in the footer.
const flashDuration = 3 * time.Second

// defaultLocalEndpoint is where `[l]` connects when nothing better is known:
// the session API's in-cluster default port, reached through an existing
// `kubectl port-forward`. Useful when abctl runs inside the mesh, or when the
// cluster's pod list isn't visible to their kubeconfig but a tunnel is.
//
// A local install listens somewhere else entirely (47601 by default), so
// RunOptions.LocalEndpoint overrides this with the address read from the
// machine's own Cortex config. Hardcoding 9094 sent `[l]` to the wrong port on
// every laptop.
const defaultLocalEndpoint = "http://localhost:9094"

// localEndpointOr returns the resolved local endpoint, falling back to the
// in-cluster default. One accessor so `[l]`, the footer and the help overlay
// cannot disagree about where the key goes.
func (m *model) localEndpointOr() string {
	if m.localEndpoint != "" {
		return m.localEndpoint
	}
	return defaultLocalEndpoint
}

// pipelineStore resolves where `e` should read and write the pipeline, paired
// with the base URL whose /reload/status confirms the edit landed. Returns a
// nil store when neither target is available, which is the only case `e`
// refuses.
//
// The two are returned together because they must describe the SAME proxy:
// writing a local file and then polling a pod's reload status — or the
// reverse — would report success for an edit that never took effect.
//
// Local wins when the endpoint on screen IS the local Cortex. Deliberately a
// question about the connection rather than about how abctl was started, so
// `--endpoint http://127.0.0.1:47601` aimed at your own proxy edits it just
// like a bare `abctl` does, and `[l]` gains the same ability mid-session.
// Compared against localEndpoint rather than localEndpointOr(): the fallback
// is the in-cluster 9094, and matching that would claim a hand-run
// port-forward to a POD is this machine's config file.
func (m *model) pipelineStore() (edit.Store, string) {
	if m.client != nil && m.localEndpoint != "" && sameEndpoint(m.client.Endpoint(), m.localEndpoint) &&
		m.localConfigPath != "" && m.localStatsURL != "" {
		return edit.FileStore{Path: m.localConfigPath}, m.localStatsURL
	}
	if m.editRunner != nil && m.selectedNamespace != "" && m.selectedPod != "" && m.statusURL != "" {
		return edit.ConfigMapStore{
			Run:       m.editRunner,
			Namespace: m.selectedNamespace,
			Pod:       m.selectedPod,
		}, m.statusURL
	}
	return nil, ""
}

// sameEndpoint reports whether two session-API URLs name the same server.
//
// Not a string compare. localEndpoint is built by dialURL, which emits
// "http://127.0.0.1:47601" for the built-in config, while `--endpoint
// http://localhost:47601` is the same proxy spelled the way a human types it.
// An exact compare rejected that and flashed "…or point abctl at a Cortex
// running on this machine" at an operator who had just done exactly that —
// the same misleading refusal this whole change exists to remove.
//
// Loopback names are folded together; every other host must match outright, so
// a config bound to a specific LAN address is not confused with anything else.
// Ports are compared as written: two proxies on one host differ only by port.
func sameEndpoint(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	if ua.Scheme != ub.Scheme || ua.Port() != ub.Port() {
		return false
	}
	ha, hb := ua.Hostname(), ub.Hostname()
	if isLoopbackHost(ha) && isLoopbackHost(hb) {
		return true
	}
	return ha == hb
}

// isLoopbackHost covers the spellings of "this machine" that appear in a
// config, on a command line, and in Go's own URL output. net.IP.IsLoopback
// handles 127.0.0.0/8 and ::1; "localhost" is not an IP, so it is named.
func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// localProbeTimeout bounds the pre-connect reachability check for `[l]`.
// Without it, a dead localEndpoint would leave the operator in an empty
// session view wondering why nothing streams; with it they get a footer
// error and stay in the picker.
const localProbeTimeout = 2 * time.Second

// refreshInterval is how often abctl re-fetches /v1/sessions from the
// server to reconcile its local list. Cheap, and the only mechanism by
// which rekeys (default → contextId) propagate to the client UI — the
// server itself doesn't emit a rekey signal on the stream, so short-lived
// stub sessions would otherwise linger in the TUI.
const refreshInterval = 2 * time.Second

// The NAMESPACES and PODS panes harvest ONCE PER VISIT, on arrival, with no interval — so there
// is no constant to declare here. The visit is the trigger; pickerHarvested and pickerShowing
// carry the state, and Init spends the first visit's budget.
//
// The scan is OPPORTUNISTIC, not something the pane needs: the picker is rarely visited, and the
// reason to walk a transcript tree from it is that the machine is idle waiting for someone to
// choose a pod. Idle-time work belongs at the moment you arrive, so a clock set by some earlier
// harvest is the wrong pacing for it — a 3-minute interval used to sit here, and once the
// per-visit cap existed it could only suppress the one scan each visit was allowed.
//
// The sessions LIST harvests on separate terms — see untitledSettleDelay and untitledBackoff —
// because it can tell an unnamed row from a named one. These panes hold no session rows, which
// is why one scan per visit is the most they can sensibly ask for.

// untitledSettleDelay is how long a session must be quiet before an unnamed row triggers a
// re-harvest, and it is the whole reason this poll is affordable.
//
// New traffic means the transcript is being APPENDED TO, so harvesting the instant an event
// lands would read a file the agent is still writing — and the title tiers read the LAST
// prompt, which is exactly the part still arriving. Waiting for a pause means the read sees a
// complete turn.
//
// Five seconds because it only has to outlast the gap between events within one turn, not the
// turn itself: the harvest is incremental and re-reads only transcripts whose mtime moved, so
// being early costs a re-scan of one file rather than of the tree. Longer would make a brand
// new session sit nameless for no benefit; shorter would harvest mid-write repeatedly.
const untitledSettleDelay = 5 * time.Second

// untitledBackoffCap bounds the exponential backoff on fruitless sessions-pane harvests.
//
// Three minutes because that is what the picker panes used to spend on this tree
// unconditionally, back when a fixed interval paced them — a cadence nobody objected to for
// walking a transcript tree. A session that can never be named now costs that at worst, rather
// than a tree walk every untitledSettleDelay forever.
const untitledBackoffCap = 3 * time.Minute

// untitledBackoff is how long to wait before the next sessions-pane harvest, given how many
// consecutive harvests have named nothing.
//
// Doubling from untitledSettleDelay: 5s, 10s, 20s … capped at untitledBackoffCap. The first
// miss still retries quickly, because the common reason a just-appeared session is unnamed is
// that its transcript was a moment behind — and that case resolves on the next attempt.
//
// SHIFTS RATHER THAN MULTIPLIES, and the shift is bounded before it is applied: misses is
// unbounded (a viewer left open overnight), and 5s << 62 overflows int64 into a negative
// duration, which would make the gate fire on EVERY tick — the exact failure the backoff
// exists to prevent, arrived at by way of the fix for it.
func untitledBackoff(misses int) time.Duration {
	if misses <= 0 {
		return untitledSettleDelay
	}
	// 2^24 * 5s is already far past the cap, so anything beyond that is the cap by definition
	// and never needs to be computed.
	if misses > 24 {
		return untitledBackoffCap
	}
	d := untitledSettleDelay << uint(misses)
	if d > untitledBackoffCap {
		return untitledBackoffCap
	}
	return d
}

// Tea messages.
type tickMsg time.Time
type refreshTickMsg time.Time
type sessionsLoadedMsg []session.SessionSummary
type pipelineLoadedMsg *apiclient.PipelineView
type snapshotLoadedMsg struct {
	// olderNotFetched is how many events precede the ones in this response, from the
	// server's own count of the session. Non-zero means the timeline starts where the
	// window starts, not where the session does — a distinction the operator cannot
	// otherwise make, and the one that made a 5000-event session look like a 3-row one.
	olderNotFetched int
	id              string
	events          []pipeline.SessionEvent
	// serverOldest is the Seq of the oldest event the server still holds, so paging can
	// tell "there is more behind me" from "this is the beginning of the session".
	serverOldest uint64
	// projected is the server's echo: it applied view=summary, so these events carry
	// no message bodies and the detail pane has to fetch the row it opens. False
	// means a proxy that predates the projection returned full events — in which
	// case the detail pane already has everything and must not fetch.
	projected bool
}

// olderPageLoadedMsg carries a page from BEFORE the events already held — the result of
// [o]. Separate from snapshotLoadedMsg because that one REPLACES the timeline and this one
// extends it backwards; conflating them would drop everything the operator had loaded.
//
// gen is the request generation, so a response from a superseded request can be recognised
// and dropped rather than stitched in as current.
type olderPageLoadedMsg struct {
	id           string
	gen          uint64
	events       []pipeline.SessionEvent
	serverOldest uint64
}

// olderPageFailedMsg reports a failed [o] fetch.
//
// Its own type rather than errMsg, whose handler decides severity by string-prefixing
// `where` and treats anything unrecognised as a terminal connection failure — so a single
// failed page claimed the event stream was dead when it was healthy, and left the paging
// state marked in-flight, refusing every retry.
type olderPageFailedMsg struct {
	id  string
	gen uint64
	err error
}
type streamMsg apiclient.StreamEvent
type streamClosedMsg struct{}
type errMsg struct {
	where string
	err   error
}

// agentsLoadedMsg carries the result of Lister.ListAgents from the picker
// loader Cmd.
type agentsLoadedMsg struct {
	namespaces []cluster.AgentNamespace
	err        error
}

// portForwardReadyMsg carries the result of PortForwarder.Start. On success,
// pf and endpoint are set; on failure, err is set.
type portForwardReadyMsg struct {
	pf       cluster.PortForward
	endpoint string
	err      error
}

// editorExitedMsg is sent by openEditorCmd when the user's $EDITOR
// process exits. err is nil on a clean (0) exit. Carries gen so a
// stale editor result from Edit 1 can't overwrite Edit 2's tempfile
// processing.
type editorExitedMsg struct {
	gen int
	err error
}

// genFetchedMsg / genAppliedMsg / genPolledMsg / genRolledBackMsg wrap
// the upstream edit-package messages with the editState.generation
// captured at Cmd-issue time. Handlers drop messages whose gen doesn't
// match m.editState.generation, which prevents Edit 1's late results
// from leaking onto Edit 2's overlay (and would otherwise be able to
// trigger an unintended rollback).
type genFetchedMsg struct {
	gen int
	edit.FetchedMsg
}
type genAppliedMsg struct {
	gen int
	edit.AppliedMsg
}
type genPolledMsg struct {
	gen int
	edit.PolledMsg
}
type genRolledBackMsg struct {
	gen int
	edit.RolledBackMsg
}

// withGen wraps a tea.Cmd to tag its result with the supplied gen.
// Inspects the upstream msg type and rewrites it into the matching
// generational wrapper. Unknown types pass through untouched.
func withGen(gen int, c tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		switch m := c().(type) {
		case edit.FetchedMsg:
			return genFetchedMsg{gen: gen, FetchedMsg: m}
		case edit.AppliedMsg:
			return genAppliedMsg{gen: gen, AppliedMsg: m}
		case edit.PolledMsg:
			return genPolledMsg{gen: gen, PolledMsg: m}
		case edit.RolledBackMsg:
			return genRolledBackMsg{gen: gen, RolledBackMsg: m}
		default:
			return m
		}
	}
}

// Model is the top-level Bubble Tea model.
type model struct {
	endpoint string
	client   *apiclient.Client
	// localEndpoint is what `[l]` connects to and what the footer and help
	// overlay name. Resolved from the local Cortex config when there is one.
	localEndpoint string

	ctx    context.Context
	cancel context.CancelFunc

	// parentCtx is the un-cancelled root ctx the picker was constructed
	// with. Used to derive a fresh m.ctx / m.cancel when the user backs
	// out of a session view to switch pods. Nil in bypass mode (no picker)
	// — Esc-from-Sessions is a no-op there.
	parentCtx context.Context

	// Data caches.
	sessions []session.SessionSummary
	// events was labelled a ring buffer and has never been one. Nothing trims an entry in
	// place; every write is one of six, and the CONTEXT(1M) gauge folds forward off this map,
	// so each one owes contextRun an action. The full inventory, because the gauge reads an
	// unchanged LENGTH as "nothing new to fold" and two of these change content without
	// changing length:
	//
	//	app.go   streamed append      grows           fold the delta (the design case)
	//	app.go   snapshot load        replaces        rebaseSessionContext — PROJECTED events
	//	paging   older-page merge     replaces        rebaseSessionContext — projected, and
	//	                                             the page cap can drop newer events too
	//	detail   replaceHeldEvent     same length!    rebaseSessionContext — swaps in the one
	//	                                             UNPROJECTED copy abctl ever gets
	//	app.go   backToPodsPane       whole map       m.contextRun = nil: a different pod is a
	//	                                             different workload behind the same id
	//	keys.go  picker release       deletes one     nothing: the figure outlives the events,
	//	                                             which were released to save memory
	//
	// A seventh path that trimmed the front of an entry would be invisible to the length check
	// if it also appended, so it would have to rebase.
	//
	// AND EVERY ROW ABOVE THAT REBASES ALSO OWES rebuildSessionsTable, which is a second
	// obligation and not a restatement of the first. Updating contextRun makes the figure right;
	// the sessions table holds the gauge as a BAKED string, so nothing on screen changes until
	// the rows are rebuilt. The streamed append had that call all along and the other three did
	// not, so a session abctl streamed showed a gauge while a session it merely opened showed a
	// dash — corrected in contextRun, and only visible when the next /v1/sessions poll happened
	// to repaint. See TestSessionsTable_ASnapshotRepaintsTheGaugeItFilled.
	events map[string][]pipeline.SessionEvent // sessionID → every event held for it
	// contextRun is the CONTEXT(1M) gauge's answer per session, folded forward as events
	// arrive rather than recomputed from the whole slice — see sessionContextFor. The row
	// loop asks for every session on every rebuild, and a rebuild happens on every streamed
	// event, so a full scan there is O(events) per session per event. It is a remembered
	// maximum rather than a cache of the slice: the events can stop carrying the evidence
	// (view=summary strips it) while the answer stays true.
	contextRun map[string]contextRun
	// sessionsData is what an agent knows about its own sessions that the proxy does
	// not — a title, mostly. Read once at startup from ~/.cortex/session-metadata.json,
	// which `abctl experimental read-claude-sessions` writes; empty when that has never
	// run, which renders as an empty TITLE column rather than as a failure.
	//
	// Keyed by the same session id the proxy buckets on, so a lookup is direct. Nil-safe
	// by construction: a read on a nil map yields the zero SessionMetadata, so an
	// unharvested session, an unknown id and an absent file all render the same.
	sessionsData map[string]SessionMetadata
	// harvest refreshes sessionsData in the background once the UI is up. Nil disables it.
	harvest HarvestFunc
	// lastHarvest is when the most recent harvest was STARTED. Read by ONE caller: the sessions
	// pane's backoff gate, which asks whether untitledBackoff has elapsed since then. The picker
	// no longer has a cadence to measure — it harvests on arrival — so nothing there reads this.
	//
	// STAMPED BY EVERY PATH, THOUGH, INCLUDING THE ONES THAT NEVER READ IT, and that is a real
	// coupling rather than a tidy one: a picker scan on arrival, and Init's startup scan, both
	// move this stamp, so the sessions pane's first settle-triggered harvest can be held off for
	// up to one untitledSettleDelay after the operator picks a pod. Bounded at 5s, and defensible
	// — the tree was just walked, so there is little to gain from walking it again — but it means
	// the two paths are not as independent as their separate triggers suggest. A dedicated
	// lastSessionsHarvest would decouple them; deliberately not done here.
	lastHarvest time.Time
	// harvesting guards against stacking: a harvest walks a transcript tree, and a second pass while
	// the first is in flight would duplicate the work and race its own write of the metadata file.
	harvesting bool
	// pickerHarvested says the namespaces/pods panes have already harvested during this visit.
	//
	// Cleared by the arrival edge in the refreshTickMsg branch rather than at each site that
	// assigns m.pane: there are two dozen of those across app.go and keys.go, and one added later
	// would silently inherit "already harvested" from a previous visit. pickerShowing is what
	// that edge compares against.
	pickerHarvested bool
	// pickerShowing is whether the previous refresh tick found the picker on screen, for
	// detecting ARRIVAL at it without every pane switch having to announce itself. Only the
	// picker harvest reads it.
	//
	// A BOOL, NOT THE PREVIOUS paneID, for two reasons that point the same way. The budget it
	// clears covers a visit, and a visit spans paneNamespaces and panePods both — tracking the
	// exact pane made namespaces→pods→namespaces three visits and re-walked the transcript tree
	// at each hop. And a paneID field cannot express "no pane yet" without the paneNone sentinel
	// that previousPane and pipelineReturnPane each need a comment to explain: paneNamespaces is
	// the zero value, so a zero-valued previous-pane field claims the picker model is already
	// where it starts and eats that model's first arrival. False is the honest zero here — no
	// tick has seen the picker yet — so both constructors are correct without seeding anything.
	pickerShowing bool
	// untitledMisses counts consecutive sessions-pane harvests that named nothing, and backs the
	// next one off exponentially — see untitledBackoff.
	//
	// A SESSION THAT CAN NEVER BE NAMED IS THE COMMON CASE HERE, not the exception: a session
	// with no transcript under the agent's config dir (a different agent, a pruned tree, a
	// CLAUDE_CONFIG_DIR that moved) stays unnamed no matter how often the tree is walked. Without
	// a backoff its row keeps the settle gate satisfied forever, so the pane re-walks the whole
	// transcript tree every untitledSettleDelay for a title that is never coming.
	//
	// Reset on any harvest that named something, so a tree that starts producing titles returns to
	// the fast cadence immediately rather than staying penalised for earlier silence.
	//
	// AND RESET WHEN AN UNNAMED SESSION ARRIVES THAT WAS NOT HERE BEFORE — see untitledCounted,
	// which is what makes that detectable. Without it this counter is model-wide while the thing
	// it is meant to describe is per-session: one row that can never be named drives it to the
	// untitledBackoffCap, and a genuinely new session appearing afterwards inherits that 3m wait
	// for its first title. That is #1109's symptom with a longer fuse, and it is the ordinary
	// case — an operator watching a pod gets a new session while an unnameable one is on screen.
	untitledMisses int
	// untitledCounted is the set of session ids that were unnamed at the last harvest scoring,
	// so the next one can tell a NEW unnamed row from the same unnameable row asked again.
	//
	// WHY A SET AND NOT A PER-SESSION BACKOFF. Keying untitledMisses by session id is the more
	// literal reading of "the counter is not per-session", and it is the bigger change: the gate
	// would have to pick a deadline across rows, which is a policy question this PR does not need
	// to answer — the harvest is one tree walk for ALL sessions, so per-session deadlines would
	// still share a single scan and the first row due would set the cadence for everyone. What
	// the defect actually costs is a stale penalty carried onto a fresh row, and an arrival reset
	// ends that without inventing a per-row schedule. So the backoff stays global and describes
	// "how fruitless has this LIST been lately", which is what a single tree walk can honour.
	//
	// HOLDS UNNAMED IDS ONLY, not every id seen. A session that already has a title is not what
	// the settle gate or the backoff is about, and admitting it here would mean a row LOSING its
	// title later (a hand-edited metadata file, a merge that blanks one) read as "not new" and
	// skipped the reset. Membership answers exactly one question — was this row already counted
	// against the backoff as unnameable — so only rows that were counted belong in it.
	//
	// REBUILT AT EACH SCORING RATHER THAN ADDED TO, so ids drop out when their session leaves the
	// list or gains a title. An append-only set would grow for the life of the process and, worse,
	// would remember a session that went away and came back as "already counted" — abctl's own
	// docs note a session id can be re-created after eviction, and a returning id is a new row to
	// an operator watching the pane.
	untitledCounted map[string]bool
	eventCt         uint64 // monotonic counter
	lastTick        time.Time
	lastCt          uint64
	rate            float64

	// Connection status.
	connState connStateInfo

	// UI state.
	pane paneID
	// usage is the Usage pane's view state (metric, window, scope, snapshot).
	usage usageState

	// spend backs the always-on spend strip. Separate from usage on purpose —
	// see spendState, which records why sharing one poll chain would blank the
	// strip every time the Usage pane's window changed.
	spend spendState

	// eventColumns is which events-table columns are shown. Keyed by a stable id
	// rather than an index, so a future column inserted in the middle does not
	// silently change what an existing selection means.
	//
	// Derived from Settings.Events at startup and written back to it when the
	// picker closes. Kept as a map on the model rather than read from Settings on
	// every cell, because rebuildEventsTable consults it per column per row — up to
	// 500 rows on every SSE event and every resize.
	eventColumns map[eventColumnID]bool
	// eventColsDropped is how many selected columns did not fit the terminal on the
	// last rebuild. Surfaced in the footer: with every column on the table needs
	// ~168 columns, and the excess was clipped with nothing saying so (#866).
	eventColsDropped int
	// colPicker is open while `c` owns the keyboard; colCursor is the highlighted
	// column within it.
	colPicker bool
	colCursor int
	// sortCol is the column the events table is ordered by, and sortDesc its
	// direction (#865). The empty id means CHRONOLOGICAL — arrival order — which is
	// both the default and what rebuildEventsTable uses internally regardless: the
	// request/response pairing walks the chronological slice, and only the finished
	// rows are reordered. See sortEventRows.
	//
	// Descending is what a freshly chosen column gets, because the question the
	// issue asks is "which events took longest / cost most" and that answer belongs
	// at the top.
	sortCol      eventColumnID
	sortDesc     bool
	selectedSess string
	// filter is the ACTIVE filter, which is not the same as the saved one:
	// backToPodsPane clears this on teardown so a filter cannot survive a pod
	// switch and read as data loss. Settings.Filter is the persisted value that
	// seeds it, and that clear deliberately does not write back.
	filter    string
	filtering bool
	// filterBeforeEdit is the committed filter as it stood when `/` was pressed, so
	// Esc can restore it. Esc is documented as cancelling and means cancel everywhere
	// else in abctl, but it used to clear the filter outright — and once the filter
	// began persisting, that turned a mis-keyed Esc into the permanent loss of a
	// committed filter. Clearing is still one action: empty the box and press Enter.
	filterBeforeEdit string
	paused           bool
	// hideInactive toggles whether passthrough / skip-only messages are
	// hidden from the events table. False (default) shows every message —
	// the operator asked to see all network traffic, processed or not.
	// Toggle with `s` to focus on plugin activity (deny/modify/allow/
	// observe). hiddenInactive is the count from the most recent
	// rebuildEventsTable, surfaced in the footer so a filtered timeline
	// doesn't read as data loss.
	hideInactive   bool
	hiddenInactive int

	// olderNotFetched is how many events a session holds that its snapshot did not
	// carry, reported by the server, keyed by session id.
	//
	// Keyed rather than a single number, because snapshots land asynchronously and for
	// whichever session was selected when the fetch started. A single field let a late
	// snapshot for an abandoned session describe the one on screen, and showed the
	// previous session's count in the window between selecting a session and its
	// snapshot arriving. Beside hiddenInactive in spirit — both answer "is this the
	// whole timeline?" — but that one is a filter the operator chose and this is a
	// bound they did not.
	olderNotFetched map[string]int

	// paging holds the backward-paging state of any session the operator has walked off
	// the tail of, keyed by session id. Absent — the normal case — means the timeline is
	// the live tail and streamed events append to it.
	paging map[string]*pagingState

	// pageGen numbers page requests monotonically across the whole model, so a response
	// from a superseded request is recognisable. Per-model rather than per-session state
	// because a session's state is torn down and rebuilt by [t] followed by [o], and a
	// per-state counter would reissue a generation an in-flight request already holds.
	pageGen uint64

	flash      string
	flashUntil time.Time
	// flashSticky keeps the current flash up until the next keypress instead of
	// expiring on flashUntil. Set only by setStickyFlash (yank), so every other
	// flash producer keeps its timed behaviour.
	flashSticky   bool
	width, height int
	// bodyHeight is the inner height available to panes (terminal height
	// minus title + footer). Cached by layout() so rebuildEventsTable can
	// size the events table after accounting for the IDENTITY banner.
	bodyHeight int

	// Panel components.
	sessionsTbl table.Model
	// sessionRowIDs is the FULL session id for each row of sessionsTbl, in the same order.
	//
	// It exists because a rendered cell is not a data channel, and this pane used one as if it
	// were: selectedSessionID read rows[cursor][0], so the moment the SESSION cell began
	// truncating a 36-character UUID to its column width, the truncated string with an ellipsis
	// became m.selectedSess. Measured consequences, all from one keypress on a normal session:
	// no snapshot was fetched (the id missed the live map and took the cached-only branch), the
	// events pane rendered empty (it missed m.events), the id sent to the server was the
	// truncated one, and — worst — the release loop's `cached != id` was true for every
	// full-id key, so opening a session DELETED THE EVENT CACHE FOR EVERY SESSION INCLUDING
	// ITSELF. That is exactly the unrecoverable loss #870 and the comment above that loop exist
	// to prevent.
	//
	// Kept in lockstep with the rows, built in the same loop, and the only writer is
	// rebuildSessionsTable. A parallel slice rather than a map because the lookup key is the
	// cursor's row INDEX, and rather than indexing m.sessions because the rows are filtered and
	// interleaved with cached-only entries, so position does not map back.
	sessionRowIDs []string
	eventsTbl     table.Model
	pipelineTbl   table.Model
	catalogTbl    table.Model
	detailVp      viewport.Model
	detailEvent   *pipeline.SessionEvent
	// detailRow is the full events-pane row (event + any folded CONNECT
	// tunnel) the detail view was opened on. Kept so layout() can re-render
	// the detail pane on resize without re-deriving the tunnel fold.
	detailRow    eventRow
	detailPlugin *apiclient.PipelinePlugin
	filterInput  textinput.Model

	// visibleRows holds the eventRow for each rendered row in eventsTbl —
	// one per network message. Populated by rebuildEventsTable so
	// selectedEvent / selectedEventRow can return the message the cursor is
	// on (and any folded tunnel) without re-walking the cache. Reset on
	// every rebuild.
	visibleRows []eventRow

	// selectedEventKey pins the events-pane cursor to an event across
	// rebuilds (see eventKey). Zero value = unpinned.
	selectedEventKey eventKey

	// pipeline is the fetched plugin composition. nil until the initial
	// GetPipeline response arrives; the pipeline pane shows "(loading…)"
	// until then.
	pipeline *apiclient.PipelineView

	// pipelineFetching is set while a /v1/pipeline request is outstanding, so
	// the 2s refresh tick cannot stack fetches against a slow endpoint.
	pipelineFetching bool

	// helpVisible toggles the [?] key-help overlay. Deliberately a flag
	// rather than a paneID: the overlay must be openable over ANY pane
	// (picker included) without disturbing m.pane / m.previousPane, which
	// the catalog's Esc-return already owns.
	helpVisible bool
	// helpVp scrolls the help overlay's body. Its own viewport rather than
	// a shared one because the overlay can open over the detail panes,
	// which would otherwise have their scroll position clobbered.
	helpVp viewport.Model

	// catalog is the registered-plugin catalog from /v1/plugins,
	// fetched lazily when the user first opens the catalog pane via
	// `P`. Cached for the session; `r` from the catalog pane refreshes.
	// nil before the first fetch; the catalog pane shows "(loading…)".
	catalog *apiclient.PluginCatalog
	// previousPane lets `Esc` from the catalog pane return to whichever
	// pane the user came from instead of always defaulting to one.
	previousPane paneID

	// pipelineReturnPane is where `Esc` from the pipeline pane goes back to.
	// A FIELD OF ITS OWN, not previousPane, for the reason usageState grew
	// returnPane: previousPane belongs to the catalog, and the catalog can be
	// opened from the pipeline — that write sets it to panePipeline and the
	// catalog's own esc then clears it, so esc from the pipeline would land on
	// the default instead of the pane the operator opened it from. Three
	// surfaces sharing one field cannot work; each keeps its own.
	pipelineReturnPane paneID

	// streamCh is the single SSE channel from the apiclient. Opened once
	// in Init; re-pumped on every streamMsg until it closes.
	streamCh <-chan apiclient.StreamEvent

	// Picker dependencies and state. nil + empty when --endpoint bypasses
	// the picker.
	lister        cluster.Lister
	portForwarder cluster.PortForwarder
	namespaces    []cluster.AgentNamespace
	namespacesTbl table.Model
	podsTbl       table.Model

	selectedNamespace string // set on Enter from Namespaces pane
	selectedPod       string // set on Enter from Pods pane

	pickerErr string // single-line picker error shown in footer

	// loading is true while a loadAgentsCmd is in flight. Prevents
	// concurrent `r` keypresses (or `r` during initial load) from
	// dispatching parallel ListAgents calls.
	loading bool

	// activePF is the live port-forward tunnel, if any. Closed on pod-switch
	// or quit.
	activePF cluster.PortForward

	// localDirect is true when the session view was entered via `[l]`
	// (direct connection to localEndpoint) rather than by picking a pod.
	// There is no pod to go back to, so Esc returns to the Namespaces
	// pane instead of Pods.
	localDirect bool

	// editState tracks an in-flight pipeline edit (the "e" flow).
	// editState.phase == editPhaseDone means no edit is active.
	editState editState

	// statusURL is the agent's :9093 stat-server URL via the picker's
	// port-forward; populated by portForwardReadyMsg. Used by edit.PollCmd
	// to watch /reload/status.
	statusURL string

	// editRunner is the kubectl Runner the edit flow uses for fetch/apply.
	// Set in newPickerModel to edit.DefaultRunner; tests inject a stub.
	editRunner edit.Runner

	// eventsBuiltFor is the session the events table was last built for. When it
	// differs from selectedSess the next rebuild is an OPENING, which is the only
	// moment the OpenAtOldest preference applies — every later rebuild (the
	// two-second poll, a filter, a column toggle) must preserve where the operator
	// is, not re-anchor them to an end.
	eventsBuiltFor string

	// fullFetched is the set of event Seqs, per session, whose bodies have been
	// fetched for the detail pane. Reset with m.events, so it cannot outlive the rows
	// it describes.
	//
	// Tracked rather than inferred from the event: see needsFullEvent for why a
	// presence check cannot answer this for response events.
	fullFetched map[string]map[uint64]bool

	// serverProjects records that this proxy honours view=summary, learned from its
	// echo on any snapshot. It decides whether the detail pane has to fetch the row
	// it opens: a proxy that predates the projection already sent whole events, and
	// asking it for one again would be a round trip for bytes abctl is holding.
	//
	// A server capability, so one flag rather than one per session.
	serverProjects bool

	// localConfigPath is the config file of the Cortex on this machine, and
	// localStatsURL is where that Cortex serves /reload/status. Both set from
	// RunOptions, and only when one answered — pipelineStore requires both, so
	// either being empty means `e` has no local target.
	//
	// Passed in rather than resolved here, like save: this package does no
	// home-directory or filesystem lookup, which is what keeps its tests off
	// $HOME.
	localConfigPath string
	localStatsURL   string

	// save persists Settings when a setting changes. A callback rather than a path
	// so this package needs no home-directory or filesystem logic, and so its tests
	// never touch $HOME. Nil disables saving, which is what every test wants.
	save func(UserSettings) error
}

// New returns a fresh model pointed at the given client. ctx governs both
// the HTTP calls and the SSE goroutine; cancelling it shuts everything down.
func New(ctx context.Context, c *apiclient.Client) tea.Model {
	ctx, cancel := context.WithCancel(ctx)

	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Prompt = "/ "
	// Seed the input, not just m.filter: the filter box renders only while filtering,
	// so a restored filter was applied invisibly — the list came back truncated with
	// nothing on screen saying why. Worse, `/` then one character replaced the saved
	// filter with that character, and `/` then Esc persisted an empty one, discarding
	// it for good.
	ti.SetValue(Settings.Filter)

	// Resolved once: sortSelection walks eventColumns to validate the persisted name,
	// and the two fields are two halves of one answer.
	sortCol, sortDesc := Settings.sortSelection()

	// The usage pane's view, restored once at startup rather than in openUsage:
	// re-entering the pane must not reset a choice made during the session.
	usageMetric, usageWindowIdx, usageGroup := Settings.usageSelection()

	// Read once here, not per refresh: the file changes only when someone runs the
	// harvester, and the sessions list refreshes every two seconds.
	sessionMeta := loadSessionMetadataForModel()

	return &model{
		endpoint:     c.Endpoint(),
		client:       c,
		ctx:          ctx,
		cancel:       cancel,
		events:       make(map[string][]pipeline.SessionEvent),
		pane:         paneSessions,
		eventColumns: Settings.columnSelection(),
		sortCol:      sortCol,
		sortDesc:     sortDesc,
		usage:        usageState{metric: usageMetric, windowIdx: usageWindowIdx, group: usageGroup},
		filter:       Settings.Filter,
		sessionsData: sessionMeta,
		sessionsTbl:  newSessionsTable(),
		eventsTbl:    newEventsTable(),
		pipelineTbl:  newPipelineTable(),
		catalogTbl:   newCatalogTable(),
		previousPane: paneNone,
		// paneNone, not the zero value: paneNamespaces is 0, and a return pane
		// of "namespaces" would send esc from the pipeline into the picker.
		pipelineReturnPane: paneNone,
		detailVp:           viewport.New(0, 0),
		filterInput:        ti,
		lastTick:           time.Now(),
		connState:          connStateInfo{phase: connConnecting},
	}
}

// initSessionView fires the session-view bootstrap: SSE pump, first
// fetch, ticks. Caller must have set m.client and m.ctx.
func (m *model) initSessionView() tea.Cmd {
	m.streamCh = m.client.Stream(m.ctx, "")
	return tea.Batch(
		m.loadSessionsCmd(),
		m.loadPipelineCmd(),
		streamPump(m.streamCh),
		tickCmd(),
		refreshTickCmd(),
		// The spend strip's chain starts HERE rather than in Init, so it also starts when
		// the user backs out to the pod picker and enters a DIFFERENT pod: Init runs once,
		// but m.client is replaced on every re-entry, and a chain armed against the old one
		// would report the previous pod's spend. startSpendPolling bumps the generation, so
		// re-entry replaces the chain rather than adding a second one.
		m.startSpendPolling(),
	)
}

// backToPodsPane returns the picker to the Pods pane, tearing down the
// current session view (SSE pump, ticks) and port-forward. The pod list
// is preserved so the user picks a different pod immediately. A fresh
// ctx / cancel is derived from m.parentCtx so the next session-view
// entry has a usable context.
//
// When the session was entered via `[l]` there is no pod to return to,
// so the destination is the Namespaces pane instead.
func (m *model) backToPodsPane() {
	// Cancel current ctx — stops the SSE goroutine and any in-flight
	// session/pipeline fetches.
	if m.cancel != nil {
		m.cancel()
	}
	// Close the active PF (also waits for stderr-drain to flush).
	if m.activePF != nil {
		_ = m.activePF.Close()
		m.activePF = nil
	}
	// The status URL described that forward, so it dies with it. Not merely
	// tidy: it is one of the four fields pipelineStore reads to decide an edit
	// targets a pod, and a URL for a closed forward is never the right answer.
	// portForwardReadyMsg sets a fresh one for the next pod.
	m.statusURL = ""

	// Reset session-view state so the next pod starts fresh.
	m.client = nil
	m.streamCh = nil
	m.sessions = nil
	m.events = make(map[string][]pipeline.SessionEvent)
	// In lockstep with m.events: a Seq recorded as fetched must not survive the rows
	// it described, or the next session to reuse that Seq would be assumed complete.
	m.fullFetched = nil
	// In lockstep with m.events, and the one case where the gauge's remembered figure is
	// genuinely void rather than merely unsupported by what is held: a different pod is a
	// different workload, so the same session id now means someone else's conversation. Left
	// behind, an id present on both pods with a matching event count takes the length-check
	// hit and reports the previous pod's context. sessionContextFor reallocates lazily.
	m.contextRun = nil
	// In lockstep with m.events. A count describing a session whose events are gone is
	// the bug that made this map per-session in the first place, just with a narrower
	// window: re-entering the events pane on a matching id before its snapshot lands.
	m.olderNotFetched = nil
	// A different pod is a different aggregator: keep the view options the
	// operator chose, drop the data they described.
	m.usage.snap = nil
	m.usage.err = nil
	m.usage.lastFetch = time.Time{}
	// Invalidate anything in flight against the old pod: its reply must not land
	// as if it described the new one.
	m.usage.reqSeq++
	m.usage.tickGen++
	// Same for the spend strip, and for the same reason. Not optional just because the
	// strip is always on: a strip left showing the previous pod's dollars while the
	// operator picks a new one is a figure attributed to the wrong workload, which is
	// worse than a blank strip. invalidate bumps both generations, so a reply already in
	// flight against the old pod is dropped rather than applied.
	m.spend.invalidate()
	m.eventCt = 0
	m.lastCt = 0
	m.rate = 0
	m.pipeline = nil
	// Drop the cached /v1/plugins snapshot too — a different pod is a
	// different framework instance with potentially different plugin
	// versions registered. The next `P` press refetches.
	m.catalog = nil
	m.catalogTbl.SetRows(nil)
	// pickerHarvested and pickerShowing are deliberately NOT reset here. They are edge-derived: the
	// next refresh tick recomputes "is the picker showing", finds it disagrees with the recorded
	// pickerShowing, and clears the budget on that inequality. Resetting them here would make this
	// a second writer of state that already has exactly one.
	//
	// The backoff describes THE POD BEING LEFT, so it must not price the next one's first
	// harvest. Those misses were recorded against a session list that is now gone (m.sessions is
	// cleared just above), and a different pod is a different set of sessions with a different
	// chance of being nameable. Left behind, six fruitless harvests here mean the next pod's list
	// waits the 3m cap before its first scan instead of untitledSettleDelay — measured, not
	// supposed.
	m.untitledMisses = 0
	// AND THE SET THAT PRICES IT. Leaving it behind carried the previous pod's unnamed ids into
	// the next connection, where a shared id — the `default` bucket every pod has, or a redeployed
	// agent reusing one — read as "already counted" and lost its fresh-row reset, counting a miss
	// it had not earned. The scoring rebuild above also clears this on the first harvest after the
	// list empties, so this assignment is not what closes the hole; it is here because this
	// function's job is to discard what described the pod being left, and the set describes it.
	m.untitledCounted = nil
	m.previousPane = paneNone
	// Same reason: a return pane recorded against the pod being left would send
	// the next `P`-then-esc back into a pane belonging to the previous connection.
	m.pipelineReturnPane = paneNone
	// Close the column picker with the pane it belongs to. The paneEvents gates
	// on the key block and in View() make it inert and invisible once we leave,
	// but the flag itself outlives the pane: entering a session on the next pod
	// puts m.pane back to paneEvents and the popup nobody reopened is there
	// again, owning the keyboard until the user finds esc.
	m.colPicker = false
	m.detailEvent = nil
	m.detailPlugin = nil
	m.selectedSess = ""
	m.selectedEventKey = eventKey{}
	m.filter = ""
	m.filtering = false
	// The filter's line comes off the height budget while it is open, so dropping the flag
	// has to give it back — see layout().
	m.layout()
	// The input too, not just the value. Since the input is seeded from saved
	// settings it is a second source of truth, and leaving it behind meant that after
	// backing out and entering the next pod, `/` presented the OLD filter text
	// already in the box — one keystroke then committed "github-toolx" and Enter
	// persisted it, over a list the footer correctly showed as unfiltered.
	m.filterInput.SetValue("")
	m.filterBeforeEdit = ""
	m.visibleRows = nil
	m.connState = connStateInfo{phase: connConnecting}

	// Re-derive ctx for the next session view.
	m.ctx, m.cancel = context.WithCancel(m.parentCtx)

	if m.localDirect {
		// Entered via `[l]`: no pod was ever selected, so the Pods pane
		// would render an empty table for a namespace the user never
		// picked. Go back to where they actually were.
		m.localDirect = false
		m.pane = paneNamespaces
		return
	}
	m.pane = panePods
}

// syncHelpViewport (re)builds the help overlay's content and sizes its
// viewport to the current terminal. Called when the overlay opens and on
// every resize while it's open, so the body re-wraps and the scroll range
// stays correct. resetScroll is true only on open — a resize should keep
// the reader where they were.
//
// THE BODY ACTUALLY RE-WRAPS NOW. This comment claimed it did while
// helpBodyLines took no width at all — it built one width-blind string and the
// viewport clipped whatever overran, so the overlay's longest line lost its
// second half on an 80-column terminal and said nothing about it. The wrap
// budget is the terminal minus the frame the panel draws around the viewport.
func (m *model) syncHelpViewport(resetScroll bool) {
	frameW := styleBorder.GetHorizontalBorderSize() + helpPadX*2
	body := helpBodyLines(m.pane, m.width-frameW)
	w, h := helpViewportSize(m.width, m.height, helpBodyWidth(body))
	m.helpVp.Width = w
	m.helpVp.Height = h
	m.helpVp.SetContent(body)
	if resetScroll {
		m.helpVp.GotoTop()
		return
	}
	// Keeping the reader where they were still means clamping to what the new size
	// can show. Height is a plain field, so assigning it above moved maxYOffset
	// without touching YOffset, and SetContent only clamps against the LINE COUNT —
	// so growing the terminal under a scrolled-down overlay left the offset past the
	// bottom, rendering the body with dead space below it. SetYOffset is the
	// clamping setter, and a no-op when the offset is already in range.
	m.helpVp.SetYOffset(m.helpVp.YOffset)
}

// Init fires the initial fetch + starts the SSE pump and the tick.
// In picker mode (paneNamespaces), it loads the agent list instead.
func (m *model) Init() tea.Cmd {
	// The harvest runs alongside whatever the pane loads, never before it. It is the one
	// startup task with no bearing on what the first frame shows: the metadata file already on
	// disk names every session the last run saw, so a harvest only ever ADDS titles — and
	// reading a large ~/.claude takes about as long as everything else at startup put
	// together. Batched rather than sequenced so neither waits on the other.
	// Stamped here, not on arrival: the sessions pane's backoff is measured from when a harvest
	// STARTS, so leaving it zero would make its first tick look overdue by the whole age of the
	// clock regardless of when this harvest actually ran.
	if m.harvest != nil {
		m.harvesting = true
		m.lastHarvest = time.Now()
	}
	if m.pane == paneNamespaces {
		// Picker mode — load agents, then idle until user picks a pod.
		m.loading = true
		// INIT'S HARVEST *IS* THE FIRST VISIT'S ARRIVAL HARVEST, so record the arrival here: the
		// operator is already on paneNamespaces when this runs, and the picker's rule is one walk
		// per visit. Without this the first tick walked the whole transcript tree a SECOND time
		// about two seconds into startup. The retired interval was what used to hide that.
		//
		// BOTH FIELDS, because either alone leaves the double scan in place. pickerHarvested is
		// the spent budget; pickerShowing is what stops the first tick from reading this as a
		// fresh arrival into the picker and clearing that budget again. They are one fact — "the
		// picker is showing and its scan is done" — and Init is where it first becomes true.
		m.pickerHarvested = true
		m.pickerShowing = true
		return tea.Batch(loadAgentsCmd(m.ctx, m.lister), harvestCmd(m.harvest))
	}
	return tea.Batch(m.initSessionView(), harvestCmd(m.harvest))
}

// loadPipelineCmd fetches /v1/pipeline. The plugin composition is static for
// the life of a process, but the view also carries each plugin's live
// Metrics counters — so a single fetch at startup would freeze them at zero,
// which on a fresh proxy is every number a user ever sees. It is refetched
// when a metrics-bearing pane is open; see refreshTickMsg.
func (m *model) loadPipelineCmd() tea.Cmd {
	return func() tea.Msg {
		pv, err := m.client.GetPipeline(m.ctx)
		if err != nil {
			// Report as a load with no view so the in-flight flag clears; a
			// failure that left it set would wedge refresh for the session.
			return pipelineLoadedMsg(nil)
		}
		return pipelineLoadedMsg(pv)
	}
}

// catalogLoadedMsg carries the result of /v1/plugins. Distinct from
// pipelineLoadedMsg because the catalog is the registered set, not
// the active chain.
type catalogLoadedMsg struct {
	catalog *apiclient.PluginCatalog
	err     error
}

// loadCatalogCmd fetches /v1/plugins. Called lazily when the user
// presses `P` to enter the catalog pane; the result is cached on the
// model and refreshed on demand.
func (m *model) loadCatalogCmd() tea.Cmd {
	return func() tea.Msg {
		cat, err := m.client.GetPluginCatalog(m.ctx)
		return catalogLoadedMsg{catalog: cat, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func refreshTickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return refreshTickMsg(t) })
}

// loadSessionsCmd fetches the current session list. Used at startup and after
// each successful reconnect so we don't miss new sessions that appeared
// while the stream was down.
func (m *model) loadSessionsCmd() tea.Cmd {
	return func() tea.Msg {
		summaries, err := m.client.ListSessions(m.ctx)
		if err != nil {
			return errMsg{where: "list sessions", err: err}
		}
		return sessionsLoadedMsg(summaries)
	}
}

// streamPump returns a tea.Cmd that blocks on the stream channel for one
// message, emits a streamMsg (or streamClosedMsg if the channel closed),
// and schedules itself again. This keeps all state mutation on the Tea
// event loop — no concurrent access to m.
func streamPump(ch <-chan apiclient.StreamEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamClosedMsg{}
		}
		return streamMsg(ev)
	}
}

// snapshotCmd fetches a single session's full event list. Used when the
// user drills into a session the stream hasn't fully populated yet (e.g.
// events that predate Subscribe).
func (m *model) snapshotCmd(id string) tea.Cmd {
	return func() tea.Msg {
		view, err := m.client.GetSession(m.ctx, id)
		if err != nil {
			return errMsg{where: "snapshot " + id, err: err}
		}
		older := 0
		if view.TotalEvents > len(view.Events) {
			older = view.TotalEvents - len(view.Events)
		}
		return snapshotLoadedMsg{
			id: id, events: view.Events, olderNotFetched: older, serverOldest: view.OldestSeq,
			projected: view.View == apiclient.SummaryView,
		}
	}
}

// Update handles every message + dispatches the next Cmd.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		// Re-size the help body too if it's currently up, preserving the
		// reader's scroll position.
		if m.helpVisible {
			m.syncHelpViewport(false)
		}
		return m, nil

	case tickMsg:
		// In picker mode, skip the rate calculation — m.client may be nil
		// after a back-out. Keep the ticker alive so it's ready when the
		// user re-enters a session.
		if m.pane == paneNamespaces || m.pane == panePods {
			return m, tickCmd()
		}
		now := time.Time(msg)
		// Rate over the last tick.
		delta := now.Sub(m.lastTick).Seconds()
		if delta > 0 {
			m.rate = float64(m.eventCt-m.lastCt) / delta
		}
		m.lastTick, m.lastCt = now, m.eventCt
		return m, tickCmd()

	case sessionsLoadedMsg:
		// The server list says what is LIVE. It does not say what is worth
		// keeping on screen.
		//
		// This used to drop cached events for every session the list omitted, and
		// bounce the user back to the sessions pane. The session store is
		// in-memory and per-pod, so abctl's copy is the only copy: a proxy restart
		// (or any blip that empties /v1/sessions, which arrives as a normal
		// message, not an error) destroyed the events someone was mid-investigation
		// on, about two seconds after they looked away. That is #870.
		//
		// Cached events are now released in exactly one place: when the user
		// returns to the picker and selects a different session (see keys.go).
		// Nothing here deletes, and nothing here changes the focused pane.
		m.sessions = []session.SessionSummary(msg)
		m.connState.phase = connOpen
		m.rebuildSessionsTable()
		if m.pane == paneEvents {
			m.rebuildEventsTable()
		}
		return m, nil

	case harvestedMsg:
		// THE ONLY PLACE harvesting IS CLEARED, and harvestCmd always returns a harvestedMsg —
		// on success, on a harvester error (which it swallows) and on a nil map alike — so the
		// flag is held for exactly one in-flight scan. That total coverage is the invariant:
		// any future path that can drop this message instead of delivering it latches
		// harvesting = true and silently disables every later harvest, in both the picker and
		// the sessions pane, for the life of the process. There is no watchdog. A harvest that
		// cannot report must still send this message.
		m.harvesting = false
		// COUNT THE MISS BEFORE THE MERGE, since the merge is what would hide it. A harvest that
		// named nothing NEW leaves every unnamed row unnamed, so the settle gate stays satisfied
		// and the next tick would walk the tree again at the same cadence; untitledBackoff is what
		// turns that into a widening retry. Reset on any harvest that named something, so a tree
		// which starts producing titles returns to the fast cadence at once.
		//
		// "NAMED SOMETHING" MEANS A TITLE THIS MODEL DID NOT ALREADY HAVE, not a non-empty result.
		// An incremental harvest returns the whole merged map — every session it has ever seen —
		// so len(msg.meta) > 0 is true on every call once the file exists, and counting that as
		// progress would leave the backoff permanently reset and the loop intact.
		// SCORED ONLY WHEN THERE ARE ROWS TO SCORE IT AGAINST. harvestNamedSomething asks whether
		// this result names a session m.sessions holds and could not name, so with that list empty
		// the answer is false no matter how much the harvest learned. Counting it would move the
		// backoff on no evidence, and it is the common case rather than a corner: Init harvests
		// before the session fetch it is batched alongside has returned, and backing out to the
		// picker sets m.sessions to nil, so every picker-mode harvest lands with zero rows. The
		// picker now harvests on arrival, which is the normal way in — so without this guard the
		// ordinary path to a session list would inflate untitledMisses several steps before the
		// first row was ever drawn, and the backoff would start already widened.
		//
		// UNSCOREABLE IS NEITHER, so the counter holds rather than resetting: a harvest nobody
		// could judge is no reason to believe the tree started producing titles either.
		//
		// AN UNNAMED ROW THIS SCORING HAS NOT SEEN BEFORE RESETS THE COUNTER, whatever the harvest
		// found, because the accumulated penalty was earned by OTHER rows. A row that can never be
		// named pins untitledMisses at the cap, and the next session to appear is a fresh question
		// the tree has never been asked — making it wait out a 3m backoff earned by a different
		// session is the bug. Ordered before the miss count so a tick that both gains a new row and
		// fails to name anything resets rather than incrementing: the new row has not been tried
		// yet, so there is no evidence against it to count.
		//
		// THE SET IS REBUILT ON EVERY HARVEST, INCLUDING UNSCOREABLE ONES, and that is outside the
		// len() > 0 guard for a reason the guard itself cannot serve. The guard decides whether
		// there is EVIDENCE to score; the set records WHAT WAS ON SCREEN when it was last asked.
		// Those are different questions, and keeping the set inside the guard answered the second
		// one with a stale snapshot: an empty list left the previous list's ids in place, so a
		// session that left and came back with no non-empty scoring in between was read as
		// "already counted" and inherited the full 3m cap for its first title — measured at
		// misses=9, which is the very defect the fresh-row reset exists to remove. An empty list
		// has no unnamed rows, so rebuilding here correctly empties the set, and the returning row
		// is new again.
		counted := make(map[string]bool, len(m.sessions))
		fresh := false
		for _, sess := range m.sessions {
			if m.sessionHasTitle(sess.ID) {
				continue
			}
			counted[sess.ID] = true
			if !m.untitledCounted[sess.ID] {
				fresh = true
			}
		}
		// ASSIGNED BEFORE THE BRANCH, so every harvest records what it judged. Doing it only on one
		// arm would leave the set describing some earlier tick, and the next new row would be
		// compared against a stale snapshot.
		m.untitledCounted = counted
		if len(m.sessions) > 0 {
			switch {
			case fresh:
				m.untitledMisses = 0
			case m.harvestNamedSomething(msg.meta):
				m.untitledMisses = 0
			default:
				m.untitledMisses++
			}
		}
		// Merge, never replace. The harvest sees one agent's config dir, while the map it is
		// merging into was loaded from a file that may carry entries from another dir or from a
		// transcript since pruned — the same reason the harvester itself upserts. Replacing
		// would blank titles the viewer is already showing.
		//
		// A failed or empty harvest is silent and identical: the viewer is open, so there is
		// nowhere to report without corrupting the frame, and the cost is a column that stays as
		// it was. main warns about what it can before the alt screen goes up.
		if len(msg.meta) > 0 {
			if m.sessionsData == nil {
				m.sessionsData = map[string]SessionMetadata{}
			}
			for id, meta := range msg.meta {
				m.sessionsData[id] = meta
			}
			// Repaint what names sessions. The sessions table is the only place a title is
			// rendered into a cell; every other user of sessionLabel builds its text on each
			// View, so those pick the new names up on the next frame with nothing to do here.
			m.rebuildSessionsTable()
		}
		return m, nil

	case usageLoadedMsg:
		// Discard anything but the newest request's reply. Two rapid `w` presses
		// leave two requests in flight; without this an out-of-order response
		// repaints a stale window under the current heading. Comparing an id
		// rather than the view fields means a future option cannot silently
		// escape the check.
		if msg.req != m.usage.reqSeq {
			return m, nil
		}
		m.usage.loading = false
		if msg.err != nil {
			if errors.Is(msg.err, apiclient.ErrNotFound) {
				m.usage.err = errUsageUnsupported
			} else {
				m.usage.err = msg.err
			}
			return m, nil
		}
		m.usage.err = nil
		m.usage.snap = msg.snap
		m.usage.lastFetch = time.Now()
		return m, nil

	case usageTickMsg:
		// Stop when the pane loses focus, and drop ticks from a previous visit:
		// a quick exit and re-entry would otherwise leave two chains alive, each
		// rescheduling its own successor.
		if m.pane != paneUsage || msg.gen != m.usage.tickGen {
			return m, nil
		}
		return m, tea.Batch(m.fetchUsage(), usageTick(m.usage.tickGen))

	case refreshTickMsg:
		// ARRIVING AT THE PICKER ENDS ITS ONE-HARVEST-PER-VISIT BUDGET. Detected here, on the
		// edge, so the two dozen places that assign m.pane do not each have to remember to clear
		// it — and so one added later cannot quietly skip the harvest by inheriting a set flag.
		//
		// THE EDGE IS INTO THE PICKER AS A WHOLE, not into either of its panes. A visit spans
		// both: enter on a namespace goes to panePods and esc comes back, and keying the budget
		// on raw pane equality made each of those hops a fresh visit — so drilling into a
		// namespace and backing out re-walked the entire transcript tree per hop, which is the
		// opposite of what one-per-visit is for. What matters is whether the operator is newly
		// in the picker at all, so that is what the edge compares.
		if showing := m.pane == paneNamespaces || m.pane == panePods; showing != m.pickerShowing {
			m.pickerShowing = showing
			m.pickerHarvested = false
		}
		// In picker mode, skip the fetch — m.client may be nil after a
		// back-out. Keep the ticker alive so it's ready when the user
		// re-enters a session.
		if m.pane == paneNamespaces || m.pane == panePods {
			// HARVEST ON ARRIVAL, EXACTLY ONCE PER VISIT, so a session started in another terminal
			// is named by the time the user scrolls to it. This is the one pane where someone may
			// sit for minutes with nothing else refreshing the titles on screen.
			//
			// pickerHarvested is the spent budget and pickerShowing is the arrival edge; Init sets
			// both for the first visit. There is no interval on purpose — the scan is opportunistic
			// idle-time work, so it belongs at the moment of arrival rather than on a clock some
			// earlier harvest set. See the comment above refreshInterval.
			//
			// Riding the existing refresh ticker rather than a second tea.Tick: one timer is easier
			// to reason about than two, and this branch already returns on every tick. That is how
			// the scan is DELIVERED, not what paces it.
			//
			// These panes hold no session rows, so nothing here can tell a fruitless walk from a
			// useful one, and the sessions pane's backoff cannot help because this path does not
			// consult it. One walk per visit is the most they can sensibly ask for.
			if m.harvest != nil && !m.harvesting && !m.pickerHarvested {
				m.harvesting = true
				m.pickerHarvested = true
				m.lastHarvest = time.Now()
				return m, tea.Batch(harvestCmd(m.harvest), refreshTickCmd())
			}
			return m, refreshTickCmd()
		}
		// RE-HARVEST FOR THE SESSIONS LIST, which is a picker as much as the two panes below
		// are: it gains a row whenever a session appears and cannot name it without re-reading
		// the transcripts. Gated on a row that is actually unnamed AND settled, so a list whose
		// titles are all known triggers nothing — see untitledSettled.
		//
		// ONLY WHILE THIS PANE IS THE VISIBLE ONE. m.pane is exactly that — the panes above
		// return before reaching here — so the check is the pane equality itself, and a harvest
		// never runs on behalf of a list nobody is looking at.
		//
		// BACKED OFF BY untitledBackoff rather than the flat settle delay, because a session that
		// can never be named keeps this gate satisfied forever: without the backoff an unnameable
		// row re-walks the transcript tree every untitledSettleDelay for a title that is not
		// coming. See untitledMisses.
		//
		// STARTED HERE, NOT RETURNED FROM HERE. This pane's tick must still reach the session
		// fetch at the bottom of this branch, so the harvest is batched into that return rather
		// than short-circuiting it — returning early instead would trade the titles for the
		// 2s refresh of every other cell in the row.
		var harvestNow tea.Cmd
		// ONE now FOR THE WHOLE DECISION, so the backoff and the settle test are provably
		// answered about the same instant rather than two readings of the clock a few
		// microseconds apart.
		now := time.Now()
		if m.pane == paneSessions && m.harvest != nil && !m.harvesting &&
			now.Sub(m.lastHarvest) >= untitledBackoff(m.untitledMisses) &&
			m.untitledSettled(now) {
			m.harvesting = true
			m.lastHarvest = now
			harvestNow = harvestCmd(m.harvest)
		}
		// Refresh the pipeline view too while a pane that displays plugin
		// Metrics is open, so counters tick rather than sitting at whatever
		// they were when the session was first opened. Skipped elsewhere:
		// the composition itself does not change, so polling it while nobody
		// is looking at metrics would be pure overhead.
		// Guard against stacking fetches: the tick is 2s and the HTTP timeout is
		// 10s, so a stalled endpoint would otherwise accumulate ~5 concurrent
		// requests and keep adding one every tick.
		if (m.pane == panePluginDetail || m.pane == panePipeline) && !m.pipelineFetching {
			m.pipelineFetching = true
			// harvestNow is deliberately absent: it is only ever set under m.pane == paneSessions,
			// and reaching here requires panePluginDetail or panePipeline, so passing it would
			// imply a combination that cannot occur. tea.Batch would drop the nil harmlessly —
			// the point is not to suggest otherwise to the next reader.
			return m, tea.Batch(m.loadSessionsCmd(), m.loadPipelineCmd(), refreshTickCmd())
		}
		return m, tea.Batch(m.loadSessionsCmd(), refreshTickCmd(), harvestNow)

	case pipelineLoadedMsg:
		m.pipelineFetching = false
		if msg == nil {
			return m, nil // fetch failed; keep the view we have
		}
		m.pipeline = (*apiclient.PipelineView)(msg)
		m.rebuildPipelineTable()
		// Re-render an open plugin detail pane against the new view. Without
		// this the pane keeps showing the snapshot it was opened with, so
		// Metrics would still read (none) however long traffic ran.
		if m.pane == panePluginDetail && m.detailPlugin != nil {
			if p := m.livePipelinePlugin(m.detailPlugin); p != nil {
				// A refresh, not an opening: hold the reader's scroll position.
				m.showPluginDetail(p, false)
			}
		}
		return m, nil

	case catalogLoadedMsg:
		if msg.err != nil {
			m.setFlash("catalog fetch failed: " + msg.err.Error())
			return m, nil
		}
		m.catalog = msg.catalog
		m.rebuildCatalogTable()
		return m, nil

	case snapshotLoadedMsg:
		// Every event the snapshot carried, untrimmed. This used to cut to the most
		// recent 1000 on the claim that it "matches the server's default cap" — the
		// server's was 500, so the two never matched, and neither trims now.
		//
		// Which leaves m.events unbounded in BOTH dimensions, and worth stating because
		// it is a laptop's memory: nothing caps depth now that the per-session 1000 is
		// gone, and entries are only ever released wholesale — on a pod or endpoint
		// switch (backToPodsPane resets the whole map) or an explicit operator prune — while
		// cachedOnlySessionIDs deliberately keeps sessions the server has stopped
		// listing. abctl holds the same full prompt and completion strings the proxy
		// does, so resident size tracks the traffic it has watched.
		//
		// Learned from the echo, not assumed: see model.serverProjects.
		m.serverProjects = msg.projected
		// Only update if we're still focused on this session.
		m.events[msg.id] = msg.events
		// A wholesale replacement the length check cannot see, so the gauge is re-folded
		// over what just landed. NOT dropped: msg.events is projected (view=summary
		// strips the manifest and the message count), so dropping the figure here blanked
		// the column the moment an operator opened a session — see rebaseSessionContext.
		m.rebaseSessionContext(msg.id, msg.events)
		if m.olderNotFetched == nil {
			m.olderNotFetched = map[string]int{}
		}
		m.olderNotFetched[msg.id] = msg.olderNotFetched
		// A snapshot IS the tail, so it ends any paging window over this session and
		// resumes live appends. This is where [t] completes, and the only place the state
		// is cleared — see returnToTail for why it has to outlive the request.
		delete(m.paging, msg.id)
		if m.pane == paneEvents && m.selectedSess == msg.id {
			m.rebuildEventsTable()
		}
		// AND THE SESSIONS ROW, because the rebase above changed this session's CONTEXT(1M)
		// figure and the table holds BAKED cells — rebuildSessionsTable renders each gauge to a
		// string once and View() reprints whatever was baked, so a figure nothing repaints is
		// still the dash it just disproved.
		//
		// UNCONDITIONAL, unlike the line above it, and that is the whole fix rather than an
		// oversight. This snapshot was issued by Enter on the sessions pane and normally lands
		// while the operator is still in the timeline they opened — milliseconds against a local
		// proxy — and esc back out is a bare pane switch that rebuilds nothing (keys.go). So a
		// `m.pane == paneSessions` guard would repaint only in the race and skip the ordinary
		// case, which is how this reached a user: open an idle row, come straight back out, and
		// the gauge appears about a second later when the /v1/sessions poll happens to rebuild
		// the table. The sessions table is a RETAINED component; what matters is what its rows
		// say when it is next painted, not which pane is focused when they are built.
		m.rebuildSessionsTable()
		return m, nil

	case olderPageLoadedMsg:
		m.applyOlderPage(msg)
		if m.pane == paneEvents && m.selectedSess == msg.id {
			m.rebuildEventsTable()
		}
		// applyOlderPage rebases as well, so the gauge owes the same repaint — see the snapshot
		// arm above for why it is not guarded on the focused pane.
		m.rebuildSessionsTable()
		return m, nil

	case olderPageFailedMsg:
		m.failOlderPage(msg)
		if m.pane == paneEvents && m.selectedSess == msg.id {
			m.rebuildEventsTable()
		}
		return m, nil

	case streamMsg:
		// In picker mode, m.streamCh is nil (cleared by backToPodsPane) and
		// m.handleStreamEvent would mutate stale state. Drop late-arriving
		// events from the previous session.
		if m.pane == paneNamespaces || m.pane == panePods {
			return m, nil
		}
		ev := apiclient.StreamEvent(msg)
		m.handleStreamEvent(ev)
		// Re-pump the same channel for the next message. A single apiclient
		// goroutine fills the channel for the duration of ctx.
		return m, streamPump(m.streamCh)

	case spendLoadedMsg:
		m.applySpendLoaded(msg)
		return m, nil

	case spendTickMsg:
		// ONE CASE FOR EVERY SPAN, because the tick carries the span it belongs to. The
		// four chains poll on four cadences — a ring read every 20s, up to thirty-one day
		// files every 5 minutes — and each reschedules only itself.
		//
		// TWO guards in one, and unlike the usage chain this tick carries no pane or
		// session scope: the span has to be a real one and the generation has to be live.
		// See spendTickIsCurrent.
		if !m.spendTickIsCurrent(msg.span, msg.gen) {
			return m, nil
		}
		return m, tea.Batch(m.fetchSpendSpan(msg.span), spendTick(msg.span, msg.gen))

	case spendDrawerLoadedMsg:
		m.applySpendDrawerLoaded(msg)
		return m, nil

	case spendDrawerTickMsg:
		// The drawer's own chain, and it stops when the drawer is CLOSED rather than running
		// for the session: its span can be a ledger window, so refreshing forever would walk
		// day files to redraw rows nobody is looking at.
		//
		// GATED ON expanded, NOT ON spendDrawerVisible(). Those are different questions and
		// conflating them killed the chain for good. m.spend.expanded deliberately survives a
		// move to a pane that cannot host the drawer and a resize below spendDrawerMinHeight —
		// see the esc handler, which leaves the flag alone precisely so returning finds the
		// drawer as the operator left it — while spendDrawerVisible() reports whether it is on
		// screen RIGHT NOW. Stopping on "not visible" meant: open it on Sessions, press `u`, and
		// the next tick returned without rescheduling. Nothing re-arms the chain but `$` itself,
		// so coming back to Sessions rendered the pre-switch snapshot forever — a stale money
		// figure with no age indicator and no disclosure anywhere, which is the failure
		// applySpendLoaded's own doc forbids for the band.
		//
		// The tick still FETCHES nothing it does not need: an off-screen drawer keeps its chain
		// alive on a five-minute cadence, which is one request per five minutes to have the rows
		// current the moment the operator comes back.
		if !m.spendDrawerTickIsCurrent(msg.gen) || !m.spend.expanded {
			return m, nil
		}
		return m, tea.Batch(m.fetchSpendDrawer(), spendDrawerTick(msg.gen, m.spend.pollInterval()))

	case streamClosedMsg:
		// In picker mode, ignore the close from the previous session —
		// there is no stream to reconnect and flipping connState would
		// leave stale "reconnecting" state visible when the user picks a
		// new pod.
		if m.pane == paneNamespaces || m.pane == panePods {
			return m, nil
		}
		m.connState.phase = connReconnecting
		return m, nil

	case detailEventLoadedMsg:
		m.applyDetailEvent(msg)
		return m, nil

	case errMsg:
		// Per-request failures (snapshot/pipeline) shouldn't flip the whole
		// connection into a terminal failed state — the stream can still be
		// healthy. Flash the error and leave connState alone. Only failures
		// from the initial sessions list (which runs before the stream opens)
		// mark the connection as failed so the user sees why nothing is
		// appearing.
		if strings.HasPrefix(msg.where, "snapshot") || msg.where == "get pipeline" {
			m.setFlash(msg.where + " failed: " + msg.err.Error())
			// A failed tail snapshot has to re-arm [t] and leave the paged window
			// described as it stands, rather than stranding the operator on a middle
			// page whose footer claims a refresh is under way.
			m.tailReturnFailed(strings.TrimPrefix(msg.where, "snapshot "))
			return m, nil
		}
		m.connState.phase = connFailed
		m.connState.err = msg.err
		return m, nil

	case agentsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.pickerErr = msg.err.Error()
			return m, nil
		}
		m.namespaces = msg.namespaces
		m.rebuildNamespacesTable()
		// If the user is on the Pods pane (e.g., reloaded via `r`), refresh
		// the pods table from the new data. If the previously-selected
		// namespace no longer exists, gracefully back out to the
		// Namespaces pane so the user isn't stranded looking at an
		// empty Pods table for a vanished namespace.
		if m.pane == panePods {
			found := false
			for _, ns := range m.namespaces {
				if ns.Name == m.selectedNamespace {
					found = true
					break
				}
			}
			if !found {
				m.selectedNamespace = ""
				m.pane = paneNamespaces
			} else {
				m.rebuildPodsTable()
			}
		}
		return m, nil

	case localConnectedMsg:
		m.loading = false
		if msg.err != nil {
			m.pickerErr = "localhost:9094: " + msg.err.Error()
			return m, nil
		}
		m.pickerErr = ""
		// No port-forward subprocess and no pod identity: this is a direct
		// connection to whatever is already listening locally. activePF stays
		// nil because there is nothing to tear down.
		//
		// The pod fields are CLEARED rather than assumed empty. They are only
		// empty when no pod was ever visited: backToPodsPane clears ~20 fields
		// but not these, so "pick a pod → esc → [l]" arrives here with
		// selectedNamespace / selectedPod naming the pod and statusURL naming
		// its closed port-forward. Since pipelineStore falls back to the
		// ConfigMap branch on exactly those three, leaving them set let `e`
		// kubectl-apply against a pod the screen was not showing — and poll a
		// forward this path never established. Harmless while `[l]` sessions
		// were assumed storeless; a wrong-target write once they were not.
		m.selectedNamespace = ""
		m.selectedPod = ""
		m.statusURL = ""
		m.endpoint = msg.endpoint
		m.client = msg.client
		m.localDirect = true
		m.pane = paneSessions
		return m, m.initSessionView()

	case portForwardReadyMsg:
		if msg.err != nil {
			m.pickerErr = "port-forward: " + msg.err.Error()
			return m, nil
		}
		m.activePF = msg.pf
		m.endpoint = msg.endpoint
		m.statusURL = msg.pf.StatusEndpoint()
		m.client = apiclient.New(m.endpoint)
		m.pane = paneSessions
		return m, m.initSessionView()

	case genFetchedMsg:
		if msg.gen != m.editState.generation || m.editState.phase != editPhaseFetching {
			return m, nil
		}
		if msg.Err != nil {
			m.editState.phase = editPhaseError
			m.editState.err = msg.Err.Error()
			return m, nil
		}
		m.editState.fetched = msg.Fetched
		m.editState.tempPath = msg.TempPath
		m.editState.phase = editPhaseEditing
		// FetchCmd may have fetched the catalog inline so it could
		// render the templates section. Cache it so the catalog pane
		// (P) and the post-save validator both reuse it without a
		// second round-trip.
		if msg.Catalog != nil {
			m.catalog = msg.Catalog
		}
		return m, openEditorCmd(m.editState.generation, msg.TempPath)

	case editorExitedMsg:
		if msg.gen != m.editState.generation || m.editState.phase != editPhaseEditing || m.editState.fetched == nil {
			return m, nil
		}
		if msg.err != nil {
			m.editState.phase = editPhaseError
			m.editState.err = "editor exited: " + msg.err.Error()
			return m, nil
		}
		edited, err := os.ReadFile(m.editState.tempPath)
		if err != nil {
			m.editState.phase = editPhaseError
			m.editState.err = "read edited file: " + err.Error()
			return m, nil
		}
		// Strip the templates reference section before any downstream
		// processing — the empty-check, YAML parse, splice preview, and
		// validation all expect to see only the active pipeline subtree.
		// No-op when the catalog wasn't included at fetch time (no fence
		// marker present in the buffer).
		edited = edit.StripTemplates(edited)
		// Fix 2: reject empty input before it can silently wipe the pipeline.
		if len(bytes.TrimSpace(edited)) == 0 {
			m.editState.phase = editPhaseError
			m.editState.err = "edited file is empty; press [Esc] to abort"
			return m, nil
		}
		// Fix 1: normalize trailing newline so Splice doesn't concatenate the
		// last edit line with the next top-level YAML key.
		if len(edited) > 0 && edited[len(edited)-1] != '\n' {
			edited = append(edited, '\n')
		}
		m.editState.editedRaw = edited
		originalSubtree := m.editState.fetched.InnerYAML[m.editState.fetched.PipelineStart:m.editState.fetched.PipelineEnd]
		// Fix 3: surface "no changes" rather than silently transitioning to done.
		if string(edited) == string(originalSubtree) {
			m.setFlash("no changes; nothing to apply")
			m.editState = editState{phase: editPhaseDone}
			return m, nil
		}
		// Fix 4: preserve YAML parse error line/col for the overlay.
		var yamlVal any
		if err := yaml.Unmarshal(edited, &yamlVal); err != nil {
			m.editState.phase = editPhaseError
			m.editState.err = "invalid YAML: " + err.Error()
			return m, nil
		}
		// The user's edited subtree parses standalone, but the framework
		// loads the WHOLE inner YAML. An indentation mismatch between
		// the user's edit and the splice site can produce a subtree that
		// parses on its own yet breaks the combined doc. Splice + reparse
		// to catch that here, instead of after a 60s kubelet round-trip.
		previewInner := edit.Splice(
			m.editState.fetched.InnerYAML,
			m.editState.fetched.PipelineStart,
			m.editState.fetched.PipelineEnd,
			edited,
		)
		var combinedVal any
		if err := yaml.Unmarshal(previewInner, &combinedVal); err != nil {
			m.editState.phase = editPhaseError
			m.editState.err = "invalid YAML after splice: " + err.Error() +
				"\n(probably an indentation mismatch — the pipeline: subtree must start at column 0)"
			return m, nil
		}
		// Pre-apply validation against the catalog. Skipped silently
		// when the catalog hasn't been fetched yet (operator hasn't
		// pressed P); the framework's validateRelationships is the
		// source of truth and runs again on reload regardless.
		var catalog []apiclient.PluginCatalogEntry
		if m.catalog != nil {
			catalog = m.catalog.Plugins
		}
		m.editState.validationErrs = edit.ValidatePipeline(edited, catalog)
		m.editState.diff = edit.Diff(originalSubtree, edited)
		m.editState.phase = editPhaseDiff
		return m, nil

	case genAppliedMsg:
		// Drop stale or aborted-edit AppliedMsg deliveries.
		if msg.gen != m.editState.generation || m.editState.phase != editPhaseApplying {
			return m, nil
		}
		if msg.Err != nil {
			m.editState.phase = editPhaseError
			m.editState.err = "apply failed: " + msg.Err.Error()
			return m, nil
		}
		m.editState.applyTime = msg.ApplyTime
		m.editState.phase = editPhaseWaiting
		return m, withGen(m.editState.generation, edit.PollCmd(
			m.ctx, m.editState.statusURL, msg.ApplyTime,
			describeTarget(m.editState.store)))

	case genPolledMsg:
		// Drop stale (different gen), fully-aborted (phase=Done), or
		// out-of-state-machine deliveries. fetched can be nil in those
		// cases; both Waiting (overlay) and Background (footer flash)
		// are valid targets for an in-flight watch.
		if msg.gen != m.editState.generation ||
			(m.editState.phase != editPhaseWaiting && m.editState.phase != editPhaseBackground) ||
			m.editState.fetched == nil {
			return m, nil
		}
		bg := m.editState.phase == editPhaseBackground
		switch msg.Result.Status {
		case edit.PollSuccess:
			if bg {
				m.setFlash("hot-reload succeeded")
			}
			m.editState = editState{phase: editPhaseDone}
			return m, m.loadPipelineCmd()
		case edit.PollFailure, edit.PollTimeout:
			// Reload didn't take. The running pipeline is still the previous
			// one; reconcile the stored config back to match.
			//
			// Caveat: the rollback bytes come from m.editState.fetched —
			// captured at this edit's Fetch time — so a third party who
			// changed the config between our forward apply and this rollback
			// has their change silently reverted too. This is not a true undo.
			// In a cluster that third party is an operator reconcile, a
			// kubectl edit or a kustomize apply, and Apply's
			// --force-conflicts=true and field-manager=abctl let us win; on a
			// local file it is another editor holding the same path. Either
			// way the framework's running pipeline is unaffected (build
			// failure → keeps the previous in-memory pipeline), but the stored
			// config can lose third-party state. Surfacing a "third-party
			// change detected" path is a future option.
			reason := msg.Result.LastError
			if msg.Result.Status == edit.PollTimeout {
				// The deadline this store actually waited, not a literal: a
				// hardcoded "120s" was already wrong for a local edit, which
				// gives up after LocalPollDeadline. Same Target PollCmd was
				// given, so the number reported is the number waited.
				reason = "reload not observed in " +
					describeTarget(m.editState.store).Deadline().String()
			}
			origManifest, mErr := m.editState.store.Build(
				m.editState.fetched,
				m.editState.fetched.InnerYAML,
			)
			if mErr != nil {
				if bg {
					m.setFlash("hot-reload failed: " + reason + "; rollback build failed too")
					m.editState = editState{phase: editPhaseDone}
					return m, nil
				}
				m.editState.phase = editPhaseError
				m.editState.err = "reload failed: " + reason +
					"\n(rollback build failed: " + mErr.Error() + ")"
				return m, nil
			}
			// Stay backgrounded if user already Esc'd.
			if bg {
				m.editState.phase = editPhaseBackground
			} else {
				m.editState.phase = editPhaseRollback
			}
			return m, withGen(m.editState.generation, edit.RollbackCmd(m.ctx, m.editState.store, origManifest, reason))
		}
		return m, nil

	case genRolledBackMsg:
		if msg.gen != m.editState.generation ||
			(m.editState.phase != editPhaseRollback && m.editState.phase != editPhaseBackground) {
			return m, nil
		}
		bg := m.editState.phase == editPhaseBackground
		// The rollback messages name the target too. Sending a local operator to
		// kubectl about a ConfigMap that does not exist is the same lie Target
		// was introduced to kill; the overlay renderer was only half of it.
		rbTarget := describeTarget(m.editState.store)
		if bg {
			if msg.Err != nil {
				m.setFlash("hot-reload failed: " + msg.ReloadErr +
					"; rollback failed: " + msg.Err.Error())
			} else {
				m.setFlash("hot-reload failed: " + msg.ReloadErr +
					"; rolled back to previous " + rbTarget.Noun)
			}
			m.editState = editState{phase: editPhaseDone}
			return m, nil
		}
		m.editState.phase = editPhaseError
		if msg.Err != nil {
			m.editState.err = "reload failed: " + msg.ReloadErr +
				"\nrollback also failed: " + msg.Err.Error() +
				"\n" + rbTarget.Noun + " and running pipeline are out of sync; " + rbTarget.OutOfSyncHint
			return m, nil
		}
		m.editState.err = "reload failed: " + msg.ReloadErr +
			"\nrolled back to previous " + rbTarget.Noun
		return m, nil

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}

	// Delegate to the active pane's component.
	switch m.pane {
	case paneSessions:
		var cmd tea.Cmd
		m.sessionsTbl, cmd = m.sessionsTbl.Update(msg)
		return m, cmd
	case paneEvents:
		var cmd tea.Cmd
		m.eventsTbl, cmd = m.eventsTbl.Update(msg)
		return m, cmd
	case paneDetail:
		var cmd tea.Cmd
		m.detailVp, cmd = m.detailVp.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleStreamEvent routes a single StreamEvent from the apiclient.
func (m *model) handleStreamEvent(ev apiclient.StreamEvent) {
	if ev.Status.Phase != "" {
		switch ev.Status.Phase {
		case "open":
			m.connState.phase = connOpen
		case "reconnecting":
			m.connState.phase = connReconnecting
			m.connState.attempt = ev.Status.Attempt
			m.connState.nextRetry = time.Now().Add(ev.Status.Wait)
		}
		return
	}
	if ev.Event == nil {
		return
	}
	if m.paused {
		return
	}
	e := *ev.Event
	m.eventCt++
	// Counted above but NOT appended when this session's timeline has been paged off its
	// tail: a streamed event belongs at the end, and the end is not what is on screen, so
	// appending would put a live event directly after one from the middle of the session
	// and present the two as consecutive. The rate counter still moves because the traffic
	// is real; [t] returns to the tail and picks these up in the refetch.
	if !m.pagedBack(e.SessionID) {
		buf := append(m.events[e.SessionID], e)
		m.events[e.SessionID] = buf
	}

	// Bump updatedAt on the session summary if we already have it.
	//
	// UpdatedAt only. This used to also write EventCount = len(buf), which made the
	// EVENTS column mean two different things depending on which code path last
	// touched the row: the local cache length here, the server's own count on the
	// two-second poll. The two disagreed by construction — abctl's buffer holds what
	// it snapshotted plus what it has streamed since attaching, the server's count is
	// every event the session produced — so the cell visibly flipped between them, 500
	// against 1000 back when both sides capped. The server's count is the one that is
	// complete, so it is the only one that writes here now; the poll refreshes it
	// within two seconds of anything changing.
	for i := range m.sessions {
		if m.sessions[i].ID == e.SessionID {
			m.sessions[i].UpdatedAt = e.At
			goto sortAndRebuild
		}
	}
	// New session → create a stub summary; next list refresh will replace it.
	m.sessions = append(m.sessions, session.SessionSummary{
		ID: e.SessionID, CreatedAt: e.At, UpdatedAt: e.At, EventCount: 1, Active: true,
	})
sortAndRebuild:
	sort.Slice(m.sessions, func(i, j int) bool {
		return m.sessions[i].UpdatedAt.After(m.sessions[j].UpdatedAt)
	})
	m.rebuildSessionsTable()
	if m.pane == paneEvents && m.selectedSess == e.SessionID {
		// TODO: coalesce these rebuilds. Every streamed event rebuilds the whole table
		// for the session being watched: ~1.5ms flat plus ~0.23ms per 1000 events held
		// (measured: 3.5ms at 10k, 12ms at 50k, 25ms at 100k). The flat part is this
		// rebuild-per-event design and predates unbounded retention, which it dominates
		// below ~5k events; past that the growth term takes over, so a session long
		// enough will outrun the arrival rate. Rebuilding at most once per tick would
		// bound it. Left alone because no session anyone has today is near it.
		m.rebuildEventsTable()
	}
}

// View composes the full screen. The [?] key-help overlay is layered on
// top of whatever the pane rendered, so it works over the picker and the
// session views alike.
func (m *model) View() string {
	base := m.paneView()
	if m.helpVisible {
		return overlayCenter(base, renderHelpOverlay(m.helpVp, m.width, m.height), m.width, m.height)
	}
	// Same paneEvents scoping as the key block: an async pane change must not leave
	// the popup drawn over a pane it does not belong to.
	if m.colPicker && m.pane == paneEvents {
		return overlayCenter(base,
			renderColumnPicker(m.eventColumns, m.colCursor, m.width, m.height,
				m.sortCol, m.sortDesc),
			m.width, m.height)
	}
	return base
}

// paneView renders the active pane without the help overlay.
func (m *model) paneView() string {
	// Edit overlay takes over the screen while an edit is in flight.
	// editPhaseBackground intentionally falls through — the user backed
	// out and wants the normal UI back; flash messages handle reporting.
	if m.editState.phase != editPhaseDone && m.editState.phase != editPhaseBackground {
		return renderEditOverlay(m.editState, m.width, m.height)
	}

	if m.pane == paneNamespaces {
		title := "abctl · pick namespace"
		// m.namespaces == nil → still loading (don't flash the empty-state
		// hint mid-load); non-nil empty slice → loaded, no agents found.
		var body string
		if m.namespaces != nil && len(m.namespaces) == 0 && m.pickerErr == "" {
			body = styleHint.Render(
				"No AuthBridge agents found in this cluster.\n" +
					"Press [l] to connect to " + m.localEndpointOr() + " (an existing\n" +
					"port-forward), or use `abctl --endpoint http://...`.")
		} else {
			body = m.namespacesTbl.View()
		}
		footer := m.helpView()
		if m.pickerErr != "" {
			footer = "error: " + m.pickerErr + "    " + footer
		}
		// Fitted like the session-view footer: an error prefix can push even a
		// short picker hint past the terminal width, and a wrapped footer costs a
		// row of the table above it.
		footer = fitHintLine(footer, m.width)
		// The divider closes the top block here as on every other pane, which on a picker is
		// the title alone. layout() reserves its row unconditionally, so a pane that skipped
		// drawing it would leave a row nobody fills.
		return lipgloss.JoinVertical(lipgloss.Left,
			styleTitle.Render(title),
			styleMuted.Render(renderDivider(m.width)),
			body,
			styleHint.Render(footer),
		)
	}
	if m.pane == panePods {
		title := "abctl · " + m.selectedNamespace + " · pick pod"
		body := m.podsTbl.View()
		footer := m.helpView()
		if m.pickerErr != "" {
			footer = "error: " + m.pickerErr + "    " + footer
		}
		// Fitted like the session-view footer: an error prefix can push even a
		// short picker hint past the terminal width, and a wrapped footer costs a
		// row of the table above it.
		footer = fitHintLine(footer, m.width)
		return lipgloss.JoinVertical(lipgloss.Left,
			styleTitle.Render(title),
			styleMuted.Render(renderDivider(m.width)),
			body,
			styleHint.Render(footer),
		)
	}
	if m.width == 0 {
		return "initializing…"
	}
	var title string
	var body string
	switch m.pane {
	case paneSessions:
		// NO SCOPE NOTE. The title used to carry " · lifetime totals", to say that the table's
		// figures are per-session sums rather than slices of a clock window. It is gone, because
		// it misread in the one direction that matters:
		//
		//   - "lifetime" NAMES A SPAN, and this table has no single span. Each row covers its own
		//     session, first event to last, and no two rows need cover the same duration. There
		//     was no one duration for the word to be true about.
		//   - WHERE IT DID IMPLY A SPAN, it implied the wrong one. The session store is in memory
		//     and resets when the proxy restarts, so a session's lifetime cannot exceed proxy
		//     uptime — measured on a freshly restarted local proxy, the COST column summed to
		//     $4.04, matching the band's rolling hour, while the band's day read $18.80. The word
		//     that sounds like "everything ever" was labelling the SHORTEST span on screen.
		//   - It sat at the end of the title, one line above a band whose nearest cells are
		//     explicitly clock-windowed, so it read as covering those too.
		//
		// The contrast carries it instead: every band cell names its own span, and the table is
		// the only thing on screen with a SESSION column. The [?] overlay still states it in full
		// for a reader who wants it spelled out.
		title = fmt.Sprintf("abctl · %s", m.endpoint)
		body = m.sessionsTbl.View()
	case paneEvents:
		// Fitted to the terminal rather than to a fixed 36: a bare UUID is 36 characters, so
		// the old constant truncated a titled session ALWAYS and an untitled one never —
		// and it clipped "0e61b82d-8578-4d16-a18e…" on a 200-column terminal with room to
		// spare. sessionHeader measures the room actually available.
		title = m.sessionHeader(m.selectedSess, "")
		body = m.eventsTbl.View()
		if banner := identityBanner(m.events[m.selectedSess], m.width); banner != "" {
			body = banner + "\n" + body
		}
	case paneDetail:
		title = m.sessionHeader(m.selectedSess, "event")
		body = m.detailVp.View()
	case panePipeline:
		// Names itself, because nothing else does any more. While the tab strip
		// was in the title it said which of the two top-level views was showing;
		// with the strip gone, a pane whose title read only "abctl · <endpoint>"
		// would be indistinguishable from Sessions in a screenshot.
		title = fmt.Sprintf("abctl · %s · pipeline", m.endpoint)
		if m.pipeline == nil {
			body = styleHint.Render("(loading pipeline…)")
		} else {
			body = m.pipelineTbl.View()
		}
	case panePluginDetail:
		name := "plugin"
		if m.detailPlugin != nil {
			name = m.detailPlugin.Name
		}
		title = fmt.Sprintf("abctl · pipeline · %s", name)
		body = m.detailVp.View()
	case paneUsage:
		scope := "all"
		if m.usage.session != "" {
			scope = m.usage.session
		}
		title = fmt.Sprintf("abctl · %s · usage · %s", m.endpoint, scope)
		body = m.renderUsage(m.width, m.bodyHeight)
	case paneCatalog:
		title = fmt.Sprintf("abctl · %s · catalog", m.endpoint)
		if m.catalog == nil {
			body = styleHint.Render("(loading catalog…)")
		} else if len(m.catalog.Plugins) == 0 {
			body = styleHint.Render("(no registered plugins reported by /v1/plugins)")
		} else {
			body = m.catalogTbl.View()
		}
	}

	header := styleTitle.Render(title)
	if m.filtering {
		body = m.filterInput.View() + "\n" + body
	}
	// A row slice rather than a fixed JoinVertical, so the strip's row can be absent
	// without needing a second call site. It sits directly under the title because that
	// is the whole requirement: spend read BEFORE the data rather than navigated to.
	//
	// Styled AFTER fitting. renderSpendBand measures DISPLAY COLUMNS (lipgloss.Width, see
	// bandCell.width), and styleMuted only adds a colour escape so the column count is
	// unchanged — but fitting an already-styled string would measure the escape bytes and
	// silently over-truncate.
	//
	// Nothing here touches eventsTbl: the strip holds no cursor, filter or scroll state,
	// so it cannot perturb the pane it sits above.
	rows := []string{header}
	// EXACTLY WHAT layout() RESERVED, blank where there is nothing to say. Both reservations are
	// height-gated and must stay that way — layout() runs only from the WindowSizeMsg handler, so
	// a content-gated reservation would go stale the moment a poll landed, and stale in the
	// direction that overflows. So the render fills them rather than the reservation tracking the
	// render.
	//
	// The case that made this necessary: the renderer can return FEWER lines than the reservation,
	// which left the strip's row and the drawer's five unfilled and the footer six rows above the
	// bottom of the terminal. The same arithmetic covers a drawer left open on a pane that cannot
	// host it.
	//
	// NOT "blank before the first poll answers", which an earlier version of this said: an
	// unanswered band draws its four labels over four em dashes — that IS the honest "we have not
	// looked". Measured, the band comes back blank in one case only: a width so narrow that not
	// even one cell fits, which is 4 columns or less. So `drew` below tracks "the strip is visible
	// AND at least one cell fit", and it is not vacuous — it is the gate that keeps the breakdown
	// off a screen with no figure above it.
	if m.spendStripReservesRow() {
		band := make([]string, spendBandLines)
		drew := false
		if m.spendStripVisible() {
			// Styled AFTER fitting, for the reason stated above the row slice: the renderer
			// measures display columns and an escape sequence is not one.
			//
			// THE STYLED FORM COMES FROM THE RENDERER NOW, not from wrapping its output here. The
			// band used to be two rows and this line muted the whole label row —
			// `styleMuted.Render(band[0]), band[1]` — which is a hierarchy that exists only while
			// labels and figures live on separate rows. Folded to one row, wrapping the line would
			// mute the figures with the labels and flatten the contrast into uniform grey, so the
			// mute moved to where a cell knows which half is which. See bandCell.renderMuting.
			//
			// ONE CALL, because this is bubbletea's per-event render path and spendSummary walks
			// all four poll chains. `drew` comes back from the renderer rather than being tested
			// on the string it returns — see renderSpendBandStyled for why the styled form cannot
			// answer that question.
			band, drew = renderSpendBandStyled(m.spendSummary(), m.width)
		}
		for len(band) < spendBandLines {
			band = append(band, "")
		}
		rows = append(rows, band...)

		if m.spendDrawerReservesRows() {
			var lines []string
			// The breakdown goes directly under the figure it breaks down, and ONLY when the
			// strip itself drew — a headless breakdown would be a pane, which is the one thing
			// this is not.
			if drew && m.spendDrawerVisible() {
				// The axis and span come off the SNAPSHOT, not off what was last requested: see
				// drawerLabels.
				axis, window := m.drawerLabels()
				lines = renderSpendDrawer(m.spend.drawer.snap, m.spend.drawer.err, axis, window, m.width)
			}
			for len(lines) < spendDrawerLines {
				lines = append(lines, "")
			}
			for _, line := range lines {
				rows = append(rows, styleMuted.Render(line))
			}
		}
	}
	// The rule that closes the top block, AFTER the band and its drawer so it always sits
	// between the whole block and the body rather than inside it. Appended unconditionally,
	// which is what lets layout() reserve it without asking anything — see renderDivider.
	rows = append(rows, styleMuted.Render(renderDivider(m.width)))
	rows = append(rows, body, m.footerView())
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// sessionHeader renders a session-scoped title bar: "abctl · <label>" plus an optional
// suffix ("event"), clipped only if the terminal genuinely cannot hold it.
//
// Clipping against m.width rather than a per-pane constant. The constants it replaced were 36
// and 24 — 36 being exactly the length of a UUID, so the events header truncated every titled
// session and no untitled one, while the detail header clipped the id itself at 24 on a
// terminal of any size. Neither number was a fact about the screen.
//
// The label is clipped from the LEFT, so what survives a narrow terminal is the id and the end
// of the title, not the "abctl · " that is on every screen anyway. Same reasoning as
// sessionTitleCell: the distinguishing end of both a path and a UUID-suffixed label is the
// right one.
func (m *model) sessionHeader(id, suffix string) string {
	label := m.sessionLabel(id)
	head := "abctl · "
	tail := ""
	if suffix != "" {
		tail = " · " + suffix
	}
	// A floor of 12, so a very narrow terminal shows a stub of the label rather than dropping
	// it: the header is the only thing on screen naming which session these events belong to.
	//
	// The floor can exceed what is left, and deliberately does: a 12-column stub on a
	// 20-column terminal is worth one wrapped line, where a label cut to 3 columns is worth
	// nothing. What is NOT deliberate is skipping the truncation entirely — the guard was
	// `room > 0`, and "abctl · " is 8 columns, so a width of 8 or less emitted the label at
	// full length rather than as a stub. Not reachable on a real terminal; the point is that
	// the narrow case now goes through one rule instead of two.
	room := m.width - lipgloss.Width(head) - lipgloss.Width(tail)
	if room < 12 {
		room = 12
	}
	label = truncLeft(label, room)
	return head + label + tail
}

// The "[Sessions] Pipeline" tab strip used to be rendered here, by viewTabs.
//
// It is gone because it implied a peerage that was never true. Sessions is the
// surface abctl exists for; the pipeline is config, read occasionally. And the
// strip was a map with two of four destinations on it — Usage (`u`) and the
// plugin catalog (`C`) are top-level panes too, and neither was ever a tab. One
// rule now covers all three: Sessions is the app, and every other top-level
// surface is opened by a key and returns to the pane that opened it.
//
// The keys are advertised in each pane's footer and in the [?] overlay, which is
// the surface that cannot run out of room. `P` sits near the TAIL of the sessions
// footer for that reason — fitHintLine drops hints from the front, so a key at the
// tail outlives the ones ahead of it. See helpView for the measured widths.

// trunc clips a string to n DISPLAY COLUMNS with a trailing ellipsis.
//
// Columns, not runes, since every caller budgets in columns: a table cell's fitted width, a
// detail pane's inner width. A rune count is a different number the moment the input is not
// ASCII — measured, an 11-column budget returned 21 columns of CJK and 14 of emoji.
//
// Three production callers: the sessions table's id cell (twice, both hex-ish ids) and the events
// pane's identity block, whose `line` helper wraps the JWT `subject`, `client` and `scopes` claims
// at events_pane.go:1058. Those claims are remote-controlled, so "ASCII in practice" is an
// observation about today's tokens rather than a guarantee.
//
// Latent rather than observed, which is why the fix is a redirect and not a rewrite: on ASCII
// this is byte-for-byte what the old rune-counting version returned, so no current caller
// changes behaviour (there is a probe for that equivalence in the sessions title tests). The
// column-aware implementation lives in sessions_pane.go beside its left-truncating mirror.
//
// truncStr in events_pane.go is the byte-indexed variant for fixed-width ASCII table cells — if
// that ever needs to handle multi-byte input, point it here too.
func trunc(s string, n int) string {
	return truncRight(s, n)
}

// yankDirRel is the yank output directory, relative to the user's home. It
// follows the same convention as abctl's other durable state (cmd_claudecode.go's
// cortexCfgRel / stateRel), so there is one ~/.cortex tree rather than a new one.
const yankDirRel = ".cortex/abctl-events"

// yankDir returns the absolute directory yank writes into.
//
// Not os.TempDir(): on macOS that is /var/folders/<opaque>/T, so a yanked file
// landed on a 92-character path nobody could find or retype, which is #868.
//
// Not a fixed path under /tmp either, which is where this started. /tmp is
// world-writable, and os.MkdirAll returns nil for a path that already exists
// whatever its owner or mode — so a fixed /tmp/<name> cannot enforce 0700, can
// be pre-created as a symlink that redirects where events land, and on a shared
// host is squatted by whoever yanks first, permanently breaking everyone else.
// Session events carry identity subjects, raw LLM completions and tool
// arguments, so none of that is acceptable for a directory holding them.
//
// ~/.cortex is the user's own tree and is normally 0700, which makes those cases
// unreachable — but abctl never creates it, so its mode is whatever an installer
// or the user left. With a 0755 ~/.cortex, MkdirAll neither tightens the mode nor
// refuses to follow an abctl-events symlink someone planted, and the event lands
// in their directory. Verified. So the guarantee is enforced here rather than
// assumed: yankEventToFile Lstats the directory and refuses to write unless it is
// a real directory, owned by this user, with no group or world access.
//
// Still far shorter than where this started, and a location users already know.
func yankDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	if home == "" {
		// Separate branch, not folded into the one above: %w on a nil error
		// renders as "%!w(<nil>)", which would make this defensive path
		// unreadable to whoever ever hits it.
		return "", errors.New("cannot determine your home directory: it is empty")
	}
	return filepath.Join(home, yankDirRel), nil
}

// checkYankDir refuses to write into a directory that does not actually protect
// its contents. MkdirAll returns nil for a path that already exists whatever its
// owner or mode, and it happily follows a symlink — so an abctl-events symlink
// planted by another local user would silently redirect events carrying identity
// subjects and raw LLM completions into their directory.
//
// Checks every component from the home directory down, not just the leaf: a
// symlinked ~/.cortex redirects the whole subtree just as effectively, and the
// leaf-only version of this function missed it (verified — the event landed in the
// attacker's tree). Lstat, not Stat: Stat resolves the link and would report the
// target's mode.
//
// The mode requirement applies only to the directories abctl owns (~/.cortex and
// abctl-events), not to the home directory itself: a real home is commonly 0750
// or 0755 — this machine's is 0750 — and demanding 0700 there would refuse to
// yank on an ordinary account. Every component is still checked for a symlink,
// which is the redirection risk.
//
// A loose mode on a directory abctl owns is tightened rather than refused, which
// is what the rest of the tree already does for this exact problem:
// writeBuiltinConfig in cmd/authbridge-proxy/local.go chmods ~/.cortex to 0700
// after MkdirAll, and again one level down for the CA directory. Self-healing
// beats handing the user a chmod to run by hand, and if the chmod fails — someone
// else owns it — the refusal below still stands. Symlinks and non-directories stay
// hard refusals; there is no chmod out of those.
//
// Ownership is deliberately not checked via syscall.Stat_t: that type does not
// exist on Windows and would break this package's cross-compile.
func checkYankDir(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil {
		return err
	}

	// home first (symlink check only), then each component abctl owns.
	fi, err := os.Lstat(home)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write session events "+
			"through it", home)
	}

	path := home
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing to write session events "+
				"through it", path)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			// Tighten in place; only report if that does not work.
			if err := os.Chmod(path, 0o700); err != nil {
				return fmt.Errorf("%s has mode %v and could not be tightened "+
					"(%v); session events need no group or world access "+
					"(chmod 700 %s)", path, perm, err, path)
			}
		}
	}
	return nil
}

// yankEventToFile writes the currently-focused event to a fresh file in yankDir
// as pretty JSON and returns the path. Uses os.CreateTemp so the file is
// created with 0600 perms (session events carry identity subjects, raw
// LLM completions, and tool arguments — the operator-only default keeps
// them off shared / CI hosts). CreateTemp also implies O_EXCL, so there is no
// create-then-chmod window.
func yankEventToFile(e *pipeline.SessionEvent) (string, error) {
	dir, err := yankDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := checkYankDir(dir); err != nil {
		return "", err
	}
	ts := time.Now().UTC().Format("20060102-150405")
	// No "abctl-event-" name prefix: inside a directory already called
	// abctl-events it says nothing. The random tail stays — it is what keeps
	// two yanks in the same second from clobbering each other.
	f, err := os.CreateTemp(dir, ts+"-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// RunOptions selects the entry mode for abctl's TUI.
//
// If Endpoint is non-empty, abctl skips the picker and connects directly
// to that URL — preserving the pre-picker behavior (and the documented
// `--endpoint` flag).
//
// Otherwise, abctl uses Lister + PortForwarder to render the picker.
// Both must be non-nil in picker mode.
type RunOptions struct {
	Endpoint      string
	Lister        cluster.Lister
	PortForwarder cluster.PortForwarder
	// LocalEndpoint overrides where `[l]` connects. Empty means
	// defaultLocalEndpoint.
	LocalEndpoint string
	// LocalConfigPath and LocalStatsURL describe the Cortex running on this
	// machine: the config file it was started with, and where it serves
	// /reload/status. Together they let `e` edit its pipeline directly —
	// no kubectl, no ConfigMap, since the proxy already watches that file.
	//
	// Set only alongside LocalEndpoint, i.e. only when a local Cortex
	// answered. Either left empty disables local editing rather than
	// half-enabling it, because an apply we cannot confirm reloaded is
	// worse than an edit we declined to start.
	LocalConfigPath string
	LocalStatsURL   string
	// Save persists the user's settings whenever one changes. Deliberately not a
	// list of the keypresses that trigger it: the list was already stale once, and
	// the triggers live with the settings they write. A callback rather than a path
	// keeps $HOME and the YAML out of this package, so its tests need neither.
	//
	// Nil disables persistence: what tests pass, and what main passes when there is
	// no resolvable home directory to write to.
	Save func(UserSettings) error
	// Harvest reads session titles from a coding agent's own transcripts, in the
	// background, once the UI is up.
	//
	// Nil means no harvest: what tests pass, and what main passes under
	// --skip-claude-metadata. The viewer then shows whatever the metadata file already
	// held — the titles from the last run — so this only ever ADDS names.
	Harvest HarvestFunc
}

// Run starts the bubbletea program. See RunOptions for mode selection.
func Run(ctx context.Context, opts RunOptions) error {
	var m *model
	if opts.Endpoint != "" {
		c := apiclient.New(opts.Endpoint)
		m = New(ctx, c).(*model)
	} else {
		if opts.Lister == nil || opts.PortForwarder == nil {
			return fmt.Errorf("picker mode requires both Lister and PortForwarder; pass --endpoint to bypass")
		}
		m = newPickerModel(ctx, opts.Lister, opts.PortForwarder)
	}
	m.localEndpoint = opts.LocalEndpoint
	m.localConfigPath = opts.LocalConfigPath
	m.localStatsURL = opts.LocalStatsURL
	// After the constructor branch, so the two paths cannot disagree about it:
	// newPickerModel and New would otherwise each need their own copy.
	m.save = opts.Save
	m.harvest = opts.Harvest
	defer func() {
		if m.activePF != nil {
			_ = m.activePF.Close()
		}
	}()
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// openEditorCmd returns a tea.Cmd that suspends bubbletea, runs $EDITOR
// (vi if unset) on path, and emits editorExitedMsg when the editor exits.
// gen is captured at call time so the handler can detect a stale exit
// from an aborted-then-restarted edit.
func openEditorCmd(gen int, path string) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command("sh", "-c", editor+" "+path)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorExitedMsg{gen: gen, err: err}
	})
}

// shortHost renders an endpoint for a cramped footer: "http://localhost:9094"
// becomes "localhost:9094". Display only — never used to dial.
func shortHost(endpoint string) string {
	s := strings.TrimPrefix(endpoint, "http://")
	return strings.TrimPrefix(s, "https://")
}
