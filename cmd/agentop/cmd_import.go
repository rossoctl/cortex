package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/session/importer"
)

// importTimeout is the limit for one import. The proxy sends the header before the work starts,
// so this is the limit on the work, not on the header wait.
const importTimeout = time.Hour

// importAgents are the names --agent accepts. The proxy answers 400 for a name it has no source for.
var importAgents = []string{"claude-code", "bob", "codex", "opencode", "pi"}

// runImport asks the local Cortex to import agent transcripts into its session archive and its
// cost ledger, and prints one line for each agent.
func runImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentop import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agent := fs.String("agent", "", "import only this `agent` ("+strings.Join(importAgents, ", ")+"); omit for all")
	since := fs.String("since", "", "import only sessions changed after this `date`: YYYY-MM-DD (local midnight) or <N>d")
	dryRun := fs.Bool("dry-run", false, "count what an import does, and write nothing")
	endpoint := fs.String("endpoint", "",
		"session API URL of the proxy (default: the Cortex installed on this machine)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `agentop import — import agent transcripts into Cortex

Usage:
  agentop import                          import every agent the proxy has a source for
  agentop import --agent claude-code      import only Claude Code
  agentop import --since 30d              import only sessions changed in the last 30 days
  agentop import --dry-run                count what an import does, and write nothing

The proxy reads the transcripts and writes them to its session archive and its cost
ledger, marked as imported. It does not change a session that it captured live. An
import again of an erased session brings the session back.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentop import: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *agent != "" && !slices.Contains(importAgents, *agent) {
		fmt.Fprintf(stderr, "agentop import: --agent %q is not known; use one of %s\n", *agent, strings.Join(importAgents, ", "))
		return 2
	}
	req := importer.Request{Agent: *agent, DryRun: *dryRun}
	if *since != "" {
		t, err := parseImportSince(*since, time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "agentop import: %v\n", err)
			return 2
		}
		req.Since = t
	}

	target := *endpoint
	if target == "" {
		target = localSessionEndpoint()
	}
	if target == "" {
		if why := localConfigProblem(); why != "" {
			fmt.Fprintf(stderr, "agentop import: no --endpoint given, and %s\n", why)
			fmt.Fprintln(stderr, "  fix the file; `agentop service status` says whether Cortex is still serving")
			return 1
		}
		fmt.Fprintln(stderr, "agentop import: no --endpoint given and no local Cortex is configured")
		fmt.Fprintln(stderr, "  is Cortex installed and running? `agentop service status`")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()
	onProgress, stopProgress := startProgress(stderr)
	rep, err := apiclient.New(target).Import(ctx, req, onProgress)
	stopProgress()
	switch {
	case errors.Is(err, apiclient.ErrImportRunning):
		fmt.Fprintln(stderr, "agentop import: an import is already running")
		return 1
	case errors.Is(err, apiclient.ErrNotFound):
		fmt.Fprintln(stderr, "agentop import: the proxy has no import route")
		fmt.Fprintln(stderr, "  the session archive is off, or the proxy is not bound to loopback only")
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "agentop import: %v\n", err)
		return 1
	}

	prefix := ""
	if rep.DryRun {
		prefix = "dry run: "
	}
	code := 0
	for _, a := range rep.Agents {
		fmt.Fprintf(stdout, "%s%s: %d imported, %d replaced, %d skipped (live), %d skipped (current), %d skipped (expired), %d lines not recognized\n",
			prefix, a.Agent, a.Imported, a.Replaced, a.SkippedLive, a.SkippedCurrent, a.SkippedExpired, a.SkippedLines)
		if a.Unreadable > 0 {
			fmt.Fprintf(stdout, "  unreadable: %d transcripts could not be read\n", a.Unreadable)
		}
		if a.Stopped != "" {
			fmt.Fprintf(stdout, "  stopped: %s, %d sessions not imported\n", a.Stopped, a.NotImported)
		}
		if a.Error != "" {
			fmt.Fprintf(stdout, "  error: %s\n", a.Error)
			code = 1
		}
	}
	return code
}

// parseImportSince reads --since: YYYY-MM-DD is local midnight of that day, and <N>d is N times
// 24 hours before now.
func parseImportSince(s string, now time.Time) (time.Time, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 1 {
			return time.Time{}, fmt.Errorf("--since %q: use YYYY-MM-DD or <N>d with N of 1 or more", s)
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	t, err := time.ParseInLocation(time.DateOnly, s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: use YYYY-MM-DD or <N>d", s)
	}
	return t, nil
}

// startProgress shows the import's progress on w, only when w is a terminal: a spinner until
// the proxy sends its first Progress line, and then a bar. onProgress is nil when w is not a
// terminal, so the request asks for no progress. stop ends the display and clears the line.
func startProgress(w io.Writer) (onProgress func(importer.Progress), stop func()) {
	if !isTerminal(w) {
		return nil, func() {}
	}
	var mu sync.Mutex
	var last *importer.Progress
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		frames := `|/-\`
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			mu.Lock()
			if last == nil {
				fmt.Fprintf(w, "\rimporting %c", frames[i%len(frames)])
			} else {
				fmt.Fprintf(w, "\r\033[K%s", progressBar(*last))
			}
			mu.Unlock()
			select {
			case <-done:
				fmt.Fprint(w, "\r\033[K")
				return
			case <-tick.C:
			}
		}
	}()
	return func(p importer.Progress) {
			mu.Lock()
			last = &p
			mu.Unlock()
		}, func() {
			close(done)
			<-stopped
		}
}

// progressBarWidth is the number of cells in the bar.
const progressBarWidth = 20

// progressBar is one line: "importing claude-code [█████░░░]  25%  1/4 sessions".
func progressBar(p importer.Progress) string {
	pct := 100
	if p.Total > 0 {
		pct = min(100, max(0, p.Done*100/p.Total))
	}
	full := pct * progressBarWidth / 100
	return fmt.Sprintf("importing %s [%s%s] %3d%%  %d/%d sessions", p.Agent,
		strings.Repeat("█", full), strings.Repeat("░", progressBarWidth-full), pct, p.Done, p.Total)
}
