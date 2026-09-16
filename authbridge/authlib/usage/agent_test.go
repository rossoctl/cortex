package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

func TestSnapshot_GroupAgentBreaksDownByClient(t *testing.T) {
	a := New()
	e1 := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e1.Client = &pipeline.EventClient{Name: "claude-code", Version: "2.1.14"}
	e2 := inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001)
	e2.Client = &pipeline.EventClient{Name: "opencode", Version: "0.4.2"}
	a.Record("s1", e1)
	a.Record("s1", e2)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	if got := series["claude-code/2.1.14"].InputTokens; got != 10 {
		t.Errorf("claude-code InputTokens = %d, want 10", got)
	}
	if got := series["opencode/0.4.2"].InputTokens; got != 20 {
		t.Errorf("opencode InputTokens = %d, want 20", got)
	}
}

func TestSnapshot_GroupAgentFoldsAnAbsentClientUnderUnknown(t *testing.T) {
	// "unknown" rather than "", because a blank row reads as a bug rather than as
	// unattributed traffic — and the savings work requires missing data and an
	// honest zero to be distinguishable.
	//
	// "unknown" is a RESERVED BUCKET, not an agent name. See EventClient.Label.
	a := New()
	e := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e.Client = nil
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	if _, blank := series[""]; blank {
		t.Error(`series has an "" key`)
	}
	if got := series["unknown"].InputTokens; got != 10 {
		t.Errorf("unknown InputTokens = %d, want 10", got)
	}
}

func TestSnapshot_GroupAgentDoesNotInventANameForAnUnrecognisedAgent(t *testing.T) {
	// The other half of the absence rule. An agent that WAS reported but matched no
	// known name must appear under its raw User-Agent, not fold into the reserved
	// "unknown" bucket: pooling it with untagged traffic is what would make a new
	// coding agent invisible until a parser update ships.
	a := New()
	e := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e.Client = pipeline.ParseUserAgent("SomeNewAgent/9.9")
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	if _, pooled := series["unknown"]; pooled {
		t.Error(`an unrecognised agent folded under "unknown"; it must keep its raw name`)
	}
	if got := series["SomeNewAgent/9.9"].InputTokens; got != 10 {
		t.Errorf("SomeNewAgent InputTokens = %d, want 10 (series: %v)", got, keys(series))
	}
}

func TestSnapshot_GroupAgentCoversNonInferenceTrafficToo(t *testing.T) {
	// byAgent has no inference guard, unlike byMethod. So this axis counts MCP and
	// tool traffic as well, and its denominator differs from group=model's — the
	// same hazard group=endpoint has, and it must be documented the same way.
	a := New()
	e := &pipeline.SessionEvent{
		At: time.Now(), Phase: pipeline.SessionResponse, StatusCode: 200, Host: "tool",
		Client: &pipeline.EventClient{Name: "claude-code", Version: "2.1.14"},
	}
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	if got := series["claude-code/2.1.14"].Requests; got != 1 {
		t.Errorf("Requests = %d, want 1 — non-inference traffic is attributed too", got)
	}
}

func TestSnapshot_GroupAgentBoundsTheRetainedLabel(t *testing.T) {
	// The label is derived from a request header, so an attacker sets its length.
	// byAgent must be bounded by maxLabelLen exactly as every other label map is;
	// a new accumulator that bypassed truncateLabel would be an unbounded retention
	// on the synchronous session-append path.
	a := New()
	e := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e.Client = pipeline.ParseUserAgent(strings.Repeat("A", 10_000))
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	for k := range series {
		if len(k) > maxLabelLen {
			t.Errorf("byAgent retained a %d-byte label, want <= %d", len(k), maxLabelLen)
		}
	}
}

func TestSnapshot_GroupAgentBoundsCardinality(t *testing.T) {
	// Cardinality is set off-host too: a caller varying its User-Agent every request
	// would otherwise add a retained map entry per request, in a ring that frees a
	// slot only a full lap later. Past the cap, keys fold into overflowLabel so the
	// segments still sum to the bucket total.
	a := New()
	for i := 0; i < maxLabelsPerBucket*3; i++ {
		e := inferenceEvent("m", 1, 0, 0, 0, 0, 0b1001)
		e.Client = pipeline.ParseUserAgent("agent-" + strings.Repeat("x", i%7) + "/" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		a.Record("s1", e)
	}

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	if len(series) > maxLabelsPerBucket {
		t.Errorf("byAgent holds %d labels, want <= %d", len(series), maxLabelsPerBucket)
	}
	if _, ok := series[overflowLabel]; !ok {
		t.Errorf("no %q key; excess cardinality must be visible, not dropped (got %d keys)",
			overflowLabel, len(series))
	}
}

func TestParseGroup_AcceptsAgent(t *testing.T) {
	got, err := ParseGroup("agent")
	if err != nil {
		t.Fatalf("ParseGroup(\"agent\"): %v", err)
	}
	if got != GroupAgent {
		t.Errorf("= %q, want %q", got, GroupAgent)
	}
}

func TestParseGroup_ErrorNamesAgent(t *testing.T) {
	// The message enumerates the valid set instead of echoing the caller's input
	// (see ParseGroup), so a group that is missing from it is undiscoverable.
	_, err := ParseGroup("nope")
	if err == nil {
		t.Fatal("ParseGroup(\"nope\") returned no error")
	}
	if !strings.Contains(err.Error(), "agent") {
		t.Errorf("error %q does not name agent", err)
	}
}
