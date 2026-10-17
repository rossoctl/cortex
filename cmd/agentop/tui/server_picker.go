package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
)

// serverPicker is S's picker, modal while the model holds one: where agent's new sessions go.
type serverPicker struct {
	agent string
	// session is the id of the session this picker moves, "" when it picks the
	// agent's new sessions instead. A session move is S on a sessions-pane row.
	session string
	// sessionTokens is the session's prompt context, 0 when nothing can be said:
	// what the confirmation line below the entries shows, since the first turn at
	// the new server pays it uncached.
	sessionTokens int
	// entries[0] is the agent's own choice (not routed); the rest are the servers by name.
	entries []serverChoice
	// cursor is the highlighted entry. It opens on the one in force, and nothing here remembers
	// which that was: the route can change while the picker is up, so ↵ is not measured against
	// it (applyServerChoice).
	cursor int
}

// serverChoice is one picker entry. name is "" for the agent's own choice.
type serverChoice struct {
	name, host, mapping string
}

// serverSwitch is a write S started, until its result lands. session is the id of
// the session being moved, "" when the write routes the agent's new sessions.
type serverSwitch struct {
	agent, server, session string
}

// serverSwitchedMsg is the result of the write S started. statsURL is where it polled the
// proxy's reload, for a flash that has to point there and for the refetch to check it is still
// what is on screen; staying is sessionsStaying's count as ↵ found it, and inForce whether the
// router as ↵ found it already sent agent's new sessions to server.
//
// SETTLED AT ↵, NOT WHEN IT LANDS: the write can take up to edit.LocalPollDeadline, and a reader
// who leaves for another connection meanwhile would otherwise have that connection's sessions
// counted, and its router read, as this machine's.
type serverSwitchedMsg struct {
	agent, server, statsURL string
	// session is the id of the session that was moved, "" when the write routed
	// the agent's new sessions; note is the move's cost line, "" when the write
	// was not a move or nothing could be said about it.
	session string
	note    string
	staying int
	inForce bool
	res     edit.WriteResult
	err     error
}

// serverKeyRefusal is why S on the row labelled label opens nothing, or "" when it may open.
// Row-specific reasons first, so the message is about what the reader is pointing at.
func (m *model) serverKeyRefusal(label string) string {
	switch {
	case label == "":
		return "routing is chosen per agent: highlight an agent's row, then press S"
	case label == otherAgents:
		return "Other pools every client Cortex does not recognise; route one by name with agentop server use <server> --agent <agent>"
	}
	// The router's own rule, so S cannot write an agent name the proxy would refuse to reload. It
	// is what turns the no-User-Agent row away: its requests belong to no one agent, so it cannot
	// be routed, and its SERVER cell is blank for the same reason (agentServerCell).
	if err := routerconfig.CheckAgent(pipeline.AgentName(label)); err != nil {
		return err.Error()
	}
	if _, _, ok := m.localCortexTarget(); !ok {
		return "S changes this machine's Cortex, and agentop is not attached to it — or its stats server did not answer at startup"
	}
	// THE ONE PLACE A ROUTER THAT CANNOT TAKE A CHOICE IS REFUSED, and serverKeyOffered asks the
	// same routerStatus, through activeRouter, so the footer never offers S where this refuses it.
	// Absent (or off, which /v1/pipeline cannot tell from absent), under on_error: observe — in the
	// words `agentop server` uses — or a config agentop cannot read.
	if _, why := m.routerStatus(); why != "" {
		return why
	}
	if m.serverSwitch != nil {
		if m.serverSwitch.session != "" {
			return "a move of session " + sanitizeLabel(m.serverSwitch.session) + " is still in progress"
		}
		return "a server switch for " + m.serverSwitch.agent + " is still in progress"
	}
	return ""
}

