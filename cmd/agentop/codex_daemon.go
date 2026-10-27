package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// codexDaemonPIDRel is where Codex records its app-server daemon, under CODEX_HOME.
//
// THE DAEMON IS WHY ENABLE CAN LOOK DONE AND NOT BE. Codex runs a long-lived background
// server (`codex app-server --managed-daemon`) that makes the model calls for every
// codex window, and a window is only a screen that hands it each prompt. The daemon
// reads $CODEX_HOME/.env when it starts and never again, so a daemon already running
// when enable writes that file keeps calling OpenAI directly: the window's own startup
// checks and telemetry reach Cortex, its prompts do not, and nothing says so. Disable has
// the mirror problem — the daemon keeps the proxy it started with, so it breaks if
// Cortex then stops. Measured on Codex 0.163.0: a daemon started on Oct 23 sent every
// prompt straight to chatgpt.com after an enable on Oct 27.
const codexDaemonPIDRel = "app-server-daemon/daemon.pid"

// codexDaemonRestartCmd is Codex's own command for applying a changed environment. It
// stops the daemon and starts it again, which ends any Codex work running through it.
const codexDaemonRestartCmd = "codex app-server daemon restart"

// codexDaemon is a running app-server daemon, as its pid file and the process table
// describe it.
type codexDaemon struct {
	PID     int
	Started string // the pid file's processStartTime, as Codex wrote it; may be empty
}

// codexDaemonAlive reports whether pid names a running process. Signal 0 delivers
// nothing; it only asks whether the process exists and can be signalled. A process of
// another user answers EPERM, which still means it is alive.
var codexDaemonAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// runningCodexDaemon returns the daemon recorded under codexHome when its process is
// still alive. ok is false when there is no pid file, it cannot be read, or the process
// it names has exited — a stale file Codex leaves behind after a crash or a reboot.
func runningCodexDaemon(codexHome string) (codexDaemon, bool) {
	b, err := os.ReadFile(filepath.Join(codexHome, codexDaemonPIDRel))
	if err != nil {
		return codexDaemon{}, false
	}
	var rec struct {
		PID              int    `json:"pid"`
		ProcessStartTime string `json:"processStartTime"`
	}
	if json.Unmarshal(b, &rec) != nil || !codexDaemonAlive(rec.PID) {
		return codexDaemon{}, false
	}
	return codexDaemon{PID: rec.PID, Started: rec.ProcessStartTime}, true
}

// noteCodexDaemon tells the operator, after enable or disable has changed envPath, that
// a running daemon still has the environment it started with, and how to apply the
// change. It warns rather than restarting: a restart ends every Codex session running
// through the daemon, in any terminal or editor, and that is the operator's call. A
// daemon that is not running needs nothing — the next codex starts one with the new file.
func noteCodexDaemon(envPath string, enabled bool, stdout io.Writer) {
	d, ok := runningCodexDaemon(filepath.Dir(envPath))
	if !ok {
		return
	}
	started := ""
	if d.Started != "" {
		started = ", started " + d.Started
	}
	effect := "Until it restarts, Codex's prompts go straight to OpenAI,\n  not through Cortex."
	if !enabled {
		effect = "Until it restarts, it still sends through Cortex, and\n  stops working if Cortex does."
	}
	fmt.Fprintf(stdout, "\nCodex's background app-server is running (pid %d%s)\n"+
		"  with the settings it started with. %s\n"+
		"  To apply the change now:\n"+
		"    %s   # ends any Codex session running through it\n",
		d.PID, started, effect, codexDaemonRestartCmd)
}
