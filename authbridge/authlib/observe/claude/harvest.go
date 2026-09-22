package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConfigDirEnv is the variable Claude Code itself honours for relocating its config
// tree. Read here so a user who has moved it is not told there are no sessions.
const ConfigDirEnv = "CLAUDE_CONFIG_DIR"

// ErrCorruptMetadata reports that an existing metadata file could not be trusted, so a
// merge refused rather than rebuilding from scratch and dropping its entries.
//
// Exported because the repair is a CLI affordance: `abctl experimental
// read-claude-sessions` names --merge=false as the way past, and only the command layer
// knows its own flags. Callers discriminate with errors.Is.
var ErrCorruptMetadata = errors.New("corrupt session metadata")

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
	//     wrong file, and also why it is not the default: run it with a --dir narrower than the one
	//     that produced the file and it discards the difference without asking.
	Merge bool
	// Incremental skips transcripts no newer than the entry already recorded for
	// them, so only recently-touched sessions are parsed.
	//
	// Requires Merge, and Harvest refuses the combination rather than trusting the
	// caller: skipped transcripts contribute nothing to this harvest's map, so
	// rebuilding the file from an incremental run would drop every session it
	// skipped. This is the mode `abctl observe` uses, where the harvest is a side
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
	// Total = Harvested + Kept + Recovered.
	Kept int
	// Total counts the entries written.
	Total int
	// Partial names transcripts whose read ended early, so their titles may be stale.
	// Not an error: those sessions are still in the map with the best title found.
	Partial []string
	// Recovered counts entries another run wrote concurrently and this one put back.
	Recovered int
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
// implementation serve both `abctl experimental read-claude-sessions`, where the harvest
// is the subject and reports in full, and `abctl observe`, where it is a side effect that
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
	if opts.Merge {
		if unlock, lerr := lockMetadata(path); lerr == nil {
			defer unlock()
		}
	}

	// Read first, so an incremental harvest knows what it already has. Under Merge this
	// same map is also what unseen entries are kept from, so it is read once and used
	// for both.
	var existing map[string]SessionMetadata
	if opts.Merge {
		if existing, err = ReadMetadata(path); err != nil {
			// Wrapped in a sentinel rather than returned bare. A corrupt file read as
			// absent would silently rebuild from scratch under the flag whose whole
			// purpose is not losing entries — the same trap readState exists to close
			// for claude-code-state.json. The caller names the repair.
			return res, fmt.Errorf("%w: %w", ErrCorruptMetadata, err)
		}
	}

	var since map[string]SessionMetadata
	if opts.Incremental {
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

	// A concurrent Merge run could have written between our read and our rename, and
	// os.Rename would have replaced its entries with a map that never contained them —
	// entries that are unrecoverable once Claude Code prunes the transcript they came from.
	//
	// Closed by re-reading and re-merging rather than by an interprocess lock. A lock is the
	// textbook answer and is what review asked for twice, but there is no file locking anywhere
	// in this tree, so it would introduce a primitive on the shared write path of two commands.
	// This costs one extra read of a small file per run and introduces nothing.
	//
	// HOW OFTEN: more often than an earlier version of this comment claimed. "A race that needs
	// two harvests running at once" was fair when only `abctl experimental read-claude-sessions`
	// harvested, and nobody runs that twice concurrently. Every `abctl observe` now harvests by
	// default, and several viewers open at once is ordinary on a machine with one session per
	// branch — so the window is narrow, not exotic.
	//
	// What still makes it acceptable is the shape of the data, not the odds: the merge is
	// commutative on distinct keys, two runs over the same config dir agree on every shared key
	// anyway, and the only lossy case — an entry one run holds and the other does not — is
	// exactly what the pass below restores. The worst outcome is a title that has to be
	// harvested again, not a corrupt file.
	if opts.Merge {
		recovered, rerr := recoverConcurrentEntries(path, meta)
		if rerr != nil {
			return res, fmt.Errorf("writing %s: %w", path, rerr)
		}
		res.Recovered = recovered
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
	// Total = Harvested + Kept + Recovered.
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
			}
		}
	}
	return out, partial, skipped, nil
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

// titleFromTranscript reads one transcript and returns the best name for it.
//
// The model-generated title when the transcript carries one, otherwise the working
// directory it ran in, otherwise "". Both are LAST-wins: a session can be titled more
// than once and can change directory, and the newest claim is the current one.
//
// Returns "" rather than an error for an unreadable or malformed transcript. A
// missing title costs a label; refusing the whole harvest over one bad file would
// cost every other name. That is the opposite of toolscan's choice on the same files,
// and deliberately so: there, under-reporting makes it propose removing MORE tools,
// so it must fail loudly.
//
// The second return reports a read that ENDED EARLY — a line past the 16MB buffer, or an
// I/O error partway through. The title is still returned, because whatever was found
// before the stop is better than nothing, but the caller must be able to say so: this
// function's fallbacks make a truncated read indistinguishable from a session that simply
// has no title, and the difference matters when the answer is a stale title from early in
// a long session rather than the current one.
func titleFromTranscript(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied transcript dir
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only

	sc := bufio.NewScanner(f)
	// Transcript lines routinely exceed the default 64KB — the largest single line
	// measured locally was 1.36MB — and a Scanner past its limit stops silently, which
	// would drop the title of exactly the longest sessions. Same buffer as
	// toolscan.scanFile.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	var title, cwd string
	for sc.Scan() {
		line := sc.Bytes()
		// Hot path: most lines are conversation turns carrying neither field. A
		// substring test over the raw bytes is far cheaper than parsing them, and
		// bytes.Contains avoids the copy that strings.Contains(string(line), …) makes.
		if !bytes.Contains(line, []byte(`"aiTitle"`)) && !bytes.Contains(line, []byte(`"cwd"`)) {
			continue
		}
		var e transcriptMeta
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.Cwd != "" {
			cwd = e.Cwd
		}
		// Guarded on Type as well as on the value: "aiTitle" appearing on some other
		// line kind is not a title claim.
		if e.Type == "ai-title" && e.AiTitle != "" {
			title = e.AiTitle
		}
	}
	// Reported, not discarded. A truncated read still yields whatever was found before the
	// stop — which is why the title is returned alongside the error rather than dropped —
	// but "token too long" means every LATER title claim in the file went unseen, and
	// last-wins is the rule here, so the name returned may be an old one. The buffer above
	// makes this rare; silence made it invisible.
	err = sc.Err()
	if title != "" {
		return title, err
	}
	return cwd, err
}

