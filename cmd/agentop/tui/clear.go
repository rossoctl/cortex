package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// eraseTarget is what a confirmation erases.
type eraseTarget int

const (
	// eraseAll is X: every session, by DELETE /v1/sessions.
	eraseAll eraseTarget = iota
	// eraseSession is x on the sessions pane: the highlighted session.
	eraseSession
	// eraseAgent is x on the agents pane: every session of the highlighted agent.
	eraseAgent
)

// clearConfirm is X's and x's confirmation, modal while the model holds one. It asks before
// anything is erased, and says what will be: the counts come from ?archived=true, the proxy's own
// list of everything it holds, rather than from what agentop happens to have cached.
type clearConfirm struct {
	target eraseTarget
	// id is eraseSession's session, agent eraseAgent's label.
	id, agent string
	counted   bool
	list      apiclient.SessionList
	countErr  error
	// ids are what a per-session target erases, taken from list when it lands — exactly the
	// sessions the dialog counted. Empty for eraseAll, which erases by one request.
	ids []string
	// row and name are eraseSession's session as that list describes it and as the sessions
	// table names it.
	row  session.SessionSummary
	name string
	// clearing is y pressed, with the request in flight.
	clearing bool
}

type clearCountsMsg struct {
	// cc is the confirmation this list was fetched for.
	cc   *clearConfirm
	list apiclient.SessionList
	err  error
}

type clearDoneMsg struct {
	res apiclient.ClearResult
	err error
}

// openClearConfirm is X: put the confirmation up and count what it would erase.
func (m *model) openClearConfirm() tea.Cmd {
	return m.openEraseConfirm(&clearConfirm{target: eraseAll})
}

// openSessionErase is x on the sessions pane: erase the highlighted session.
func (m *model) openSessionErase() tea.Cmd {
	id := m.selectedSessionID()
	if id == "" {
		return nil
	}
	return m.openEraseConfirm(&clearConfirm{target: eraseSession, id: id})
}

// openAgentErase is x on the agents pane: erase every session of the highlighted agent — or, on
// All agents, everything, which is X's confirmation and X's single request.
func (m *model) openAgentErase() tea.Cmd {
	scope, ok := m.selectedAgentScope()
	if !ok {
		return nil
	}
	if scope == "" {
		return m.openClearConfirm()
	}
	return m.openEraseConfirm(&clearConfirm{target: eraseAgent, agent: scope})
}

// openEraseConfirm puts cc up and fetches the list it counts from.
func (m *model) openEraseConfirm(cc *clearConfirm) tea.Cmd {
	m.clearConfirm = cc
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		list, err := c.ListSessionsArchived(ctx)
		return clearCountsMsg{cc: cc, list: list, err: err}
	}
}

// clearConfirmKey owns the keyboard while the confirmation is up. y erases; n, N, esc, q and
// ctrl+c keep everything — cancelling is never the dangerous direction, so every key that reads
// as "get me out" does it. Every other key is swallowed, so nothing reaches the pane beneath.
func (m *model) clearConfirmKey(msg tea.KeyMsg) tea.Cmd {
	cc := m.clearConfirm
	if cc.clearing {
		// Already sent. esc only hides the dialog; the answer still lands as a flash.
		if msg.String() == "esc" {
			m.clearConfirm = nil
		}
		return nil
	}
	switch msg.String() {
	case "y", "Y":
		// A per-session target with nothing counted has nothing to erase: only n and esc apply.
		if cc.target != eraseAll && len(cc.ids) == 0 {
			return nil
		}
		cc.clearing = true
		c, ctx := m.client, m.ctx
		if cc.target == eraseAll {
			return func() tea.Msg {
				res, err := c.ClearSessions(ctx)
				return clearDoneMsg{res: res, err: err}
			}
		}
		// The buckets folded into these sessions go with them: the operator saw those calls
		// inside the session, and they would otherwise come back as rows of their own.
		ids, name, agent := m.withFolded(cc.ids), `session "`+cc.name+`"`, cc.target == eraseAgent
		if agent {
			name = "of " + sanitizeLabel(cc.agent) + "'s sessions"
		}
		return func() tea.Msg {
			msg := eraseSessions(ctx, c, ids)
			msg.name, msg.agent = name, agent
			return msg
		}
	case "n", "N", "esc", "q", "ctrl+c":
		m.clearConfirm = nil
	}
	return nil
}

