package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/session"
	"github.com/rossoctl/rossocortex/authbridge/cmd/abctl/apiclient"
	"github.com/rossoctl/rossocortex/authbridge/cmd/abctl/cluster"
	"github.com/rossoctl/rossocortex/authbridge/cmd/abctl/edit"
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
)

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

// maxEventsPerSession caps per-session event retention in the TUI. Matches
// the server's default maxEvents cap so we don't hold more than the server
// itself does.
const maxEventsPerSession = 1000

// flashDuration is how long a one-shot status message (e.g. yank
// confirmation) stays in the footer.
const flashDuration = 3 * time.Second

// refreshInterval is how often abctl re-fetches /v1/sessions from the
// server to reconcile its local list. Cheap, and the only mechanism by
// which rekeys (default → contextId) propagate to the client UI — the
// server itself doesn't emit a rekey signal on the stream, so short-lived
// stub sessions would otherwise linger in the TUI.
const refreshInterval = 2 * time.Second

// Tea messages.
type tickMsg time.Time
type refreshTickMsg time.Time
type sessionsLoadedMsg []session.SessionSummary
type pipelineLoadedMsg *apiclient.PipelineView
type snapshotLoadedMsg struct {
	id     string
	events []pipeline.SessionEvent
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

	ctx    context.Context
	cancel context.CancelFunc

	// parentCtx is the un-cancelled root ctx the picker was constructed
	// with. Used to derive a fresh m.ctx / m.cancel when the user backs
	// out of a session view to switch pods. Nil in bypass mode (no picker)
	// — Esc-from-Sessions is a no-op there.
	parentCtx context.Context

	// Data caches.
	sessions []session.SessionSummary
	events   map[string][]pipeline.SessionEvent // sessionID → ring buffer
	eventCt  uint64                             // monotonic counter
	lastTick time.Time
	lastCt   uint64
	rate     float64
	drops    uint64

	// Connection status.
	connState connStateInfo

	// UI state.
	pane         paneID
	selectedSess string
	filter       string
	filtering    bool
	paused       bool
	// hideInactive toggles whether passthrough / skip-only messages are
	// hidden from the events table. False (default) shows every message —
	// the operator asked to see all network traffic, processed or not.
	// Toggle with `s` to focus on plugin activity (deny/modify/allow/
	// observe). hiddenInactive is the count from the most recent
	// rebuildEventsTable, surfaced in the footer so a filtered timeline
	// doesn't read as data loss.
	hideInactive   bool
	hiddenInactive int
	flash          string
	flashUntil     time.Time
	width, height  int
	// bodyHeight is the inner height available to panes (terminal height
	// minus title + footer). Cached by layout() so rebuildEventsTable can
	// size the events table after accounting for the IDENTITY banner.
	bodyHeight int

	// Panel components.
	sessionsTbl table.Model
	eventsTbl   table.Model
	pipelineTbl table.Model
	catalogTbl  table.Model
	detailVp    viewport.Model
	detailEvent *pipeline.SessionEvent
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

	// pipeline is the fetched plugin composition. nil until the initial
	// GetPipeline response arrives; the pipeline pane shows "(loading…)"
	// until then.
	pipeline *apiclient.PipelineView

	// catalog is the registered-plugin catalog from /v1/plugins,
	// fetched lazily when the user first opens the catalog pane via
	// `P`. Cached for the session; `r` from the catalog pane refreshes.
	// nil before the first fetch; the catalog pane shows "(loading…)".
	catalog *apiclient.PluginCatalog
	// previousPane lets `Esc` from the catalog pane return to whichever
	// pane the user came from instead of always defaulting to one.
	previousPane paneID

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
}

// New returns a fresh model pointed at the given client. ctx governs both
// the HTTP calls and the SSE goroutine; cancelling it shuts everything down.
func New(ctx context.Context, c *apiclient.Client) tea.Model {
	ctx, cancel := context.WithCancel(ctx)

	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Prompt = "/ "

	return &model{
		endpoint:     c.Endpoint(),
		client:       c,
		ctx:          ctx,
		cancel:       cancel,
		events:       make(map[string][]pipeline.SessionEvent),
		pane:         paneSessions,
		sessionsTbl:  newSessionsTable(),
		eventsTbl:    newEventsTable(),
		pipelineTbl:  newPipelineTable(),
		catalogTbl:   newCatalogTable(),
		previousPane: paneNone,
		detailVp:     viewport.New(0, 0),
		filterInput:  ti,
		lastTick:     time.Now(),
		connState:    connStateInfo{phase: connConnecting},
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
	)
}

