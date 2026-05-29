package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/apiclient"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
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
)

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
	// showSkips toggles whether Action=skip rows render in the events
	// table. False (default) hides them — most operators care about
	// allow/deny/modify/observe events. Toggle with `s`. hiddenSkips
	// is the count from the most recent rebuildEventsTable, surfaced
	// in the footer so a sparse-looking timeline doesn't read as
	// data loss.
	showSkips     bool
	hiddenSkips   int
	flash         string
	flashUntil    time.Time
	width, height int
	// bodyHeight is the inner height available to panes (terminal height
	// minus title + footer). Cached by layout() so rebuildEventsTable can
	// size the events table after accounting for the IDENTITY banner.
	bodyHeight int

	// Panel components.
	sessionsTbl  table.Model
	eventsTbl    table.Model
	pipelineTbl  table.Model
	detailVp     viewport.Model
	detailEvent  *pipeline.SessionEvent
	detailPlugin *apiclient.PipelinePlugin
	filterInput  textinput.Model

	// visibleRows holds the invocationRow spec for each rendered row in
	// eventsTbl. Populated by rebuildEventsTable so selectedEvent can
	// return the (event, invocation) tuple the cursor is on without
	// re-walking the cache. Reset on every rebuild.
	visibleRows []invocationRow

	// pipeline is the fetched plugin composition. nil until the initial
	// GetPipeline response arrives; the pipeline pane shows "(loading…)"
	// until then.
	pipeline *apiclient.PipelineView

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
}

// New returns a fresh model pointed at the given client. ctx governs both
// the HTTP calls and the SSE goroutine; cancelling it shuts everything down.
func New(ctx context.Context, c *apiclient.Client) tea.Model {
	ctx, cancel := context.WithCancel(ctx)

	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Prompt = "/ "

	return &model{
		endpoint:    c.Endpoint(),
		client:      c,
		ctx:         ctx,
		cancel:      cancel,
		events:      make(map[string][]pipeline.SessionEvent),
		pane:        paneSessions,
		sessionsTbl: newSessionsTable(),
		eventsTbl:   newEventsTable(),
		pipelineTbl: newPipelineTable(),
		detailVp:    viewport.New(0, 0),
		filterInput: ti,
		lastTick:    time.Now(),
		connState:   connStateInfo{phase: connConnecting},
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
		m.client = apiclient.New(m.endpoint)
		m.pane = paneSessions
		return m, m.initSessionView()

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
