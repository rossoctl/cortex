package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// updateStatusFile is the file that agentop update --auto writes in ~/.cortex.
const updateStatusFile = "update.json"

const (
	updateFirstWait = 10 * time.Minute
	updateTick      = time.Hour
	updateEvery     = 24 * time.Hour
)

// updateTrigger starts the updater unit at most once a day. It reads no config
// and makes no network call: agentop update --auto makes all the decisions.
type updateTrigger struct {
	file      string           // ~/.cortex/update.json
	now       func() time.Time // wall clock
	start     func() error     // starts the updater unit
	lastStart time.Time        // zero: never
}

// tick starts the updater when the status file is missing or older than a day.
// It returns true when it called start.
func (t *updateTrigger) tick() bool {
	now := t.now().Round(0) // wall clock: a monotonic Sub leaves out sleep
	if fi, err := os.Stat(t.file); err == nil && now.Sub(fi.ModTime()) <= updateEvery {
		return false
	}
	// A run that stops before it writes the file must not cause a start each hour.
	if !t.lastStart.IsZero() && now.Sub(t.lastStart) < updateEvery {
		return false
	}
	t.lastStart = now
	if err := t.start(); err != nil {
		slog.Debug("cortex: cannot start the updater unit", "error", err)
	}
	return true
}

// startUpdaterUnit starts the updater unit that agentop service install writes.
func startUpdaterUnit() error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("launchctl", "kickstart", "gui/"+strconv.Itoa(os.Getuid())+"/io.rossoctl.cortex.update").Run()
	case "linux":
		return exec.Command("systemctl", "--user", "start", "--no-block", "cortex-update.service").Run()
	}
	return errors.New("no updater unit on " + runtime.GOOS)
}

// runUpdateTrigger calls tick after updateFirstWait, then each updateTick until
// ctx is done. Each tick compares wall clock times, because a 24-hour timer
// does not count the time that a laptop sleeps.
func runUpdateTrigger(ctx context.Context, t *updateTrigger) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(updateFirstWait):
	}
	t.tick()
	tk := time.NewTicker(updateTick)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.tick()
		}
	}
}
