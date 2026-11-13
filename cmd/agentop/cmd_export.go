package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// exportTimeout bounds the one request this command makes, body read included.
//
// GENEROUS AGAINST THE WALK IT BOUNDS, deliberately: the server decodes every segment in
// the archive single-threaded, which measured ~10s for a 30-day archive, and this budget
// exists to catch a live-but-pathologically-slow walk — not to be tight against a healthy
// one. A proxy that stalls is caught sooner and elsewhere: HeaderTimeout (2 minutes)
// bounds the wait for the first byte, and the walk flushes a line per record, so silence
// on a live connection means no records, not no progress.
//
// Unlike costFetchTimeout this is not "the bound the request actually gets" — a caller
// scripting this client may pass its own deadline and apiclient defers to it. It is the
// budget THIS command asks for, which is the shape runCost gives costFetchTimeout.
const exportTimeout = 10 * time.Minute

// runExport writes the archive's content-free records to a file or stdout: the
// `GET /v1/export` endpoint as a command, for the analysis that happens outside the tool.
//
// Same shape as runCost: its own FlagSet writing to stderr, an exit code returned rather
// than os.Exit, so main owns process exit and a test can call this directly. stdout may
// carry the NDJSON itself, so every word this command prints for a human goes to stderr.
func runExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentop export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "",
		"write the export to this `file` (default: stdout, for piping)")
	force := fs.Bool("force", false,
		"overwrite --out when the file already exists; without it an existing file is refused")
	sessions := &sessionIDs{}
	fs.Var(sessions, "session",
		"export only this `session`, repeatable and comma-separated; omit for every archived session")
	since := fs.String("since", "",
		"export only events newer than this far back — a duration such as 168h, or a day count like 7d")
	endpoint := fs.String("endpoint", "",
		"session API URL of the proxy (default: the Cortex installed on this machine)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `agentop export — the session archive as content-free records

Usage:
  agentop export                       every archived event, one NDJSON line each, to stdout
  agentop export --out FILE            write the export to a file; --force overwrites
  agentop export --since 168h          only events from the last week
  agentop export --session ID          one session, repeatable or comma-separated
  agentop export --endpoint URL        ask a specific proxy rather than the local one

Each line is one archived event reduced to ~30 content-free scalars — timings, token
counts, hosts, models, outcomes — never message content, completions or tool arguments,
which is what makes the file safe to hand to anything the full events must not reach. A
month of traffic is about 12MB. The reading rules that keep a consumer from
misinterpreting the fields (zero means "not recorded", never a real zero; order by
"at" then "seq") are in Cortex's docs/session-export.md.

The export reads the durable archive, not memory: a session with nothing flushed yet
does not appear, and the live tail is /v1/events, not this.

An unknown --session or a malformed --since is refused by the proxy before it writes a
byte, naming as many of the sessions it does hold as the refusal has room for, then a
count of the rest; a proxy with no session archive (a cluster sidecar, or one predating
/v1/export) answers 404.

A 30-day archive walks in about ten seconds server-side; this command waits up to ten
minutes before giving up.

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
	if *force && *out == "" {
		fmt.Fprintln(stderr, "agentop export: --force is for --out; stdout cannot be overwritten")
		return 2
	}
	if fs.NArg() > 0 {
		// Every sibling command rejects stray positionals, and this is the one where a
		// stray positional hurts most: `agentop export out.ndjson` would dump a month of
		// NDJSON into the terminal and call it success.
		fmt.Fprintf(stderr, "agentop export: unexpected argument %q — write the export to a file with --out\n",
			fs.Arg(0))
		return 2
	}

	target := *endpoint
	if target == "" {
		target = localSessionEndpoint()
	}
	if target == "" {
		if why := localConfigProblem(); why != "" {
			fmt.Fprintf(stderr, "agentop export: no --endpoint given, and %s\n", why)
			fmt.Fprintln(stderr, "  fix the file; `agentop service status` says whether Cortex is still serving")
			return 1
		}
		fmt.Fprintln(stderr, "agentop export: no --endpoint given and no local Cortex is configured")
		fmt.Fprintln(stderr, "  is Cortex installed and running? `agentop service status`")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	body, err := apiclient.New(target).Export(ctx, sessions.ids, *since)
	if err != nil {
		fmt.Fprintf(stderr, "agentop export: %v\n", err)
		switch {
		case errors.Is(err, apiclient.ErrNotFound):
			// A reachable proxy with no archive — a cluster sidecar, or an install with
			// session.archive off — or one predating /v1/export entirely, which answers
			// the same 404 from an unregistered route. Different problem from a down
			// proxy, different fix.
			fmt.Fprintln(stderr, "  this proxy has no session archive, or predates /v1/export (`agentop update`); a cluster sidecar never runs one")
		case errors.Is(err, apiclient.ErrBadRequest):
			// Normally the proxy's own words are the whole answer: the archived session
			// ids it does hold, or the accepted since forms. Nothing this side adds would
			// be more authoritative than the archive it just asked.
			//
			// But those words only arrive if they fit what getBody reads of a 400 body
			// (512 bytes). #1830 sizes the refusal to a byte budget so they do, and a
			// proxy this binary shipped with answers inside it — but an older proxy caps
			// by id count, not bytes, and a future message may be longer still. When the
			// detail did not survive, the error above is a bare "bad request" and a typo
			// would otherwise be a dead end, so point at where the ids can be read
			// directly rather than leaving the user with nothing.
			if len(sessions.ids) > 0 && !strings.Contains(err.Error(), ": no archived session") {
				fmt.Fprintf(stderr, "  the proxy did not name the sessions it holds; list them with:\n    curl -s '%s/v1/sessions?archived=true'\n", target)
			}
		default:
			fmt.Fprintln(stderr, "  is Cortex running? `agentop service status`")
		}
		return 1
	}
	defer body.Close()

	// Opened after the request answered, so a refused query (unknown session, bad since)
	// never creates or touches the output file — the typo costs nothing, which is the
	// whole reason the server validates before its first byte.
	var dest io.Writer = stdout
	where := "(stdout)"
	if *out != "" {
		flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if !*force {
			flags |= os.O_EXCL
		}
		f, ferr := os.OpenFile(*out, flags, 0o644)
		if ferr != nil {
			if os.IsExist(ferr) {
				fmt.Fprintf(stderr, "agentop export: %s already exists; --force overwrites it\n", *out)
				return 1
			}
			fmt.Fprintf(stderr, "agentop export: %v\n", ferr)
			return 1
		}
		defer f.Close()
		dest = f
		where = *out
	}

	// Counted on the way through, not by re-reading the file: the summary line is the
	// one thing this command says on success, and it must agree with what was written
	// even when the output is a pipe nobody could seek back on.
	counted := &lineCounter{w: dest}
	if _, err := io.Copy(counted, body); err != nil {
		fmt.Fprintf(stderr, "agentop export: %v\n", err)
		fmt.Fprintf(stderr, "  the walk failed part-way; %s holds every complete record before it\n", where)
		return 1
	}
	fmt.Fprintf(stderr, "  %s records, %s → %s\n",
		plainCount(counted.lines), byteSize(counted.bytes), where)
	return 0
}

// sessionIDs is the repeatable --session flag: --session a --session b, or --session a,b.
//
// Comma-split because ids are UUIDs and the literal "default" — in practice neither
// contains a comma, so the split cannot invent an id. A header-derived id can contain
// one (id_headers accepts any id without control runes); such an id still exports
// through the raw endpoint, which takes it as a single session parameter.
//
// A value that yields no ids is an error, not an absence: `--session "$(ids)"` with an
// empty expansion is the exact scripting case the comma split exists for, and reading it
// as "every session" would turn a broken pipe into a full-archive export — the one
// silent widening this command must not do.
type sessionIDs struct{ ids []string }

func (s *sessionIDs) String() string { return strings.Join(s.ids, ",") }

func (s *sessionIDs) Set(v string) error {
	got := 0
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			s.ids = append(s.ids, id)
			got++
		}
	}
	if got == 0 {
		return fmt.Errorf("--session needs at least one id")
	}
	return nil
}

// lineCounter counts newlines and bytes passing through it, for the summary line.
type lineCounter struct {
	w     io.Writer
	lines int64
	bytes int64
}

func (l *lineCounter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	l.bytes += int64(n)
	for _, b := range p[:n] {
		if b == '\n' {
			l.lines++
		}
	}
	return n, err
}

// byteSize renders a byte count the way a summary line has room for: 12.5MB, not
// 12582912.
func byteSize(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimZero(float64(n)/1e6) + "MB"
	case n >= 1_000:
		return trimZero(float64(n)/1e3) + "kB"
	default:
		return fmt.Sprintf("%dB", n)
	}
}
