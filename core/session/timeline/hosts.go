package timeline

import (
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// buildHosts pairs each outbound request with its response by RequestID. A request with no
// response is left open, which is honest for one in flight and for one whose stream died.
func buildHosts(t *Timeline, events []pipeline.SessionEvent, now time.Time) {
	open := map[string]int{}
	// A tunnel the proxy recorded a close for stayed opaque; a bridged one records none.
	closed := map[string]bool{}
	for i := range events {
		if e := &events[i]; e.Tunnel && e.Phase == pipeline.SessionResponse && e.RequestID != "" {
			closed[e.RequestID] = true
		}
	}
	// A tunnel open with no reason is either a bridged CONNECT, whose decrypted request is
	// its own row, or an opaque tunnel the transparent listener records before it knows. Held
	// until the next event says which.
	pendingOpen := -1
	settle := func(next *pipeline.SessionEvent) {
		if pendingOpen < 0 {
			return
		}
		if next != nil && !closed[events[t.Hosts[pendingOpen].idx].RequestID] && (next.Phase == pipeline.SessionRequest || next.Phase == pipeline.SessionDenied) && !next.Tunnel &&
			(HostKey(next.Host) == t.Hosts[pendingOpen].Host || HostKey(next.RequestedHost) == t.Hosts[pendingOpen].Host) {
			// Bridged: drop the open.
			delete(open, events[t.Hosts[pendingOpen].idx].RequestID)
			t.Hosts[pendingOpen].Host = ""
		}
		pendingOpen = -1
	}
	for i := range events {
		e := &events[i]
		if e.Direction != pipeline.Outbound || e.Host == "" {
			continue
		}
		settle(e)
		switch e.Phase {
		case pipeline.SessionRequest:
			if e.RequestID != "" {
				if j, ok := open[e.RequestID]; ok {
					loseHost(&t.Hosts[j]) // a reused id: the earlier request never got its answer
				}
				open[e.RequestID] = len(t.Hosts)
			}
			if e.Tunnel && e.TunnelReason == "" {
				pendingOpen = len(t.Hosts)
			}
			t.Hosts = append(t.Hosts, Host{Host: HostKey(e.Host), Kind: kindOf(e), Start: e.At, Seq: e.Seq, idx: i})
		case pipeline.SessionResponse, pipeline.SessionDenied:
			j, ok := open[e.RequestID]
			if !ok || e.RequestID == "" {
				// A request the pipeline refused before recording it, or a CONNECT refused
				// at the door, records only the denial. Blocked egress is the host event
				// that matters most, so it is a span of its own.
				if e.Phase == pipeline.SessionDenied {
					at := e.At
					t.Hosts = append(t.Hosts, Host{Host: HostKey(e.Host), Kind: kindOf(e), Start: at, End: &at, Status: e.StatusCode, Error: true, Seq: e.Seq})
				}
				continue
			}
			delete(open, e.RequestID)
			h := &t.Hosts[j]
			// The response's own At can trail the request by minutes more than its duration
			// on a stream the client held open; Duration is what the request took.
			end := e.At
			if e.Duration > 0 {
				end = h.Start.Add(e.Duration)
			}
			h.End, h.Status = &end, e.StatusCode
			h.Error = e.Phase == pipeline.SessionDenied || e.Error != nil || e.StatusCode >= 400
			if h.Kind == KindHTTP && (e.Inference != nil || e.MCP != nil) {
				h.Kind = kindOf(e)
			}
		}
	}
	settle(nil)
	// A request with no response is in flight only while it is recent. Measured against the
	// later of the session's newest event and now, so a hung request that is itself the
	// session's last event still ages out.
	ref := t.End
	if now.After(ref) {
		ref = now
	}
	for i := range t.Hosts {
		if h := &t.Hosts[i]; h.End == nil && h.Host != "" && ref.Sub(h.Start) > hostLostAfter {
			loseHost(h)
		}
	}
	kept := t.Hosts[:0]
	for _, h := range t.Hosts {
		if h.Host != "" {
			kept = append(kept, h)
		}
	}
	t.Hosts = kept
}

// loseHost ends an unanswered request where it started. A copy, not &h.Start: Clip moves
// spans, and a pointer into the slice would then read a neighbour's start.
func loseHost(h *Host) {
	if h.End != nil {
		return
	}
	at := h.Start
	h.End, h.Lost = &at, true
}

// hostLostAfter is how long an unanswered request may sit before it is called lost. Ten
// minutes covers the longest real model calls (the slowest seen was under 22 minutes, and
// those were the rare tail), while not drawing a dead stream as busy.
const hostLostAfter = 10 * time.Minute

func kindOf(e *pipeline.SessionEvent) Kind {
	switch {
	case e.Inference != nil:
		return KindLLM
	case e.MCP != nil:
		return KindMCP
	case e.Tunnel:
		return KindTunnel
	}
	return KindHTTP
}
