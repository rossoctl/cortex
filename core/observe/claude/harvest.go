package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConfigDirEnv is the variable Claude Code itself honours for relocating its config
// tree. Read here so a user who has moved it is not told there are no sessions.
const ConfigDirEnv = "CLAUDE_CONFIG_DIR"

// ErrCorruptMetadata reports that an existing metadata file was there and could not be
// read, so a merge refused rather than replacing a file whose contents are unknown.
//
// NARROWER THAN THE NAME SUGGESTS: a file that reads fine but does not parse no longer
// comes back here, because Harvest rebuilds it instead. What is left is every file Harvest
// refuses because a rebuild might replace good entries — one that could not be read (a
// permission or I/O failure) and one too large to read whole (ErrMetadataTooLarge). The two
// need different advice, so a caller printing a remedy must check the narrower sentinel
// first rather than assuming this one means permissions.
//
// Exported because the repair is a CLI affordance: `agentop experimental
// read-claude-sessions` names --merge=false as the way past, and only the command layer
// knows its own flags. Callers discriminate with errors.Is.
var ErrCorruptMetadata = errors.New("corrupt session metadata")

// ErrLockTimeout reports that the metadata lock could not be acquired before its deadline,
// so the harvest proceeded UNLOCKED rather than not at all.
//
// Its own error so a caller can tell "this filesystem cannot flock" from "another process
// is holding it": both proceed unlocked, but only the second means a concurrent harvest is
// real and a lost update is possible.
//
// Declared here rather than in lock_unix.go so it is part of the package API on every
// platform — an errors.Is against it must compile where the lock is a no-op too, which is
// the Windows cross-check lock_other.go exists to keep working.
var ErrLockTimeout = errors.New("timed out waiting for the session metadata lock")

// ErrMetadataTooLarge reports that the metadata file is larger than ReadMetadata's cap, so it
// could not be read whole and was refused rather than rebuilt over.
//
// Exported, unlike errMetadataNotJSON, because the remedy differs and callers must not give the
// wrong one: this file is intact and its permissions are fine, so "fix the permissions" is wrong
// advice. It is also permanent — every launch refuses again — which a caller may want to say
// before a full-screen viewer hides it.
var ErrMetadataTooLarge = errors.New("session metadata file is too large to read")

// errMetadataNotJSON marks the one read failure Harvest can recover from by rebuilding.
//
// A SENTINEL RATHER THAN A STRING MATCH on ReadMetadata's message, and rather than
// treating every non-permission error as a parse failure: io.ReadAll can fail with EIO
// mid-file, which reads exactly like a truncated document and must not be rebuilt over.
// Classifying on what the error IS, not on what it is not, keeps the destructive branch
// reachable only from the one case that is provably a parse failure.
//
// Unexported: callers outside this package have no decision to make with it, since
// Harvest acts on it before they see anything.
var errMetadataNotJSON = errors.New("metadata is not valid JSON")

// Options selects how much work one harvest does.
type Options struct {
	// ConfigDir is the agent config directory to read. Empty resolves via
	// DefaultConfigDir.
	ConfigDir string
	// Merge upserts into the existing file rather than replacing it.
	//
	// A harvest only ever sees the sessions its config dir holds, so replacing would
	// silently drop entries the file already had — another config directory, or a
	// session whose transcript Claude Code has since pruned.
	//
	// THE TWO SIDES OF THAT, both deliberate and neither free:
	//
	//   - Merge KEEPS an entry forever once written. A session whose transcript Claude Code has
	//     pruned keeps its title in the file indefinitely, since no harvest ever sees it again to
	//     notice it is gone. The file therefore grows monotonically with sessions-ever-seen rather
	//     than sessions-that-exist. Cheap — one small JSON object per session — but it means the
	//     file is a cache with no eviction, and `--merge=false` is the only thing that prunes it.
	//   - Merge:false DROPS every entry this harvest did not see, which includes entries from any
	//     OTHER config dir and any session pruned since. That is what makes it the way to rebuild a
	//     file whose entries are WRONG — a file that parses but says the wrong thing, which nothing
	//     else can fix — and also why it is not the default: run it with a --dir narrower than the
	//     one that produced the file and it discards the difference without asking. It is no longer
	//     needed for a file that does not parse; Harvest rebuilds that one itself, and reports it
	//     on Result.Rebuilt.
	Merge bool
	// Incremental skips transcripts no newer than the entry already recorded for
	// them, so only recently-touched sessions are parsed.
	//
	// Requires Merge, and Harvest refuses the combination rather than trusting the
	// caller: skipped transcripts contribute nothing to this harvest's map, so
	// rebuilding the file from an incremental run would drop every session it
	// skipped. This is the mode `agentop observe` uses, where the harvest is a side
	// effect of opening the viewer and a full scan of every transcript (measured:
	// 0.73-1.18s over 207MB) is too much to pay before the first frame.
	Incremental bool
}

