package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// dayLayout names a day file. Sortable, and the same layout Query parses back, so
// a human listing the directory reads the same dates the API serves.
const dayLayout = "2006-01-02"

// defaultRetentionDays is how many day files are kept.
//
// An active 8h day writes roughly 480 minutes x a few label combinations, about
// 350 KB, so a month of them is on the order of 10 MB — small enough that nobody has to
// think about it, long enough to answer "what did this month cost".
//
// THIRTY-ONE, AND IT HAS TO BE. This was 30, under a doc claiming it was "long enough to
// answer what did last month cost" — and it was one day short of doing so. prune keeps the
// window [ref-(retainDays-1), ref], so N retention days is exactly N distinct dates, and
// answering window=month on the 31st of a 31-day month needs day files for the 1st through
// the 31st. At 30 the first of the month was pruned on the morning of the 31st and a
// month-to-date total silently lost its first day: no error, no caveat, a figure too small,
// and a budget that looked further from its limit than it was.
//
// usage.WindowMonthLocalDays ITSELF, not a literal agreeing with it. An earlier version was
// written out as 31 "because this package does not import usage" — which was never true here:
// query.go has imported it since this package learned to answer a symbolic window, so the only
// thing the literal bought was a test to keep two numbers equal.
//
// NOT THE SAME CASE AS config.minCostLedgerRetentionDays, whose literal stays: that package is
// the leaf every binary loads to parse its config and must not import the aggregator at all.
// Here the import already exists, so the agreement can be structural instead of asserted. What
// the number means is in usage: the most distinct local dates a month-to-date window can touch,
// which TestWindowMonthLocalDays_IsTheLongestMonthsDateCount walks a calendar to confirm.
const defaultRetentionDays = usage.WindowMonthLocalDays

// expiredSuffix marks a day file that prune has CONDEMNED but not yet deleted.
//
// PRUNE RENAMES BEFORE IT UNLINKS, and the reason is that prune's cutoff is computed from a
// clock it cannot verify. The floor that protects it — counting retention back from the older
// of the clock's day and the newest day file — defeats itself once a forward-skewed process
// has WRITTEN a file: that file then agrees with the wrong clock, both readings move together,
// and no rule using only those two inputs can tell the state apart from time having genuinely
// passed. Detection is not available here, so recoverability is what is left.
//
// A file ending in this suffix is invisible to every read: dayFromName requires a ".jsonl"
// extension, so an expired file is matched by no window, contributes to no total, and is not a
// candidate for "newest". It is deleted only when a LATER prune agrees it is still outside the
// window — two independent passes, which a single wrong cutoff cannot supply.
//
// AND IT IS RESTORED IF THE CUTOFF RECEDES. That is the property this whole mechanism is for:
// a prune driven by a skewed clock condemns real history, and when the clock is corrected the
// next prune renames those files back rather than leaving the operator to do it. A wrong prune
// becomes a delay in retention instead of a loss of cost history.
const expiredSuffix = ".expired"

// MaxRetentionDays mirrors config.maxCostLedgerRetentionDays, which owns the derivation.
// Restated rather than imported for the reason the floor is: this package must not depend on
// the config loader.
//
// EXPORTED SO THE PIN CAN BE REAL. The claim here used to be that a test kept the two equal; it
// did not — the test compared this constant against a THIRD copy of the literal in its own file,
// so moving the config ceiling left it green and the durable side clamping to a number the
// validator no longer admits. Two unexported constants in two packages cannot be compared by any
// test, so one of them has to be reachable: config's own test asserts against this one.
const MaxRetentionDays = 3650

// maxRetentionDays is the internal spelling, kept so the call sites below read unchanged.
const maxRetentionDays = MaxRetentionDays

// fileMode is 0o600 because these files record spend. 0o644 would make one
// account's bill readable by every other account on a shared machine, and there is
// no reader that needs it.
const fileMode = 0o600

// dirMode is 0o700 for the same reason: a world-listable directory of day files
// discloses which days someone worked even before a file is read.
const dirMode = 0o700

// store is the on-disk half: one append-only JSON-lines file per local day,
// retention by deletion.
//
// One file per day rather than one per process or one growing forever, because
// retention then costs a directory listing and an unlink instead of a rewrite, and
// because Query walks the dates it was asked for rather than scanning everything
// the ledger has ever held.
type store struct {
	dir string
	// retainDays is how many day files survive prune. Zero means the default.
	retainDays int
	// loc is the zone the ledger's DAY BOUNDARY is defined in, and the single answer
	// to "which day does this instant belong to" for every part of this file.
	//
	// It is the Writer's clock zone, which is time.Local in production because every
	// producer and every reader derives its timestamps from time.Now(). Deciding it in one
	// place is what makes the agreement structural rather than a convention nobody wrote
	// down: path names the file while readDay is called with a day derived from the CALLER's
	// zone, and if those two ever disagree — anything in the pipeline calling .UTC() would
	// do it — a row near midnight is filed under a date no query for that local day ever
	// visits: written, retained for 30 days, and invisible to every read.
	loc *time.Location
}