// applyClearCounts fills in the confirmation's figures, unless it was dismissed meanwhile, and for
// a per-session target picks the ids it will erase out of the same list.
func (m *model) applyClearCounts(msg clearCountsMsg) {
	cc := m.clearConfirm
	// A count fetched for a dialog that has since closed must not fill the next one.
	if cc == nil || cc != msg.cc {
		return
	}
	cc.counted, cc.list, cc.countErr = true, msg.list, msg.err
	if msg.err != nil {
		return
	}
	for _, s := range msg.list.Sessions {
		switch {
		case cc.target == eraseSession && s.ID == cc.id:
			cc.ids, cc.row = []string{s.ID}, s
			if cc.name = m.sessionTitleFor(s.ID, s.Title); cc.name == "" {
				cc.name = sanitizeLabel(s.ID)
			}
		case cc.target == eraseAgent && m.sessionListed(s, cc.agent):
			cc.ids = append(cc.ids, s.ID)
		}
	}
}

// applyClearDone reports the clear, and on success drops what agentop held about the sessions it
// erased. Applied whether or not the confirmation is still up: esc while clearing only hid it.
func (m *model) applyClearDone(msg clearDoneMsg) tea.Cmd {
	m.clearConfirm = nil
	if msg.err != nil {
		if errors.Is(msg.err, apiclient.ErrClearUnsupported) {
			m.setFlash(msg.err.Error())
		} else {
			m.setFlash("nothing cleared: " + msg.err.Error())
		}
		return nil
	}
	m.resetAfterClear()
	r := msg.res
	switch {
	case r.ArchiveError != "":
		m.setFlash(fmt.Sprintf("cleared %d sessions from memory, but not from disk: %s", r.Sessions, r.ArchiveError))
	case r.ArchivedSessions > 0 || r.Bytes > 0:
		m.setFlash(fmt.Sprintf("cleared %d sessions, and %d (%s) from disk; the cost ledger is kept",
			r.Sessions, r.ArchivedSessions, formatBytes(r.Bytes)))
	default:
		m.setFlash(fmt.Sprintf("cleared %d sessions; the cost ledger is kept", r.Sessions))
	}
	if m.pane == paneAgents {
		return tea.Batch(m.loadSessionsCmd(), m.fetchAgentRowsCmd(agentsOpenNever, paneNone))
	}
	return m.loadSessionsCmd()
}

// erasedMsg is a per-session erase finished.
type erasedMsg struct {
	// name is what the flash calls what was erased: `session "<name>"`, or `of <agent>'s
	// sessions`; agent says it was a whole agent's.
	name  string
	agent bool
	asked int
	// gone are the ids erased whole or already gone. memOnly are the ids erased from memory but not
	// from disk (a 500), and diskErr the first archiveError saying why. res sums what both took.
	gone    []string
	memOnly []string
	diskErr string
	res     apiclient.ClearResult
	// err is the first outright failure, nil when every id was erased.
	err error
}

// eraseSessions sends DELETE /v1/sessions/{id} for each id in turn. A 404 counts as gone: the
// session is not there. A 500 with sessions > 0 erased memory but not disk: that is an erase, from
// memory only, and its archiveError is kept to say why disk was not; with sessions 0 it erased
// nothing, and is a failure. A refusal or a proxy that predates the route stops the run, since
// every remaining id would get the same answer; anything else is a failure, the first one kept,
// and the run carries on.
func eraseSessions(ctx context.Context, c *apiclient.Client, ids []string) erasedMsg {
	out := erasedMsg{asked: len(ids)}
	fail := func(err error) {
		if out.err == nil {
			out.err = err
		}
	}
	for _, id := range ids {
		res, err := c.DeleteSession(ctx, id)
		switch {
		case errors.Is(err, apiclient.ErrSessionGone):
			out.gone = append(out.gone, id)
		case errors.Is(err, apiclient.ErrClearRefused), errors.Is(err, apiclient.ErrDeleteUnsupported):
			fail(err)
			return out
		case err != nil:
			fail(err)
		case res.ArchiveError != "" && res.Sessions == 0:
			fail(errors.New(res.ArchiveError))
		case res.ArchiveError != "":
			out.memOnly = append(out.memOnly, id)
			if out.diskErr == "" {
				out.diskErr = res.ArchiveError
			}
		default:
			out.gone = append(out.gone, id)
		}
		// res is zero on every error.
		out.res.Sessions += res.Sessions
		out.res.ArchivedSessions += res.ArchivedSessions
		out.res.Bytes += res.Bytes
	}
	return out
}

