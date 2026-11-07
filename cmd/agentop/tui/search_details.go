package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
)

// THE EVENTS PANE'S `details` BOX (#1680). With the box on, Enter, n and N find the next row
// whose detail view shows the query: every field the pane draws, the raw capture too. agentop
// checks the summary rows it holds, and the proxy's search route checks what the summary drops
// and the capture. A row matches when either side matches it. Nothing is fetched to check a row.

// detailHas reports whether the detail pane, opened on r, shows q: in a header line, or in a
// line of the tree with every fold open. It reads the lines before wrapping, with the matcher
// the pane's own search uses. So a row it reports opens on a highlighted match.
func detailHas(r eventRow, q string) bool {
	head, root := detailContent(r)
	for _, h := range head {
		for _, l := range strings.Split(h, "\n") {
			if find.Contains(ansi.Strip(l), q) {
				return true
			}
		}
	}
	if root == nil {
		return false
	}
	for _, l := range renderTree(root, foldState{all: true}) {
		if find.Contains(ansi.Strip(l.text), q) {
			return true
		}
	}
	return false
}

// detailSearch is one details search: the request in flight, or its answer.
type detailSearch struct {
	gen   uint64
	query string
	state detailSearchState
	// noRoute is an answer from a proxy without the search route.
	noRoute bool
	// hits is the held rows that either side matched, by event key.
	hits map[eventKey]bool
	// last is the newest seq the proxy read, for each session id it was asked about.
	last map[string]uint64
	// older counts the proxy's matches older than the rows agentop holds.
	older int
	// forward and inclusive are the step to take when the answer lands.
	forward, inclusive bool
}

type detailSearchState int

const (
	searchSending detailSearchState = iota
	searchReady
	searchFailed
)

// detailSearchMsg is the answer to one details search.
type detailSearchMsg struct {
	gen   uint64
	hits  map[eventKey]bool
	last  map[string]uint64
	older int
	err   error
}

// sendDetailSearch sends the events pane's committed query, and takes the step when the answer
// lands: from the cursor, the cursor's own row included when inclusive (Enter).
func (m *model) sendDetailSearch(forward, inclusive bool) tea.Cmd {
	m.cancelDetailSearch()
	q := m.search[paneEvents]
	if q == "" || m.client == nil {
		return nil
	}
	m.detailSearch = &detailSearch{gen: m.detailSearchGen, query: q, forward: forward, inclusive: inclusive}
	// The command runs on its own goroutine, and replaceHeldEvent writes the held events while it
	// runs. So it gets a copy of each held row.
	rows := make([]eventRow, 0, len(m.visibleRows))
	for _, r := range m.visibleRows {
		if r.event == nil {
			continue // a subagent block's header row
		}
		e := *r.event
		r.event = &e
		r.raw, _ = m.heldRaw(&e)
		rows = append(rows, r)
	}
	ctx, client, gen, sel := m.ctx, m.client, m.detailSearchGen, m.selectedSess
	return func() tea.Msg { return runDetailSearch(ctx, client, gen, sel, rows, q) }
}

// runDetailSearch asks the proxy about each session id among rows, and checks each row's summary
// itself. A linked bucket's events have their own id and seqs, so each id is asked one time.
func runDetailSearch(ctx context.Context, c *apiclient.Client, gen uint64, sel string, rows []eventRow, q string) detailSearchMsg {
	msg := detailSearchMsg{gen: gen, hits: map[eventKey]bool{}, last: map[string]uint64{}}
	held, oldest := map[string]map[uint64]bool{}, map[string]uint64{}
	for _, r := range rows {
		s := eventSession(sel, r.event)
		if held[s] == nil {
			held[s] = map[uint64]bool{}
			oldest[s] = r.event.Seq
		}
		oldest[s] = min(oldest[s], r.event.Seq)
	}
	found := map[string]map[uint64]bool{}
	for s := range held {
		res, err := c.SearchSession(ctx, s, q)
		if err != nil {
			msg.err = err
			return msg
		}
		found[s] = map[uint64]bool{}
		for _, seq := range res.Seqs {
			found[s][seq] = true
			if seq < oldest[s] {
				msg.older++
			}
		}
		msg.last[s] = res.LastSeq
	}
	for _, r := range rows {
		if found[eventSession(sel, r.event)][r.event.Seq] || detailHas(r, q) {
			msg.hits[keyOf(r.event)] = true
		}
	}
	return msg
}

