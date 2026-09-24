package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/rossoctl/cortex/authbridge/authlib/observe/claude"
)

const experimentalUsage = `abctl experimental — unstable helpers, no compatibility promise

Usage:
  abctl experimental read-claude-sessions [--dir PATH]

Actions:
  read-claude-sessions   harvest session titles from Claude Code's own transcripts
                         into ~/.cortex/session-metadata.json. Run
                         "abctl experimental read-claude-sessions --help" for the detail.

Everything under this verb may change or disappear in any release, including the
shape of any file it writes. The namespace exists so a half-built idea can ship and
be used without the rest of abctl having to promise it forever — if something here
proves itself, it graduates to its own subcommand and this spelling goes away.

Exit status: 0 done, 1 something went wrong, 2 a usage error.
`

const readClaudeSessionsUsage = `abctl experimental read-claude-sessions — name Cortex's sessions from Claude Code's transcripts

Usage:
  abctl experimental read-claude-sessions [--dir PATH]

Cortex buckets traffic by session id, and a session id is a UUID. Claude Code knows
more about the same session: it writes a transcript per session under its config
directory, carrying a model-generated title and the directory the session ran in.
This reads those transcripts and writes what it finds to
~/.cortex/session-metadata.json, keyed by the same UUID Cortex uses — so a reader can
put a name next to a row. "abctl observe" is that reader, and does this by default.

Each entry carries a title, the agent type ("Claude Code"), the config directory it
came from, and the transcript it was read from. The title is the transcript's
model-generated one when it has one, and otherwise the working directory: most
sessions never get a title, so most entries name a directory.

The config directory is ` + claude.ConfigDirEnv + ` when set, and ~/.claude otherwise.
Only transcripts one level down (projects/<project>/<id>.jsonl) are read: deeper
files are subagent transcripts, whose names are not session ids.

By default a run UPSERTS: entries for the sessions it finds are added or updated, and
entries already in the file are left alone. That matters because a harvest only sees
the sessions one config directory holds, so replacing the file would silently drop
metadata for anything else it had — another config directory, or a session whose
transcript Claude Code has since pruned. --merge=false rebuilds the file from this
harvest alone, which is the way to drop stale entries on purpose.

Two consequences worth knowing. Under the default, an entry stays once written: a session
whose transcript Claude Code has pruned keeps its title indefinitely, because no later
harvest sees it again to notice it is gone, so the file grows with sessions-ever-seen.
--merge=false is what prunes it -- and it drops every entry this run did not see, including
entries harvested from a different --dir, so running it with a narrower --dir than the one
that built the file discards the difference.

abctl observe reads this file to name sessions in the sessions table; see
"abctl observe --help".

Flags:
  --dir PATH     config directory to read instead of ` + claude.ConfigDirEnv + ` / ~/.claude
  --merge=false  replace the file instead of upserting into it

Exit status: 0 done, 1 the directory could not be read or the file could not be
written, 2 a usage error.
`

// runExperimental dispatches on the action name. Returns the process exit code.
func runExperimental(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, experimentalUsage)
		return 2
	}
	action := args[0]
	// Same shape as `abctl configure` and `abctl service`: an explicit --help is a
	// request for the list, so it must not be read as the name of an action that does
	// not exist. Explicit help goes to stdout at exit 0, which is what makes it
	// pipeable; a missing or wrong action stays an error on stderr at exit 2.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, experimentalUsage)
		return 0
	}

	switch action {
	case "read-claude-sessions":
		return runReadClaudeSessions(args[1:], stdout, stderr)
	default:
		// The named list answers a typo; the usage block after it answers "what else
		// can this do", which is what someone who guessed wrong most likely wanted.
		fmt.Fprintf(stderr, "abctl: unknown experimental action %q (read-claude-sessions)\n", action)
		fmt.Fprint(stderr, experimentalUsage)
		return 2
	}
}

