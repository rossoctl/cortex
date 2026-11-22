package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// exportTimeout bounds the one request this command makes, body read included.
//
// GENEROUS AGAINST THE WALK IT BOUNDS, deliberately: the server decodes every segment in
// the archive single-threaded, which EXTRAPOLATES to ~10s for a 30-day archive — a figure
// from a smaller fixture, not a measurement of one, and the walk re-decodes per page, so
// the real number for a busy month may be well above it (#1830 review). Nothing benchmarks
// the walk yet: #1895 covers measuring it, and the two paging fixes that would change it.
//
// This budget exists to catch a live-but-pathologically-slow walk rather than to be tight
// against a healthy one, so an optimistic estimate costs nothing here — but do not quote
// the ten seconds as a measurement.
//
// A proxy that stalls is caught sooner and elsewhere: HeaderTimeout (2 minutes)
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
	gzipOut := fs.Bool("gzip", false,
		"write the file gzip-compressed; requires --out and a file name ending in .gz")
	sessions := &sessionIDs{}
	fs.Var(sessions, "session",
		"export only this `session`, repeatable and comma-separated; omit for every archived session")
	since := fs.String("since", "",
		"export only events newer than this far back — a duration such as 168h, or a day count like 7d")
	withToolCalls := fs.Bool("with-tool-calls", false,
		"also export one record per tool call, paired with its result — still content-free: "+
			"durations and sizes, never arguments or output")
	endpoint := fs.String("endpoint", "",
		"session API URL of the proxy (default: the Cortex installed on this machine)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `agentop export — the session archive as content-free records

Usage:
  agentop export                       every archived event, one NDJSON line each, to stdout
  agentop export --out FILE            write the export to a file; --force overwrites
  agentop export --gzip --out F.gz     write it gzip-compressed, for sharing
  agentop export --since 168h          only events from the last week
  agentop export --session ID          one session, repeatable or comma-separated
  agentop export --with-tool-calls     add a record per tool call, paired with its result
  agentop export --endpoint URL        ask a specific proxy rather than the local one

Each line is one archived event reduced to 37 content-free scalars — timings, token
counts, hosts, models, outcomes — never message content, completions or tool arguments,
which is what makes it shareable where the full events are not. It is not anonymous,
though: hosts, models, session ids and timestamps remain, and together they describe who
talked to what and when. A month of traffic is about 12MB. The reading rules that keep a
consumer from misinterpreting the fields (zero means "not recorded", never a real zero;
order by "at" then "seq") are in Cortex's docs/session-export.md.

--with-tool-calls adds a second kind of line: one per tool call, carrying how long it
took, how it ended, and how many bytes its result was. It answers "which tools ran and
what did those turns cost" without a second request and without joining by hand. The new
records stay content-free on the same terms as the rest — no arguments, no output, and no
tool-use id, so a record cannot be joined back to a conversation. A built-in tool name
(Bash, Read) appears as itself; an MCP tool's name is a hash under a key this install
keeps, because an MCP name is operator-defined and can carry a customer or project name.

Two reading rules matter for the new lines, and both cost real accuracy. Every line of
one turn's batch repeats that turn's "modelMs" and "gapMs", so take them once per
"callSeq" rather than summing them. And a batch of calls shares one measured window,
marked "durationShared": summing their durations counts that window several times.

Without the flag the output is byte-for-byte what it was before tool-call records
existed, so an existing consumer is unaffected. With it, every line carries a "kind" —
"event" or "toolCall" — so a reader can select either.

The export reads the durable archive, not memory: a session with nothing flushed yet
does not appear, and the live tail is /v1/events, not this.

An unknown --session or a malformed --since is refused by the proxy before it writes a
byte, naming as many of the sessions it does hold as the refusal has room for, then a
count of the rest; a proxy with no session archive (a cluster sidecar, or one predating
/v1/export) answers 404.

A large archive can take a while to walk server-side; this command waits up to ten
minutes before giving up. A walk that dies part-way exits non-zero and says so: the
output holds every complete record written before the failure, never a short file
reported as a whole one. With --gzip the file is left without its gzip trailer instead,
so every tool that opens it reports it as incomplete rather than valid-but-short.

--gzip writes the same records through gzip, to a file ending in .gz. It needs --out:
stdout is for piping, and a pipe that wants gzip already has gzip. A file ending in .gz
without --gzip is refused rather than written as plain NDJSON under a name that says
gzip.

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
	// filepath.Ext, not a slice of the last three characters: Ext returns "" for a name
	// with no dot, so a one-character --out cannot underflow the slice. Judged
	// case-folded, the way a user typing WEEK.GZ means the same file as week.gz.
	gzNamed := *out != "" && strings.EqualFold(filepath.Ext(*out), ".gz")
	if *gzipOut {
		if *out == "" {
			// Same refusal shape as --force without --out: stdout is a pipe, and a pipe
			// that wants gzip already has gzip. A binary stream on a terminal helps nobody.
			fmt.Fprintln(stderr, "agentop export: --gzip is for --out; pipe stdout through gzip yourself")
			return 2
		}
		if !gzNamed {
			// The name is the only thing that tells a consumer what the bytes are. A gzip
			// stream in a file called .ndjson is the misleading name this command refuses
			// in the other direction below.
			fmt.Fprintf(stderr, "agentop export: --gzip writes a file ending in .gz; %s does not\n", *out)
			return 2
		}
	} else if gzNamed {
		// Plain NDJSON under a name that says gzip would hand `gzip -d` a file it reports
		// as corrupt, with nothing in the name warning the user who wrote it that way.
		fmt.Fprintf(stderr, "agentop export: %s ends in .gz; add --gzip to write gzip, or drop the suffix for plain NDJSON\n", *out)
		return 2
	}
	// An EXPLICITLY EMPTY --since is a usage error, not an absent floor, for the reason
	// --session "" is: `--since "$(window)"` with an empty expansion is the scripting case
	// this flag is typed into, and reading it as "every event ever" turns a broken variable
	// into a full-archive export. fs.Visit sees only the flags actually given, so an unset
	// --since still means no floor. Trimmed, as sessionIDs.Set trims: `--since " "` out of a quoted expansion is the
	// same broken variable, and letting it through only moves the refusal to the server,
	// which answers 400 after a round trip rather than 2 before one.
	var emptySince bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "since" && strings.TrimSpace(f.Value.String()) == "" {
			emptySince = true
		}
	})
	if emptySince {
		fmt.Fprintln(stderr, "agentop export: --since needs a value, such as 168h or 7d; omit it to export every event")
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
	// Kept rather than inlined: Endpoint() is target with its trailing slashes trimmed,
	// which is what the hint below must print. `--endpoint http://host:47601/` would
	// otherwise produce `http://host:47601//v1/sessions?archived=true`, and ServeMux
	// answers a doubled slash with a 307 whose HTML body is all `curl -s` would show —
	// a fallback leading into a second dead end.
	client := apiclient.New(target)
	body, err := client.Export(ctx, sessions.ids, *since, *withToolCalls)
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
			//
			// ErrNoDetail, not a match on the server's wording: the condition is "the
			// refusal would not say why", and a refusal that named a DIFFERENT problem
			// has said why. Matching on the absence of "no archived session" made
			// `--session s1 --since bogus` print this hint under the proxy's own
			// perfectly clear complaint about since, sending the reader to look at
			// sessions.
			if len(sessions.ids) > 0 && errors.Is(err, apiclient.ErrNoDetail) {
				fmt.Fprintf(stderr, "  the proxy did not name the sessions it holds; list them with:\n    curl -s '%s/v1/sessions?archived=true'\n", client.Endpoint())
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
	var outFile *os.File
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
		outFile = f
		dest = f
		where = *out
	}

	// Counted on the way through, not by re-reading the file: the summary line is the
	// one thing this command says on success, and it must agree with what was written
	// even when the output is a pipe nobody could seek back on.
	//
	// With --gzip the counter sits BEFORE the gzip writer — body → lineCounter →
	// gzip.Writer → file — so it counts the uncompressed records, and its write-error
	// flag still catches a failed write at the file, which gzip.Writer propagates.
	var gz *gzip.Writer
	countedDest := dest
	if *gzipOut {
		gz = gzip.NewWriter(dest)
		countedDest = gz
	}
	counted := &lineCounter{w: countedDest}
	if _, err := io.Copy(counted, body); err != nil {
		fmt.Fprintf(stderr, "agentop export: %v\n", err)
		// Which END broke decides what to say, and io.Copy's error alone does not tell
		// them apart. A failed write is the local disk (--out to a small partition), and
		// it can stop mid-line, so promising whole records there would be wrong twice:
		// wrong cause, wrong guarantee. A failed read is the walk, which only ever ends
		// the stream at a record boundary.
		//
		// The gzip writer is DELIBERATELY FLUSHED, NEVER CLOSED, here. Close writes the
		// trailer that makes the stream pass its CRC check; closing it now would hand
		// the consumer a file every tool reports as whole, off a copy that failed.
		// Flush writes a sync-flush block and no trailer, so the file still reads as
		// incomplete while holding every byte the copy wrote — and the walk only ever
		// ends a stream between records, so what it holds ends at a record boundary:
		// the same every-complete-record guarantee the plain path gives.
		if counted.err != nil {
			fmt.Fprintf(stderr, "  writing %s failed; it may end in a partial line\n", where)
		} else if *gzipOut {
			if ferr := gz.Flush(); ferr != nil {
				// The flush is a write; a disk that failed the copy can fail it too,
				// and it is the same end breaking, so it gets the same words.
				fmt.Fprintf(stderr, "agentop export: %v\n", ferr)
				fmt.Fprintf(stderr, "  writing %s failed; it is incomplete\n", where)
				return 1
			}
			// The macOS caveat is one clause, not the docs' paragraph: /usr/bin/gzip
			// there reads in 64kB pieces and drops the last one off a stream without
			// a trailer. A full inflate loses nothing; the docs say how.
			fmt.Fprintf(stderr, "  the walk failed part-way; %s is incomplete; gzip -dc %s recovers the records before it (on macOS its gzip may drop the last 64kB)\n",
				where, shellArg(*out))
		} else {
			fmt.Fprintf(stderr, "  the walk failed part-way; %s holds every complete record before it\n", where)
		}
		return 1
	}
	if gz != nil {
		// The success path closes the gzip stream, which writes its trailer. A Close
		// failure is a real failure, and the realistic one for a full disk: the header
		// write during the copy succeeds, flate buffers the tail, and this final flush
		// is the write that hits ENOSPC — so it gets the same "writing failed" line the
		// in-copy path gives, not a bare error.
		if err := gz.Close(); err != nil {
			fmt.Fprintf(stderr, "agentop export: %v\n", err)
			fmt.Fprintf(stderr, "  writing %s failed; it is incomplete\n", where)
			return 1
		}
		// The compressed size comes from the handle this command wrote, after the
		// trailer is written; the record count and the uncompressed bytes beside it
		// come from the copy. One stat, no second read of the file.
		var onDisk int64
		if fi, err := outFile.Stat(); err == nil {
			onDisk = fi.Size()
		}
		fmt.Fprintf(stderr, "  %s records, %s → %s (%s gzip)\n",
			plainCount(counted.lines), byteSize(counted.bytes), where, byteSize(onDisk))
		if code := reportToolCallSkew(stderr, *withToolCalls, counted, where); code != 0 {
			return code
		}
		return 0
	}
	fmt.Fprintf(stderr, "  %s records, %s → %s\n",
		plainCount(counted.lines), byteSize(counted.bytes), where)
	if code := reportToolCallSkew(stderr, *withToolCalls, counted, where); code != 0 {
		return code
	}
	return 0
}

