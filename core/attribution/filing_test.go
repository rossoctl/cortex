package attribution

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

func TestFilingRules(t *testing.T) {
	pending := Attribution{Agent: "claude-code", Proc: "100.100000", Source: pipeline.SessionSourcePending}
	pendingID := session.PendingID("claude-code", "100.100000")
	header := Attribution{Session: "s1", Header: session.ClaudeCodeSessionHeader, Source: pipeline.SessionSourceHeader}
	process := Attribution{Session: "s2", Source: pipeline.SessionSourceProcess}
	noAgent := Attribution{Session: session.DefaultSessionID}

	for _, tc := range []struct {
		name       string
		at         Attribution
		trusted    string
		active     string
		wantPlugin string
		wantSource pipeline.SessionSource
		wantRecord string
	}{
		{"header id", header, "", "other", "s1", pipeline.SessionSourceHeader, "s1"},
		{"process id", process, "", "other", "s2", pipeline.SessionSourceProcess, "s2"},
		{"pending process", pending, "", "other", pendingID, pipeline.SessionSourcePending, pendingID},
		// A process of no agent: recorded under default, and no plugin session, not even the active one.
		{"default", noAgent, "", "other", "", "", session.DefaultSessionID},
		{"active session", Attribution{}, "", "other", "other", pipeline.SessionSourceActive, "other"},
		{"nothing known", Attribution{}, "", "", "", "", session.DefaultSessionID},
		{"trusted id wins over a header", header, "vm-1", "other", "vm-1", pipeline.SessionSourceConnection, "vm-1"},
		{"trusted id wins over pending", pending, "vm-1", "other", "vm-1", pipeline.SessionSourceConnection, "vm-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active := func() string { return tc.active }
			if sid, src := tc.at.PluginSession(tc.trusted, active); sid != tc.wantPlugin || src != tc.wantSource {
				t.Errorf("PluginSession = (%q, %q), want (%q, %q)", sid, src, tc.wantPlugin, tc.wantSource)
			}
			if got := tc.at.RecordSession(tc.trusted, active); got != tc.wantRecord {
				t.Errorf("RecordSession = %q, want %q", got, tc.wantRecord)
			}
		})
	}
}

func TestFiledSession(t *testing.T) {
	for _, tc := range []struct {
		name, pinned, active, want string
	}{
		{"response follows its request's pin", "s1", "other", "s1"},
		{"tunnel with no chain reads the active session", "", "other", "other"},
		{"nothing pinned or active goes to default", "", "", session.DefaultSessionID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FiledSession(tc.pinned, func() string { return tc.active }); got != tc.want {
				t.Errorf("FiledSession(%q) = %q, want %q", tc.pinned, got, tc.want)
			}
		})
	}
}

// The active session is read when a row is written, not before: a pinned row and a trusted
// id never read it, and a tunnel row reads it each time (#1187).
func TestFilingReadsActiveOnlyWhenNeeded(t *testing.T) {
	calls := 0
	active := func() string { calls++; return "now" }
	FiledSession("s1", active)
	Attribution{}.RecordSession("vm-1", active)
	Attribution{}.PluginSession("vm-1", active)
	process := Attribution{Session: "s2", Source: pipeline.SessionSourceProcess}
	process.RecordSession("", active)
	process.PluginSession("", active)
	if calls != 0 {
		t.Fatalf("active read %d times for answers that do not need it", calls)
	}
	if FiledSession("", active) != "now" || calls != 1 {
		t.Fatalf("FiledSession did not read active at call time (calls %d)", calls)
	}
}
