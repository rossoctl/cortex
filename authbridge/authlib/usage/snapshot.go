package usage

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Group selects which label breakdown a Snapshot carries.
type Group string

const (
	GroupNone   Group = "none"
	GroupMethod Group = "method"
	GroupStatus Group = "status"
	GroupPlugin Group = "plugin"
	GroupHost   Group = "host"
)

// ParseGroup validates a group parameter. Empty means GroupNone.
func ParseGroup(s string) (Group, error) {
	switch Group(s) {
	case "", GroupNone:
		return GroupNone, nil
	case GroupMethod:
		return GroupMethod, nil
	case GroupStatus:
		return GroupStatus, nil
	case GroupPlugin:
		return GroupPlugin, nil
	case GroupHost:
		return GroupHost, nil
	}
	// Deliberately does NOT echo the caller's value: this message is returned
	// over an unauthenticated endpoint, and reflecting arbitrary query input
	// into a response body is how a reflected-content issue starts. The valid
	// set is short enough that naming it is more useful than quoting the input.
	return "", errors.New("unknown group (want none, method, status, plugin or host)")
}

// Snapshot is the wire shape of GET /v1/usage.
type Snapshot struct {
	// Window is the requested span, e.g. "10m".
	Window string `json:"window"`
	// BucketSeconds is the resolution of the buckets actually returned, which is
	// the requested resolution rounded to a whole multiple of BucketWidth. A
	// client reads this rather than assuming: asking for a resolution the
	// storage cannot divide evenly gets the nearest one that works, not an
	// error.
	BucketSeconds int `json:"bucketSeconds"`
	// Session is the session this covers, or "" for all sessions combined.
	Session string `json:"session,omitempty"`
	Group   Group  `json:"group"`
	// Buckets runs oldest to newest and always has Window/BucketWidth entries,
	// including zeroed ones for idle minutes.
	Buckets []Bucket `json:"buckets"`
	// Totals sums every bucket, so a client need not re-add them to render a
	// summary line.
	Totals Counts `json:"totals"`
	// Priced reports whether ANY request in this window produced a cost. False
	// means CostMicros is absent everywhere, and a client must say "cost
	// unavailable" rather than display $0.00 — which would read as "this traffic
	// was free".
	//
	// True does NOT mean every request was priced. Compare Totals.PricedRequests
	// against Totals.Requests: cost comes from a plugin that may not be in the
	// pipeline for all traffic, and once rates are per-endpoint a deployment can
	// price some endpoints and not others. Where those differ the dollar total
	// covers only the priced subset, and a client showing it must say so.
	Priced bool `json:"priced"`
	// UnpricedBy counts the requests that could NOT be priced, keyed
	// "<endpoint> <model>". Present only when something was unpriced.
	//
	// A gap has to be nameable, not just countable. "Cost is incomplete" gives an
	// operator nothing to act on; "api.openai.com gpt-5: 412" names the pricing
	// entry to add. Traffic carrying no model is excluded — naming every non-LLM
	// call the proxy handled would bury the real gaps.
	//
	// Summed across the window from the raw buckets, so it is unaffected by the
	// requested resolution, exactly like Totals.
	UnpricedBy map[string]int64 `json:"unpricedBy,omitempty"`
	// PricedBy counts priced requests by the provenance of their figure —
	// "authoritative" when the gateway reported it, otherwise the rate table's level
	// ("configured", "discovered", "bundled").
	//
	// Without it a total is unqualified, and the spec's own success criterion asks
	// for cost "labelled with provenance": $12.40 assembled from a gateway's own
	// numbers and $12.40 modelled from a shipped vendor-list table are not equally
	// trustworthy figures, and nothing else in the response distinguishes them.
	//
	// Summed from the raw buckets alongside Totals, so it is unaffected by the
	// requested resolution.
	PricedBy map[string]int64 `json:"pricedBy,omitempty"`
}

