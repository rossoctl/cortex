package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
)

// THE EVENTS PANE'S `details` BOX (#1680). With the box on, Enter, n and N find the next row
// whose detail view shows the query: every field the pane draws, the raw capture too. agentop
// checks the summary rows it holds, and the proxy's search route checks what the summary drops
// and the capture. A row matches when either side matches it. Nothing is fetched to check a row.

// detailHas reports whether the detail pane, opened on r, shows q: in a line of either section
// with every fold open, the header lines too. It reads the lines before wrapping, with the matcher
// the pane's own search uses. So a row it reports opens on a highlighted match.
func detailHas(r eventRow, q string) bool {
	for _, l := range renderTree(detailContent(r, false), foldState{all: true}) {
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
	// hits is the held rows the search checked, by event key: true when either side matched it.
	hits map[eventKey]bool
	// last is the newest seq the proxy read, and oldest the oldest seq agentop held, for each
	// session id it was asked about. A held row older than oldest (a page the ▲ row loaded) makes
	// the answer stale. A row newer than last (a new event) makes a step that reaches it search
	// again.
	last, oldest map[string]uint64
	// newest is the newest seq agentop showed for each session id, when it asked. A row newer than
	// newest that the search did not check is a new row, as one newer than last is.
	newest map[string]uint64
	// older counts the proxy's matches older than the rows agentop holds.
	older int
	// forward and inclusive are the step to take when the answer lands.
	forward, inclusive bool
	// cancel ends the request, so the proxy stops its pass when the search is dropped.
	cancel context.CancelFunc
}

type detailSearchState int

const (
	searchSending detailSearchState = iota
	searchReady
	searchFailed
)

// detailSearchMsg is the answer to one details search.
type detailSearchMsg struct {
	gen                  uint64
	hits                 map[eventKey]bool
	last, oldest, newest map[string]uint64
	older                int
	err                  error
}

// sendDetailSearch sends the events pane's committed query, and takes the step when the answer
// lands: from the cursor, the cursor's own row included when inclusive (Enter).
func (m *model) sendDetailSearch(forward, inclusive bool) tea.Cmd {
	m.cancelDetailSearch()
	q := m.search[paneEvents]
	if q == "" || m.client == nil {
		return nil
	}
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	m.detailSearch = &detailSearch{gen: m.detailSearchGen, query: q, forward: forward, inclusive: inclusive, cancel: cancel}
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
		if r.resp != nil {
			resp := *r.resp
			r.resp = &resp
			r.respRaw, _ = m.heldRaw(&resp)
		}
		rows = append(rows, r)
	}
	client, gen, sel := m.client, m.detailSearchGen, m.selectedSess
	return func() tea.Msg { return runDetailSearch(ctx, client, gen, sel, rows, q) }
}