// erasedFlash is a per-session erase's result line. It counts erased whole and erased from memory
// only together as erased, then says how many of those disk kept and how many failed outright.
func erasedFlash(msg erasedMsg) string {
	mem := len(msg.memOnly)
	erased := len(msg.gone) + mem
	failed := msg.asked - erased
	if erased == 0 && msg.err != nil {
		if errors.Is(msg.err, apiclient.ErrDeleteUnsupported) {
			return msg.err.Error()
		}
		return "nothing erased: " + msg.err.Error()
	}
	head := fmt.Sprintf("erased %d of %d %s", erased, msg.asked, msg.name)
	if failed == 0 {
		head = "erased " + msg.name
		if msg.agent {
			head = fmt.Sprintf("erased all %d %s", erased, msg.name)
		}
	}
	switch {
	case mem > 0 && mem == erased:
		head += " from memory, but not from disk: " + msg.diskErr
	case mem > 0:
		head += fmt.Sprintf("; %d from memory only: %s", mem, msg.diskErr)
	case failed == 0:
		disk := ""
		if msg.res.Bytes > 0 {
			disk = fmt.Sprintf(" (%s from disk)", formatBytes(msg.res.Bytes))
		}
		return head + disk + "; the cost ledger is kept"
	}
	if failed > 0 {
		head += fmt.Sprintf("; %d failed: %v", failed, msg.err)
	}
	return head
}

// applyErased reports a per-session erase and forgets what agentop held about each session it
// erased — from memory only too. Memory dropped that one, so the proxy soon stops listing it, and
// events cached here would keep it as a cached-only row x could no longer erase; if its history
// is still on disk, the list brings the row back and its events reload from there. Applied
// whether or not the confirmation is still up: esc while erasing only hid it.
func (m *model) applyErased(msg erasedMsg) tea.Cmd {
	m.clearConfirm = nil
	m.forgetSessions(slices.Concat(msg.gone, msg.memOnly))
	m.setFlash(erasedFlash(msg))
	if m.pane == paneAgents {
		return tea.Batch(m.loadSessionsCmd(), m.fetchAgentRowsCmd(agentsOpenNever, paneNone))
	}
	return m.loadSessionsCmd()
}

// forgetSessions drops what agentop holds about each erased session — its cached events, paging
// and fetch state, and the selection and detail view where they point at it — and marks it erased
// at a new generation, so a snapshot asked for before the erase is not stored after it. The
// sessions table keeps the cursor's index, which is now the row after the erased one.
//
// DROPPING THE CACHED EVENTS IS LOAD-BEARING: a session the server no longer lists, with events
// cached here, is shown as a cached-only row (cachedOnlySessionIDs).
func (m *model) forgetSessions(ids []string) {
	if len(ids) == 0 {
		return
	}
	m.eraseGen++
	if m.erasedAt == nil {
		m.erasedAt = map[string]uint64{}
	}
	for _, id := range ids {
		m.erasedAt[id] = m.eraseGen
		delete(m.events, id)
		delete(m.fullFetched, id)
		delete(m.olderNotFetched, id)
		delete(m.paging, id)
		delete(m.contextRun, id)
		if m.selectedSess == id {
			m.selectedSess = ""
			m.selectedEventKey = eventKey{}
			m.detailEvent = nil
			m.detailPlugin = nil
		}
	}
	m.sessions = slices.DeleteFunc(m.sessions, func(s session.SessionSummary) bool {
		return slices.Contains(ids, s.ID)
	})
	at := m.sessionsTbl.Cursor()
	m.rebuildSessionsTable()
	setCursorVisible(&m.sessionsTbl, at)
}