// Result is what one harvest did, for a caller that wants to report it.
type Result struct {
	// ConfigDir is the directory actually read, after DefaultConfigDir resolution.
	ConfigDir string
	// Path is the metadata file written.
	Path string
	// Harvested counts the entries this run read from transcripts.
	Harvested int
	// Skipped counts transcripts left unparsed because they had not changed. Always
	// zero unless Options.Incremental.
	Skipped int
	// Kept counts entries retained from the existing file that this harvest did not see.
	// Always zero unless Options.Merge, and excludes Recovered — an entry another run wrote
	// concurrently was not in the file this run read, so it is not "kept" from it.
	//
	// Total = Harvested + Kept + Recovered. Replaced is NOT a term: it counts keys overwritten in
	// place, which do not change Total, so including it would drive this negative.
	Kept int
	// Total counts the entries written.
	Total int
	// Partial names transcripts whose read ended early, so their titles may be stale.
	// Not an error: those sessions are still in the map with the best title found.
	Partial []string
	// Recovered counts entries another run wrote concurrently that this one ADDED back — ids it
	// did not have. Counts only the shape that grows the map, so it can be subtracted from Total;
	// see Replaced for the other one.
	Recovered int
	// Replaced counts shared ids where another run's copy was fresher and this one adopted it.
	//
	// Its own field rather than folded into Recovered because it REPLACES a key: the map does not
	// grow, so counting it as recovered drove Kept negative and the subcommand printed
	// "-1 kept from the existing file". Not part of the Total identity below for the same reason.
	Replaced int
	// Rebuilt reports that the existing file did not parse, so this harvest replaced it from
	// the transcripts instead of merging into it.
	//
	// Worth a field because the counts alone cannot show it: a rebuild looks exactly like an
	// ordinary first run — everything Harvested, nothing Kept — and the entries it dropped
	// (sessions whose transcripts are gone) leave no trace anywhere for a caller to notice.
	Rebuilt bool
	// LockTimedOut reports that the metadata lock could not be taken before its deadline, so
	// this harvest ran unlocked and a concurrent run's rename may erase its entries.
	//
	// A field because the alternative is silence: without it, the one failure mode this lock
	// exists to prevent is invisible from outside, and "some titles vanished" has no diagnosis.
	LockTimedOut bool
	// LockFailed carries a lock failure that was NOT a timeout — flock refused for some other
	// reason, e.g. the lock file could not be created or the filesystem rejected the call.
	//
	// Separate from LockTimedOut because the two need different words: a timeout means someone
	// else holds it, while this means locking did not work at all, and reporting "timed out" for
	// an EIO would send the reader looking for a process that does not exist. Empty when locking
	// succeeded, was not attempted (no Merge), or is a no-op on this platform.
	//
	// Same consequence as a timeout, which is why it is reported at all: the harvest proceeds
	// UNLOCKED, so a concurrent run's rename can erase everything it wrote. Before this, that
	// state was the only one where wholesale loss was possible and nothing recorded it.
	LockFailed string
	// Meta is what the run wrote: the merged whole under Merge, or just this harvest
	// otherwise. Keyed by session id.
	//
	// Returned because the caller usually wants it and Harvest already holds it. Without
	// this a caller had to re-read the file it just wrote — a third read of the same file
	// in one launch, and subtly not the same thing either: a re-read returns whatever is
	// on disk when it lands, so a concurrent run's union can arrive instead of this run's
	// result. Harmless under merge semantics, but it made the function say less than it knew.
	//
	// The map is the one that was saved, not a copy, so a caller must not mutate it.
	Meta map[string]SessionMetadata
}