// newStore prepares dir, creating it if needed. A nil loc means time.Local.
func newStore(dir string, retainDays int, loc *time.Location) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("costledger: no directory configured")
	}
	if retainDays <= 0 {
		retainDays = defaultRetentionDays
	}
	// CLAMPED HERE TOO, not only in config.Validate, because what is on the other side of this
	// bound is an irreversible delete. A retention large enough to wrap prune's AddDate
	// produces a cutoff in the FUTURE and takes the whole ledger with it (see
	// config.maxCostLedgerRetentionDays for the measurement), and Validate only runs on a
	// value that arrived through a config file — every other caller of newStore, including a
	// test and any future in-process construction, would reach prune unguarded.
	//
	// CLAMPED RATHER THAN REFUSED, unlike in the config, and the asymmetry is the point: a
	// bad config should fail to load loudly, while a running proxy asked for an absurd
	// retention should keep the maximum sane amount of history rather than refuse to record
	// cost at all. Warn, because a silent clamp is how the two definitions drift.
	if retainDays > maxRetentionDays {
		slog.Warn("costledger: retention_days is past the maximum; clamping",
			"requested", retainDays, "using", maxRetentionDays,
			"reason", "retention is counted back with AddDate, which normalises, so a large enough value wraps the cutoff into the future and prune deletes every day file including today")
		retainDays = maxRetentionDays
	}
	if loc == nil {
		loc = time.Local
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("costledger: cannot create %s: %w", dir, err)
	}
	return &store{dir: dir, retainDays: retainDays, loc: loc}, nil
}

// dayOf is the package dayOf resolved in the STORE's zone. The one place the day an
// instant belongs to is decided, so path, readDay, prune and Query's day walk cannot
// disagree.
//
// The result identifies a day; it is NOT that day's first instant. See the package dayOf
// for why it is carried at noon and what a local midnight cost.
func (s *store) dayOf(t time.Time) time.Time {
	return dayOf(t.In(s.loc))
}

// path is the day file a timestamp belongs to.
//
// dayOf-derived rather than formatting t directly, so the name is explicitly the
// LEDGER DAY of t rather than whatever date t's own zone happens to print.
func (s *store) path(t time.Time) string {
	return filepath.Join(s.dir, s.dayOf(t).Format(dayLayout)+".jsonl")
}

// dayFromName is the inverse of path: the ledger day a day file's NAME spells, in the
// same representation dayOf produces. Reports false for any name this package did not
// write, so prune never dates a file it does not understand.
//
// PARSED IN UTC AND REBUILT IN s.loc, which is not the same thing as
// time.ParseInLocation(dayLayout, base, s.loc) and is the whole point of the function
// existing. ParseInLocation resolves a bare date to that day's LOCAL MIDNIGHT, and in a
// zone whose DST transition is at 00:00 that instant does not exist — so
// "2026-03-08.jsonl" in America/Havana comes back as 2026-03-07 23:00, thirteen hours
// before the day prune compares it against. Every comparison prune makes is `Before` a day
// derived from dayOf, so a side skewed by thirteen hours decides a deletion: measured, with
// dayOf skewed the same way, retainDays 2 on 2026-03-09 in Havana unlinked the 2026-03-08
// file — yesterday, inside the window. The two directions of this mapping are compared
// against each other, so leaving them in different representations makes the next change to
// either end a deletion nobody predicted.
//
// The date TEXT has no zone in it, so parsing it in UTC cannot be ambiguous or absent;
// the zone belongs to the day it names, which is what dayNoon applies.
func (s *store) dayFromName(name string) (time.Time, bool) {
	if filepath.Ext(name) != ".jsonl" {
		return time.Time{}, false
	}
	d, err := time.Parse(dayLayout, name[:len(name)-len(".jsonl")])
	if err != nil {
		return time.Time{}, false
	}
	y, m, day := d.Date()
	return dayNoon(y, m, day, s.loc), true
}

// append writes rows to whichever day files they belong to, and reports HOW MANY ROWS
// DID NOT REACH DISK alongside the first error.
//
// Grouped by day rather than assuming one, because a flush can straddle local
// midnight: the minute that closes at 00:00 belongs to yesterday's file while the
// one that opened belongs to today's. No long-lived handle is held for the same
// reason — a handle cached across a day boundary would keep writing yesterday's
// file forever, which is the bug that makes a day silently gain 24 hours of rows.
//
// The count is SUMMED PER DAY FILE, so a batch whose second file is unwritable reports only
// that file's rows rather than all of them. Defensive rather than a live case — a batch is
// usually one minute, so byDay has one entry — but it is what keeps the count correct when a
// batch does carry two days, which the grouping above already assumes it can.
func (s *store) append(rows []Row) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	byDay := map[string][]Row{}
	for _, r := range rows {
		p := s.path(r.At)
		byDay[p] = append(byDay[p], r)
	}
	// The first error is returned but every day is still attempted: a failure
	// writing one file is no reason to drop the rows destined for another.
	var firstErr error
	var lost int
	for p, dayRows := range byDay {
		dayLost, err := writeLines(p, dayRows)
		lost += dayLost
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return lost, firstErr
}

