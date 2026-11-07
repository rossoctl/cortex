package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
	"github.com/rossoctl/cortex/core/pipeline"
)

// THE EVENTS PANE'S `details` BOX (#1680). With the box on, Enter, n and N find the next row
// whose detail view shows the query: every field the pane draws, the raw capture too. The
// events list carries summaries, so the scan fetches each event it checks, one at a time, and
// drops it after the check. It stops at the first match, so its cost is the distance to the
// next match, not the size of the session.

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

// detailScanBatch is how many events one update checks with no fetch. Then the scan yields,
// so a long scan against a proxy that sends whole events still lets keys and the stream in.
const detailScanBatch = 20

type detailScanState int

const (
	scanRunning detailScanState = iota
	scanFound
	scanNone
)

// detailScan is one details search. It walks the visible rows from where it started, in one
// direction, wrapping, until a match or until it has visited each row one time.
type detailScan struct {
	gen     uint64
	query   string
	forward bool
	// origin is where the cursor was, for Esc.
	origin searchSpot
	// at is the row visited last. left and total count the rows, for the footer.
	at, left, total int
	state           detailScanState
}

// detailScanFetchedMsg is the full event a scan fetched to check.
type detailScanFetchedMsg struct {
	gen   uint64
	key   eventKey
	event *pipeline.SessionEvent
	raw   *pipeline.RawCapture
	err   error
}

// detailScanNextMsg continues a scan that yielded after a batch.
type detailScanNextMsg struct{ gen uint64 }

// startDetailScan starts a details search for the events pane's committed query from the
// cursor. inclusive lets the cursor's own row match: Enter starts there, n and N start past it.
func (m *model) startDetailScan(forward, inclusive bool) tea.Cmd {
	m.cancelDetailScan()
	q, n := m.search[paneEvents], len(m.visibleRows)
	if q == "" || n == 0 {
		return nil
	}
	if q != m.detailCheckedQ {
		m.detailChecked, m.detailCheckedQ = map[eventKey]bool{}, q
	}
	at := m.eventsTbl.Cursor()
	if inclusive {
		// One step back, so the first step lands on the cursor's row.
		if forward {
			at--
		} else {
			at++
		}
	}
	m.detailScan = &detailScan{gen: m.detailScanGen, query: q, forward: forward,
		origin: eventsSurface{m}.spot(), at: at, left: n, total: n}
	return m.runDetailScan()
}

// runDetailScan visits rows until a match, a fetch, a full batch, or no rows left.
func (m *model) runDetailScan() tea.Cmd {
	s := m.detailScan
	for checked := 0; s.left > 0; {
		if checked == detailScanBatch {
			gen := s.gen
			return func() tea.Msg { return detailScanNextMsg{gen: gen} }
		}
		n := len(m.visibleRows)
		if n == 0 {
			break
		}
		if s.forward {
			s.at = ((s.at+1)%n + n) % n
		} else {
			s.at = ((s.at-1)%n + n) % n
		}
		s.left--
		r := m.visibleRows[s.at]
		if r.event == nil {
			continue // a subagent block's header row
		}
		k := keyOf(r.event)
		if matched, ok := m.detailChecked[k]; ok {
			if matched {
				m.foundDetail(k)
				return nil
			}
			continue
		}
		if m.needsFullEvent(r.event) && m.client != nil {
			ctx, client, gen := m.ctx, m.client, s.gen
			sess, seq := eventSession(m.selectedSess, r.event), r.event.Seq
			return func() tea.Msg {
				ev, raw, err := client.GetEventRaw(ctx, sess, seq)
				return detailScanFetchedMsg{gen: gen, key: k, event: ev, raw: raw, err: err}
			}
		}
		r.raw, _ = m.heldRaw(r.event)
		checked++
		if m.checkDetailRow(r, k) {
			return nil
		}
	}
	s.state = scanNone
	return nil
}

// checkDetailRow records whether r's detail shows the query, and ends the scan on a match.
func (m *model) checkDetailRow(r eventRow, k eventKey) bool {
	matched := detailHas(r, m.detailScan.query)
	m.detailChecked[k] = matched
	if matched {
		m.foundDetail(k)
	}
	return matched
}

// foundDetail ends the scan on the row of k, which can have moved since the scan visited it.
func (m *model) foundDetail(k eventKey) {
	if i := findByKey(m.visibleRows, k); i >= 0 {
		eventsSurface{m}.reveal(i)
	}
	m.detailScan.state = scanFound
}

// detailScanLive reports whether gen is the running scan, on the events pane. A scan whose pane was
// left ends here, so its late reply cannot move a cursor the operator no longer sees.
func (m *model) detailScanLive(gen uint64) bool {
	s := m.detailScan
	if s == nil || s.gen != gen || s.state != scanRunning {
		return false
	}
	if m.pane != paneEvents {
		m.cancelDetailScan()
		return false
	}
	return true
}

// applyDetailScanFetched checks a fetched event and drops it. The event goes into no list: the
// scan holds one event at a time, never the session.
func (m *model) applyDetailScanFetched(msg detailScanFetchedMsg) tea.Cmd {
	if !m.detailScanLive(msg.gen) {
		return nil
	}
	switch {
	case errors.Is(msg.err, apiclient.ErrNotFound):
		// The proxy evicted it. Nothing to check.
	case msg.err != nil:
		m.cancelDetailScan()
		m.setFlash("details search stopped: " + msg.err.Error())
		return nil
	default:
		if i := findByKey(m.visibleRows, msg.key); i >= 0 {
			r := m.visibleRows[i]
			r.event, r.raw = msg.event, msg.raw
			if m.checkDetailRow(r, msg.key) {
				return nil
			}
		}
	}
	return m.runDetailScan()
}

// applyDetailScanNext continues a scan after a batch.
func (m *model) applyDetailScanNext(msg detailScanNextMsg) tea.Cmd {
	if !m.detailScanLive(msg.gen) {
		return nil
	}
	return m.runDetailScan()
}

func (m *model) detailScanRunning() bool {
	return m.detailScan != nil && m.detailScan.state == scanRunning
}

// cancelDetailScan ends any scan. The cursor stays where it is.
func (m *model) cancelDetailScan() {
	m.detailScanGen++
	m.detailScan = nil
}

// stopDetailScan is Esc during a scan: the scan ends and the cursor goes back.
func (m *model) stopDetailScan() {
	if s := m.detailScan; s != nil {
		eventsSurface{m}.restore(s.origin)
	}
	m.cancelDetailScan()
}

// resetDetailSearch clears the box and the results with the events pane's query, when the
// operator opens another session.
func (m *model) resetDetailSearch() {
	m.cancelDetailScan()
	m.searchDetails = false
	m.detailChecked, m.detailCheckedQ = nil, ""
}

// detailSearchStatus is the footer's account of the details search.
func (m *model) detailSearchStatus() string {
	q := m.searchQuery(paneEvents)
	if q == "" {
		return ""
	}
	s := m.detailScan
	switch {
	case m.searching && m.searchPane == paneEvents:
		return fmt.Sprintf("[/%s details: Enter to search]", q)
	case s == nil || s.query != q:
		return fmt.Sprintf("[/%s details]", q)
	case s.state == scanRunning:
		return fmt.Sprintf("[/%s details: searching %d of %d…]", q, s.total-s.left, s.total)
	case s.state == scanFound:
		return fmt.Sprintf("[/%s details: match]", q)
	}
	return fmt.Sprintf("[/%s details: no match in %d rows]", q, s.total)
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