// backToPodsPane returns the picker to the Pods pane, tearing down the
// current session view (SSE pump, ticks) and port-forward. The pod list
// is preserved so the user picks a different pod immediately. A fresh
// ctx / cancel is derived from m.parentCtx so the next session-view
// entry has a usable context.
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

	// Reset session-view state so the next pod starts fresh.
	m.client = nil
	m.streamCh = nil
	m.sessions = nil
	m.events = make(map[string][]pipeline.SessionEvent)
	m.eventCt = 0
	m.lastCt = 0
	m.rate = 0
	m.drops = 0
	m.pipeline = nil
	// Drop the cached /v1/plugins snapshot too — a different pod is a
	// different framework instance with potentially different plugin
	// versions registered. The next `P` press refetches.
	m.catalog = nil
	m.catalogTbl.SetRows(nil)
	m.previousPane = paneNone
	m.detailEvent = nil
	m.detailPlugin = nil
	m.selectedSess = ""
	m.filter = ""
	m.filtering = false
	m.visibleRows = nil
	m.connState = connStateInfo{phase: connConnecting}

	// Re-derive ctx for the next session view.
	m.ctx, m.cancel = context.WithCancel(m.parentCtx)

	m.pane = panePods
}

// Init fires the initial fetch + starts the SSE pump and the tick.
// In picker mode (paneNamespaces), it loads the agent list instead.
func (m *model) Init() tea.Cmd {
	if m.pane == paneNamespaces {
		// Picker mode — load agents, then idle until user picks a pod.
		m.loading = true
		return loadAgentsCmd(m.ctx, m.lister)
	}
	return m.initSessionView()
}

// loadPipelineCmd fetches /v1/pipeline once at startup. The pipeline is
// static for the duration of a process so there's no periodic refresh.
func (m *model) loadPipelineCmd() tea.Cmd {
	return func() tea.Msg {
		pv, err := m.client.GetPipeline(m.ctx)
		if err != nil {
			return errMsg{where: "get pipeline", err: err}
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
		return snapshotLoadedMsg{id: id, events: view.Events}
	}
}

// Update handles every message + dispatches the next Cmd.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
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
		// Server list is authoritative. Reconcile: drop cached events for
		// sessions the server no longer knows about (typically the
		// bootstrap "default" bucket after rekey). If the focused session
		// disappeared, back out to the sessions pane so the user isn't
		// stranded on an empty events view.
		serverIDs := make(map[string]bool, len(msg))
		for _, s := range msg {
			serverIDs[s.ID] = true
		}
		for id := range m.events {
			if !serverIDs[id] {
				delete(m.events, id)
			}
		}
		if m.selectedSess != "" && !serverIDs[m.selectedSess] && m.pane != paneSessions {
			m.selectedSess = ""
			m.pane = paneSessions
		}
		m.sessions = []session.SessionSummary(msg)
		m.connState.phase = connOpen
		m.rebuildSessionsTable()
		if m.pane == paneEvents {
			m.rebuildEventsTable()
		}
		return m, nil

	case refreshTickMsg:
		// In picker mode, skip the fetch — m.client may be nil after a
		// back-out. Keep the ticker alive so it's ready when the user
		// re-enters a session.
		if m.pane == paneNamespaces || m.pane == panePods {
			return m, refreshTickCmd()
		}
		return m, tea.Batch(m.loadSessionsCmd(), refreshTickCmd())

	case pipelineLoadedMsg:
		m.pipeline = (*apiclient.PipelineView)(msg)
		m.rebuildPipelineTable()
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
		// Only update if we're still focused on this session.
		m.events[msg.id] = trim(msg.events, maxEventsPerSession)
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

	case errMsg:
		// Per-request failures (snapshot/pipeline) shouldn't flip the whole
		// connection into a terminal failed state — the stream can still be
		// healthy. Flash the error and leave connState alone. Only failures
		// from the initial sessions list (which runs before the stream opens)
		// mark the connection as failed so the user sees why nothing is
		// appearing.
		if strings.HasPrefix(msg.where, "snapshot") || msg.where == "get pipeline" {
			m.setFlash(msg.where + " failed: " + msg.err.Error())
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
		return m, withGen(m.editState.generation, edit.PollCmd(m.ctx, m.statusURL, msg.ApplyTime))

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
			// In-pod reload didn't take. The running pipeline is still
			// the previous one; reconcile the ConfigMap back to match.
			//
			// Caveat: the rollback bytes come from m.editState.fetched —
			// captured at this edit's Fetch time. With Apply's
			// --force-conflicts=true and field-manager=abctl, if a third
			// party (operator reconcile, kubectl edit, kustomize) modified
			// the ConfigMap between our forward apply and this rollback,
			// our rollback silently reverts their change too. This is not
			// a true undo. The framework's running pipeline is unaffected
			// (build failure → keeps the previous in-memory pipeline), but
			// the on-disk ConfigMap can lose third-party state. Surfacing
			// a "third-party change detected" path is a future option.
			reason := msg.Result.LastError
			if msg.Result.Status == edit.PollTimeout {
				reason = "reload not observed in 120s"
			}
			origManifest, mErr := edit.BuildManifest(
				m.editState.fetched.ConfigMapYAML,
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
			return m, withGen(m.editState.generation, edit.RollbackCmd(m.ctx, m.editRunner, origManifest, reason))
		}
		return m, nil

	case genRolledBackMsg:
		if msg.gen != m.editState.generation ||
			(m.editState.phase != editPhaseRollback && m.editState.phase != editPhaseBackground) {
			return m, nil
		}
		bg := m.editState.phase == editPhaseBackground
		if bg {
			if msg.Err != nil {
				m.setFlash("hot-reload failed: " + msg.ReloadErr +
					"; rollback failed: " + msg.Err.Error())
			} else {
				m.setFlash("hot-reload failed: " + msg.ReloadErr +
					"; rolled back to previous ConfigMap")
			}
			m.editState = editState{phase: editPhaseDone}
			return m, nil
		}
		m.editState.phase = editPhaseError
		if msg.Err != nil {
			m.editState.err = "reload failed: " + msg.ReloadErr +
				"\nrollback also failed: " + msg.Err.Error() +
				"\nConfigMap and running pipeline are out of sync; check kubectl"
			return m, nil
		}
		m.editState.err = "reload failed: " + msg.ReloadErr +
			"\nrolled back to previous ConfigMap"
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
	buf := m.events[e.SessionID]
	buf = append(buf, e)
	if len(buf) > maxEventsPerSession {
		buf = buf[len(buf)-maxEventsPerSession:]
	}
	m.events[e.SessionID] = buf

	// Bump updatedAt on the session summary if we already have it.
	for i := range m.sessions {
		if m.sessions[i].ID == e.SessionID {
			m.sessions[i].UpdatedAt = e.At
			m.sessions[i].EventCount = len(buf)
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
		m.rebuildEventsTable()
	}
}

// View composes the full screen.
func (m *model) View() string {
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
					"Use `abctl --endpoint http://...` to connect to a session API directly.")
		} else {
			body = m.namespacesTbl.View()
		}
		footer := "[↑↓/jk] nav  [↵] open  [r] reload  [q] quit"
		if m.pickerErr != "" {
			footer = "error: " + m.pickerErr + "    " + footer
		}
		return lipgloss.JoinVertical(lipgloss.Left,
			styleTitle.Render(title),
			body,
			styleHint.Render(footer),
		)
	}
	if m.pane == panePods {
		title := "abctl · " + m.selectedNamespace + " · pick pod"
		body := m.podsTbl.View()
		footer := "[↑↓/jk] nav  [↵] connect  [Esc] back  [r] reload  [q] quit"
		if m.pickerErr != "" {
			footer = "error: " + m.pickerErr + "    " + footer
		}
		return lipgloss.JoinVertical(lipgloss.Left,
			styleTitle.Render(title),
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
		title = fmt.Sprintf("abctl · %s · %s", m.endpoint, viewTabs(paneSessions))
		body = m.sessionsTbl.View()
	case paneEvents:
		title = fmt.Sprintf("abctl · %s", trunc(m.selectedSess, 36))
		body = m.eventsTbl.View()
		if banner := identityBanner(m.events[m.selectedSess]); banner != "" {
			body = banner + "\n" + body
		}
	case paneDetail:
		title = fmt.Sprintf("abctl · %s · event", trunc(m.selectedSess, 24))
		body = m.detailVp.View()
	case panePipeline:
		title = fmt.Sprintf("abctl · %s · %s", m.endpoint, viewTabs(panePipeline))
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
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		body,
		m.footerView(),
	)
}