// dayFile is what writeLinesTo needs of an open day file: append the bytes, get them
// onto the device, close.
//
// An interface rather than *os.File so the failure path is testable — ENOSPC is not
// something a unit test can arrange, and surviving one is the whole point of this being
// a single Write instead of N. See TestWriteLines_ATornAppendDoesNotRollBackAnotherWritersRows.
//
// DELIBERATELY NARROWER THAN *os.File: no Truncate, and no Stat to derive a size from.
// That absence IS the fix rather than an oversight — see appendBytes for why nothing on
// this path may ever shorten a day file — so keep both out of here and no future edit
// can quietly reintroduce the rollback.
type dayFile interface {
	Write(b []byte) (int, error)
	Sync() error
	Close() error
}

// writeLines appends one day's rows as JSON lines to the day file at path, returning
// how many of them did not land. See writeLinesTo.
//
// FSYNCS THE DIRECTORY when the day file had to be CREATED, and that is a durability hole
// rather than a nicety. writeLinesTo fsyncs the FILE, which puts the bytes on the device —
// but a file's NAME lives in its parent directory, and that directory entry is not covered
// by the file's own fsync. Without this, the first append of each day could be lost
// entirely to power loss: the data synced, the name never recorded, and the day file simply
// absent afterwards with nothing anywhere saying a minute had been written. Every later
// append that day is safe without it, because the entry already exists.
//
// The cost is ONE extra fsync PER DAY, on the writer goroutine, never on a request path —
// which is why the existence check is worth making rather than syncing the directory on
// every append. A stat that races another writer's create is harmless: both would sync a
// directory entry that is already there.
//
// Reported like a failed file Sync, not as a lost row: the rows ARE in the file and every
// reader will see them, and what cannot be claimed is that the file survives the host
// losing power. Writer.write already separates those two cases (see its "zero lost with a
// non-nil error" branch), so this arrives as an error with a drop count of zero.
func writeLines(path string, rows []Row) (int, error) {
	// Checked BEFORE the open, which is the only place it can be: the open creates the
	// file, after which "did this exist" is unanswerable.
	_, statErr := os.Stat(path)
	creating := os.IsNotExist(statErr)
	lost, err := writeLinesTo(rows, func() (dayFile, error) {
		return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	})
	if !creating {
		return lost, err
	}
	if derr := syncNewDayFileName(filepath.Dir(path)); derr != nil && err == nil {
		// Behind a write error, never in front of it: a tear is what a caller can act on,
		// and an unsynced directory entry only adds that the same device is failing in a
		// second way.
		err = derr
	}
	return lost, err
}

// syncNewDayFileName is syncDir, indirected for ONE reason: an fsync has no userspace
// effect, so nothing in this package — or any test of it — can otherwise tell the call from
// a no-op. A durability step nothing can pin is a step a future edit removes silently, and
// this one was already missing once.
//
// Replaced only by this package's tests, exactly like Writer.betweenWindowReads. Nil is
// never a valid value.
var syncNewDayFileName = syncDir

// syncDir fsyncs a directory, so a file just created in it has a NAME that survives power
// loss and not only contents that do.
//
// BEST EFFORT BY DESIGN on the platforms where it is not a thing: opening a directory for
// reading and fsyncing it is POSIX behaviour, and a filesystem that refuses either returns
// an error here which the caller reports as a durability claim it cannot make. It never
// affects whether the rows are readable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("costledger: cannot open %s to make a new day file's name durable: %w", dir, err)
	}
	serr := f.Sync()
	cerr := f.Close()
	if serr != nil {
		return fmt.Errorf("costledger: a new day file's rows are on the device but its name in "+
			"%s is not, so power loss can leave the file absent: %w", dir, serr)
	}
	return cerr
}

