package usage

import (
	"net"
	"strings"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
)

// unlabelledLabel is the reserved key for a caveat a producer disclosed WITHOUT saying
// which kind it was: an event from a version predating the field that names it.
//
// One spelling for both maps that need one — PricedBy's provenance and IncompleteBy's
// reason — because it is the same situation in both: the producer stated the fact and not
// the label. It cannot collide with a real value; pricing's reasons are hyphenated
// lowercase ("output-uncounted", "split-unreported") and its provenance levels are
// ("authoritative", "configured", "discovered", "bundled").
//
// LABELLED, never dropped, so IncompleteBy's counts sum to Counts.IncompleteRequests. A
// client subtracting the map from the counter to find "the rest" must get zero; silently
// omitting a row would make that difference read as requests whose figures are exact.
const unlabelledLabel = "unlabelled"

// maxLabelsPerBucket caps distinct keys in one bucket's grouping map.
//
// The model name comes off the request body, so its cardinality is set by
// whatever the client sends, not by anything this process controls. Unbounded, a
// caller varying the model string every request would add a retained map entry
// and a retained string per request, in a ring that only frees a slot when it is
// reused a full lap later — 6h at one minute. That is memory growth driven from
// off-host, on the synchronous session-append path.
//
// 64 is well above any real deployment: a pod talks to a handful of models, a
// few status codes and its own plugin list. Past the cap, further keys fold into
// overflowLabel so the totals stay correct and the excess is visible rather than
// silently dropped.
const maxLabelsPerBucket = 64

// overflowLabel collects everything past maxLabelsPerBucket. Named rather than
// dropped so a chart's segments still sum to the bucket total, and so an
// operator can see that cardinality was capped instead of wondering why a model
// is missing.
const overflowLabel = "(other)"

// maxLabelLen bounds one retained label; see event.MaxLabelLen.
const maxLabelLen = event.MaxLabelLen

// hostLabel reduces an :authority to the host alone, so "api.anthropic.com:443"
// and "api.anthropic.com" are one series rather than two.
//
// net.SplitHostPort, not a Cut on ":": an IPv6 literal is full of colons, and
// splitting on the first turns "[::1]:9094" into "[". SplitHostPort errors on an
// authority carrying no port at all, so that case falls through to the input.
//
// The brackets then have to come off separately, or IPv6 splits the very way this
// function exists to prevent: SplitHostPort unwraps them when it succeeds
// ("[::1]:9094" -> "::1"), leaving a port-less "[::1]" as its own band for the
// same upstream.
//
// A port with no host (":443") reduces to "", which the caller treats as absent
// and folds into the renderer's "(unlabelled)" remainder. That is the right
// answer for an authority that names no host: there is nothing to label it with,
// and inventing a band for it would be worse than counting it as unattributed.
//
// The same reduction exists in three listeners and in agentop's events pane, each
// private to its package; this is a fourth rather than a shared helper because
// core/cost/usage must not import a listener, and hoisting one is a refactor this
// change does not need.
func hostLabel(authority string) string {
	if h, _, err := net.SplitHostPort(authority); err == nil {
		return h
	}
	// No port: still strip brackets, so a bare "[::1]" matches the "::1" that the
	// ported form reduces to.
	return strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]")
}

// truncateLabel caps a label at maxLabelLen BYTES, cutting on a rune boundary.
func truncateLabel(s string) string {
	return model.TruncateLabel(s, maxLabelLen)
}

// ringLabel prepares one string for a bucket's label map: sanitised, then capped.
//
// THE RING IS A SERVING SURFACE OF ITS OWN. The model comes off the parsed request body and
// the endpoint is the host the workload asked for, so without this GET /v1/usage serves a
// model name containing an ANSI escape straight out of memory, while the ledger's copy of the
// same label is clean. Both sanitize with model.SanitizeControlRunes, so one hostile label
// is one series in both halves. CWE-150.
//
// THE ORDER MATTERS, for the reason ledger.rowLabel gives: sanitising can triple a
// string's length — every replaced byte becomes a 3-byte U+FFFD — so capping first would let
// 96 control bytes become 288 and break the bound.
func ringLabel(s string) string {
	return truncateLabel(model.SanitizeControlRunes(s))
}

// addLabel adds c under key, or under overflowLabel once the map is full, and returns the key it
// used — which a caller keeping a second tally in step with this one must key on.
func addLabel(m *map[string]Counts, key string, c Counts) string {
	if *m == nil {
		*m = make(map[string]Counts, 4)
	}
	// Reserve the last slot for overflowLabel: switching to it only once the map
	// is already full would make the overflow key itself the (cap+1)th entry.
	if _, ok := (*m)[key]; !ok && len(*m) >= maxLabelsPerBucket-1 {
		key = overflowLabel
	}
	cur := (*m)[key]
	cur.Add(c)
	(*m)[key] = cur
	return key
}
