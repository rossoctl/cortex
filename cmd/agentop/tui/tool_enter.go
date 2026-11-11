package tui

import (
	"strconv"

	"github.com/rossoctl/cortex/core/pipeline"
)

// toolTarget is the exchange that Enter on tool row t opens (#1795), from all rows of the
// session that agentop holds, not only the rows that show. Rule 1.1: when the tool ended and
// is not lost, the request sent at its End, which holds its call and its output; inRequest is
// then true. Rule 1.2: else, or when no such request is held, the exchange whose response
// asked for the tool. num is the # of the target: its cell when its row shows, else its
// session-wide number, else "". ok is false when agentop holds neither exchange.
func (m *model) toolTarget(t *toolRow) (row eventRow, num string, inRequest bool, ok bool) {
	rows := buildEventRows(m.events[m.selectedSess])
	ids, partner := computeEventPairs(rows)
	rows, _ = mergeExchanges(rows, partner)
	find := func(match func(eventRow) bool) bool {
		for _, r := range rows {
			if r.event != nil && match(r) {
				row, ok = r, true
				return true
			}
		}
		return false
	}
	if t.End != nil && !t.Lost {
		inRequest = find(func(r eventRow) bool {
			return r.event.Phase == pipeline.SessionRequest && r.event.At.Equal(*t.End)
		})
	}
	if !ok && !find(func(r eventRow) bool {
		resp := exchangeResp(r)
		return resp != nil && resp.Seq == t.Seq && eventSession(m.selectedSess, resp) == m.selectedSess
	}) {
		return eventRow{}, "", false, false
	}
	if i := findByKey(m.visibleRows, keyOf(row.event)); i >= 0 {
		num = m.numAt(i)
	} else if n, has := ids[row.event]; has {
		num = strconv.Itoa(n)
	}
	return row, num, inRequest, true
}