// sessionKeyRefusal is why S on the session id's row opens nothing, or "" when it
// may open. The router's own rule first — a synthetic id cannot be moved, and S
// never writes an entry the proxy would refuse to reload — then the same
// conditions the agent picker refuses on.
func (m *model) sessionKeyRefusal(id string) string {
	if err := routerconfig.CheckSessionID(id); err != nil {
		return err.Error()
	}
	if _, _, ok := m.localCortexTarget(); !ok {
		return "S changes this machine's Cortex, and agentop is not attached to it — or its stats server did not answer at startup"
	}
	if _, why := m.routerStatus(); why != "" {
		return why
	}
	if m.serverSwitch != nil {
		return "a server switch is still in progress"
	}
	return ""
}

// serverKeyOffered reports whether the footer advertises S: on the highlighted row, S would open.
// The same serverKeyRefusal, so the hint and the key cannot disagree — no [S] on All agents,
// Other or a row the router will not route, away from this machine's Cortex, without a router
// that routes, or while a switch is in flight, where S would only say why not. Per row, as the
// footer's [↵] is.
func (m *model) serverKeyOffered() bool {
	label, ok := m.selectedAgentScope()
	return ok && m.serverKeyRefusal(label) == ""
}

// openServerPicker is S on the AGENTS pane: the picker over the pane, its cursor on the value
// the agent has now — or, in its place, the panel saying why there is none (renderServerNotice).
func (m *model) openServerPicker() tea.Cmd {
	label, ok := m.selectedAgentScope()
	if !ok {
		return nil
	}
	if why := m.serverKeyRefusal(label); why != "" {
		m.serverNotice = why
		return nil
	}
	router, _ := m.activeRouter()
	p := &serverPicker{agent: pipeline.AgentName(label), entries: []serverChoice{{}}}
	for _, name := range slices.Sorted(maps.Keys(router.Servers)) {
		s := router.Servers[name]
		p.entries = append(p.entries, serverChoice{name: name, host: servers.Host(s), mapping: servers.Mapping(s)})
		if router.Agents[p.agent] == name {
			p.cursor = len(p.entries) - 1
		}
	}
	m.serverPicker = p
	return nil
}

// serverPickerKey owns the keyboard while the picker is up. ↵ applies the highlighted entry, as
// every agentop picker does; esc, q and ctrl+c close it writing nothing, as X's dialog does.
// Every other key is swallowed, S included: no picker key toggles.
func (m *model) serverPickerKey(msg tea.KeyMsg) tea.Cmd {
	p := m.serverPicker
	switch msg.String() {
	case "up", "k":
		p.cursor = max(p.cursor-1, 0)
	case "down", "j":
		p.cursor = min(p.cursor+1, len(p.entries)-1)
	case "home", "g":
		p.cursor = 0
	case "end", "G":
		p.cursor = len(p.entries) - 1
	case "enter":
		return m.applyServerChoice()
	case "esc", "q", "ctrl+c":
		m.serverPicker = nil
	}
	return nil
}