// runDetailSearch asks the proxy about each session id among rows, and checks each row's summary
// itself. A linked bucket's events have their own id and seqs, so each id is asked one time. An
// id the proxy no longer holds (an evicted bucket) has no matches there, and does not fail the
// search.
func runDetailSearch(ctx context.Context, c *apiclient.Client, gen uint64, sel string, rows []eventRow, q string) detailSearchMsg {
	msg := detailSearchMsg{gen: gen, hits: map[eventKey]bool{}, last: map[string]uint64{}, oldest: map[string]uint64{},
		newest: map[string]uint64{}}
	for _, r := range rows {
		s := eventSession(sel, r.event)
		if _, ok := msg.oldest[s]; !ok {
			msg.oldest[s] = r.event.Seq
		}
		msg.oldest[s] = min(msg.oldest[s], r.event.Seq)
		msg.newest[s] = max(msg.newest[s], r.event.Seq)
	}
	found := map[string]map[uint64]bool{}
	for s := range msg.oldest {
		res, err := c.SearchSession(ctx, s, q)
		if errors.Is(err, apiclient.ErrNotFound) {
			msg.last[s] = msg.newest[s]
			continue
		}
		if err != nil {
			msg.err = err
			return msg
		}
		found[s] = map[uint64]bool{}
		for _, seq := range res.Seqs {
			found[s][seq] = true
			if seq < msg.oldest[s] {
				msg.older++
			}
		}
		msg.last[s] = res.LastSeq
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return msg // dropped: its answer is not used, and a row with a capture is slow to check
		}
		seqs := found[eventSession(sel, r.event)]
		msg.hits[keyOf(r.event)] = seqs[r.event.Seq] || (r.resp != nil && seqs[r.resp.Seq]) || detailHas(r, q)
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
	d.cancel()
	d.hits, d.last, d.oldest, d.newest, d.older, d.state = msg.hits, msg.last, msg.oldest, msg.newest, msg.older, searchReady
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

// stepDetails is n and N with the box on: nothing while a search runs, else the next match at
// once, or a new search first when there is no answer for the query, the last one failed, a held
// row is older than the answer covered, or the step reaches a row newer than it. A live session
// gets new rows every few seconds, and a pass over a long one takes seconds, so a step that does
// not reach the new rows does not wait for them.
func (m *model) stepDetails(forward bool) tea.Cmd {
	d := m.detailSearch
	switch {
	case d != nil && d.state == searchSending && d.query == m.search[paneEvents]:
		return nil
	case d == nil || d.query != m.search[paneEvents] || d.state == searchFailed || m.detailSearchStale():
		return m.sendDetailSearch(forward, false)
	}
	fresh := m.detailNewRows()
	stops := slices.Concat(m.detailMatchRows(), fresh)
	slices.Sort(stops)
	i, ok := nextMatch(stops, m.eventsTbl.Cursor(), forward, false)
	if !ok {
		return nil
	}
	if _, isNew := slices.BinarySearch(fresh, i); isNew {
		return m.sendDetailSearch(forward, false)
	}
	eventsSurface{m}.reveal(i)
	return nil
}

// detailSearchStale reports whether a held row is outside what the answer covered for its
// session in a way a step cannot go around: a session it did not ask about, a row older than
// what agentop held when it asked, or a row it did not check that is no newer than what it showed
// (a hidden one).
func (m *model) detailSearchStale() bool {
	d := m.detailSearch
	for _, r := range m.visibleRows {
		if r.event == nil {
			continue
		}
		s := eventSession(m.selectedSess, r.event)
		_, checked := d.hits[keyOf(r.event)]
		if _, ok := d.last[s]; !ok || r.event.Seq < d.oldest[s] || (!checked && r.event.Seq <= d.newest[s]) {
			return true
		}
	}
	return false
}

// detailNewRows is the visible rows newer than what the proxy read for their session, or newer
// than what agentop held and not checked, ascending: the rows the answer has nothing to say about.
func (m *model) detailNewRows() []int {
	d := m.detailSearch
	var out []int
	for i, r := range m.visibleRows {
		if r.event == nil {
			continue
		}
		s := eventSession(m.selectedSess, r.event)
		_, checked := d.hits[keyOf(r.event)]
		// newestSeq: a response that lands after the answer changes a row the answer checked.
		if newestSeq(r) > d.last[s] || (!checked && r.event.Seq > d.newest[s]) {
			out = append(out, i)
		}
	}
	return out
}

// newestSeq is the seq of the newer half of row r (#1681).
func newestSeq(r eventRow) uint64 {
	if r.resp != nil {
		return max(r.event.Seq, r.resp.Seq)
	}
	return r.event.Seq
}

func (m *model) detailSearching() bool {
	return m.detailSearch != nil && m.detailSearch.state == searchSending
}

// cancelDetailSearch drops the search and any answer still to come, and ends its request.
func (m *model) cancelDetailSearch() {
	if d := m.detailSearch; d != nil && d.cancel != nil {
		d.cancel()
	}
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
		older = fmt.Sprintf(" · %d older (▲ row)", d.older)
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