// reportToolCallSkew fails the command when --with-tool-calls was asked for and the proxy did
// not apply it. Returns the exit code: 0 when there is nothing to report.
//
// NON-ZERO, NOT A WARNING, because the file is not what was asked for. A warning on stderr is
// lost in a script — `agentop export --with-tool-calls --out f.ndjson && analyse f.ndjson`
// would run the analysis over an export with no tool calls in it and silently produce a
// different answer. The summary line stays, so the user can see what WAS written.
//
// THE SIGNAL IS `kind`, NOT THE PRESENCE OF toolCall RECORDS. A current server stamps `kind` on
// every record whenever the opt-in applies (spec T28); a proxy predating the parameter ignores
// it and answers 200 with the plain stream, where no record carries `kind` at all. Keying on
// toolCall records instead would report skew for an archive that simply ran no tools.
//
// An empty export is not skew: with no records there is no `kind` to find, and nothing was
// misreported either.
func reportToolCallSkew(stderr io.Writer, asked bool, counted *lineCounter, where string) int {
	if !asked || counted.lines == 0 || counted.sawKind {
		return 0
	}
	fmt.Fprintf(stderr, "agentop export: this proxy ignored --with-tool-calls; %s holds no tool-call records\n", where)
	fmt.Fprintln(stderr, "  it predates the option (`agentop update`), so it served the plain export instead of refusing")
	return 1
}

