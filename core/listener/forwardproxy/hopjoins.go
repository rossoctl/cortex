package forwardproxy

import (
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
)

// hopJoinTTL is how long a join waits for its gateway row (spec Section 1, "Joins").
const hopJoinTTL = 60 * time.Second

// hopJoin is a hopped request on a bridged tunnel that waits for its gateway row.
type hopJoin struct {
	tl    *tunnelLog
	timer *time.Timer
}

// hopCut is an error of a cut that Cortex made on a hopped request, the status the
// client got, and when.
type hopCut struct {
	fail   model.EventError
	status int
	at     time.Time
}

// join holds tl open until ingestion gives the gateway row of requestID, or the join
// expires. A plain request (tl nil) has no join.
func (h *Hop) join(requestID string, tl *tunnelLog) {
	if tl == nil {
		return
	}
	tl.await()
	h.joinMu.Lock()
	defer h.joinMu.Unlock()
	h.joins[requestID] = &hopJoin{tl: tl, timer: time.AfterFunc(h.ttl, func() {
		if j := h.takeJoin(requestID); j != nil {
			j.tl.unawait(false) // Decision 14
		}
	})}
}

// takeJoin removes the join of requestID and stops its timer. nil: no join.
func (h *Hop) takeJoin(requestID string) *hopJoin {
	h.joinMu.Lock()
	defer h.joinMu.Unlock()
	j := h.joins[requestID]
	if j == nil {
		return nil
	}
	delete(h.joins, requestID)
	j.timer.Stop()
	return j
}

// dropJoin removes the join of requestID with no row: its rows are already recorded.
func (h *Hop) dropJoin(requestID string) {
	if j := h.takeJoin(requestID); j != nil {
		j.tl.unawait(false)
	}
}

// AppendRequest writes a request row through its bridged tunnel's pairing path and
// reports whether it did. False: the caller appends the row.
func (h *Hop) AppendRequest(requestID, sid string, ev model.SessionEvent, c *model.RequestCapture) bool {
	j := h.takeJoin(requestID)
	if j == nil {
		return false
	}
	return j.tl.joined(sid, ev, c)
}

// Release drops the join of requestID with no write (Decision 13).
func (h *Hop) Release(requestID string) {
	if j := h.takeJoin(requestID); j != nil {
		j.tl.unawait(true)
	}
}

// TakeCut returns the error of a cut that Cortex made on requestID and the status Cortex
// gave the client, or nil.
func (h *Hop) TakeCut(requestID string) (*model.EventError, int) {
	h.joinMu.Lock()
	defer h.joinMu.Unlock()
	c, ok := h.cuts[requestID]
	if !ok {
		return nil, 0
	}
	delete(h.cuts, requestID)
	return &c.fail, c.status
}

// noteCut keeps fail and the status the client got as the cut of requestID for TakeCut
// (branch point 5). Cuts older than the ttl are removed here, because ingestion takes a
// cut only when its row arrives.
func (h *Hop) noteCut(requestID string, status int, fail *pipeline.EventError) {
	now := time.Now()
	h.joinMu.Lock()
	defer h.joinMu.Unlock()
	for id, c := range h.cuts {
		if now.Sub(c.at) > h.ttl {
			delete(h.cuts, id)
		}
	}
	h.cuts[requestID] = hopCut{fail: *fail, status: status, at: now}
}
