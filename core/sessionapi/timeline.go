package sessionapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// timelineMaxEvents bounds how much history one timeline reads. The largest real session seen
// was 5,764 events; this is an order of magnitude past it, so it only binds on a session that
// ran for days, and the response says so.
const timelineMaxEvents = 50_000

// handleTimeline serves the session's spans: agents, tools and hosts on one time axis, for
// agentop's Timeline pane. Computed from the session's whole history, memory first and the
// archive behind it. A 6,000-event session computes in a few milliseconds; reading one that
// lives only on disk takes over a second, so the result is cached per session until the
// session's newest event changes, and requests for one session share one compute.
//
// ?from= and ?to= (RFC 3339) keep only the spans that overlap the range; the agent list is
// always whole, so a client can lay out its rows before it has seen every window.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	from, err := timeParam(r, "from")
	if err == nil {
		var to time.Time
		if to, err = timeParam(r, "to"); err == nil {
			tl, ok := s.timelines.get(s, id)
			if !ok {
				http.NotFound(w, r)
				return
			}
			tl.Clip(from, to)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(tl); err != nil {
				slog.Debug("sessionapi: timeline encode failed", "error", err, "sessionID", id)
			}
			return
		}
	}
	writeUsageError(w, err)
}

// timelineCache holds the last computed timeline per session, keyed by the newest Seq the
// store held when it was built. Bounded: a laptop proxy holds at most a few hundred sessions,
// and only those someone opened the pane on are here.
type timelineCache struct {
	mu       sync.Mutex
	entries  map[string]timelineEntry
	prefixes map[string]*timelinePrefix
	uses     uint64
	// epoch counts erases. A compute that began before one keeps nothing it read, and a
	// request that begins after one does not join it.
	epoch  uint64
	flight singleflight.Group
}

type timelineEntry struct {
	seq uint64
	// newestAt is when the newest event was recorded. A session dropped and re-created without
	// an archive numbers from 1 again, so its seq alone can name another session's event.
	newestAt time.Time
	at       time.Time
	tl       *timeline.Timeline
}

// timelinePrefix is a resumed session's archived events below the oldest one memory holds,
// kept so that a compute for each new event does not read them from disk again.
type timelinePrefix struct {
	below  uint64
	need   int
	events []pipeline.SessionEvent
	info   archive.PageInfo
	used   uint64
	readAt time.Time
}

// timelineCacheMax bounds the cache; past it the oldest entry goes.
const timelineCacheMax = 32

// timelinePrefixEvents bounds the archived events the cache holds, across sessions; past it the
// least recently used history goes. It holds the largest real session seen (5,764 events) with
// room for a second, smaller one. A history larger than the bound is not held, and every
// compute reads it from disk.
const timelinePrefixEvents = 10_000

// timelinePrefixTTL is how long a held history outlives its read; a var so a test can shorten it.
var timelinePrefixTTL = time.Minute

// get returns a private copy, since the caller clips it, aged against now.
//
// A cached timeline is served until the session records a new event, however old it is. The
// only clock-dependent part of a timeline is which unanswered requests have aged out, and
// LoseUnanswered redoes that on the copy, so an hour-old entry reads as a fresh Build would.
// An archived session's newest seq never moves, so its one cold read is its only one.
func (c *timelineCache) get(s *Server, id string) (*timeline.Timeline, bool) {
	// The session's newest Seq: memory's when it holds the session, else the archive's,
	// which for a session only on disk never moves.
	newest, newestAt := uint64(0), time.Time{}
	if v := s.store.ViewTail(id, 1); v != nil && len(v.Events) > 0 {
		newest, newestAt = v.Events[0].Seq, v.Events[0].At
	} else if s.archive != nil {
		newest = s.archive.LastSeq(id)
	}
	now := s.clock()
	c.mu.Lock()
	e, hit := c.entries[id]
	epoch := c.epoch
	c.mu.Unlock()
	if hit && newest != 0 && e.seq == newest && e.newestAt.Equal(newestAt) {
		tl := clone(e.tl)
		tl.LoseUnanswered(now)
		return tl, true
	}
	v, _, _ := c.flight.Do(strconv.FormatUint(epoch, 10)+"/"+id, func() (any, error) {
		events, truncated, ok := s.wholeSession(id, epoch)
		if !ok {
			return nil, nil
		}
		tl := timeline.Build(id, events, now)
		tl.Truncated = truncated
		if newest != 0 {
			c.put(id, epoch, timelineEntry{seq: newest, newestAt: newestAt, at: now, tl: tl})
		}
		return tl, nil
	})
	tl, _ := v.(*timeline.Timeline)
	if tl == nil {
		return nil, false
	}
	// A request that joined another's compute ages against its own now, not that compute's.
	cp := clone(tl)
	cp.LoseUnanswered(now)
	return cp, true
}