// applyServerChoice closes the picker and writes the highlighted entry: agents.<agent> set to
// the server, or removed for the agent's own choice.
//
// THE SAME WRITE AND THE SAME CHECK AS `agentop server use` AND `reset`: edit.WritePluginConfig
// with servers.Verify, so the TUI can write nothing the command would refuse. In the background,
// because it waits for the proxy's reload; the footer shows it until serverSwitchedMsg lands.
//
// NO SHORTCUT FOR THE ENTRY THE PICKER OPENED ON. The route can change while the picker is up —
// the pane's rows poll refetches /v1/pipeline, and a shell's `agentop server use` reaches the
// file before agentop's next fetch — so "the cursor is where it opened" said nothing about what
// ↵ would change, and once flashed "already go to ete" over a proxy routing to glm. The file
// decides: edit.WritePluginConfig writes and polls nothing when it already holds the choice
// (WriteUnchanged), and serverSwitchedText words that by inForce.
//
// context.Background(), not m.ctx: the write is bounded by edit.LocalPollDeadline, and cutting
// it off when the reader leaves the connection would report a timeout for a write that landed.
// openSessionServerPicker is S on the SESSIONS pane: the same picker over the
// highlighted session's row, its cursor on the sessions: entry the config holds
// for it — or, in its place, the panel saying why there is none. The entries are
// the same servers; only what ↵ writes differs.
func (m *model) openSessionServerPicker() tea.Cmd {
	id := m.selectedSessionID()
	if id == "" {
		return nil
	}
	if why := m.sessionKeyRefusal(id); why != "" {
		m.serverNotice = why
		return nil
	}
	var summary *session.SessionSummary
	for i := range m.sessions {
		if m.sessions[i].ID == id {
			summary = &m.sessions[i]
			break
		}
	}
	agent := "unknown"
	if summary != nil && summary.Agent != "" {
		agent = summary.Agent
	}
	router, _ := m.activeRouter()
	p := &serverPicker{agent: pipeline.AgentName(agent), session: id, entries: []serverChoice{{}}}
	if summary != nil && summary.PromptContext != nil {
		p.sessionTokens = summary.PromptContext.Tokens
	}
	// What is in force: the override's server when one names the session, else the
	// server the session's inference actually went to — so Enter on the opening is
	// a no-op, never an accidental unroute of a running session.
	inForce := router.Sessions[id]
	if inForce == "" && summary != nil {
		inForce, _ = servers.ForHost(router, summary.InferenceHost)
	}
	for _, name := range slices.Sorted(maps.Keys(router.Servers)) {
		s := router.Servers[name]
		p.entries = append(p.entries, serverChoice{name: name, host: servers.Host(s), mapping: servers.Mapping(s)})
		if name == inForce {
			p.cursor = len(p.entries) - 1
		}
	}
	m.serverPicker = p
	return nil
}

func (m *model) applyServerChoice() tea.Cmd {
	p := m.serverPicker
	m.serverPicker = nil
	choice := p.entries[p.cursor]
	if p.session != "" {
		// A session move: the user's explicit instruction, so it is not measured
		// against any route — only that the router and this machine's Cortex are
		// still there to take it.
		if why := m.sessionKeyRefusal(p.session); why != "" {
			m.serverNotice = why
			return nil
		}
		path, statsURL, _ := m.localCortexTarget()
		sid, server := p.session, choice.name
		note := ""
		if p.sessionTokens > 0 {
			to := server
			if to == "" {
				to = "its own provider"
			}
			note = fmt.Sprintf("%d tokens of its context go to %s uncached on its first turn there.", p.sessionTokens, to)
		}
		m.serverSwitch = &serverSwitch{agent: p.agent, server: server, session: sid}
		return func() tea.Msg {
			res, err := edit.WritePluginConfig(context.Background(), edit.ConfigWrite{
				Path:     path,
				StatsURL: statsURL,
				Changes:  servers.SessionChanges(sid, server),
				Verify:   servers.Verify,
			})
			return serverSwitchedMsg{agent: p.agent, server: server, statsURL: statsURL, session: sid, note: note,
				res: res, err: err}
		}
	}
	// ASKED AGAIN, NOT CARRIED OVER FROM THE OPENING: the pane's rows refetch the pipeline while
	// the picker is up, so the router may have stopped routing since — and then no write would be
	// true. First, so nothing shown claims a route.
	if why := m.serverKeyRefusal(p.agent); why != "" {
		m.serverNotice = why
		return nil
	}
	path, statsURL, _ := m.localCortexTarget()
	router, _ := m.activeRouter()
	agent, server := p.agent, choice.name
	m.serverSwitch = &serverSwitch{agent: agent, server: server}
	staying := m.sessionsStaying(agent, server)
	inForce := router.Agents[agent] == server
	return func() tea.Msg {
		res, err := edit.WritePluginConfig(context.Background(), edit.ConfigWrite{
			Path:     path,
			StatsURL: statsURL,
			Changes:  servers.AgentChanges(agent, server),
			Verify:   servers.Verify,
		})
		return serverSwitchedMsg{agent: agent, server: server, statsURL: statsURL, staying: staying, inForce: inForce,
			res: res, err: err}
	}
}

