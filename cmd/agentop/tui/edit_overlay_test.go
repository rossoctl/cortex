package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
)

func TestEditOverlayRender_Fetching(t *testing.T) {
	s := editState{phase: editPhaseFetching}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "Fetching config") {
		t.Fatalf("fetching phase missing message:\n%s", out)
	}
}

// One 120s ceiling sized for a kubelet ConfigMap sync made a local edit — which
// lands in about a second — take two minutes to admit it had failed, precisely
// when something was already wrong. A ceiling, not a wait: the poll returns as
// soon as last_success moves, so only the failure path is affected.
func TestTarget_DeadlineIsPerStore(t *testing.T) {
	cluster := edit.ConfigMapStore{}.Describe().Deadline()
	local := edit.FileStore{Path: "/x/config.yaml"}.Describe().Deadline()

	if cluster != edit.ClusterPollDeadline {
		t.Errorf("cluster deadline = %v, want %v", cluster, edit.ClusterPollDeadline)
	}
	if local != edit.LocalPollDeadline {
		t.Errorf("local deadline = %v, want %v", local, edit.LocalPollDeadline)
	}
	if local >= cluster {
		t.Errorf("local deadline %v should be shorter than the cluster's %v", local, cluster)
	}
	// A zero Target must not mean "give up immediately" — that would report a
	// timeout that never happened. It floors to the conservative ceiling.
	if got := (edit.Target{}).Deadline(); got != edit.ClusterPollDeadline {
		t.Errorf("zero Target deadline = %v, want the cluster ceiling %v", got, edit.ClusterPollDeadline)
	}
	// And the nil-store path the overlay uses gets the same floor.
	if got := describeTarget(nil).Deadline(); got != edit.ClusterPollDeadline {
		t.Errorf("nil-store deadline = %v, want the cluster ceiling", got)
	}
}

// The rollback messages are built in the genPolledMsg / genRolledBackMsg
// handlers, not the overlay renderer, so they were missed the first time and
// went on sending a local operator to kubectl about a ConfigMap that does not
// exist. Asserted on the Target directly: these strings are assembled from it.
func TestTarget_RollbackWordingIsPerStore(t *testing.T) {
	cm := edit.ConfigMapStore{}.Describe()
	if cm.Noun != "ConfigMap" || cm.OutOfSyncHint != "check kubectl" {
		t.Errorf("cluster target = %+v, want the ConfigMap/kubectl wording", cm)
	}

	fs := edit.FileStore{Path: "/home/u/.cortex/config.yaml"}.Describe()
	if fs.Noun == "ConfigMap" {
		t.Error("the local target must not call itself a ConfigMap")
	}
	// The path, not a tool: there is no kubectl locally, and the operator can
	// open the file.
	if !strings.Contains(fs.OutOfSyncHint, "/home/u/.cortex/config.yaml") {
		t.Errorf("local OutOfSyncHint = %q, want it to name the file", fs.OutOfSyncHint)
	}
	if strings.Contains(fs.OutOfSyncHint, "kubectl") {
		t.Errorf("local OutOfSyncHint sends the operator to kubectl: %q", fs.OutOfSyncHint)
	}
	// Every field a message is assembled from must be populated for both, or a
	// sentence renders with a hole in it.
	for name, tg := range map[string]edit.Target{"cluster": cm, "local": fs} {
		if tg.Noun == "" || tg.WaitHint == "" || tg.UnreachableHint == "" || tg.OutOfSyncHint == "" {
			t.Errorf("%s target has an empty field: %+v", name, tg)
		}
	}
}

// The overlay must name the target it is actually writing. A local edit that
// announced "Fetching ConfigMap…" described a thing that does not exist, and
// the cluster path's kubelet-sync wait is two orders of magnitude wrong for a
// file the proxy already watches.
func TestEditOverlayRender_NamesTheStoresOwnTarget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		store      edit.Store
		wantNoun   string
		wantWait   string
		rejectNoun string
	}{{
		name:       "cluster",
		store:      edit.ConfigMapStore{},
		wantNoun:   "Fetching ConfigMap",
		wantWait:   "kubelet",
		rejectNoun: "config file",
	}, {
		name:       "local",
		store:      edit.FileStore{Path: "/tmp/config.yaml"},
		wantNoun:   "Fetching config file",
		wantWait:   "about a second",
		rejectNoun: "ConfigMap",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderEditOverlay(editState{phase: editPhaseFetching, store: tc.store}, 80, 20)
			if !strings.Contains(got, tc.wantNoun) {
				t.Errorf("fetching overlay = %q, want it to contain %q", got, tc.wantNoun)
			}
			if strings.Contains(got, tc.rejectNoun) {
				t.Errorf("fetching overlay names the other backend (%q):\n%s", tc.rejectNoun, got)
			}
			waiting := renderEditOverlay(editState{phase: editPhaseWaiting, store: tc.store}, 80, 20)
			if !strings.Contains(waiting, tc.wantWait) {
				t.Errorf("waiting overlay = %q, want the %s wait hint (%q)", waiting, tc.name, tc.wantWait)
			}
		})
	}
}

func TestEditOverlayRender_Diff(t *testing.T) {
	s := editState{
		phase: editPhaseDiff,
		diff:  "-old line\n+new line\n",
	}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "old line") || !strings.Contains(out, "new line") {
		t.Fatalf("diff content missing:\n%s", out)
	}
	if !strings.Contains(out, "[y] apply  [n] cancel") {
		t.Fatalf("confirm prompt missing:\n%s", out)
	}
}

func TestEditOverlayRender_Applying(t *testing.T) {
	s := editState{phase: editPhaseApplying}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "Applying") {
		t.Fatalf("applying phase missing message:\n%s", out)
	}
}

func TestEditOverlayRender_Waiting(t *testing.T) {
	s := editState{phase: editPhaseWaiting}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "reload") {
		t.Fatalf("waiting phase should mention reload:\n%s", out)
	}
}

func TestEditOverlayRender_Error(t *testing.T) {
	s := editState{phase: editPhaseError, err: "kubectl: forbidden"}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "forbidden") {
		t.Fatalf("error message not surfaced:\n%s", out)
	}
}
