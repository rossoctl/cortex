package tui

import (
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestMain fires every tick at once, so a test that runs a command tree gets each tick's
// message back immediately instead of waiting out the interval. A test about a tick's
// interval swaps in recordTicks.
func TestMain(m *testing.M) {
	tick = instantTick
	os.Exit(m.Run())
}

// instantTick is tea.Tick with the interval already elapsed.
func instantTick(_ time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return fn(time.Now()) }
}

// recordTicks makes tick record each interval it is given into the returned slice, still
// firing at once, until the test ends.
func recordTicks(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	prev := tick
	tick = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
		got = append(got, d)
		return instantTick(d, fn)
	}
	t.Cleanup(func() { tick = prev })
	return &got
}