// applyServerSwitched reports a finished switch, and refetches the pipeline so the SERVER
// columns show what the proxy now runs. After every outcome, a refusal included: what the proxy
// runs is the one thing a report of an uncertain write cannot settle, and the column can. A
// fetch of its own even when one is in flight: that one may predate the reload.
//
// A FAILURE IN THE PANEL S'S REFUSALS USE, whole and wrapped: refused, unreachable, not put
// back, or stopped before the write (switchFailed). A refusal's line is its lead-in — what
// happened to the file — and then the proxy's whole error, and a real reloader error alone runs
// past what 80 columns leave it, so the line, cut from the left to keep the error's tail, lost
// "refused" and "put back" first. Only on the AGENTS pane, where the panel is drawn; landing
// anywhere else it is the sticky line, since a result the reader never sees is worse than one
// cut short.
//
// SUCCESS AND A TIMEOUT STICKY, AS A YANK IS: the footer's whole line until the next key, cut
// from the left where it must be, so the count and the URL to check — each at the end of its
// line — survive. As a timed flash it sat after thirty columns of connection state, cut from the
// right.
//
// ANY PANEL UP IS CLOSED FIRST: it was opened while the switch was in flight — S saying so — and
// the key that closed it would also have dismissed the sticky line under it.
//
// THE REFETCH ONLY WHILE THIS MACHINE'S CORTEX IS STILL ON SCREEN, compared by the stats URL the
// write polled: a reader who has left for a pod meanwhile would have that pod's pipeline fetched
// and painted as the reload's result.
func (m *model) applyServerSwitched(msg serverSwitchedMsg) tea.Cmd {
	m.serverSwitch = nil
	m.serverNotice = ""
	if text := serverSwitchedText(msg, msg.staying); switchFailed(msg) && m.pane == paneAgents {
		m.serverNotice = text
	} else {
		m.setStickyFlash(text)
	}
	if _, statsURL, ok := m.localCortexTarget(); !ok || statsURL != msg.statsURL {
		return nil
	}
	m.pipelineFetching = true
	return m.loadPipelineCmd()
}

// switchFailed reports whether a finished switch did not go through: an error, before the write
// or after it, or a reload the proxy refused or never reported because it stopped answering.
// Not a timeout, which wrote the file and only could not confirm it; nor WriteUnchanged.
func switchFailed(msg serverSwitchedMsg) bool {
	return msg.err != nil || msg.res.Outcome == edit.WriteReloadFailed || msg.res.Outcome == edit.WriteStatusUnreachable
}