// Harvest reads configDir's transcripts and writes ~/.cortex/session-metadata.json.
//
// The whole of the work, with none of the reporting: every count and warning comes back
// on Result for the caller to print however suits it. That split is what lets one
// implementation serve both `agentop experimental read-claude-sessions`, where the harvest
// is the subject and reports in full, and `agentop observe`, where it is a side effect that
// must stay quiet.
func Harvest(opts Options) (Result, error) {
	if opts.Incremental && !opts.Merge {
		// Refused rather than silently honoured: an incremental harvest's map holds only
		// the sessions it actually parsed, so writing it without merging would drop every
		// entry it skipped — which is most of them, that being the point.
		return Result{}, errors.New("an incremental harvest cannot replace the file: it would drop every entry it skipped")
	}

	res := Result{ConfigDir: opts.ConfigDir}
	if res.ConfigDir == "" {
		var err error
		if res.ConfigDir, err = DefaultConfigDir(); err != nil {
			return res, err
		}
	}

	path, err := SessionMetadataPath()
	if err != nil {
		return res, err
	}
	res.Path = path

	// SERIALISE THE WHOLE READ-MODIFY-WRITE under Merge. This has to cover the transcript scan
	// too, not just the read and the save: the scan is the slow part, and it is what another run's
	// rename lands inside. Measured before this existed, two concurrent Harvests over distinct
	// config dirs left 2 of 6 sessions on disk, and the recovery pass below cannot close it — it is
	// a single re-read by construction, so it cannot converge when a rename lands inside its own
	// window. It is kept as the second line of defence, for the unlocked paths.
	//
	// Best-effort: a filesystem that cannot flock still harvests, unlocked, on the footing every
	// platform had before this. Not taken without Merge, where there is nothing to lose — that path
	// replaces the file by definition.
	// The error is recorded, not returned: proceeding unlocked is the deliberate fallback, so a
	// caller that wants to report the risk can, and one that does not still harvests.
	if opts.Merge {
		unlock, lerr := lockMetadata(path)
		switch {
		case lerr == nil:
			defer unlock()
		case errors.Is(lerr, ErrLockTimeout):
			res.LockTimedOut = true
		default:
			// ARMED, because the fallback is the same as a timeout's — harvest unlocked — but
			// nothing said so. A flock that fails for any other reason left LockTimedOut false
			// and no other trace, so the one state where a concurrent rename can erase every
			// entry this run wrote was also the one state a caller could not report.
			res.LockFailed = lerr.Error()
		}
	}

	// Read first, so an incremental harvest knows what it already has. Under Merge this
	// same map is also what unseen entries are kept from, so it is read once and used
	// for both.
	var existing map[string]SessionMetadata
	if opts.Merge {
		if existing, err = ReadMetadata(path); err != nil {
			// A FILE THAT DOES NOT PARSE IS REBUILT; a file that could not be READ is
			// refused. Refusing both is what this used to do, on the reasoning that a
			// rebuild drops entries the flag exists to keep. What that reasoning missed is
			// where the entries actually go: the only ones a rebuild loses are those whose
			// transcripts are gone, and refusing did not preserve those either — it just
			// deferred the choice onto a user who had to know --merge=false to make it,
			// while `agentop observe` showed no titles at all until they did. Since every
			// launch read the same bad file, that state never cleared itself.
			//
			// The read failure stays a refusal, and the distinction is the whole safety
			// argument: a permission or I/O error says nothing about the contents, so
			// replacing the file there would destroy entries that are very likely intact.
			if !errors.Is(err, errMetadataNotJSON) {
				return res, fmt.Errorf("%w: %w", ErrCorruptMetadata, err)
			}
			existing = map[string]SessionMetadata{}
			res.Rebuilt = true
		}
	}

	var since map[string]SessionMetadata
	if opts.Incremental && !res.Rebuilt {
		// NOT INCREMENTAL OVER A REBUILD, though today nothing observable turns on it: the
		// rebuild replaced existing with an empty map, so every transcript looks new and the
		// scan reads them all either way. NO TEST PINS THIS — a mutation removing the
		// !res.Rebuilt term passes the suite, deliberately recorded here rather than guarded
		// with a test that would only be asserting the coincidence.
		//
		// Kept because the equivalence is a property of the line above, not of this one: give
		// the rebuild any non-empty baseline — salvaged entries, a defaulted map — and a set
		// since starts skipping transcripts the rebuild exists to read. Saying "a rebuild is
		// not incremental" directly costs one term and cannot come apart.
		since = existing
	}

	meta, partial, skipped, err := ReadSessions(res.ConfigDir, since)
	if err != nil {
		return res, fmt.Errorf("reading %s: %w", res.ConfigDir, err)
	}
	res.Partial = partial
	res.Skipped = skipped
	res.Harvested = len(meta)

	if opts.Merge {
		// This harvest wins per key: it just read the transcripts, so where both have a
		// session the fresher title is here. Keys only the file has are kept — that is
		// what merging is for, and under Incremental it is also what carries every
		// skipped session forward.
		for id, m := range meta {
			existing[id] = m
		}
		meta = existing
	}
	res.Total = len(meta)

	if err := SaveMetadata(path, meta); err != nil {
		return res, fmt.Errorf("writing %s: %w", path, err)
	}

	// SECOND LINE OF DEFENCE, not the concurrency mechanism. lockMetadata above is what actually
	// serialises concurrent harvests; this pass runs behind it and matters only where the lock
	// could not be taken — a filesystem that cannot flock, or a !unix build, where lockMetadata is
	// a no-op.
	//
	// An earlier version of this comment argued the lock was unnecessary and that re-reading was
	// enough. It is not, and the argument was wrong in a way worth recording so it is not made
	// again: the merge does commute on distinct keys, but only if both writers' maps reach disk,
	// and os.Rename makes the last writer total. Measured with the lock removed, two concurrent
	// harvests over distinct config dirs left 2 of 6 sessions on disk — and six left the same.
	// This pass is a SINGLE re-read by construction, so it cannot converge when a rename lands
	// inside its own window; it narrows the race, it does not close it.
	//
	// What it does buy on an unlocked path is the one-writer-each case: an entry the other run
	// holds and this one does not is restored here, and a shared id whose other copy is fresher is
	// adopted rather than overwritten. Both are strictly better than nothing, which is why the
	// pass is kept rather than deleted once the lock existed.
	if opts.Merge {
		added, replaced, rerr := recoverConcurrentEntries(path, meta)
		if rerr != nil {
			return res, fmt.Errorf("writing %s: %w", path, rerr)
		}
		res.Recovered = added
		res.Replaced = replaced
		res.Total = len(meta)
	}
	// Set after recovery, like Kept below, so it is the map that is actually on disk.
	res.Meta = meta
	// Derived last, from the final map, so the numbers the caller prints on one line actually
	// add up. Computed before the recovery step, Kept went stale the moment recovery added an
	// entry: Total was refreshed and Kept was not, so a run that recovered anything reported
	// total != harvested + kept.
	//
	// Recovered is subtracted rather than folded in, so Kept means what the caller CALLS it —
	// "kept from the existing file". An entry another run wrote while this one worked was not in
	// the file this run read, so counting it as kept attributed it to the wrong source; the
	// arithmetic balanced either way, which is exactly why the label could drift unnoticed.
	//
	// Total = Harvested + Kept + Recovered, and ONLY those three. Recovered counts ids ADDED by the
	// recovery pass; a shared id whose fresher copy it adopted lands in Replaced instead, because
	// overwriting a key leaves Total unchanged and subtracting it here made Kept negative — an
	// incremental run with Total=1, Harvested=1 and one adopted title printed "-1 kept from the
	// existing file". Anything added to this expression has to grow the map.
	res.Kept = res.Total - res.Harvested - res.Recovered
	return res, nil
}