// writeLinesTo is writeLines with the opening of the day file injected, so a test can
// put a file that fails part-way through a write where the real one goes.
//
// Marshalled in full FIRST and written ONCE, which is what keeps a day file syntactically
// intact under a SHORT WRITE. Encoding straight to the file, a row at a time, means a short
// write — ENOSPC, EIO — leaves a fragment with no trailing newline and the next successful
// append concatenates onto it: a guaranteed syntax error at that offset. readDay resyncs
// past one, but not producing the damage beats tolerating it, and a laptop filling its disk
// is exactly when someone asks what things cost.
//
// IT DOES NOT HOLD ACROSS A CRASH. A short write is a failure this function is still running
// after, so it can append the fence newline appendBytes documents. Power loss, SIGKILL and a
// panic are failures it is not: the write may have landed partly with nothing left to fence
// it, and the file is then exactly the shape described above — a fragment that swallows
// whatever is appended next. See appendBytes for what that costs and why it is still the
// right trade.
//
// RETURNS HOW MANY ROWS DID NOT LAND, which is not the same as len(rows) whenever the write
// tore. The single Write reports the byte count it stored, the rows are laid out in that
// same buffer in order, and O_APPEND means the bytes it stored are in the file — so every
// row that ends at or before that offset is on disk and readable, and only the row
// straddling the tear and the ones after it are gone. Counting the whole batch would
// over-report loss, and Writer.Dropped is the one signal an operator has for "is my ledger
// complete": over-reporting teaches them to disbelieve it just as thoroughly as
// under-reporting hides it.
//
// WHAT THE COUNT DOES NOT INCLUDE: a Sync that failed. Those rows ARE in the file and any
// reader will see them; what is unproven is that they survive the host losing power. That
// is a durability claim this function cannot make, so it is returned as an error — and
// Flush and Close exist to hear it — but it is not a lost row and must not be counted as
// one.
func writeLinesTo(rows []Row, open func() (dayFile, error)) (int, error) {
	var buf bytes.Buffer
	// json.Encoder writes one object per line and terminates each with a newline,
	// which is exactly the JSON-lines shape readDay decodes.
	enc := json.NewEncoder(&buf)
	// ends[i] is the offset just past row i's newline. A write that stored n bytes
	// therefore stored exactly the rows whose end is <= n; see lostRows.
	ends := make([]int, 0, len(rows))
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			// A Row cannot fail to marshal — no channels, no funcs, no NaN — so this is
			// unreachable in practice. Returned rather than skipped anyway: reaching it
			// would mean the schema gained a field JSON cannot express, and silently
			// dropping that day's rows is not how anyone should find out.
			//
			// Nothing has been opened yet, let alone written, so every row is lost.
			return len(rows), err
		}
		ends = append(ends, buf.Len())
	}

	f, err := open()
	if err != nil {
		// The file could not be opened, so none of them landed.
		return len(rows), err
	}
	n, werr := appendBytes(f, buf.Bytes())
	if n > 0 {
		// FSYNCED, so a nil error means the bytes are on the device rather than in the
		// page cache. Without it, Writer.Close's whole reason for existing — "an orderly stop
		// loses nothing" — is false for a host that loses power seconds after the stop.
		// Softening that promise was the alternative; syncing is better, because the promise
		// is what callers use Flush and Close FOR. The cost is one fsync per closed minute,
		// on the writer goroutine, never on a request path. Reported rather than swallowed: a
		// sync that fails is a durability claim that cannot be made.
		//
		// ON A TORN WRITE TOO, for two reasons. The rows before the tear are exactly the ones
		// this function reports as NOT lost, and that claim is about disk, not about the page
		// cache. And the newline fence appendBytes just appended exists precisely for the
		// crash case — an unterminated fragment swallowing the next row appended — so leaving
		// the one byte whose whole purpose is crash-durability unsynced would be
		// self-defeating. The tear stays the reported error, because it is the one a caller
		// can act on; a Sync failure behind it only adds that the same device is failing in a
		// second way.
		if serr := f.Sync(); serr != nil && werr == nil {
			werr = serr
		}
	}
	if cerr := f.Close(); cerr != nil && werr == nil {
		werr = cerr
	}
	return lostRows(ends, n), werr
}

// lostRows is how many of the encoded rows a write of n bytes did not store.
//
// ends is monotonically increasing, so the first row whose end is past n is the one the
// tear landed in and everything from there on is missing. A row is counted lost when it
// is even partly short: a fragment is not a row, and readDay counts it as a skipped line
// rather than decoding it.
func lostRows(ends []int, n int) int {
	for i, end := range ends {
		if end > n {
			return len(ends) - i
		}
	}
	return 0
}

// appendBytes writes b in ONE call and, if that write landed only partly, appends a
// newline so the torn row cannot be joined to the next one.
//
// IT DOES NOT ROLL BACK, and that is the decision. A rollback truncates to a size read
// before the write, which in a directory two writers share can destroy rows this process
// never wrote — and a rollback that can be wrong is worse than none, because the failure
// it prevents (one unreadable line) is smaller than the one it causes (another writer's
// committed rows gone).
//
// THE FENCE ONLY RUNS IF THIS PROCESS IS STILL ALIVE. A crash mid-write leaves no
// terminator, so the next append joins onto the fragment and readDay loses BOTH rows —
// measured, and why Caveats.SkippedLines is documented as a floor rather than a count.
//
// No lock: this path is reached only from the single writer goroutine. Returns the bytes
// of b now in the file, so the caller can say which rows did not make it.
func appendBytes(f io.Writer, b []byte) (int, error) {
	n, err := f.Write(b)
	if err == nil {
		return n, nil
	}
	if n > 0 && n < len(b) && b[n-1] != '\n' {
		// Ended mid-row. A tear that happened to land on a line boundary needs nothing.
		if _, ferr := f.Write([]byte{'\n'}); ferr != nil {
			// Reported together with the write that tore, because the consequence outlives
			// this call: the next row appended to this file is unreadable too, and only the
			// skipped-line count will show it.
			return n, fmt.Errorf("costledger: torn append of %d bytes could not be fenced off with a "+
				"newline (%v), so the next row appended to this file will be unreadable too: %w", n, ferr, err)
		}
	}
	return n, err
}