// applyDetailSearch takes an answer: it drops one for an older search, or one that lands after
// the operator left the pane, and otherwise takes the step it was sent for.
func (m *model) applyDetailSearch(msg detailSearchMsg) {
	d := m.detailSearch
	if d == nil || d.gen != msg.gen || d.state != searchSending {
		return
	}
	if m.pane != paneEvents {
		m.cancelDetailSearch()
		return
	}
	if msg.err != nil {
		d.state, d.noRoute = searchFailed, errors.Is(msg.err, apiclient.ErrNoSearch)
		if !d.noRoute {
			m.setFlash("details search failed: " + msg.err.Error())
		}
		return
	}
	d.hits, d.last, d.older, d.state = msg.hits, msg.last, msg.older, searchReady
	m.stepDetailMatch(d.forward, d.inclusive)
}

// detailMatchRows is the visible rows the answer matched, ascending.
func (m *model) detailMatchRows() []int {
	var out []int
	for i, r := range m.visibleRows {
		if r.event != nil && m.detailSearch.hits[keyOf(r.event)] {
			out = append(out, i)
		}
	}
	return out
}

func (m *model) stepDetailMatch(forward, inclusive bool) {
	if i, ok := nextMatch(m.detailMatchRows(), m.eventsTbl.Cursor(), forward, inclusive); ok {
		eventsSurface{m}.reveal(i)
	}
}

// stepDetails is n and N with the box on: the next match at once, or a new search first when
// there is no answer for the query, the last one failed, or the session has grown since.
func (m *model) stepDetails(forward bool) tea.Cmd {
	d := m.detailSearch
	switch {
	case d == nil || d.query != m.search[paneEvents] || d.state == searchFailed || m.detailSearchStale():
		return m.sendDetailSearch(forward, false)
	case d.state == searchSending:
		return nil
	}
	m.stepDetailMatch(forward, false)
	return nil
}

// detailSearchStale reports whether a held row is newer than what the proxy read for its session.
func (m *model) detailSearchStale() bool {
	for _, r := range m.visibleRows {
		if r.event == nil {
			continue
		}
		last, ok := m.detailSearch.last[eventSession(m.selectedSess, r.event)]
		if !ok || r.event.Seq > last {
			return true
		}
	}
	return false
}

func (m *model) detailSearching() bool {
	return m.detailSearch != nil && m.detailSearch.state == searchSending
}

// cancelDetailSearch drops the search and any answer still to come.
func (m *model) cancelDetailSearch() {
	m.detailSearchGen++
	m.detailSearch = nil
}

// resetDetailSearch clears the box with the events pane's query, when another session opens.
func (m *model) resetDetailSearch() {
	m.cancelDetailSearch()
	m.searchDetails = false
}

// detailSearchStatus is the footer's account of the details search.
func (m *model) detailSearchStatus() string {
	q := m.searchQuery(paneEvents)
	if q == "" {
		return ""
	}
	d := m.detailSearch
	switch {
	case m.searching && m.searchPane == paneEvents:
		return fmt.Sprintf("[/%s details: Enter to search]", q)
	case d == nil || d.query != q:
		return fmt.Sprintf("[/%s details]", q)
	case d.state == searchSending:
		return fmt.Sprintf("[/%s details: searching…]", q)
	case d.noRoute:
		return fmt.Sprintf("[/%s details: this Cortex has no details search]", q)
	case d.state == searchFailed:
		return fmt.Sprintf("[/%s details: search failed]", q)
	}
	older := ""
	if d.older > 0 {
		older = fmt.Sprintf(" · %d older (o)", d.older)
	}
	rows := m.detailMatchRows()
	if len(rows) == 0 {
		return fmt.Sprintf("[/%s details: no match]%s", q, older)
	}
	if k, ok := slices.BinarySearch(rows, m.eventsTbl.Cursor()); ok {
		return fmt.Sprintf("[/%s details %d/%d]%s", q, k+1, len(rows), older)
	}
	return fmt.Sprintf("[/%s details: %d matches]%s", q, len(rows), older)
}

// detailsBoxWidth is the room the box takes beside the events pane's prompt.
const detailsBoxWidth = len("   [ ] details  (tab)")

// searchPromptView is the open prompt, with the details box beside it on the events pane.
func (m *model) searchPromptView() string {
	v := m.searchInput.View()
	if m.searchPane != paneEvents {
		return v
	}
	box := "[ ] details"
	if m.searchDetails {
		box = "[x] details"
	}
	return v + "   " + box + "  (tab)"
}