// DefaultConfigDir resolves Claude Code's config directory: CLAUDE_CONFIG_DIR when set,
// else ~/.claude.
func DefaultConfigDir() (string, error) {
	if d := os.Getenv(ConfigDirEnv); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	if home == "" {
		// A separate branch, not folded into the one above: %w on a nil error renders
		// as "%!w(<nil>)", which would make this path unreadable to whoever hits it.
		// Same shape as userConfigPath.
		return "", errors.New("cannot determine your home directory: it is empty")
	}
	return filepath.Join(home, ".claude"), nil
}

// ReadSessions harvests every session transcript under configDir/projects.
//
// One level down only — projects/<project>/<id>.jsonl. Deliberately not
// filepath.WalkDir, which the rest of this module uses: deeper files are subagent
// transcripts (<id>/subagents/agent-*.jsonl) whose basenames are agent ids, not
// session ids, so walking would key 92 non-sessions into the map on the machine this
// was written against.
//
// A missing projects directory is not an error: a machine that has never run Claude
// Code has none, and an empty result already says so.
//
// A non-nil since makes the read INCREMENTAL: a transcript no newer than the entry
// since already holds for it is skipped, on the reasoning that a file that has not
// changed cannot have grown a new title. Skipped sessions are absent from the returned
// map rather than copied into it, so an incremental caller MUST merge the result over
// since — see Harvest, which refuses to do otherwise. Pass nil to parse everything.
//
// The second return names transcripts whose title may be incomplete — a read that stopped
// early. Not an error: those sessions are still in the map with the best title found. The
// caller reports them, because a truncated read is otherwise indistinguishable from a
// session that has no title. The third counts what since skipped.
func ReadSessions(configDir string, since map[string]SessionMetadata) (map[string]SessionMetadata, []string, int, error) {
	projects := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]SessionMetadata{}, nil, 0, nil
		}
		return nil, nil, 0, err
	}
	var partial []string
	skipped := 0

	out := make(map[string]SessionMetadata)
	// Tracks which transcript won each id, so a duplicate is resolved by mtime below.
	won := make(map[string]time.Time)
	for _, e := range entries {
		projDir := filepath.Join(projects, e.Name())
		// os.Stat, not e.IsDir(): ReadDir reports Lstat semantics, so IsDir is FALSE for a
		// symlink pointing at a directory and every session under it was skipped without a
		// word. A relocated or shared project directory is a reasonable thing to have, and
		// #1018 asks for multiple config directories, which is the same shape.
		fi, err := os.Stat(projDir)
		if err != nil || !fi.IsDir() {
			continue
		}
		files, err := os.ReadDir(projDir)
		if err != nil {
			// One unreadable project directory must not lose the other hundred.
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(projDir, f.Name())
			// Same reason as the directory check above: a symlinked transcript reports
			// itself as an irregular file, not a regular one, so test the target.
			st, err := os.Stat(path)
			if err != nil || st.IsDir() {
				continue
			}
			id := strings.TrimSuffix(f.Name(), ".jsonl")
			// Newest wins, not last-read. ReadDir sorts by filename, so without this the
			// winner of a duplicated id is whichever project directory sorts later —
			// alphabetical order deciding which title is current, silently. Rare (measured:
			// 0 duplicates in 109 local sessions) but the cost of getting it wrong is a
			// stale title that looks authoritative.
			if prev, dup := won[id]; dup && !st.ModTime().After(prev) {
				continue
			}
			// Incremental: this transcript has not changed since the entry naming it was
			// harvested, so its title cannot have changed either. Measured on a real tree,
			// this skips 122 of 124 transcripts and 202 of 207 MB.
			//
			// Compared against the ENTRY's own file, not the metadata file's mtime: that
			// file is rewritten by every save, including saves that learned nothing about
			// this session, so its clock runs ahead of what it actually knows. An entry
			// that cannot be validated — absent, no LogFile, a LogFile naming some other
			// path, or one that no longer exists — is NOT skipped. Skipping is the
			// optimisation; parsing is the correct default.
			//
			// KNOWN GAP: mtime is the only signal, so a rewrite that PRESERVES it — rsync -t,
			// a restore from backup, a clock stepping back — defeats the skip and pins
			// whatever title the entry already had. Recording the transcript's SIZE alongside
			// the mtime would narrow it to same-mtime-and-same-size rewrites for one extra
			// int64 per entry, and the Stat here already has it. Deliberately not done in this
			// change: it is a second on-disk field for a case the explicit subcommand already
			// answers, and the field added here needs to prove itself first.
			if since != nil && !st.ModTime().After(harvestedAt(since[id], path)) {
				// A skipped transcript still WON this id: the entry being kept from `since`
				// carries its title. So claim it, and drop any weaker claim an older
				// duplicate already staked in an earlier directory — ReadDir is
				// alphabetical, not chronological, so the loser may well have been seen
				// first. Without both halves the older transcript's title survived in the
				// result and overwrote the newer one on merge, alternating the displayed
				// title between the two names on consecutive launches instead of
				// converging.
				won[id] = st.ModTime()
				delete(out, id)
				// The subagent labels are re-read anyway. A workflow (Claude Code's Workflow
				// tool) starts its agents without a write to the main transcript, so the
				// mtime above does not move while new agents start. The meta files are small.
				if subs := readSubagents(filepath.Join(projDir, id, "subagents")); !maps.Equal(subs, since[id].Subagents) {
					kept := since[id]
					kept.Subagents = subs
					out[id] = kept
				}
				skipped++
				continue
			}
			won[id] = st.ModTime()
			title, terr := titleFromTranscript(path)
			if terr != nil {
				// Collected rather than returned: one bad transcript must not cost the
				// other hundred names, which is the whole reason this is best-effort. The
				// caller prints a bounded summary so it is visible without turning a
				// 109-session harvest into 109 lines of stderr.
				partial = append(partial, fmt.Sprintf("%s: %v", path, terr))
			}
			out[id] = SessionMetadata{
				Title:          title,
				AgentType:      "Claude Code",
				AgentConfigDir: configDir,
				LogFile:        path,
				LogModTime:     st.ModTime(),
				// The subagents' labels live in a sibling DIRECTORY of the transcript
				// (<id>/subagents), whose mtime the skip above knows nothing about, so the
				// skip re-reads them on its own.
				Subagents: readSubagents(filepath.Join(projDir, id, "subagents")),
			}
		}
	}
	return out, partial, skipped, nil
}