// maxLineBytes bounds one line readDay will buffer.
//
// A row is a few hundred bytes, so 1 MiB is roughly three thousand times the real
// shape. It is deliberately not unbounded: this reads a path an operator configured,
// and a reader that will buffer a line of any length can be made to allocate
// arbitrarily by whatever else ends up in that directory.
//
// A line longer than this ENDS that day's read, with a warning, because a scanner
// cannot skip a token it refused to buffer. Every row appended after that offset is
// then unreadable, on this read and on every future one, and the file is append-only
// so the loss is permanent.
//
// "No ledger write can produce a line that long" is NOT a safe assumption, and it is the
// one this design rested on. One Encode of one struct is exactly how the damage was
// produced: Row carries Model, Model is the model name off the parsed request body, and
// with no length cap a workload naming its model with a megabyte of bytes wrote a single
// valid line past this limit and destroyed the remainder of that day. Measured: 5 priced
// requests totalling $3.25 read back as $0.25.
//
// The write side caps every label at maxLabelLen, which puts the longest line this package
// can emit under a kilobyte, so this guard is a last resort for a file corrupted by
// something other than this package rather than a live failure mode a request can reach.
// Keep it that way: any new Row field carrying caller-controlled bytes needs a cap on the
// write path, not a larger buffer here.
const maxLineBytes = 1 << 20

// readDay decodes one day file. A missing file is not an error: an idle day writes
// none, which is the normal case on a laptop.
//
// SKIPS an undecodable line and keeps going, rather than stopping at it. A scanner rather
// than one json.Decoder over the whole file, because a Decoder cannot resync: it tolerates a
// truncated FINAL line and only that, so a bad line in the MIDDLE silently truncates the
// rest of the day, permanently, with the shortened figure still labelled "today". A single
// Write on the append side keeps a mid-file syntax error from being produced in the first
// place; this is the half that keeps an already-damaged file readable.
//
// Skips are COUNTED, RETURNED and logged at Warn — not at Debug, which is below the default
// level, and not into a local, which reaches no caller, no exported counter and no API
// response. The count is what lets an operator tell "my ledger is fine" from "my ledger is
// losing lines", so it has to leave this function. See dayIssues and Caveats.SkippedLines.
func (s *store) readDay(day time.Time) ([]Row, dayIssues, error) {
	f, err := os.Open(s.path(day))
	if os.IsNotExist(err) {
		return nil, dayIssues{}, nil
	}
	if err != nil {
		return nil, dayIssues{}, err
	}
	defer func() { _ = f.Close() }()

	var out []Row
	var issues dayIssues
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		b := sc.Bytes()
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		var r Row
		if derr := json.Unmarshal(b, &r); derr != nil {
			// No offset in the message and no bytes from the line: a corrupt ledger line
			// could contain anything, and this text reaches a log an operator pastes.
			issues.skippedLines++
			continue
		}
		// RE-APPLIED ON READ, not trusted from the file. rowLabel caps and sanitises every
		// label the WRITER produces, and until this the read path assumed that was the only
		// way a line could get here — so a day file this process did not write, or wrote
		// before the cap existed, handed a multi-KB series key carrying a C1 CSI escape
		// straight through Fold to the session API and into any terminal rendering it
		// (CWE-150). The files are 0600 under $HOME, so the writer is the user; the point is
		// that the CONTENT is not this process's own output and nothing downstream re-checks
		// it. A reader that sanitises is the only place that can be sure.
		//
		// Idempotent by construction, so a row this writer produced is unchanged: rowLabel is
		// truncateLabel(sanitizeLabel(...)) and both are fixed points on their own output.
		// That is what makes doing it twice free rather than lossy.
		//
		// ALL FOUR, INCLUDING Provenance, which was the one left out. The write path caps every one
		// of them, and Provenance is the field pricedBy is reconstructed from — so a foreign file's
		// multi-KB provenance carrying a C1 CSI escape reached a series key with nothing else
		// checking it. It is a small closed set in this process's own output ("configured",
		// "discovered", …), which is exactly why it looked like it needed no cleaning: the set is a
		// property of the WRITER, and this function's whole subject is a file the writer did not
		// write.
		r.Endpoint, r.Model = rowLabel(r.Endpoint), rowLabel(r.Model)
		r.Agent, r.Provenance = rowLabel(r.Agent), rowLabel(r.Provenance)
		out = append(out, r)
	}
	if serr := sc.Err(); serr != nil {
		// Only ever an IO error or a line past maxLineBytes. Either ends the day's read,
		// so it is a warning rather than a debug line: the figure that follows is short
		// by however much came after this point, and by an amount the file cannot say.
		issues.truncated = true
		slog.Warn("costledger: stopped part-way through a day file; the total for it is short",
			"day", day.Format(dayLayout), "rowsRead", len(out),
			"linesSkipped", issues.skippedLines, "error", serr)
		return out, issues, nil
	}
	if issues.skippedLines > 0 {
		// WARN, not Debug. A number missing rows is exactly what an operator has to be
		// able to see, and the default level does not carry Debug.
		slog.Warn("costledger: skipped undecodable lines; the total for this day is short",
			"day", day.Format(dayLayout), "rowsRead", len(out), "linesSkipped", issues.skippedLines)
	}
	return out, issues, nil
}