// viewTabs renders the top-level tab strip "[Sessions] Pipeline" with the
// active pane bracketed. Rendered in the title bar of top-level views.
func viewTabs(active paneID) string {
	sess := "Sessions"
	pipe := "Pipeline"
	if active == paneSessions {
		sess = styleTitle.Render("[" + sess + "]")
		pipe = styleHint.Render(pipe)
	} else {
		sess = styleHint.Render(sess)
		pipe = styleTitle.Render("[" + pipe + "]")
	}
	return sess + " " + pipe
}

// trunc clips a string to n runes with an ellipsis. Used for title truncation.
// truncStr in events_pane.go is the byte-indexed variant for fixed-width
// ASCII table cells — if that ever needs to handle multi-byte input, merge
// the two into a single rune-aware helper.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// yankEventToFile writes the currently-focused event to a fresh tmpfile
// as pretty JSON and returns the path. Uses os.CreateTemp so the file is
// created with 0600 perms (session events carry identity subjects, raw
// LLM completions, and tool arguments — the operator-only default keeps
// them off shared / CI hosts).
func yankEventToFile(e *pipeline.SessionEvent) (string, error) {
	ts := time.Now().UTC().Format("20060102-150405")
	f, err := os.CreateTemp(os.TempDir(), "abctl-event-"+ts+"-*.json")
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

// trim bounds a slice to the last n elements (drops oldest on overflow).
// Used when a snapshot arrives with more events than the TUI caps.
func trim[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	out := make([]T, n)
	copy(out, s[len(s)-n:])
	return out
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
