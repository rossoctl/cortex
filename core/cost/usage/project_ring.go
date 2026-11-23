package usage

import "time"

// maxProjectRings bounds Aggregator.projects. A project label comes from the client's working
// directory, so unlike a known agent name it is an open set: without a cap, a caller could grow
// this process by varying it. At about 2.2 MiB for a full ring (BenchmarkProjectRingHeap), 32
// rings bound the worst case near 71 MiB.
const maxProjectRings = 32

// projectKey names one project ring. agent "" is the ring of every agent's traffic in project;
// a non-empty agent is always one model.IsKnownAgent accepts.
type projectKey struct{ agent, project string }

// projectRing is one project's buckets plus the last time it was written, for eviction at the
// cap. An evicted ring that comes back starts empty, so it can hold fewer minutes than a window
// asks for: a reader must check it against a ring that was never evicted.
type projectRing struct {
	buckets  []bucket
	lastSeen time.Time
}

// projectRingLocked returns key's ring with lastSeen set to at, and creates it when absent, first
// evicting the coldest ring at the cap. Caller holds mu.
func (a *Aggregator) projectRingLocked(key projectKey, at time.Time) []bucket {
	if r, ok := a.projects[key]; ok {
		r.lastSeen = at
		return r.buckets
	}
	if len(a.projects) >= maxProjectRings {
		a.evictColdestProjectLocked()
	}
	r := &projectRing{buckets: make([]bucket, NumBuckets), lastSeen: at}
	a.projects[key] = r
	return r.buckets
}

// evictColdestProjectLocked drops the project ring with the oldest lastSeen, as
// evictColdestLocked does for sessions. Caller holds mu.
func (a *Aggregator) evictColdestProjectLocked() {
	var coldestKey projectKey
	var coldest time.Time
	found := false
	for k, r := range a.projects {
		if !found || r.lastSeen.Before(coldest) {
			coldestKey, coldest, found = k, r.lastSeen, true
		}
	}
	if found {
		delete(a.projects, coldestKey)
	}
}