// dayIssues is what one day file's read could not use.
//
// Returned rather than only logged, because the caller is what turns it into something
// an operator can see: Query returns it on Caveats.SkippedLines and Caveats.TruncatedDays.
// Without that, a day file that lost half its lines produced the same API response as a clean
// one — window:"today", priced:true, no caveat.
//
// ON Caveats AND NOT ON THE WRITER, which is what these three references used to say. Caveats'
// own doc gives the reason: counters on the Writer are shared, so two concurrent readers would
// swap each other's answers. The stale names sat next to the reasoning that removed them.
type dayIssues struct {
	// skippedLines is undecodable lines stepped over. The rows around them survive, so the
	// loss is bounded — but this count is a FLOOR on the rows lost, not an exact figure: a
	// line that is a crash fragment with the next append concatenated onto it is ONE
	// undecodable line holding TWO lost rows. See appendBytes for the measurement.
	skippedLines int
	// truncated reports that the read STOPPED before the end of the file. Everything
	// after that offset is missing from the answer and nothing says how much, which is
	// why it is tracked separately from a skip rather than added to it.
	truncated bool
}

// retentionCutoff is the oldest day this ledger's CONFIGURATION reaches back to: the
// [ref-(retainDays-1), ref] span prune keeps, measured from the clock's day.
//
// A STATEMENT ABOUT CONFIGURATION, NOT ABOUT WHAT WAS DELETED, and the difference is why an
// earlier version of this was wrong. It claimed to be "derived from the same expression prune
// uses" and it is not: prune floors its own ref at the NEWEST day file, so on a ledger that has
// been idle it reaches further back than this does, and files can outlive this cutoff between
// prune runs in any case.
//
// So this cannot answer "was anything deleted". Nothing in this package can: no inception date
// is persisted and prune records nothing about what it removed, so an absent old day is
// indistinguishable from a day that was never written. A three-day-old install with
// retention_days=10 has twenty-two absent days before this cutoff and lost nothing.
//
// What it CAN answer is "how far back can a request expect coverage", which is a coverage
// question and is all its one caller asks. See sessionapi's daysOutsideRetention.
func (s *store) retentionCutoff(now time.Time) time.Time {
	return s.dayOf(now).AddDate(0, 0, -(s.retainDays - 1))
}