// readSubagents reads one session's subagents/agent-<id>.meta.json files into a map keyed
// by the bare id. It also reads subagents/workflows/<run>/agent-<id>.meta.json, where
// Claude Code puts the agents that its Workflow tool starts. Nil when the directory is
// missing or holds nothing harvestable — a session that never spawned a subagent, which
// is most of them, must not carry an empty map the JSON would then spell out.
//
// Best-effort per file, for the same reason titleFromTranscript is: one unparseable meta
// file must not cost the other labels. The transcripts sitting beside the meta files are
// deliberately not read — see SessionMetadata.Subagents.
func readSubagents(dir string) map[string]SubagentInfo {
	out := readSubagentDir(dir, nil)
	runs, _ := os.ReadDir(filepath.Join(dir, "workflows"))
	for _, r := range runs {
		if r.IsDir() {
			out = readSubagentDir(filepath.Join(dir, "workflows", r.Name()), out)
		}
	}
	return out
}

// readSubagentDir adds the agent-<id>.meta.json files in dir to out, and returns out.
func readSubagentDir(dir string, out map[string]SubagentInfo) map[string]SubagentInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		// The subagent transcripts share the agent- prefix (agent-<id>.jsonl), so the
		// suffix is what says which kind of file this is.
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".meta.json")
		if id == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // operator-supplied transcript dir
		if err != nil {
			continue
		}
		var info SubagentInfo
		if json.Unmarshal(data, &info) != nil {
			continue
		}
		if out == nil {
			out = make(map[string]SubagentInfo)
		}
		out[id] = info
	}
	return out
}

