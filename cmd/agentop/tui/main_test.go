package tui

import (
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/internal/testhome"
)

// TestMain fires every tick at once, so a test that runs a command tree gets each tick's
// message back immediately instead of waiting out the interval. A test about a tick's
// interval swaps in recordTicks. It also points the home directory at a temporary one for the
// whole run, so no test can read or write the real profile (see testhome).
func TestMain(m *testing.M) {
	tick = instantTick
	cleanup := testhome.Isolate()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// instantTick is tea.Tick with the interval already elapsed.
func instantTick(_ time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return fn(time.Now()) }
}

// recordTicks makes tick record each interval it is given, still firing at once, until the
// test ends. The returned func reads the intervals so far, and is safe beside commands
// built on other goroutines.
func recordTicks(t *testing.T) func() []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var got []time.Duration
	prev := tick
	tick = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		return instantTick(d, fn)
	}
	t.Cleanup(func() { tick = prev })
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}