// prune condemns day files outside the retention window, in both directions.
//
// retainDays FILES SURVIVE, counting today: the cutoff is today minus retainDays-1. Files
// it cannot date are never touched — deleting an unrecognised file under an
// operator-configured path is the one unrecoverable mistake available here. It returns the
// first error and keeps going, so one undeletable file does not strand the rest.
//
// THE CUTOFF IS FLOORED AT THE NEWEST DAY FILE'S DATE, which stops a clock that has
// stepped forward from deleting history that is still inside the real window. It is a
// floor rather than a plausibility threshold because no threshold separates a wrong clock
// from a long idle period; deleting nothing is recoverable and deleting today is not. The
// cost is that retention is then measured from the newest DATA rather than from now, so an
// archive nothing writes to keeps its last retainDays files indefinitely.
//
// THE FUTURE END IS PRUNED TOO, AND ASYMMETRICALLY. A past file beyond the window is
// ordinary ageing; a FUTURE one cannot be, since a day file exists only because this
// process wrote rows dated then. The horizon is a whole retention window ahead rather than
// tomorrow, because the near future is where an ordinary DST or timezone move lands.
//
// See expiredSuffix for why this renames rather than unlinks, and for the residual: a
// clock stepping backward by more than the whole window is indistinguishable from time
// having passed.
func (s *store) prune(now time.Time) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	// Datable names only, and their days, so the newest is known BEFORE anything is
	// unlinked. Never touches a file it cannot date; see the doc above.
	type dayEntry struct {
		name string
		day  time.Time
	}
	var days []dayEntry
	// Condemned by an earlier pass. Collected separately so this pass can either agree (and
	// unlink) or disagree (and restore), which is the two-pass rule expiredSuffix exists for.
	var expired []dayEntry
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// A file this prune, or an earlier one, has already condemned. Kept in its own list:
		// it is invisible to reads (dayFromName rejects the extension), must not count toward
		// "newest", and is the only thing this pass is allowed to actually unlink. See
		// expiredSuffix.
		if strings.HasSuffix(name, expiredSuffix) {
			if day, ok := s.dayFromName(strings.TrimSuffix(name, expiredSuffix)); ok {
				expired = append(expired, dayEntry{name: name, day: day})
			}
			continue
		}
		// dayFromName, so a name is dated in exactly the representation dayOf produces —
		// s.loc's ledger day, at dayHour — which makes both sides of every `Before` below the
		// same kind of instant. Not an inline ParseInLocation to local midnight, which is a
		// date that does not exist in every zone; see dayFromName.
		day, ok := s.dayFromName(name)
		if !ok {
			continue
		}
		days = append(days, dayEntry{name: name, day: day})
		if day.After(newest) {
			newest = day
		}
	}
	// NO EARLY RETURN ON AN EMPTY days, and that is the whole of a defect rather than tidiness.
	// horizon is measured from the CLOCK, so a backward step larger than retention dates every real
	// file past it and ONE pass condemns all of them — leaving no live day file at all. Returning
	// here then skipped the second pass forever: the condemned files were never re-judged, so they
	// were neither unlinked nor restored when the clock came back. The bytes survive, but dayFromName
	// rejects the .expired extension, so the whole history is invisible to every read — permanently,
	// and in exactly the case expiredSuffix promises to make "a delay in retention instead of a loss
	// of cost history".
	//
	// The day retention is counted back from: the OLDER of what the clock says today is
	// and the newest day the ledger has written. They are the same day on a healthy host,
	// so this changes nothing there.
	//
	// GUARDED ON THERE BEING A NEWEST AT ALL, which the early return used to make unnecessary. With
	// days empty, newest is the zero time and would win this comparison outright, putting the cutoff
	// at year zero — nothing would ever be unlinked again and every condemned file would be restored,
	// including ones genuinely decades past retention. With no live file, the clock is the only
	// reading there is.
	ref := s.dayOf(now)
	if len(days) > 0 && newest.Before(ref) {
		ref = newest
		if gap := s.dayOf(now).Sub(newest); gap > time.Duration(s.retainDays-1)*24*time.Hour {
			// Worth a line: on a healthy host the newest day file IS today, so a gap wider
			// than the whole retention window means either the clock is wrong or the ledger
			// has not been written to in longer than it retains. Said at Warn because the
			// first of those is a host fault an operator wants to know about, and neither is
			// visible anywhere else.
			slog.Warn("costledger: the clock is further ahead of the newest day file than "+
				"retention could explain; keeping day files rather than deleting cost history",
				"clockDay", s.dayOf(now).Format(dayLayout), "newestDayFile", newest.Format(dayLayout),
				"retainDays", s.retainDays,
				"cause", "the host clock stepped forward, or this ledger has been idle longer than its retention",
				"effect", "retention is measured back from the newest day file instead of from the clock")
		}
	}
	cutoff := ref.AddDate(0, 0, -(s.retainDays - 1))
	// The other end of the window. Measured from the CLOCK's day rather than from ref: ref
	// is deliberately the older of the two readings, and using it here would move the
	// horizon backwards on an idle ledger and start condemning days that are merely newer
	// than the last one written. See the doc above for why this end exists at all and why
	// it sits a whole retention window out.
	horizon := s.dayOf(now).AddDate(0, 0, s.retainDays)

	var firstErr error

	// RE-JUDGING RUNS FIRST, and the order is load-bearing rather than stylistic.
	//
	// Both passes address the same file NAME, and a live day file can coexist with an .expired of
	// the same name — reachable exactly the way the doc above describes: a clock steps back past
	// retention, everything is condemned, the clock is corrected, and the writer records a fresh
	// file for that same day. Condemning first then renamed the live file ONTO the stale .expired,
	// destroying its rows, and left the newest rows under a name this pass was already about to
	// unlink: both copies gone in one prune, with no second independent judgement — the one property
	// expiredSuffix exists to provide. Measured: a directory holding both copies came out empty.
	//
	// Clearing the condemned names first removes the collision instead of handling it, and the two
	// passes cannot fight: this one only restores days INSIDE the window, the one below only condemns
	// days outside it, and both read bounds computed before either ran.
	for _, d := range expired {
		live := filepath.Join(s.dir, strings.TrimSuffix(d.name, expiredSuffix))
		if d.day.Before(cutoff) || d.day.After(horizon) {
			if rerr := os.Remove(filepath.Join(s.dir, d.name)); rerr != nil && firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		// Back inside the window: the cutoff that condemned this file was wrong, or the clock
		// that produced it has been corrected. Warn, because a restore means an earlier prune
		// was working from a bad reading and an operator should know the host clock moved.
		slog.Warn("costledger: restoring a day file an earlier prune condemned; it is inside the "+
			"retention window again",
			"file", d.name, "day", d.day.Format(dayLayout),
			"cutoff", cutoff.Format(dayLayout), "retainDays", s.retainDays,
			"cause", "the host clock was ahead when the earlier prune ran and has since been corrected",
			"effect", "the rows in this file are readable again rather than lost")
		if rerr := restoreDayFile(filepath.Join(s.dir, d.name), live); rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}

	for _, d := range days {
		future := d.day.After(horizon)
		if !d.day.Before(cutoff) && !future {
			continue
		}
		if future {
			// Worth a line per file, and there can only be a handful: a day file dated past the
			// whole retention window is a host-clock fault, and it is the only case here where
			// what is deleted is not simply old.
			slog.Warn("costledger: deleting a day file dated further ahead than retention could "+
				"ever reclaim; its rows were written by a clock that was wrong",
				"file", d.name, "clockDay", s.dayOf(now).Format(dayLayout),
				"horizon", horizon.Format(dayLayout), "retainDays", s.retainDays,
				"cause", "the host clock was stepped forward when those rows were recorded, or has since stepped back",
				"effect", "those rows are unreadable by any window this clock can express and are condemned; "+
					"the file is renamed with the .expired suffix and a later prune deletes it, or restores it if the clock is corrected")
		}
		// CONDEMNED, NOT DELETED. The rename is the whole mitigation: see expiredSuffix.
		//
		// Through mergeDayFiles because the destination can already exist even after the pass above
		// cleared the ones it saw — a remove or a restore up there can fail, and os.Rename would then
		// silently replace a condemned file's rows with these. Nothing is worth losing to save a
		// syscall on a path that only runs when the host clock has already misbehaved.
		from := filepath.Join(s.dir, d.name)
		if rerr := condemnDayFile(from, from+expiredSuffix); rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}
	return firstErr
}

