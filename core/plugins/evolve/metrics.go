package evolve

import (
	"sync"

	"github.com/rossoctl/cortex/core/pipeline"
)

// metrics are in-memory, per-process counters. The fail-open paths are counted
// as well as logged: a plugin that silently stopped injecting would otherwise
// look healthy.
type metrics struct {
	mu sync.Mutex

	injected     uint64 // requests whose body carried the guidelines block
	projected    uint64 // would have been injected (on_error: observe)
	cacheHits    uint64
	fetches      uint64
	fetchErrors  uint64
	injectErrors uint64 // request shapes the block could not be added to
	learned      uint64 // session batches sent to evolve
	learnErrors  uint64
	recovered    uint64
}

func (m *metrics) inc(counter *uint64) {
	m.mu.Lock()
	*counter++
	m.mu.Unlock()
}

func (m *metrics) snapshot() []pipeline.Metric {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := []struct {
		name  string
		value uint64
		note  string
	}{
		{"requests injected", m.injected, ""},
		{"requests projected", m.projected, "observe mode: block computed, not sent"},
		{"cache hits", m.cacheHits, ""},
		{"guideline fetches", m.fetches, ""},
		{"fetch errors", m.fetchErrors, "requests were forwarded without guidelines"},
		{"inject errors", m.injectErrors, "unsupported request shape; forwarded unchanged"},
		{"sessions sent to learn", m.learned, ""},
		{"learn errors", m.learnErrors, "retried on the next tick"},
		{"panics recovered", m.recovered, "the original body was forwarded"},
	}
	var out []pipeline.Metric
	for _, r := range rows {
		if r.value > 0 {
			out = append(out, pipeline.Metric{Name: r.name, Value: float64(r.value), Unit: "count", Note: r.note})
		}
	}
	return out
}