// serverSwitchedText is the report of a finished switch: what happened to the config file and to
// the routing, in the terms `agentop server use` and `reset` use (runServerWrite). staying is
// sessionsStaying's count, said only when the proxy took the change.
//
// EVERY SERVED STRING SANITISED, the server's name and the errors alike: the name arrives over an
// unauthenticated API, and an error can quote what the proxy read. The agent needs none, being
// one routerconfig.CheckAgent passed.
//
// EACH OUTCOME SAYS WHERE THE FILE WAS LEFT, because edit.WritePluginConfig leaves it in three
// different places: put back after a refusal or a proxy that stopped answering, left as written
// after a timeout, and — the one error past the write — left as found when someone changed it
// meanwhile. A flash that said "written" after a put-back would send the reader looking for an
// edit that is not there.
//
// WHAT THE READER ACTS ON GOES LAST, because the footer cuts a line too long for it from the
// left (applyServerSwitched): the URL to check, the count — and, for a failure landing off the
// AGENTS pane, the proxy's error. The lead-ins are kept short so that, at 80 columns, what
// survives of them still says what happened.
func serverSwitchedText(msg serverSwitchedMsg, staying int) string {
	if msg.session != "" {
		return sessionSwitchedText(msg)
	}
	written := msg.res.Outcome == edit.WriteReloadFailed || msg.res.Outcome == edit.WriteStatusUnreachable
	switch {
	case msg.err != nil && written:
		// The proxy refused or went away, and the file could not be put back. The error says
		// which, and why the file was not restored, with the proxy's reason in it.
		return msg.agent + ": " + sanitizeLabel(msg.err.Error())
	case msg.err != nil:
		// From before the write: nothing was written, and the proxy was not touched.
		return msg.agent + "'s server is unchanged: " + sanitizeLabel(msg.err.Error())
	}
	switch msg.res.Outcome {
	case edit.WriteReloaded:
		return switchedText(msg.agent, msg.server, staying)
	case edit.WriteUnchanged:
		// The file already holds the choice, so nothing was written and nothing polled. Said as
		// "already go to" only when the router ↵ read routed that way too; otherwise it is the
		// FILE that holds it — a shell's `agentop server use` the last fetch predates, or a hand
		// edit the proxy has not taken — and the refetch that follows shows which.
		if msg.inForce {
			return alreadyRouted(msg.agent, msg.server)
		}
		if msg.server == "" {
			return "The config file already leaves " + msg.agent + " unrouted; nothing was written."
		}
		return "The config file already routes new " + msg.agent + " sessions to " + sanitizeLabel(msg.server) +
			"; nothing was written."
	case edit.WriteReloadFailed:
		return "refused: " + msg.agent + " stays where it was, and the config file was put back: " +
			sanitizeLabel(msg.res.ReloadError)
	case edit.WriteStatusUnreachable:
		// Not a refusal: nothing said no, and whether the proxy took the change before it went
		// away is unknown. So nothing here says where msg.agent's sessions go. Without ReloadError,
		// which here is only the poller's "unreachable" and its hint, both said already: as the
		// tail it would be all that survived at 80 columns, and the put-back would not.
		return "the change to " + msg.agent + " was not confirmed: the proxy stopped answering before it reported the reload, " +
			"so the config file was put back; check the proxy is running and try again"
	case edit.WriteReloadTimedOut:
		return fmt.Sprintf("wrote the change to %s, but the proxy reported no reload within %s; check %s/reload/status",
			msg.agent, edit.LocalPollDeadline, msg.statsURL)
	default:
		// WriteNotRunning: no stats URL to poll. S writes only with one, so this is the
		// command's wording kept for a caller that does not.
		return "wrote the change to " + msg.agent + "; no Cortex answered at its stats address, so it applies when the proxy next starts"
	}
}

// sessionSwitchedText is serverSwitchedText for a session move: the same
// outcomes, said of the session that was moved rather than the agent's next ones.
func sessionSwitchedText(msg serverSwitchedMsg) string {
	id, to := sanitizeLabel(msg.session), msg.server
	if to == "" {
		to = "its own provider"
	} else {
		to = sanitizeLabel(to)
	}
	written := msg.res.Outcome == edit.WriteReloadFailed || msg.res.Outcome == edit.WriteStatusUnreachable
	switch {
	case msg.err != nil && written:
		return id + ": " + sanitizeLabel(msg.err.Error())
	case msg.err != nil:
		return "session " + id + " is unchanged: " + sanitizeLabel(msg.err.Error())
	}
	switch msg.res.Outcome {
	case edit.WriteReloaded:
		if msg.note != "" {
			return "Session " + id + " → " + to + ". " + msg.note
		}
		return "Session " + id + " → " + to + "."
	case edit.WriteUnchanged:
		return "Session " + id + " already goes to " + to + "."
	case edit.WriteReloadFailed:
		return "refused: session " + id + " stays where it was, and the config file was put back: " +
			sanitizeLabel(msg.res.ReloadError)
	case edit.WriteStatusUnreachable:
		return "the move of " + id + " was not confirmed: the proxy stopped answering before it reported the reload, " +
			"so the config file was put back; check the proxy is running and try again"
	case edit.WriteReloadTimedOut:
		return fmt.Sprintf("wrote the move of %s, but the proxy reported no reload within %s; check %s/reload/status",
			id, edit.LocalPollDeadline, msg.statsURL)
	default:
		return "wrote the move of " + id + "; no Cortex answered at its stats address, so it applies when the proxy next starts"
	}
}