// ParseWindow validates a window parameter against the storage resolution.
func ParseWindow(s string) (time.Duration, error) {
	if s == "" {
		return 10 * BucketWidth, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		// Not wrapped with the caller's string, for the reason in ParseGroup.
		// time.ParseDuration's own error quotes the input, so it is not
		// forwarded either.
		return 0, errors.New("bad window (want a duration such as 10m, 1h or 6h)")
	}
	if d < BucketWidth {
		return 0, fmt.Errorf("window %s is shorter than the %s bucket width", d, BucketWidth)
	}
	if d > MaxWindow {
		return 0, fmt.Errorf("window %s exceeds the %s retained", d, MaxWindow)
	}
	if d%BucketWidth != 0 {
		return 0, fmt.Errorf("window %s is not a multiple of %s", d, BucketWidth)
	}
	return d, nil
}

// ParseResolution validates a resolution parameter — the width of the buckets
// the caller wants back, as opposed to the window's total span.
//
// Folding happens server-side so every client gets the same arithmetic. A 1h
// window at 1m resolution is 60 bars, which no terminal renders legibly; at 5m
// it is 12. Doing that here rather than in each client means the mean-of-means
// problem below is solved once, correctly, instead of per consumer.
//
// Zero or empty means "storage resolution" (BucketWidth). A resolution finer
// than BucketWidth is an error rather than a silent upgrade: returning coarser
// data than asked for would make a client's axis labels wrong.
func ParseResolution(s string, window time.Duration) (time.Duration, error) {
	if s == "" {
		return BucketWidth, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		// Does not echo the caller's value — see ParseGroup.
		return 0, errors.New("bad resolution (want a duration such as 1m, 5m or 30m)")
	}
	if d < BucketWidth {
		return 0, fmt.Errorf("resolution %s is finer than the %s storage bucket", d, BucketWidth)
	}
	if d%BucketWidth != 0 {
		return 0, fmt.Errorf("resolution %s is not a multiple of %s", d, BucketWidth)
	}
	if d > window {
		return 0, fmt.Errorf("resolution %s exceeds the %s window", d, window)
	}
	return d, nil
}

// fold groups src into buckets of the given width, summing counts and combining
// latency statistics.
//
// Latency is the part that cannot be done naively. Averaging the per-minute
// means would weight a minute with 1 request equally with a minute with 500 —
// the mean-of-means error. Instead each source bucket's running sums are
// reconstituted (sum = mean x n, sumSq recovered from the variance identity) and
// re-accumulated, so the folded mean and standard deviation are exactly what a
// single wider bucket would have recorded.
func fold(src []Bucket, width time.Duration) []Bucket {
	if width <= BucketWidth || len(src) == 0 {
		return src
	}
	per := int(width / BucketWidth)
	out := make([]Bucket, 0, (len(src)+per-1)/per)

	for i := 0; i < len(src); i += per {
		end := i + per
		if end > len(src) {
			end = len(src)
		}
		acc := Bucket{At: src[i].At}
		var latSum, latSumSq float64
		var latN int64
		series := map[string]Counts{}

		for _, b := range src[i:end] {
			acc.Counts.Add(b.Counts)
			// Weighted by LatSamples, not Requests: the source mean was computed
			// over measured requests only, so reconstituting with Requests would
			// re-introduce the dilution latStats exists to avoid.
			if b.LatMeanMs > 0 && b.LatSamples > 0 {
				n := float64(b.LatSamples)
				sum := b.LatMeanMs * n
				// variance = sumSq/n - mean^2  =>  sumSq = n*(variance + mean^2)
				sumSq := n * (b.LatStdDevMs*b.LatStdDevMs + b.LatMeanMs*b.LatMeanMs)
				latSum += sum
				latSumSq += sumSq
				latN += b.LatSamples
			}
			for k, v := range b.Series {
				cur := series[k]
				cur.Add(v)
				series[k] = cur
			}
		}

		if latN > 0 {
			acc.LatSamples = latN
			n := float64(latN)
			acc.LatMeanMs = latSum / n
			if variance := latSumSq/n - acc.LatMeanMs*acc.LatMeanMs; variance > 0 {
				acc.LatStdDevMs = math.Sqrt(variance)
			}
		}
		if len(series) > 0 {
			acc.Series = series
		}
		out = append(out, acc)
	}
	return out
}

