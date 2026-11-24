package main

// stills renders stills.yaml: one still SVG for each beat, for docs/using-agentop.md.
// The screens come from the same real agentop as the animated demo, so they cannot
// drift from the product either: TestCommittedStillsAreCurrent regenerates them.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// stillsDir is where the stills are committed.
var stillsDir = filepath.Join("..", "..", "docs", "assets", "agentop")

// runStills writes <outDir>/<beat label>.svg for each beat of each act in path.
func runStills(path, outDir string) error {
	s, err := Load(path)
	if err != nil {
		return err
	}
	return inScratch(func(home, ledger string) error {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		for i, a := range s.Acts {
			if a.Kind != "tui" {
				return fmt.Errorf("act %d (%q): stills need kind tui, got %q", i, a.Name, a.Kind)
			}
			states, err := TUIStates(a, 0, s.Grid, home, filepath.Join(ledger, fmt.Sprint(i)))
			if err != nil {
				return fmt.Errorf("act %d (%q): %w", i, a.Name, err)
			}
			for _, st := range states {
				if st.Alt == "" {
					return fmt.Errorf("act %q: beat %q needs alt text", a.Name, st.Label)
				}
				if err := writeStill(filepath.Join(outDir, st.Label+".svg"), st, s.Grid); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// writeStill emits one state as a still: no reveal, shown from start to end.
func writeStill(path string, st State, g Grid) error {
	for i := range st.Rows {
		st.Rows[i].Reveal = Reveal{}
	}
	st.At, st.Final = 0, true
	var b strings.Builder
	if err := emitSVG(&b, []State{st}, st.Dur, g, st.Alt); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%.0f KB)\n", path, float64(b.Len())/1024)
	return nil
}