// harvestedAt reports the transcript mtime m was harvested at, for the incremental skip.
// The zero time means "cannot tell", which never skips.
//
// want is the transcript being considered. A LogFile naming anything else means this entry
// was harvested from a DIFFERENT file for the same session id — two config dirs, or a moved
// tree — and cannot vouch for this one.
//
// Reads the RECORDED time rather than re-Stat'ing LogFile. Re-Stat'ing compares the
// transcript's mtime with itself, which is never "after", so it would skip every transcript
// forever; see LogModTime.
func harvestedAt(m SessionMetadata, want string) time.Time {
	if m.LogFile == "" || m.LogFile != want {
		return time.Time{}
	}
	return m.LogModTime
}

// ReadMetadata reads the existing file, distinguishing absent from unreadable.
//
// An empty map with a nil error means genuinely no file yet — the first run, which is
// not a problem. A non-nil error means a file was there and could not be trusted; what
// the caller should DO about it depends on which error, and this function's job is only
// to keep them apart. Same distinction, for the same reason, as readState in
// cmd_claudecode.go.
//
// Three outcomes a caller can discriminate, because Harvest treats them differently:
// errMetadataNotJSON for a file whose bytes are not JSON, which Harvest rebuilds over;
// ErrMetadataTooLarge for a valid file past the read cap, which it refuses because
// rebuilding would destroy intact entries; and a bare os/io error — permission, EIO —
// which it also refuses, since nothing there says the contents are bad.
//
// A file holding JSON `null` decodes to a nil map, which is indistinguishable from an
// empty object for merging purposes, so it is normalised rather than refused.
func ReadMetadata(path string) (map[string]SessionMetadata, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]SessionMetadata{}, nil
		}
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	// BOUNDED at the same 16 MiB the viewer's own reader of this file uses, for the same reason:
	// a stray large file at this path would otherwise be read whole and decoded before the viewer
	// starts — `agentop observe` calls this synchronously to check the file is readable, so an
	// unbounded read stalls startup with nothing on screen to say why. Far past any real metadata
	// file: the measured 192-session file is 74 KB.
	//
	// THE CAP IS SHARED; THE CONTRACT AT IT IS NOT, and the difference is deliberate rather than
	// an oversight to be unified. This function has a caller that can act — it returns an error,
	// so the CLI and the pre-flight both name a remedy — and refuses loudly with
	// ErrMetadataTooLarge. tui.LoadSessionMetadata has nowhere to report anything (it runs while
	// the model is built, before tea.NewProgram owns the screen) and so returns an empty map,
	// costing the TITLE column. What both must agree on is WHICH files are too large, which is
	// why the constant and the read-one-past-it shape are duplicated there rather than eyeballed:
	// they once disagreed, and the viewer silently decoded a truncated prefix of a file this one
	// refused whole.
	//
	// Truncation is reported as ITS OWN failure, not left to surface as a JSON error. A valid
	// file over the cap decodes as a parse failure, and Harvest rebuilds over parse failures —
	// which would replace a good 17 MB file with a 400-byte one. So the cap is checked before
	// the decode, and the error deliberately carries its own sentinel: not errMetadataNotJSON,
	// so it cannot be rebuilt over; not a bare read error either, because nothing is wrong with the
	// bytes — it carries ErrMetadataTooLarge, which the CLI and the viewer's pre-flight each
	// branch on to give the right remedy. Read one over the cap to tell "exactly at the cap"
	// from "larger".
	const maxMetadataBytes = 16 << 20
	b, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxMetadataBytes {
		return nil, fmt.Errorf("%w: %s is larger than the %d byte read limit", ErrMetadataTooLarge, path, maxMetadataBytes)
	}
	var m map[string]SessionMetadata
	if uerr := json.Unmarshal(b, &m); uerr != nil {
		// Sentinel-wrapped so Harvest can tell a parse failure from a file it could not
		// read: only the former is safe to rebuild over. The message keeps the path and
		// the decoder's own detail, both of which reach the user.
		return nil, fmt.Errorf("%w: %s is not valid JSON: %w", errMetadataNotJSON, path, uerr)
	}
	if m == nil {
		return map[string]SessionMetadata{}, nil
	}
	return m, nil
}

