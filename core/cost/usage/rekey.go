package usage

import (
	"maps"
	"slices"
)

// Rekeyed moves oldID's figures to newID: the per-session ring, and oldID's row in every
// bucket's session breakdown. It implements session.Rekeyer.
//
// Without it the store would list the session under its new id while /v1/usage kept
// answering for the old one — the "listed but zeroed" shape sessionRing exists to prevent.
func (a *Aggregator) Rekeyed(oldID, newID string) {
	if oldID == newID {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logReplayLocked(func(r *Aggregator) { r.Rekeyed(oldID, newID) })
	if ring, ok := a.sessions[oldID]; ok {
		delete(a.sessions, oldID)
		relabelSession(ring.buckets, oldID, newID)
		if into, taken := a.sessions[newID]; taken {
			mergeRing(into.buckets, ring.buckets)
			if ring.lastSeen.After(into.lastSeen) {
				into.lastSeen = ring.lastSeen
			}
		} else {
			a.sessions[newID] = ring
		}
	}
	relabelSession(a.all, oldID, newID)
	for _, ring := range a.agents {
		relabelSession(ring, oldID, newID)
	}
}

func relabelSession(ring []bucket, oldID, newID string) {
	from, to := ringLabel(oldID), ringLabel(newID)
	for i := range ring {
		c, ok := ring[i].bySession[from]
		if !ok {
			continue
		}
		delete(ring[i].bySession, from)
		addLabel(&ring[i].bySession, to, c)
	}
}

// mergeRing adds src's buckets into dst slot by slot. Where the two hold different laps of a
// slot the newer one is kept.
func mergeRing(dst, src []bucket) {
	for i := range src {
		s, d := &src[i], &dst[i]
		switch {
		case s.start.IsZero() || s.start.Before(d.start):
		case d.start.Before(s.start):
			*d = *s
		default:
			d.add(s)
		}
	}
}

// add folds o into b. Label maps go through addLabel, so the cap holds, and each label's units
// follow it to the key addLabel filed it under.
func (b *bucket) add(o *bucket) {
	b.Counts.Add(o.Counts)
	b.latSum += o.latSum
	b.latSumSq += o.latSumSq
	b.latN += o.latN
	addLabels(&b.byMethod, o.byMethod, b.unitsFrom(o, GroupModel))
	addLabels(&b.byEndpoint, o.byEndpoint, b.unitsFrom(o, GroupEndpoint))
	addLabels(&b.byAgent, o.byAgent, b.unitsFrom(o, GroupAgent))
	addLabels(&b.bySession, o.bySession, nil)
	addLabels(&b.byStatus, o.byStatus, nil)
	addLabels(&b.byPlugin, o.byPlugin, nil)
	addLabels(&b.byHost, o.byHost, nil)
	addLabels(&b.byProvenance, o.byProvenance, nil)
	addLabels(&b.byUnpriced, o.byUnpriced, nil)
	addLabels(&b.byIncomplete, o.byIncomplete, nil)
	addLabels(&b.byCurrency, o.byCurrency, nil)
}

// unitsFrom notes on b, under the key a label was filed at, the units o held for it on axis g.
func (b *bucket) unitsFrom(o *bucket, g Group) func(from, to string) {
	return func(from, to string) {
		for unit := range o.labelUnits[g][from] {
			b.noteUnit(g, to, unit)
		}
	}
}

// addLabels adds every entry of src into *dst, and passes filed each key with the key it was
// filed at.
func addLabels(dst *map[string]Counts, src map[string]Counts, filed func(from, to string)) {
	for _, k := range slices.Sorted(maps.Keys(src)) {
		to := addLabel(dst, k, src[k])
		if filed != nil {
			filed(k, to)
		}
	}
}
