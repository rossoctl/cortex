package toolprune

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/tidwall/gjson"
)

// wantChangeID is the spec's ChangeID for a removed tool (design §5.5).
func wantChangeID(name, decl string) string {
	sum := sha256.Sum256([]byte(decl))
	return "tool-prune:" + name + ":" + hex.EncodeToString(sum[:6])
}

func TestPrune_ReportsOneOpPerRemovedTool(t *testing.T) {
	decls := gjson.Get(anthropicBody, "tools").Array()
	type want struct {
		index int
		name  string
	}
	removed := []want{{0, "Read"}, {2, "Bash"}}

	check := func(t *testing.T, pctx *pipeline.Context, dryRun bool) []string {
		t.Helper()
		ops := pctx.ReportedOps()
		if len(ops) != len(removed) {
			t.Fatalf("ops = %+v, want %d", ops, len(removed))
		}
		var ids []string
		for i, w := range removed {
			op := ops[i]
			decl := decls[w.index].Raw
			if op.Kind != accountant.KindRewrite {
				t.Errorf("op %d kind = %q, want rewrite", i, op.Kind)
			}
			if op.At != (accountant.Position{Section: accountant.SectionTools, Index: w.index}) {
				t.Errorf("op %d At = %+v, want tools[%d], the tool's index in the client body", i, op.At, w.index)
			}
			if op.SizeBefore.Bytes != len(decl) || op.SizeAfter.Bytes != 0 {
				t.Errorf("op %d sizes = %d -> %d, want %d -> 0", i, op.SizeBefore.Bytes, op.SizeAfter.Bytes, len(decl))
			}
			if op.ChangeID != wantChangeID(w.name, decl) {
				t.Errorf("op %d ChangeID = %q, want %q", i, op.ChangeID, wantChangeID(w.name, decl))
			}
			if op.Operator != "tool-prune" {
				t.Errorf("op %d Operator = %q, want tool-prune", i, op.Operator)
			}
			if op.DryRun != dryRun {
				t.Errorf("op %d DryRun = %v, want %v", i, op.DryRun, dryRun)
			}
			ids = append(ids, op.ChangeID)
		}
		return ids
	}

	p := configured(t, "Read", "Bash")
	first := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p, first)
	ids := check(t, first, false)

	// The next turn carries the same declarations: the same ChangeIDs, so the library prices
	// the change once.
	second := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p, second)
	if again := check(t, second, false); again[0] != ids[0] || again[1] != ids[1] {
		t.Errorf("second request ChangeIDs = %v, want %v", again, ids)
	}

	// Under observe nothing is sent, so the ops are dry runs.
	observed := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, configured(t, "Read", "Bash"), observed, pipeline.ErrorPolicyObserve)
	check(t, observed, true)
}

func TestPrune_ReportsNoOpWhenNothingIsRemoved(t *testing.T) {
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, configured(t, "NotInTheManifest"), pctx)
	if ops := pctx.ReportedOps(); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}