// recoverConcurrentEntries re-reads the just-written file and takes back anything another process
// left there while this one was working. Returns how many entries it adopted.
//
// A NARROWING, NOT A FIX. Callers serialise with lockMetadata; this runs behind that lock and earns
// its keep only where the lock could not be taken. Re-merging what is on disk over what we just
// wrote does yield the union when exactly one other writer is involved and its rename already
// landed — but it is a single pass, so a rename inside its own window is still lost, and with the
// lock removed two concurrent harvests measurably lose entries (2 of 6). Do not reintroduce the
// argument that this makes the write path safe on its own.
//
// Two shapes are adopted: an id only the other run has, and a shared id whose copy on disk is
// FRESHER by LogModTime. The second was missing and mattered more than it looks — the run that
// skipped a transcript holds the older title, so without it a rename recorded by the other run was
// written straight back out, and stayed reverted until the transcript changed again. Ties and
// incomparable pairs keep ours: no basis for preferring theirs, and we know our own provenance.
//
// A read error here is deliberately NOT fatal: our own save already succeeded, so the file on
// disk is valid and this is a best-effort recovery of someone else's entries. A write error is
// fatal, because at that point the file on disk is missing entries this function set out to
// put back.
func recoverConcurrentEntries(path string, meta map[string]SessionMetadata) (added, replaced int, err error) {
	after, err := ReadMetadata(path)
	if err != nil {
		return 0, 0, nil
	}
	for id, theirs := range after {
		ours, have := meta[id]
		if !have {
			// An id only they harvested. GROWS the map, so it counts toward Recovered, which the
			// caller's arithmetic treats as a new entry.
			meta[id] = theirs
			added++
			continue
		}
		// A SHARED id, and this is the half that was missing. Taking ours unconditionally wrote
		// our value back over theirs, so a run holding a STALE title for a session — which under
		// Incremental is simply the run that skipped that transcript — reverted a rename the other
		// run had just recorded, and it stayed reverted until the transcript changed again.
		//
		// Resolved by LogModTime, the same field the incremental skip compares, so "fresher" means
		// one thing across this package. Ours wins every tie and every incomparable pair (neither
		// side carrying a time, which is also what pre-upgrade entries look like): there is no
		// basis for preferring theirs, and this run at least knows its own provenance.
		//
		// Counted SEPARATELY, because this branch REPLACES a key rather than adding one: len(meta)
		// is unchanged. Folding it into the same counter made the caller's
		// Kept = Total - Harvested - Recovered go negative — an incremental run that harvested one
		// transcript and adopted one fresher shared title reported Total=1, Harvested=1,
		// Recovered=1, so Kept=-1, and the subcommand printed "-1 kept from the existing file".
		// Two shapes, two counters; only the one that grows the map may be subtracted from a total.
		if theirs.LogModTime.After(ours.LogModTime) {
			meta[id] = theirs
			replaced++
		}
	}
	if added == 0 && replaced == 0 {
		return 0, 0, nil
	}
	// Either shape needs the save: the map now differs from what we were about to write.
	if err := SaveMetadata(path, meta); err != nil {
		return 0, 0, err
	}
	return added, replaced, nil
}