// sessionsStaying counts agent's running sessions that a switch to server leaves where they are:
// the ones whose inference traffic last went somewhere other than server's host. For the agent's
// own choice (server ""), every one whose inference last went to any configured server.
//
// RESET'S COUNT IS AN UPPER BOUND, and says only what is true of each session it counts. The
// router acts only on requests a client already addresses to a server's host, so an agent's own
// choice is itself one of the servers — the one its client is pointed at — and its sessions there
// stay where new ones go too. Nothing this side names that server: it is the client's setting,
// and the summary carries where a session's requests went, not where they were addressed. So
// those sessions are counted with the rest. Each counted session does stay where it is; what the
// count cannot promise is that every one of them is somewhere new sessions will not be.
//
// HOW MANY WILL NOT MOVE TO THE NEW CHOICE, NOT HOW MANY MOVE. The router keeps every running
// session where it started — by its pin, or, for one quiet since routing changed, by the server
// its history records — so a switch moves none of them. A session already on server stays put
// too, but it is where new sessions go, so naming it would say nothing; one with no inference
// yet is not anywhere, and its first request goes to the new choice.
//
// RUNNING MEANS A SESSION THIS PROXY HAS SEEN A REQUEST FROM. One resumed after a restart counts,
// its host its history's (InferenceHostFromHistory): the router keeps its pins in the plugin
// store, so its pin survived. A row only the archive holds has sent nothing since and is not
// counted. SessionSummary.Active would not do — it marks the one most recently updated session.
//
// Read with the router as it was: a switch changes agents, not servers, so the hosts are the same.
func (m *model) sessionsStaying(agent, server string) int {
	router, _ := m.activeRouter()
	n := 0
	for _, s := range m.sessions {
		if pipeline.AgentName(s.Agent) != agent || (s.Resident != nil && !*s.Resident) || s.InferenceHost == "" {
			continue
		}
		on, ok := servers.ForHost(router, s.InferenceHost)
		if (server == "" && ok) || (server != "" && on != server) {
			n++
		}
	}
	return n
}

// switchedText is the flash for a switch the proxy reloaded. A count of none says nothing, so it
// is left out rather than spelled as zero.
func switchedText(agent, server string, staying int) string {
	text := "New " + agent + " sessions → " + sanitizeLabel(server) + "."
	if server == "" {
		text = "New " + agent + " sessions are no longer routed."
	}
	switch {
	case staying == 1:
		text += " 1 running session stays where it is."
	case staying > 1:
		text += fmt.Sprintf(" %d running sessions stay where they are.", staying)
	}
	return text
}

// alreadyRouted is the flash for a choice that is already in force; `agentop server use` and
// `reset` say the same.
func alreadyRouted(agent, server string) string {
	if server == "" {
		return agent + " is not routed; nothing to change."
	}
	return "New " + agent + " sessions already go to " + sanitizeLabel(server) + "."
}

// serverNoticeKey owns the keyboard while S's panel is up, as serverPickerKey does the picker's:
// esc, q, ctrl+c and ↵ close it, and nothing else acts — S included, so pressing it again does
// not open and close the panel by turns.
func (m *model) serverNoticeKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q", "ctrl+c", "enter":
		m.serverNotice = ""
	}
	return nil
}

