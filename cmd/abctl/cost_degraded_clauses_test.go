package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// EVERY COUNTER usage.Degraded CARRIES MUST HAVE A CLAUSE in costDegradedText, and this asserts
// the equality rather than leaving it to be maintained.
//
// A counter with no clause falls through to the "did not say how much it lost" branch, which is
// wrong on every word when the amount IS statable — and worse, is erased entirely the moment any
// other counter is also set, because that branch only renders when the clause list is empty:
//
//	only the unclaused counter   -> "…without saying how much it lost; rows are missing"
//	plus SkippedLines=3          -> "…skipped 3 unreadable lines"   <- the other number vanishes
//
// That is the defect the clause list was written to end, reproduced at the rendering step. It
// shipped once already: the field count and the clause count diverged and the existing table test
// had a case named "all four" and none for the fifth.
//
// BY REFLECTION over the struct, so a field added to usage.Degraded fails HERE rather than
// rendering a wrong sentence in production. Each counter is set alone, and its own number has to
// appear in the output.
func TestCostDegradedText_HasAClauseForEveryCounter(t *testing.T) {
	dt := reflect.TypeOf(usage.Degraded{})
	if dt.NumField() < 3 {
		t.Fatalf("usage.Degraded has %d fields; the reflection is not seeing the type, so this "+
			"proves nothing", dt.NumField())
	}

	// A value that cannot appear by accident in any other part of the sentence.
	const marker = 37

	for i := 0; i < dt.NumField(); i++ {
		name := dt.Field(i).Name
		t.Run(name, func(t *testing.T) {
			if dt.Field(i).Type.Kind() != reflect.Int64 {
				t.Fatalf("Degraded.%s is %s, not int64: this test assumes counters and must be "+
					"updated with the type", name, dt.Field(i).Type.Kind())
			}
			d := &usage.Degraded{}
			reflect.ValueOf(d).Elem().Field(i).SetInt(marker)

			got := costDegradedText(d)
			if got == "" {
				t.Fatalf("Degraded{%s: %d} rendered nothing at all", name, marker)
			}
			if !strings.Contains(got, "37") {
				t.Errorf("Degraded{%s: %d} rendered %q, which does not state the number — so this "+
					"counter has no clause and fell through to the generic branch. Add one; see "+
					"costDegradedText's own comment.", name, marker, got)
			}
			// And it must not claim the amount is unknown while holding it.
			if strings.Contains(got, "without saying how much") {
				t.Errorf("Degraded{%s: %d} rendered %q — the generic \"did not say how much\" "+
					"branch, for a fault whose amount is right there", name, marker, got)
			}
		})
	}

	// MIXED WITH ANOTHER COUNTER, each number still appears. This is the half that the "all four"
	// table case covered and the missing-clause case did not: a clause list composes, a
	// fall-through does not, so an unclaused counter disappears in any mixture.
	for i := 0; i < dt.NumField(); i++ {
		name := dt.Field(i).Name
		if name == "SkippedLines" {
			continue
		}
		d := &usage.Degraded{SkippedLines: 3}
		reflect.ValueOf(d).Elem().Field(i).SetInt(marker)
		got := costDegradedText(d)
		if !strings.Contains(got, "37") {
			t.Errorf("Degraded{%s: %d, SkippedLines: 3} rendered %q — %s vanished when mixed "+
				"with a counter that does have a clause", name, marker, got, name)
		}
		// THE CLAUSE, not the digit. "3 " or " 3" cannot fail while the marker is 37: the other
		// counter renders inside a phrase, so " 37" satisfies " 3" and both halves of the
		// condition were false for every field — a third instance of the unreachable-assertion
		// class this PR fixes twice elsewhere. Naming the phrase also makes the failure legible:
		// what must survive the mixture is the skipped-lines clause, not the character "3".
		if !strings.Contains(got, "skipped 3 unreadable line") {
			t.Errorf("Degraded{%s: %d, SkippedLines: 3} rendered %q — the skipped-lines clause "+
				"vanished when mixed with %s", name, marker, got, name)
		}
	}
}

// Retention coverage is deliberately NOT a Degraded counter, and this pins that boundary.
//
// It says the window asked for days retention does not reach, which is a coverage statement the
// server can prove — not "rows are missing from the sum", which it cannot: nothing records the
// ledger's inception or what prune deleted, so a fresh install would report loss it never had.
// It is printed with the unpriced coverage gap instead.
func TestUsageDegraded_CarriesNoRetentionCoverageField(t *testing.T) {
	dt := reflect.TypeOf(usage.Degraded{})
	for i := 0; i < dt.NumField(); i++ {
		if n := dt.Field(i).Name; strings.Contains(n, "Retention") {
			t.Errorf("usage.Degraded.%s exists — retention coverage is not damage, and putting "+
				"it here makes costDegradedText render \"rows are missing\" for a deployment "+
				"that simply keeps less history than the window asked for", n)
		}
	}
	if _, ok := reflect.TypeOf(usage.Snapshot{}).FieldByName("DaysOutsideRetention"); !ok {
		t.Error("usage.Snapshot has no DaysOutsideRetention field, so the coverage statement " +
			"reaches no client at all")
	}
}