// writeAll writes body to w. A var, not a call, purely as a test seam.
//
// The bug it guards is the `err :=` shadow in SaveMetadata: a scoped error would be dropped,
// and Close and Rename would then rename a TRUNCATED file over the good one. Nothing a test
// can provoke on a working filesystem — a short write without an error is not something a
// real file does — so the injection point is the only way to reach that path.
var writeAll = func(w io.Writer, body []byte) (int, error) { return w.Write(body) }

// syncFile flushes f to disk. A test seam that lets a Sync failure be injected.
var syncFile = func(f *os.File) error { return f.Sync() }

// SaveMetadata writes the map atomically, creating ~/.cortex if needed.
//
// Same mechanics as saveUserConfig, for the same reasons: CreateTemp rather than a
// fixed path+".tmp" (crash debris at a predictable name wedges every later write, and
// CreateTemp's unpredictable name passes O_EXCL itself), Rename rather than truncate
// (a half-written file would be read back as malformed), dir 0700 and file 0600. Plus
// an fsync between write and rename, so a crash leaves either the old file or the new
// one. File-level guarantee only: a crash between rename and the directory entry hitting
// disk still loses the update. #1554 tracks the directory-fsync upgrade across the tree.
//
// SECURITY: the residual symlink risk is closed the way saveUserConfig closes it, not with
// tui's checkYankDir Lstat sweep. An earlier version of this comment argued the sweep was
// unnecessary because these paths name the reader's own ~/.claude tree — but saveUserConfig's
// own tripwire names "a filesystem path" as the trigger for the sweep's posture, and reading
// that clause as "unless the path looks harmless" is how a tripwire stops working. What the
// two cases actually share is the mechanism: os.CreateTemp picks a name an attacker cannot
// predict and passes O_EXCL itself, and os.Rename REPLACES a symlink at the destination
// rather than following it, so a planted ~/.cortex/session-metadata.json symlink is
// overwritten, not written through.
//
// What the sweep would add here is a check on the DIRECTORY chain, and it is declined for the
// reason saveUserConfig declines it: its other half chmods 0700 in place, so merely running
// this would retighten a ~/.cortex an installer deliberately set. The narrower half of that
// tradeoff — refusing to write through a symlinked ~/.cortex — is the part worth having, and
// is a follow-up noted in the PR rather than a silent omission.
//
// What would change this calculus outright: harvesting an agent whose config lives somewhere
// the user cannot already read, or adding any field carrying transcript CONTENT rather than a
// path. Either makes this file carry something its reader could not otherwise obtain.
func SaveMetadata(path string, meta map[string]SessionMetadata) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// err, not a fresh `err :=`: a scoped one would be discarded and Close and Rename
	// would then see success, renaming a truncated file over the good one. The bug
	// saveUserConfig's comment records having made.
	//
	// Through writeAll, not f.Write directly, so a test can make the write fail while Close
	// and Rename still succeed — the only shape that catches the shadowing above, and one no
	// real filesystem produces on demand. cmd/agentop has its own copy of this seam for
	// saveUserConfig; the duplication is two lines and buys each module its own tripwire.
	_, err = writeAll(f, body)
	if err == nil {
		err = syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// CreateTemp is 0600 already; chmod is belt-and-braces against a umask surprise.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