// transcriptMeta is the minimum shape needed from a transcript line. Decoding only
// these fields keeps the parse cheap on multi-megabyte lines.
type transcriptMeta struct {
	Type    string `json:"type"`
	AiTitle string `json:"aiTitle"`
	Cwd     string `json:"cwd"`
}

// ReadMetadata reads the existing file, distinguishing absent from unreadable.
//
// An empty map with a nil error means genuinely no file yet — the first run, which is
// not a problem. A non-nil error means a file was there and could not be trusted, and
// the caller must say so out loud rather than proceeding: under merge, treating a
// corrupt file as empty would discard exactly the entries the flag exists to keep.
// Same distinction, for the same reason, as readState in cmd_claudecode.go.
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
	// BOUNDED, the same 16 MiB the viewer's own reader of this file uses. Both now cap it, and
	// for the same reason: a stray large file at this path would otherwise be read whole and
	// decoded before the viewer starts — `abctl observe` calls this synchronously to check the
	// file is readable, so an unbounded read stalls startup with nothing on screen to say why.
	// Far past any real metadata file: the measured 192-session file is 74 KB.
	//
	// Truncation surfaces as a JSON error rather than as silent data loss, which is the right
	// outcome here: this reader's caller refuses to merge over a file it cannot parse, so a
	// file too large to read is treated like any other unreadable one instead of quietly
	// becoming a smaller map.
	const maxMetadataBytes = 16 << 20
	b, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes))
	if err != nil {
		return nil, err
	}
	var m map[string]SessionMetadata
	if uerr := json.Unmarshal(b, &m); uerr != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, uerr)
	}
	if m == nil {
		return map[string]SessionMetadata{}, nil
	}
	return m, nil
}

// recoverConcurrentEntries re-reads the just-written file and restores any entry another
// process added while this one was working. Returns how many it restored.
//
// Correct because the merge is idempotent and commutative on distinct keys: re-merging what is
// now on disk over what we just wrote yields the union either way, whichever process renamed
// last. Bounded to a single pass — a second collision means someone is running this in a loop,
// and the entries survive in the transcripts for the next run.
//
// A read error here is deliberately NOT fatal: our own save already succeeded, so the file on
// disk is valid and this is a best-effort recovery of someone else's entries. A write error is
// fatal, because at that point the file on disk is missing entries this function set out to
// put back.
func recoverConcurrentEntries(path string, meta map[string]SessionMetadata) (int, error) {
	after, err := ReadMetadata(path)
	if err != nil {
		return 0, nil
	}
	recovered := 0
	for id, theirs := range after {
		ours, have := meta[id]
		if !have {
			// An id only they harvested. The original and still the main case.
			meta[id] = theirs
			recovered++
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
		// Counted in `recovered`, which is what forces the save below — without it the map would
		// hold the fresher title and never write it. The count is therefore "entries this pass
		// took from the other run", which covers both shapes; the caller prints it as
		// "written concurrently by another run", still true of a shared id it just adopted.
		if theirs.LogModTime.After(ours.LogModTime) {
			meta[id] = theirs
			recovered++
		}
	}
	if recovered == 0 {
		return 0, nil
	}
	if err := SaveMetadata(path, meta); err != nil {
		return 0, err
	}
	return recovered, nil
}

// SaveMetadata writes the map atomically, creating ~/.cortex if needed.
//
// Same mechanics as saveUserConfig, for the same reasons: CreateTemp rather than a
// fixed path+".tmp" (crash debris at a predictable name wedges every later write, and
// CreateTemp's unpredictable name passes O_EXCL itself), Rename rather than truncate
// (a half-written file would be read back as malformed), dir 0700 and file 0600.
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
	// TRIPWIRE LOST IN THE MOVE, recorded rather than left silent: in package main this was
	// `writeAll(f, body)`, an indirected io.Writer.Write that a test could swap for a failing
	// one — added because the shadowing bug above is invisible to every test that writes to a
	// working filesystem. That var stays in cmd/abctl for saveUserConfig, which is what its
	// own test swaps, and it cannot travel here without duplicating it across two modules. No
	// test ever reached it through the harvest path (the write-failure test uses an
	// unwritable directory instead), so nothing regressed today — but the injection point is
	// gone, so a future edit that reintroduces the shadowing has one fewer way to be caught.
	// Restoring it is three lines if that ever feels too thin.
	_, err = f.Write(body)
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
