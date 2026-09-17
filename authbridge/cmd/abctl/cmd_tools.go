package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/toolscan"
)

const toolsUsage = `abctl tools — read agent logs to measure tool use

Usage:
  abctl tools scan [--days N | --all] [--keep Name,Name] [--dir PATH] [--write CONFIG]

Actions:
  scan   consult local coding agent logs to determine agent tool use

Run "abctl tools scan --help" for the detail.
`

const toolsScanUsage = `abctl tools scan — consult local coding agent logs to determine agent tool use

Usage:
  abctl tools scan [--days N | --all] [--keep Name,Name] [--dir PATH] [--write CONFIG]

Currently scan only consults Claude Code logs.

Claude Code resends its entire tool manifest on every turn, which can consume
many tokens, often describing tools the agent will never call. scan measures tool
use, producing a YAML fragment that can be applied to Cortex configuration
causing some tools to be redacted from inference, saving token cost.

Flags:
  --days N        window in days to consider a tool "used" (default 30)
  --all           no window: every tool call in every transcript counts
  --keep LIST     comma-separated tool names to withhold from the candidate list
  --dir PATH      transcript directory (default ~/.claude/projects)
  --write CONFIG  patch the remove: list of the tool-prune entry in CONFIG in
                  place; without it, the YAML block is printed for you to paste

Transcripts record tools that were called, never tools that were offered, so a
name abctl does not recognise is never proposed for removal.
`

// thinEvidenceTools is the number of distinct called tools below which the scan
// warns that its proposal is aggressive. Chosen as a smell test, not a
// threshold with meaning: a real session touches Read/Edit/Bash and more within
// minutes, so fewer than this says "barely any history" rather than "these are
// the tools I use".
const thinEvidenceTools = 5