// Snapshot returns the last window of buckets, oldest first.
//
// The newest bucket is the one containing now, still filling — a client
// rendering it should expect its final value to rise. Reporting it partially is
// better than withholding it: an operator watching a live chart wants the
// current minute visible.
//
// An unknown session yields a snapshot of all-zero buckets rather than an error:
// a session that has produced no priceable traffic yet is a normal state, and
// the caller already knows whether the session exists from /v1/sessions.
func (a *Aggregator) Snapshot(window, resolution time.Duration, sessionID string, group Group) Snapshot {
	if resolution < BucketWidth {
		resolution = BucketWidth
	}
	n := int(window / BucketWidth)
	if n < 1 {
		n = 1
	}
	if n > NumBuckets {
		n = NumBuckets
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	ring := a.all
	if sessionID != "" {
		r, ok := a.sessions[sessionID]
		if !ok {
			ring = nil // fall through: emits zeroed buckets at the right times
		} else {
			ring = r.buckets
		}
	}

	newest := a.now().Truncate(BucketWidth)
	out := Snapshot{
		Window:        window.String(),
		BucketSeconds: int(resolution / time.Second),
		Session:       sessionID,
		Group:         group,
		Buckets:       make([]Bucket, 0, n),
	}

	for i := n - 1; i >= 0; i-- {
		t := newest.Add(-time.Duration(i) * BucketWidth)
		b := Bucket{At: t}
		if ring != nil {
			if src := &ring[slot(t)]; src.start.Equal(t) {
				b.Counts = src.Counts
				b.LatMeanMs, b.LatStdDevMs = src.latStats()
				b.LatSamples = src.latN
				b.Series = src.series(group)
			}
		}
		out.Totals.Add(b.Counts)
		if ring != nil {
			if src := &ring[slot(t)]; src.start.Equal(t) {
				for k, v := range src.byUnpriced {
					if out.UnpricedBy == nil {
						out.UnpricedBy = make(map[string]int64, len(src.byUnpriced))
					}
					out.UnpricedBy[k] += v.Requests
				}
				for k, v := range src.byProvenance {
					if out.PricedBy == nil {
						out.PricedBy = make(map[string]int64, len(src.byProvenance))
					}
					out.PricedBy[k] += v.Requests
				}
			}
		}
		out.Buckets = append(out.Buckets, b)
	}
	// Derived after the loop: Totals is only complete once every bucket has been
	// added, so this cannot be set in the literal above.
	out.Priced = out.Totals.PricedRequests > 0

	// Fold last: totals are summed from the raw buckets above and are unaffected
	// by grouping width, so a client's summary line agrees with its chart no
	// matter which resolution it asked for.
	out.Buckets = fold(out.Buckets, resolution)
	return out
}

// latStats derives mean and population standard deviation from the running
// sums. Guarded against a small negative variance, which float cancellation can
// produce when every sample is identical.
func (b *bucket) latStats() (mean, stddev float64) {
	if b.latN == 0 || b.latSum == 0 {
		return 0, 0
	}
	// Divide by the number of MEASURED requests, not all of them. A response with
	// a zero duration is unmeasured, not instant, and counting it in the divisor
	// understates the mean in proportion to how many such responses there were.
	n := float64(b.latN)
	mean = b.latSum / n
	variance := b.latSumSq/n - mean*mean
	if variance <= 0 {
		return mean, 0
	}
	return mean, math.Sqrt(variance)
}

// series copies the requested label map. Copied, not aliased: the caller holds
// only a read lock, and handing out the live map would let a JSON encoder read
// it while a later Record mutates it.
func (b *bucket) series(g Group) map[string]Counts {
	var src map[string]Counts
	switch g {
	case GroupMethod:
		src = b.byMethod
	case GroupStatus:
		src = b.byStatus
	case GroupPlugin:
		src = b.byPlugin
	case GroupHost:
		src = b.byHost
	default:
		return nil
	}
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]Counts, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
