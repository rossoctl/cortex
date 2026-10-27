//go:build race

package timeline

// raceEnabled reports a -race build, under which everything runs several times slower; see
// TestBuildBudget.
const raceEnabled = true