// runReadClaudeSessions harvests Claude Code's transcripts into
// ~/.cortex/session-metadata.json. Returns the process exit code.
func runReadClaudeSessions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("experimental read-claude-sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Silenced, and printed from the Parse result instead: Parse calls Usage for a bad
	// flag as well as for -h, and only its return value says which happened. Scanning
	// argv for "--help" cannot tell them apart, because a flag may consume it as a
	// VALUE — `--dir --help` would then put the whole usage on stdout while the error
	// went to stderr, splitting one failure across both streams.
	fs.Usage = func() {}
	dir := fs.String("dir", "", "Claude Code config directory")
	// Default TRUE: the file is keyed by session id, and a harvest only ever sees the
	// sessions its config dir holds. Overwriting would therefore make a second run
	// with a different --dir, or a run after Claude Code pruned its own transcripts,
	// silently drop entries the file already had — losing metadata that nothing else
	// records. Upserting is the behaviour that cannot lose data; --merge=false is the
	// explicit way to ask for a clean rebuild.
	merge := fs.Bool("merge", true, "upsert into the existing file rather than replacing it")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, readClaudeSessionsUsage)
			return 0
		}
		fmt.Fprint(stderr, readClaudeSessionsUsage)
		return 2
	}
	// Positional arguments are a mistake, not something to ignore: this command takes
	// none, so `read-claude-sessions ~/.claude` most likely means the user meant --dir
	// and silently reading the default instead is a worse answer than saying so.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "abctl: unexpected argument %q (did you mean --dir %s?)\n", fs.Arg(0), fs.Arg(0))
		return 2
	}

	// Everything below the flags is claude.Harvest's job; this function's remaining work
	// is reporting it. NOT incremental: this command's whole subject IS the harvest, so it
	// re-reads every transcript — that is what makes it the way to recover a file whose
	// entries are wrong, and what --merge=false needs in order to rebuild from scratch.
	// `abctl observe` is the incremental caller.
	res, err := claude.Harvest(claude.Options{ConfigDir: *dir, Merge: *merge})
	if err != nil {
		if errors.Is(err, claude.ErrCorruptMetadata) {
			// NOW ONLY THE UNREADABLE FILE reaches here: one that does not parse is rebuilt by
			// Harvest itself. This one could not be read at all, so --merge=false is no longer
			// the thing to suggest — it would hit the same read. Name what a human can do.
			fmt.Fprintf(stderr, "abctl: %v\n"+
				"  Fix the file's permissions, or move it aside and re-run.\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}

	// Warned about, and not fatal: these sessions have a title, it just may be an older one
	// than the transcript's last claim. Capped at three names plus a count, so a systemic
	// problem is as visible as a single bad file without burying the result.
	if len(res.Partial) > 0 {
		fmt.Fprintf(stderr, "abctl: %d transcript(s) read incompletely; their titles may be stale:\n", len(res.Partial))
		for i, msg := range res.Partial {
			if i == 3 {
				fmt.Fprintf(stderr, "  ... and %d more\n", len(res.Partial)-3)
				break
			}
			fmt.Fprintf(stderr, "  %s\n", msg)
		}
	}

	if res.Recovered > 0 {
		fmt.Fprintf(stderr, "abctl: merged %d entry(s) written concurrently by another run\n", res.Recovered)
	}

	// Said out loud because the counts cannot show it: a rebuild reports everything harvested
	// and nothing kept, which is exactly what a first run reports. The entries it dropped —
	// sessions whose transcripts are gone — leave no trace for the operator to notice.
	if res.Rebuilt {
		fmt.Fprintf(stderr, "abctl: the existing file could not be parsed; rebuilt it from %s\n", res.ConfigDir)
	}

	// Reported rather than silent: the count is the only way to notice that a wrong
	// --dir found nothing, and zero is not an error — a machine that has never run
	// Claude Code legitimately has no transcripts.
	//
	// Under --merge the total alone would be ambiguous: "wrote 109" reads the same
	// whether this run harvested all 109 or harvested 2 and kept 107 from the file.
	// Both numbers are reported so a wrong --dir is visible even when the file already
	// held a good harvest.
	//
	// Keyed on *merge alone, not on the totals differing. Gating on len(meta) != harvested
	// dropped the breakdown in exactly the case it was needed: when the harvest is a
	// SUPERSET of the file, kept is 0 and the totals match, so a --merge run printed the
	// same bare line as --merge=false and the operator could not tell which they had got.
	if *merge {
		fmt.Fprintf(stdout, "Wrote %d session(s) to %s (%d from %s, %d kept from the existing file)\n",
			res.Total, res.Path, res.Harvested, res.ConfigDir, res.Kept)
	} else {
		fmt.Fprintf(stdout, "Wrote %d session(s) to %s\n", res.Total, res.Path)
	}
	if res.Harvested == 0 {
		fmt.Fprintf(stdout, "No transcripts under %s/projects — is that the right config directory?\n", res.ConfigDir)
	}
	return 0
}