func (c *timelineCache) put(id string, epoch uint64, e timelineEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return
	}
	if c.entries == nil {
		c.entries = map[string]timelineEntry{}
	}
	if _, ok := c.entries[id]; !ok && len(c.entries) >= timelineCacheMax {
		var oldest string
		for k, v := range c.entries {
			if oldest == "" || v.at.Before(c.entries[oldest].at) {
				oldest = k
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[id] = e
}

// forget drops what the cache holds of one session; clear drops everything.
func (c *timelineCache) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	delete(c.entries, id)
	delete(c.prefixes, id)
}

func (c *timelineCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.entries, c.prefixes = nil, nil
}

// archived reads a session's archived events below cursor, keeping them when cursor is the
// oldest event memory holds and they reach it.
func (c *timelineCache) archived(s *Server, id string, epoch, cursor uint64, need int) ([]pipeline.SessionEvent, archive.PageInfo, bool, bool) {
	if cursor > 1 {
		c.mu.Lock()
		p := c.prefixes[id]
		if p != nil && p.below == cursor && p.need >= need && s.clock().Sub(p.readAt) < timelinePrefixTTL {
			c.uses++
			p.used = c.uses
		} else {
			p = nil
		}
		c.mu.Unlock()
		// Retention can delete the oldest segments; the held events then no longer match disk.
		if p != nil {
			info, ok, err := s.archive.Page(id, cursor, 0, func(*pipeline.SessionEvent) bool { return false })
			if ok && err == nil && info.OldestSeq == p.info.OldestSeq {
				return p.events, info, true, false
			}
		}
	}
	events, info, ok, failed := s.readArchive(id, cursor, need)
	if cursor > 1 && ok && !failed && len(events) > 0 && len(events) <= timelinePrefixEvents && events[len(events)-1].Seq == cursor-1 {
		var in session.Interner
		for i := range events {
			in.InternEvent(&events[i])
		}
		c.putPrefix(id, epoch, &timelinePrefix{below: cursor, need: need, events: events, info: info, readAt: s.clock()})
	}
	return events, info, ok, failed
}

func (c *timelineCache) putPrefix(id string, epoch uint64, p *timelinePrefix) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return
	}
	if c.prefixes == nil {
		c.prefixes = map[string]*timelinePrefix{}
	}
	c.uses++
	p.used = c.uses
	c.prefixes[id] = p
	// Captures the id and read time, not p, so a replaced history is not kept alive by its timer.
	at := p.readAt
	time.AfterFunc(timelinePrefixTTL, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if q := c.prefixes[id]; q != nil && q.readAt.Equal(at) {
			delete(c.prefixes, id)
		}
	})
	for {
		total, oldest := 0, ""
		for k, v := range c.prefixes {
			total += len(v.events)
			if k != id && (oldest == "" || v.used < c.prefixes[oldest].used) {
				oldest = k
			}
		}
		if total <= timelinePrefixEvents || oldest == "" {
			return
		}
		delete(c.prefixes, oldest)
	}
}

// clone copies the span slices Clip and LoseUnanswered rewrite. The spans' time pointers are
// shared, which is safe: neither writes through one, LoseUnanswered only replaces a nil End
// with a pointer of its own. slices.Clone keeps an empty list empty rather
// than nil, so it still encodes as [] and not null.
func clone(tl *timeline.Timeline) *timeline.Timeline {
	cp := *tl
	cp.Agents = slices.Clone(tl.Agents)
	cp.Turns = slices.Clone(tl.Turns)
	cp.Tools = slices.Clone(tl.Tools)
	cp.Returns = slices.Clone(tl.Returns)
	cp.Hosts = slices.Clone(tl.Hosts)
	cp.Jobs = slices.Clone(tl.Jobs)
	return &cp
}

// wholeSession returns up to timelineMaxEvents of the session's newest events in Seq order,
// with the archive's history behind what memory holds. truncated reports that older events
// exist past the bound.
func (s *Server) wholeSession(id string, epoch uint64) ([]pipeline.SessionEvent, bool, bool) {
	view := s.store.ViewTail(id, timelineMaxEvents)
	if s.archive != nil {
		view = mergeArchived(id, 0, timelineMaxEvents, view, func(cursor uint64, need int) ([]pipeline.SessionEvent, archive.PageInfo, bool, bool) {
			return s.timelines.archived(s, id, epoch, cursor, need)
		})
	}
	if view == nil {
		return nil, false, false
	}
	// Truncated when the oldest event served is not the oldest held. Both the store and the
	// archive merge set OldestSeq only when events were left out. TotalEvents is not the
	// signal: the merge reports the larger of two counts, which can equal what was served.
	truncated := view.OldestSeq != 0 && len(view.Events) > 0 && view.Events[0].Seq > view.OldestSeq
	return view.Events, truncated, true
}

func timeParam(r *http.Request, name string) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: want an RFC 3339 time, got %q", name, raw)
	}
	return t, nil
}
