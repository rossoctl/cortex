package main

// tuistates walks the agentop beats and turns each settled screen into a state.

import (
	"fmt"
	"time"
)

// firstBeatStagger is how long the opening screen takes to paint itself in, row
// by row. The table is static by the time it is captured — agentop is not animating
// anything — so the reveal is the emitter's, and it exists because a table that
// simply appears reads as a screenshot while one that fills reads as a live tool.
const firstBeatStagger = 70 * time.Millisecond

// TUIStates captures one screen per beat from a real agentop model.
func TUIStates(a Act, start time.Duration, g Grid, home, ledgerDir string) ([]State, error) {
	var sum time.Duration
	for _, b := range a.Beats {
		if b.Hold <= 0 {
			return nil, fmt.Errorf("act %q: beat %q needs a positive hold", a.Name, b.Label)
		}
		sum += b.Hold
	}
	if sum != a.Runtime {
		return nil, fmt.Errorf("act %q: beat holds sum to %s but the act is %s", a.Name, sum, a.Runtime)
	}

	if err := WriteTitles(home, a.Fixture); err != nil {
		return nil, fmt.Errorf("session titles: %w", err)
	}
	c, err := NewCapturer(a.Fixture, g.Cols, g.Rows, ledgerDir, a.Beats[0].At)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	var (
		states []State
		at     = start
	)
	for bi, b := range a.Beats {
		if a.Fixture.Replay != nil {
			c.advance(b.At)
		}
		c.Press(b.Keys...)
		rows := screenRows(c.Screen(), g)
		if bi == 0 {
			staggerRows(rows)
		}
		states = append(states, State{
			Label: b.Label,
			Alt:   b.Alt,
			Rows:  rows,
			At:    at,
			Dur:   b.Hold,
		})
		at += b.Hold
	}
	return states, nil
}

// staggerRows makes a captured screen paint in from the top.
func staggerRows(rows []Row) {
	n := 0
	for i := range rows {
		if len(rows[i].Runs) == 0 {
			continue
		}
		rows[i].Reveal = Reveal{Kind: "fill", At: time.Duration(n) * firstBeatStagger}
		n++
	}
}