// restoreDayFile puts a condemned day file back, without discarding a live one of the same name.
//
// A PLAIN RENAME LOST DATA HERE. os.Rename replaces its destination, and the destination exists
// whenever the writer recorded that day again after the condemnation — the ordinary end of the
// scenario this file's retention doc describes. Measured: a day holding a condemned $1.00 and a live
// $2.00 came out of prune holding $1.00, under a Warn that said "the rows in this file are readable
// again rather than lost".
//
// MERGED RATHER THAN REFUSED, because both files hold real rows for that day and either one alone is
// a wrong answer. The format is append-only JSON lines and readDay already sums several rows per
// minute, so concatenation is lossless — and it cannot double-count, because these two files were
// written on opposite sides of the condemnation and a row is a per-minute aggregate of the events
// that actually happened in it.
func restoreDayFile(condemned, live string) error {
	return mergeDayFiles(condemned, live)
}

// condemnDayFile is the same protection in the other direction: the live file's rows join whatever
// an earlier prune already condemned under that name, instead of replacing them.
func condemnDayFile(from, expired string) error {
	return mergeDayFiles(from, expired)
}

// mergeDayFiles moves src to dst, appending rather than replacing when dst already exists.
//
// The rename is kept for the ordinary case — one syscall, atomic, and the only case that happens on a
// healthy host. The append path exists only where a host clock fault has left two files for one day.
func mergeDayFiles(src, dst string) error {
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return os.Rename(src, dst)
	} else if err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	// O_RDWR, not O_WRONLY: the tail check below reads the last byte through this same handle.
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	// A TERMINATOR FIRST IF THE TAIL IS MISSING ONE. appendBytes never shortens a file but a torn
	// write can leave it without its final newline, and appending onto that would glue two rows into
	// one unparseable line — turning a recoverable fault into a lost row at each join.
	fi, err := out.Stat()
	if err != nil {
		return err
	}
	if fi.Size() > 0 {
		tail := make([]byte, 1)
		if _, rerr := out.ReadAt(tail, fi.Size()-1); rerr == nil && tail[0] != '\n' {
			if _, werr := out.Write([]byte("\n")); werr != nil {
				return werr
			}
		}
	}
	if _, cerr := io.Copy(out, in); cerr != nil {
		return cerr
	}
	// Synced before the source is unlinked: without it a crash in between loses the rows that were
	// in the file this call is about to delete.
	if serr := out.Sync(); serr != nil {
		return serr
	}
	slog.Warn("costledger: two day files existed for one day; their rows were merged rather than one "+
		"set being discarded",
		"kept", filepath.Base(dst), "mergedFrom", filepath.Base(src),
		"cause", "the host clock moved far enough for a day to be condemned and then written again",
		"effect", "both sets of rows are in the surviving file; a per-minute total is the sum of what each held")
	return os.Remove(src)
}