// closeServerPanels drops S's picker and its panel. Called wherever the AGENTS pane is entered
// and where the connection is left: either is drawn only over that pane, but the pane can move out
// from under one, and the next visit must not find a picker for a value in force that may since
// have changed.
func (m *model) closeServerPanels() {
	m.serverPicker = nil
	m.serverNotice = ""
}

// text is the footer's state while the switch is in flight, the server's name sanitised as
// serverSwitchedText's are.
func (s *serverSwitch) text() string {
	if s.server == "" {
		return "switching " + s.agent + " to its own choice…"
	}
	return "switching " + s.agent + " → " + sanitizeLabel(s.server) + "…"
}

// serverPickerHostMax bounds the picker's host column; a longer host is cut, the name and the
// mapping beside it are what tell the entries apart.
const serverPickerHostMax = 32

// ownChoiceEntry is the picker's first entry.
const ownChoiceEntry = "its own choice (not routed)"

// renderServerNotice draws why S opens nothing, or why a switch did not go through, wrapped, as
// X's dialog wraps its text. A panel and not a flash because no footer line holds the longer
// texts whole — under on_error: observe it is the two sentences `agentop server` prints and the
// config file's path, and a refused reload is the proxy's whole error — and a text cut short
// loses the part of it that says what to do. Returns the panel only; see overlayCenter.
//
// SANITISED HERE AS WELL as where each text is made: a panel is drawn from strings agentop did
// not produce — an error the proxy reported, a path from a config file — and one escape would act
// on the terminal rather than be drawn.
func renderServerNotice(text string, width int) string {
	const wrap = 60
	w := min(wrap, max(width-6, 20))
	body := lipgloss.NewStyle().Width(w).Render(sanitizeLabel(text)) + "\n\n" + styleHint.Render("[esc] close")
	box := styleBorder.Padding(0, 2)
	if width > styleBorder.GetHorizontalBorderSize() {
		box = box.MaxWidth(width)
	}
	return box.Render(body)
}

// renderServerPicker draws S's picker. Returns the panel only; see overlayCenter.
//
// SANITISED AT RENDER TIME, as the SERVER cells are: a server's name, host and models arrive over
// an unauthenticated API. Not in the entries, which hold the name the write must send.
func renderServerPicker(p *serverPicker, width int) string {
	shown := make([]serverChoice, len(p.entries))
	nameW, hostW := 0, 0
	for i, e := range p.entries {
		shown[i] = serverChoice{name: sanitizeLabel(e.name), host: sanitizeLabel(e.host), mapping: sanitizeLabel(e.mapping)}
		if i > 0 {
			nameW = max(nameW, lipgloss.Width(shown[i].name))
			hostW = max(hostW, min(lipgloss.Width(shown[i].host), serverPickerHostMax))
		}
	}
	var b strings.Builder
	title := "New " + p.agent + " sessions use"
	if p.session != "" {
		title = "Session " + sanitizeLabel(p.session) + " uses"
	}
	b.WriteString(styleTitle.Render(title) + "\n\n")
	for i, e := range shown {
		marker := "  "
		if i == p.cursor {
			marker = "> "
		}
		line := ownChoiceEntry
		if e.name != "" {
			line = padRight(e.name, nameW) + "   " + styleMuted.Render(padRight(truncRight(e.host, hostW), hostW)) +
				"   " + e.mapping
		}
		b.WriteString(marker + line + "\n")
	}
	if p.session != "" && p.sessionTokens > 0 {
		b.WriteString("\n" + styleMuted.Render(fmt.Sprintf("%d tokens of its context go uncached on its first turn there.", p.sessionTokens)) + "\n")
	}
	b.WriteString("\n" + styleHint.Render("[↵] choose  [esc] cancel"))
	box := styleBorder.Padding(0, 1)
	if width > styleBorder.GetHorizontalBorderSize() {
		box = box.MaxWidth(width)
	}
	return box.Render(b.String())
}