// shellArg quotes a path for a command line this command suggests to the user. The
// recovery hint names the export file, and an --out with a space in it would otherwise
// paste as two arguments; a name starting with a dash would read as an option. Single
// quotes, with an embedded quote closed and reopened, and a ./ prefix for the dash —
// enough for a hint, not a promise about every shell.
func shellArg(p string) string {
	if strings.HasPrefix(p, "-") {
		p = "./" + p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
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
//
// It also keeps the last write error, because io.Copy reports a failed READ and a failed
// WRITE as the same non-nil error and the caller has to tell a dead walk from a full
// disk: only this side knows which end broke.
type lineCounter struct {
	w     io.Writer
	lines int64
	bytes int64
	err   error

	// sawKind records whether any record carried a `kind` field, which is how this command
	// tells a proxy that APPLIED --with-tool-calls from one that ignored it.
	//
	// A PROXY PREDATING THE FLAG ANSWERS 200 WITH THE PLAIN STREAM rather than refusing, so
	// without this the user asks for tool calls, gets an export without them, and is told
	// nothing. The signal is `kind` on the EVENT records — a current server stamps it on every
	// line whenever the opt-in applies (spec T28) — and NOT the presence of toolCall records,
	// because an archive with no tool calls legitimately produces none. Checking for those
	// would report skew on a perfectly good export of a session that ran no tools.
	//
	// Checked on the first chunk only. The field is on every line when it is on any, so one
	// look is enough, and this sits in the copy path of a stream that can be a month of
	// traffic — a substring scan per write would be paid on every megabyte to learn something
	// already known.
	sawKind  bool
	kindSeen bool
}

func (l *lineCounter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	l.bytes += int64(n)
	for _, b := range p[:n] {
		if b == '\n' {
			l.lines++
		}
	}
	if !l.kindSeen && n > 0 {
		l.kindSeen = true
		l.sawKind = bytes.Contains(p[:n], []byte(`"kind":`))
	}
	if err != nil {
		l.err = err
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