// resetAfterClear drops everything agentop holds that describes a session, and keeps the
// connection, the operator's choices and the spend strip — whose figures the ledger keeps.
// It lands on Sessions unless the reader is on Agents, which shows no erased session and stays.
//
// backToPodsPane is the checklist, because it resets the same things for a different reason; the
// half kept here is everything that describes the connection rather than the sessions on it.
// clearedAt is what keeps a snapshot answered before the clear from bringing a session back: see
// snapshotCmd.
func (m *model) resetAfterClear() {
	m.eraseGen++
	m.clearedAt, m.erasedAt = m.eraseGen, nil
	m.sessions = nil
	m.events = make(map[string][]pipeline.SessionEvent)
	m.fullFetched = nil
	m.contextRun = nil
	m.olderNotFetched = nil
	m.paging = nil
	m.usage.snap = nil
	m.usage.err = nil
	m.usage.lastFetch = time.Time{}
	m.usage.reqSeq++
	m.untitledMisses = 0
	m.untitledCounted = nil
	m.detailEvent = nil
	m.detailPlugin = nil
	m.selectedSess = ""
	m.selectedEventKey = eventKey{}
	m.visibleRows = nil
	if m.pane != paneAgents {
		m.pane = paneSessions
	}
	m.rebuildSessionsTable()
}

// confirmText is the confirmation's words, unwrapped, and its key hint.
func confirmText(cc *clearConfirm) (text, hint string) {
	var b strings.Builder
	hint = "[y] erase  [n] keep"
	switch {
	case !cc.counted:
		b.WriteString("Counting this Cortex's sessions…")
	case cc.target == eraseAll:
		if cc.countErr != nil {
			b.WriteString("Could not count this Cortex's sessions: " + cc.countErr.Error() + "\n\n")
			b.WriteString("Erase every session from this Cortex, in memory and on disk?")
		} else if a := cc.list.Archive; a != nil {
			fmt.Fprintf(&b, "Erase all %d sessions from this Cortex, in memory and on disk (%s)?",
				len(cc.list.Sessions), formatBytes(a.Bytes))
		} else {
			fmt.Fprintf(&b, "Erase all %d sessions from this Cortex? It keeps no history on disk.",
				len(cc.list.Sessions))
		}
		b.WriteString("\n\nThe cost ledger is kept. This cannot be undone.")
	case cc.countErr != nil:
		b.WriteString("Could not count this Cortex's sessions: " + cc.countErr.Error())
		hint = "[n] close"
	case len(cc.ids) == 0:
		if cc.target == eraseSession {
			b.WriteString("This Cortex holds no such session.")
		} else {
			fmt.Fprintf(&b, "%s has no sessions on this Cortex.", sanitizeLabel(cc.agent))
		}
		hint = "[n] close"
	default:
		what, them := "all "+formatCount(len(cc.ids))+" of "+sanitizeLabel(cc.agent)+"'s sessions", "one of these sessions"
		if cc.target == eraseSession {
			what, them = `session "`+cc.name+`"`+sessionFigures(cc.row), "this session"
		}
		if cc.list.Archive != nil {
			b.WriteString("Erase " + what + " from this Cortex, in memory and on disk?")
		} else {
			b.WriteString("Erase " + what + " from this Cortex? It keeps no history on disk.")
		}
		fmt.Fprintf(&b, "\n\nThe cost ledger is kept. An agent still using %s brings it back with its "+
			"next request, and the conversation with it, since every request re-sends it. This cannot be undone.", them)
	}
	if cc.clearing {
		hint = "erasing…"
	}
	return b.String(), hint
}

// sessionFigures is " (claude-code, 412 events)", or " (1 event)" for a session naming no agent.
func sessionFigures(s session.SessionSummary) string {
	unit := "events"
	if s.EventCount == 1 {
		unit = "event"
	}
	figs := formatCount(s.EventCount) + " " + unit
	if s.Agent != "" {
		figs = sanitizeLabel(s.Agent) + ", " + figs
	}
	return " (" + figs + ")"
}

// renderClearConfirm draws the confirmation. Returns the panel only; see overlayCenter.
func renderClearConfirm(cc *clearConfirm, width int) string {
	const wrap = 60
	w := min(wrap, max(width-6, 20))
	text, hint := confirmText(cc)
	body := lipgloss.NewStyle().Width(w).Render(text) + "\n\n" + styleHint.Render(hint)
	box := styleBorder.Padding(0, 2)
	if width > styleBorder.GetHorizontalBorderSize() {
		box = box.MaxWidth(width)
	}
	return box.Render(body)
}
