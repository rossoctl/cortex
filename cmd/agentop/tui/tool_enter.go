package tui

import (
	"slices"
	"strconv"
	"strings"

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

// toolAim is the call that Enter on a tool row puts the detail cursor on (#1795). id and pos
// are the tool's. inRequest says the target is the request that holds the call and its output
// (rule 2.1). Else it is the response that asked for the tool (rule 2.2).
type toolAim struct {
	id        string
	pos       int
	inRequest bool
}

// applyToolAim puts the detail cursor on the call of m.detailAim and opens the folds above
// it. It does this only while the cursor is at the top, so a reader move is never undone. It
// returns false when there is no aim or the tree does not hold the call yet: a summary has no
// messages until its fetch arrives.
func (m *model) applyToolAim() bool {
	a := m.detailAim
	if a == nil || m.detailCursorPath != "" || m.detailCursorOff != 0 || m.detailTree == nil || len(m.detailTree.kids) < 2 {
		return false
	}
	var target *node
	var opens []*node
	if a.inRequest {
		target, opens = m.aimInRequest(a.id)
	} else if sec := m.detailTree.kids[1]; len(sec.kids) > 0 {
		calls := sec.kids[0].child("inference").child("toolCalls")
		target = callWithID(calls, a.id, a.pos)
	}
	if target == nil {
		return false
	}
	if m.detailFolds == nil {
		m.detailFolds = map[string]foldChoice{}
	}
	open := func(p string) { m.detailFolds[p] = foldChoice{open: true, query: m.searchQuery(paneDetail)} }
	for p := target.path; p != ""; p = parentPath(p) {
		open(p)
	}
	for _, n := range opens {
		open(n.path)
	}
	m.detailCursorPath, m.detailCursorOff = target.path, 0
	return true
}

// aimInRequest is the toolCalls entry that names call id in the last assistant message of the
// open request that names it (rule 2.1), and the folds that show the call and its result: that
// entry and the next message's toolResults entry. The message index comes from the held event,
// because the tree
// no longer has toolUseIds. groupEarlier puts the first messages under one group, so the
// tree's messages are counted with the group's kids in place.
func (m *model) aimInRequest(id string) (*node, []*node) {
	req, sec := m.detailRow.event, m.detailTree.kids[0]
	if req == nil || req.Inference == nil || len(sec.kids) == 0 {
		return nil, nil
	}
	i := -1
	for j, msg := range req.Inference.Messages {
		if msg.Role == "assistant" && slices.Contains(strings.Split(msg.ToolUseIDs, ", "), id) {
			i = j
		}
	}
	var flat []*node
	if msgs := sec.kids[0].child("inference").child("messages"); msgs != nil {
		for _, k := range msgs.kids {
			if k.kind == kindGroup {
				flat = append(flat, k.kids...)
			} else {
				flat = append(flat, k)
			}
		}
	}
	if i < 0 || i >= len(flat) {
		return nil, nil
	}
	var opens []*node
	add := func(list *node, key string) *node {
		if list == nil {
			return nil
		}
		for _, e := range list.kids {
			if e.str(key) == id {
				opens = append(opens, list, e)
				return e
			}
		}
		return nil
	}
	// The cursor goes to the call itself: the message's first line is its header, and
	// Enter on a tool row means that tool.
	target := add(flat[i].child("toolCalls"), "id")
	if i+1 < len(flat) {
		add(flat[i+1].child("toolResults"), "toolUseId")
	}
	if target == nil {
		target = flat[i]
	}
	return target, opens
}

// callWithID is the entry of calls at pos when its id is id (rule 2.2), else the first entry
// with that id. pos tells apart two calls with one reused ID.
func callWithID(calls *node, id string, pos int) *node {
	if calls == nil {
		return nil
	}
	if pos < len(calls.kids) && calls.kids[pos].str("id") == id {
		return calls.kids[pos]
	}
	for _, e := range calls.kids {
		if e.str("id") == id {
			return e
		}
	}
	return nil
}