// runTools handles the `tools` subcommand. Returns the process exit code.
func runTools(args []string, stdout, stderr io.Writer) int {
	// `abctl tools --help` used to be read as an action name and answered with
	// "unknown subcommand", which sends someone asking what this command does to the
	// one place that refuses to say. Same fix, and the same stdout/exit-0 split, as
	// `abctl service --help`: an explicit request for help is a successful answer, so
	// it is pipeable; a missing or wrong action stays an error on stderr.
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprint(stdout, toolsUsage)
			return 0
		}
	}
	if len(args) == 0 || args[0] != "scan" {
		fmt.Fprint(stderr, toolsUsage)
		return 2
	}

	fs := flag.NewFlagSet("tools scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Silenced, and printed from the Parse result instead. Without either, `--help`
	// printed flag.PrintDefaults' bare "Usage of tools scan:" list — every flag
	// named, nothing saying what scan is for.
	//
	// Which STREAM it belongs on depends on why it is being printed, and only Parse
	// knows: it calls Usage for a bad flag as well as for -h. Deciding by scanning
	// argv for "--help" cannot tell the two apart, because a flag may consume it as
	// a VALUE — `--dir --help --nosuchflag` then put the whole help text on stdout
	// while the error went to stderr, splitting one failure across both streams.
	// ErrHelp is the flag package's own answer to which happened.
	fs.Usage = func() {}
	days := fs.Int("days", 30, "window in days")
	all := fs.Bool("all", false, "consider every transcript, with no recency window")
	keep := fs.String("keep", "", "comma-separated tool names to keep")
	dir := fs.String("dir", "", "transcript directory (default ~/.claude/projects)")
	write := fs.String("write", "", "patch the tool-prune remove: list in this config file")
	if err := fs.Parse(args[1:]); err != nil {
		// ErrHelp means Parse saw -h/--help, which is a successful answer and belongs
		// on stdout at exit 0. Every other parse failure is a usage error: Parse has
		// already written its own line to stderr, so the usage joins it there.
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, toolsScanUsage)
			return 0
		}
		fmt.Fprint(stderr, toolsScanUsage)
		return 2
	}
	// --days 0 is rejected rather than read as "everything": a zero-width window
	// finds nothing used, so it would propose removing every tool it knows —
	// the opposite of what someone reaching for 0 means. --all says it explicitly.
	if *days <= 0 {
		fmt.Fprintln(stderr, "abctl: --days must be positive (use --all for no window)")
		return 2
	}
	daysSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "days" {
			daysSet = true
		}
	})
	if *all && daysSet {
		fmt.Fprintln(stderr, "abctl: --all and --days are mutually exclusive")
		return 2
	}
	window := *days
	if *all {
		window = toolscan.AllTime
	}

	scanDir := *dir
	if scanDir == "" {
		d, err := toolscan.DefaultProjectsDir()
		if err != nil {
			fmt.Fprintf(stderr, "abctl: locating transcripts: %v\n", err)
			return 1
		}
		scanDir = d
	}

	res, err := toolscan.Scan(scanDir, window, strings.Split(*keep, ","))
	if err != nil {
		fmt.Fprintf(stderr, "abctl: scanning %s: %v\n", scanDir, err)
		return 1
	}
	if res.Files == 0 {
		fmt.Fprintf(stderr, "abctl: no transcripts found under %s — nothing to infer from\n", scanDir)
		return 1
	}

	fmt.Fprint(stdout, res.Summary(window))
	if *write == "" {
		fmt.Fprintln(stdout)
		fmt.Fprint(stdout, res.YAMLBlock())
		return 0
	}

	// Refuse to write a list inferred from no evidence. With zero observed tool
	// calls, "tools you have not called" degrades to "every tool I know about",
	// and the proposal above is that list — including ones a session genuinely
	// needs. This is not a rare edge: it is exactly a new install, where there is
	// little or no transcript history yet, which is also when someone is most
	// likely to accept the default. Printing the proposal is still useful; the
	// refusal is only about writing it unattended.
	if len(res.Called) == 0 {
		fmt.Fprintln(stdout)
		fmt.Fprint(stdout, res.YAMLBlock())
		// Describe the window that actually ran, and only suggest widening when
		// there is room to widen. Reporting "the last 30 day(s)" after --all was
		// both false and unactionable — it advised --days, which --all rejects.
		scope := fmt.Sprintf("the last %d day(s)", window)
		advice := "Use Claude Code for a while and re-run, widen the window with --days or --all,"
		if window <= toolscan.AllTime {
			scope = "any of your transcripts"
			advice = "Use Claude Code for a while and re-run,"
		}
		fmt.Fprintf(stderr, "\nabctl: not writing %s — the scan observed no tool calls in %s,\n"+
			"so it has no evidence for what you do not use. %s\n"+
			"or paste the block above yourself once you have checked it.\n",
			*write, scope, advice)
		return 1
	}

	// A single observed call clears the guard above and still proposes removing
	// nearly everything, because "not called" is measured against whatever little
	// was seen. The guard cannot be raised to an arbitrary N without blocking
	// legitimate light users, so warn instead and name the two ways out. Thin
	// evidence is a property of the input, not an error.
	if len(res.Called) < thinEvidenceTools {
		fmt.Fprintf(stderr, "\nabctl: thin evidence — only %d distinct tool(s) seen in %d transcript(s).\n"+
			"  A short history makes this list aggressive: anything unseen counts as unused.\n"+
			"  Consider --all, or check the list above before relying on it. Undo any name by\n"+
			"  deleting it from remove: in the config.\n", len(res.Called), res.Files)
	}

	changed, err := toolscan.PatchConfig(*write, res.Candidates)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	if changed {
		fmt.Fprintf(stdout, "\nUpdated remove: list in %s (%d tool(s)).\n", *write, len(res.Candidates))
		fmt.Fprintln(stdout, "The config is hot-reloaded; no restart needed.")
	} else {
		fmt.Fprintf(stdout, "\n%s already up to date.\n", *write)
	}
	return 0
}
