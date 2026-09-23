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
	"unicode"
	"unicode/utf8"
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
// TWO LINE KINDS CARRY A TITLE — {"type":"ai-title","aiTitle":…} and
// {"type":"agent-name","agentName":…} — and both feed the same last-wins value. Which one an
// install writes varies: one local config dir had agent-name in 16 transcripts and ai-title in
// only 2, with no overlap, so reading ai-title alone left most of those sessions named by their
// working directory instead of their real title.
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

	// lastPrompt is Claude Code's own record and outranks every reconstruction below it.
	//
	// THREE GRADES OF RECONSTRUCTED PROMPT, best first. Each is last-wins within its own grade, so a later turn
	// of the same quality replaces an earlier one but never a better one.
	//
	//   human  — origin.kind == "human" AND string content. What the person typed, stated by
	//            Claude Code rather than inferred. 640 such turns across 128 measured transcripts,
	//            every one of them a string.
	//   str    — string content with no origin field, the shape older transcripts use. Still a
	//            prompt in practice, just unattributed.
	//   blocks — text extracted from a content ARRAY. Last resort: these are where the harness
	//            injects, and a "Base directory for this skill: …" title came from one.
	var title, cwd, lastPrompt, human, str, blocks string
	for sc.Scan() {
		line := sc.Bytes()
		// Hot path: most lines are conversation turns carrying neither field. A
		// substring test over the raw bytes is far cheaper than parsing them, and
		// bytes.Contains avoids the copy that strings.Contains(string(line), …) makes.
		//
		// BARE KEYS ONLY. `"role":"user"` is more selective — it matched 22% of lines against 59%
		// for `"role"` — but it embeds a key-value PAIR and so assumes compact JSON: a line written
		// as `"role": "user"` would be skipped before decoding, silently losing the prompt. Every
		// other term here is a bare key for that reason, and the role is re-checked after the
		// decode anyway, so the narrow form bought selectivity at the cost of depending on the
		// writer's spacing. Measured on a real tree it also did not pay: 1.29s against 1.19s for
		// the bare key, because the scan is dominated by a few very large lines rather than by how
		// many lines reach the decoder.
		if !bytes.Contains(line, []byte(`"aiTitle"`)) &&
			!bytes.Contains(line, []byte(`"agentName"`)) &&
			!bytes.Contains(line, []byte(`"cwd"`)) &&
			!bytes.Contains(line, []byte(`"lastPrompt"`)) &&
			!bytes.Contains(line, []byte(`"role"`)) &&
			// A UNICODE-ESCAPED member name would not match any literal above: JSON permits
			// `"last\u0050rompt"`, which decodes to lastPrompt but is skipped by a raw-byte scan.
			// Nothing observed writes keys that way — 0 of 46,124 real lines — so this is latent,
			// and it is taken only because it is nearly free: just 0.2% of lines contain a `\u`
			// escape at all, so admitting them costs a decode on one line in five hundred.
			!bytes.Contains(line, []byte(`\u`)) {
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
		// The other spelling, treated as equal rather than as a fallback. Both feed the same
		// last-wins `title`, so whichever line appears later in the transcript is the current
		// name — the same rule that already applies between two ai-title lines. Where a
		// transcript carries both kinds they agree anyway (measured: 16 of 16), so ordering
		// them against each other would be inventing a distinction the data does not have.
		if e.Type == "agent-name" && e.AgentName != "" {
			title = e.AgentName
		}
		// LAST-WINS on the user's own prompts, kept as a third tier below both title kinds. Only
		// consulted when neither fired, so a titled session is never renamed by its transcript.
		//
		// Guarded on both the line type and the message role: an assistant turn is not a prompt,
		// and `"role"` appears on every turn either way. Tool output and Claude Code's own
		// bracketed markers are filtered by the two helpers rather than here, so this stays a
		// statement about WHICH turn counts.
		// Claude Code's own record of the last prompt. Preferred over anything reconstructed from
		// user turns below: same information, stated by the agent instead of inferred, and it
		// arrives already free of the tool output and harness envelopes those turns carry. Still
		// put through the same unwrap and synthetic checks, since the value is the prompt text and
		// a slash command is recorded in its envelope form there too.
		if e.Type == "last-prompt" && e.LastPrompt != "" {
			// ONE SHAPE, shared with the human branch below via promptCandidate, so a later fix to
			// the unwrap order cannot land in only one of them. The two used to spell the same
			// intent differently.
			if p, ok := promptCandidate(e.LastPrompt); ok {
				lastPrompt = p
			}
		}
		if e.Type == "user" && e.Message != nil && e.Message.Role == "user" {
			kind := ""
			if e.Origin != nil {
				kind = e.Origin.Kind
			}
			text, wasString := promptFromMessage(e.Message.Content)
			switch {
			case text == "":
				// Nothing typed: a tool_result array, or blocks with no text.
			case kind == "human" && wasString:
				// ATTRIBUTED, so the text is taken as-is. No synthetic-prompt filter here: a
				// slash command the user really typed arrives as
				// "<command-message>review</command-message>…", which the structural filter
				// would reject — and origin already settles authorship, so guessing from the
				// text would only overrule better evidence. The envelopes a typed turn arrives in
				// are unwrapped rather than filtered, for the same reason: the user did type it,
				// so the answer is to render what they typed rather than to drop the turn.
				// ORDER MATTERS. The command envelope is read FIRST: it is a multi-tag structure
				// whose <command-args> sit past the first tag, so stripping a leading wrapper
				// beforehand threw the arguments away and left a bare "/review". Only a turn that
				// is not a command envelope reaches the wrapper strip.
				if p, ok := promptCandidate(text); ok {
					human = p
				}
			case kind != "" && kind != "human":
				// Explicitly NOT human — "task-notification" or "peer". Discarded outright;
				// this is the traffic the text heuristics existed to catch.
			case isSyntheticPrompt(text):
				// Unattributed and looks machine-written. Still filtered, because transcripts
				// with no origin field at all have nothing better to go on.
			case wasString:
				str = text
			default:
				blocks = text
			}
		}
	}
	// Reported, not discarded. A truncated read still yields whatever was found before the
	// stop — which is why the title is returned alongside the error rather than dropped —
	// but "token too long" means every LATER title claim in the file went unseen, and
	// last-wins is the rule here, so the name returned may be an old one. The buffer above
	// makes this rare; silence made it invisible.
	err = sc.Err()
	// FOUR TIERS, in descending confidence: a title Claude Code generated, then its own record of
	// the last prompt, then the last prompt this package reconstructs from user turns, then the
	// directory the session ran in. The prompt tier is what
	// takes a tree from "mostly paths" to "mostly readable" — measured on one config dir, 110 of
	// 128 sessions had no title line of either kind and fell through to a cwd, and every one of
	// those has a usable prompt.
	//
	// Clipped at the source. A prompt is unbounded and a title is a table cell; see MaxTitleLen.
	// NORMALISED BEFORE the switch, not inside each arm. A whitespace-only candidate passes a
	// bare `!= ""` and clipTitle then empties it, so the tier below was skipped and the cell came
	// out blank — testing the clipped value is what makes each guard mean "this tier has
	// something to show".
	title, lastPrompt = clipTitle(title), clipTitle(lastPrompt)
	human, str = clipTitle(human), clipTitle(str)
	blocks = clipTitle(blocks)
	switch {
	case title != "":
		return title, err
	case lastPrompt != "":
		return lastPrompt, err
	case human != "":
		return human, err
	case str != "":
		return str, err
	case blocks != "":
		return blocks, err
	}
	// THE CWD IS NOT CLIPPED. A path's distinguishing end is its LEAF, and clipping keeps the
	// head: two sibling worktrees under a prefix of 80 runes or more clip to byte-identical
	// titles, so the column stops telling them apart — exactly what it is for. Clipping the cwd
	// was also a regression against the behaviour before this change, which never truncated here.
	//
	// LEFT-TRUNCATION IS A PROPERTY OF THE LEADING SLASH, NOT OF BEING A CWD, and this function
	// cannot convey which is which: a prompt and a cwd come back through the same string, so the
	// renderer decides by looking at the first character. A cwd therefore keeps its tail only
	// because it starts with "/" — and since this change a "/review …" prompt takes that branch
	// too, while a relative cwd would not. An earlier version of this comment claimed the renderer
	// truncates "a path" from the left, which overstated what it can know.
	//
	// So the reason to leave it long is narrower: a path is bounded by the filesystem, where a
	// prompt is unbounded free text, and the cap exists for the latter. The cell stays bounded
	// either way, because the renderer truncates whatever it is handed.
	//
	// It is still normalised — whitespace collapsed, markup and non-graphic runes dropped — so a
	// cwd cannot carry hidden characters into a cell any more than a prompt can.
	return normalizeTitle(cwd), err
}

// MaxTitleLen caps a harvested title, in RUNES.
//
// EXPORTED so the renderer's tests can hold the cross-module contract: the cap is only safe because
// every renderer re-truncates by display width, and while it was package-private neither side could
// name the other's half. cmd/abctl/tui asserts the relationship against this constant.
//
// A prompt is unbounded — the longest on the measured tree ran to several KB — and a title is a
// table cell. Clipping at the source keeps the metadata file small and stops every consumer having
// to defend itself; the viewer truncates again to whatever the column allows, which is narrower
// still. Runes, not bytes, so a multi-byte prompt is not cut mid-character.
//
// NOT A DISPLAY-COLUMN BUDGET. 80 runes of CJK occupy 160 columns, so nothing may treat this as a
// width. It is safe only because every renderer re-truncates by display width — the sessions pane
// measures with lipgloss.Width.
//
// THAT IS NOT THE ONLY RULER THE CELL MEETS, and saying only "the pane measures with lipgloss.Width"
// was incomplete: bubbles v1.0.0 applies runewidth.Truncate to every cell before styling it, and
// runewidth is NOT ANSI-aware. Today that is harmless, because a title cell carries no escape bytes —
// this package guarantees it, and the pane asserts it. But the two facts are load-bearing together: if
// a title were ever styled, the escape bytes would be charged against the column budget and an
// 11-column cell would collapse to a lone ellipsis. Anyone adding colour to a title needs to know that
// before they do it, which is why it is recorded here rather than left to be rediscovered.
//
// THAT RELATIONSHIP IS GUARDED, from the renderer's side:
// TestTitleCap_IsSafeOnlyBecauseTheRendererRemeasures in cmd/abctl/tui takes a title at exactly this
// cap in the worst case for the mismatch — MaxTitleLen runes of CJK, twice that in columns — and
// requires the rendered cell to fit anyway. Removing the renderer's truncation fails it, along with
// ten other tests in that package.
//
// An earlier version of this comment said the opposite: that no single test held the relationship
// and that "nothing fails if someone removes the renderer's truncation". That was true when written
// and false once the guard landed, which is the worse direction for a comment to be wrong in — it
// invites a maintainer to delete guarded code. This package still cannot assert the width half
// itself (it has no width library, and the exported constant is what lets the other side name it),
// so the guard lives there and this comment points at it by name.
const MaxTitleLen = 80

// maxNormalizePasses bounds normalizeTitle's fixed-point loop.
//
// Each pass only ever deletes, so the string strictly shrinks and the loop converges long before
// this — two passes is the most any observed input needs. The cap exists so a future pass that
// somehow grows the string cannot spin, not because convergence is in doubt.
const maxNormalizePasses = 8

// clipTitle trims s and caps it at MaxTitleLen runes.
//
// No ellipsis: this is not the display truncation — the TITLE column applies its own, measured in
// display columns — and a marker added here would be re-truncated downstream, leaving a cell with
// two of them.
// normalizeTitle makes s plain: no markup, no characters that fail to print as themselves, one line.
//
// SEPARATED FROM THE LENGTH CAP so the cwd fallback can have the guarantee without the clipping.
// Before this it returned through a bare strings.Fields, which collapsed whitespace and nothing
// else — so a directory name carrying an ESC sequence, a bidi override or a tag reached the file
// unfiltered, while every other tier was clean. A guarantee that holds for three tiers out of four
// is not one.
func normalizeTitle(s string) string {
	// STRIP ALL MARKUP FIRST, wherever it sits. Every earlier attempt filtered markup by SHAPE —
	// anchored at the start, or line-leading, or per block — and each round of review found another
	// shape that slipped past: markup inside <command-args>, markup mid-line on a later line, a
	// block indented with a vertical tab. Enumerating shapes cannot converge, because the shapes are
	// the harness's to choose.
	//
	// So the rule here is positional-independent and applies to every tier: a title carries no tags
	// at all. Callers still unwrap envelopes and drop harness-output wrappers, because those
	// decisions are about WHICH TEXT to use; this is about what may appear in the result.
	// ORDER, AND THEN A FIXED POINT.
	//
	// stripANSI runs FIRST because an escape sequence inside a tag name hides that tag from the only
	// pass that removes a block together with its body: "<system-\x1b[0mreminder>INJECTED</...>"
	// was invisible to stripHarnessSpans, and stripTags then unwrapped it to bare "INJECTED".
	// Removing the escapes first makes the tag whole again.
	//
	// And the three are applied until nothing changes, rather than once each. Every one of them
	// REWRITES the string, so any of them can expose work for another: removing an escape can join a
	// tag name, and removing a harness span can bring two halves of prose together.
	//
	// REDUNDANT TODAY, and recorded as such rather than left to look load-bearing: with the order
	// above, one pass handles every input tried, including escapes nested inside tags inside escapes
	// — pinning the loop to a single iteration fails no test. It is kept because the argument for
	// one pass being enough is an argument about orderings, and "the orderings someone thought of"
	// is exactly what six rounds of review kept flanking. Bounded because each pass only ever
	// deletes, so the length strictly decreases until it stabilises.
	for i := 0; i < maxNormalizePasses; i++ {
		before := s
		s = stripANSI(s)
		s = stripHarnessSpans(s)
		s = stripTags(s)
		if s == before {
			break
		}
	}

	// ONE LINE, and only characters that print as themselves.
	//
	// The old version collapsed whitespace and stopped there, leaving ESC sequences, C1 controls,
	// bidi overrides and zero-width characters in the written file. That was safe only because the
	// viewer runs sanitizeLabel over every cell — so the guarantee lived in one consumer, and
	// anything else reading ~/.cortex/session-metadata.json got the raw bytes. A title is meant to
	// be plain text with nothing hidden in it, which has to be true of the FILE, not of one reader.
	//
	// An ALLOWLIST, not a denylist of known-bad ranges: unicode.IsGraphic is false for every
	// control, format, surrogate and unassigned code point, so a category nobody enumerated cannot
	// leak through. Whitespace is normalised to a single space and everything else non-graphic is
	// dropped rather than replaced, since a run of U+FFFD tells a reader nothing.
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // leading whitespace is dropped
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		case !unicode.IsGraphic(r):
			// Controls (C0/C1/DEL), format characters — bidi overrides and isolates, ZWJ, ZWSP,
			// variation selectors — surrogates and unassigned code points. None of these print as
			// themselves, and the bidi ones actively reorder what surrounds them.
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Sk, r):
			// COMBINING AND MODIFYING characters: non-spacing marks, enclosing marks, and modifier
			// symbols (skin tones). Dropped rather than kept, which is what lets the length cut
			// below be a plain rune slice.
			//
			// They only ever attach to the character before them, so a cut that lands between the
			// two leaves a dangling mark on whatever now precedes it — or a base character shorn of
			// its accent. Keeping them would mean measuring in grapheme clusters, which needs a
			// segmentation library this module does not have. Dropping them costs an accent
			// ("café" titles as "cafe") and keeps this function simple, which is the trade the
			// rest of it already makes.
		case r >= 0x1F1E6 && r <= 0x1F1FF:
			// Dropped, which can empty a title entirely — a prompt of nothing but flags. The tier
			// switch treats an empty result as "this tier has nothing", so the session falls through
			// to the next one rather than rendering blank; see the normalisation before that switch.
			// REGIONAL INDICATORS, which only carry meaning in pairs: a cut between them turns a
			// flag into a lone letter glyph. Two runes, never independently meaningful, so they go
			// together or not at all.
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

// clipTitle normalises s and caps it at MaxTitleLen runes.
//
// What every tier but the cwd fallback goes through; that one uses normalizeTitle alone, because a
// path's distinguishing end is its leaf and clipping keeps the head.
//
// A PLAIN RUNE CUT IS SAFE HERE, because normalizeTitle removed every character that binds to its
// neighbour: combining marks, modifiers, joiners and regional indicators are all gone, so one rune
// is one grapheme and the slice cannot land inside a cluster. Without that a cut could leave a
// dangling accent or half a flag.
func clipTitle(s string) string {
	s = normalizeTitle(s)
	r := []rune(s)
	if len(r) <= MaxTitleLen {
		return s
	}
	return strings.TrimSpace(string(r[:MaxTitleLen]))
}

// stripANSI removes a CSI/OSC escape sequence together with its parameters.
//
// Dropping the ESC byte alone — which the non-graphic filter below does — leaves the payload behind:
// "\x1b[31mred\x1b[0m" became "[31mred[0m", which is not dangerous but is not a title either. The
// whole sequence goes, so the result reads as what the user wrote.
//
// Handles the two forms that carry parameters: CSI (ESC [ … final byte in @-~) and OSC
// (ESC ] … terminated by BEL or ST). Any other escape is left to the non-graphic filter, which drops
// the ESC and leaves at most one stray character.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] != 0x1b {
			b.WriteRune(rs[i])
			continue
		}
		// A LONE TRAILING ESC IS DROPPED, not written through. The old code fell to the default
		// branch and copied it, contradicting this function's own doc — harmless inside
		// normalizeTitle, where the allowlist catches it, but stripANSI has direct callers in the
		// renderer's width assertions.
		if i+1 >= len(rs) {
			break
		}
		switch rs[i+1] {
		case '[': // CSI: parameters, then a final byte in @ to ~
			j := i + 2
			for j < len(rs) && (rs[j] < '@' || rs[j] > '~') {
				j++
			}
			i = j
		case ']', 'P', 'X', '^', '_':
			// STRING-ARGUMENT SEQUENCES, all terminated by BEL or ST: OSC (]), DCS (P), SOS (X),
			// PM (^) and APC (_). Only OSC was handled, so a DCS payload — "\x1bPq …\x1b\\" —
			// lost its ESC and leaked the rest as literal text, which is a garbage title rather
			// than a dangerous one. They share a terminator, so they share a branch.
			//
			// The property test cannot tell these apart from the two-byte fallback, since both leave
			// a plain title — only the CONTENT differs, and "garbage but plain" satisfies the
			// guarantee. TestStripANSI_HandlesEveryEscapeClass is what pins the payload actually
			// going, rather than the ESC alone.
			j := i + 2
			for j < len(rs) && rs[j] != 0x07 {
				if rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\' {
					j++
					break
				}
				j++
			}
			i = j
		case '(', ')', '*', '+', '-', '.', '/', '%', '#', ' ':
			// CHARSET SELECTION and other two-byte intermediates: ESC ( B, ESC # 8 and friends take
			// exactly one more byte. Skipping only the ESC left "(B" in the title.
			i += 2
		default:
			// A two-character escape (ESC 7, ESC c, ESC =). The ESC and its single final byte go.
			i++
		}
	}
	return b.String()
}

// stripHarnessSpans removes a known harness block AND ITS BODY, wherever it sits.
//
// stripTags alone deletes the angle-bracket spans and leaves what was between them, so
// "my question\n<system-reminder>LEAK</system-reminder>" became "my question LEAK" — the tags gone
// and the injected text promoted into the title. For a harness block the BODY is the payload, so the
// whole span has to go.
//
// Only the named set is removed with its body: those tags are the harness's, so their content is
// never the user's. An unknown tag keeps its body, because there the text between the brackets may
// well be the prompt — stripTags then removes the brackets alone.
//
// Position-independent, and applied before stripTags, so a block anywhere in the string is handled:
// at the start, appended on an indented line, mid-line on a later line, or nested inside a
// <command-args> value. That last one is the leak review found this round; the rest are the leaks it
// found in the rounds before.
func stripHarnessSpans(s string) string {
	for _, tag := range harnessTagNames {
		for {
			i := strings.Index(s, tag)
			if i < 0 {
				break
			}
			// The span runs to the matching close when there is one, and TO THE END OF THE STRING
			// otherwise.
			//
			// Not to the end of the opening tag, which is what it did: an unclosed block then left
			// its body behind as bare prose — "prose <system-reminder>INJECTED payload" kept
			// "INJECTED payload" — with no escape sequence needed. A harness block that is not
			// closed is still a harness block, and everything after it is its content as far as
			// anyone can tell.
			// THE MATCHING CLOSE, counting depth — not the first one. With the same tag NESTED,
			// "<system-reminder>a<system-reminder>b</system-reminder>LEAKED</system-reminder>" ended
			// its span at the INNER close, so the outer close and everything before it survived as
			// "LEAKED". The next iteration then found no opening tag, because this one had consumed
			// it, so the fixed-point loop could not recover it either.
			//
			// Different-name nesting was already handled, since each tag is scanned separately, and
			// that is why the corpus missed this: it covered the case that worked.
			end := len(s)
			name := tag[1:] // tag is "<name"
			depth := 0
			for j := i; j < len(s); {
				switch {
				case strings.HasPrefix(s[j:], tag):
					depth++
					j += len(tag)
				case strings.HasPrefix(s[j:], "</"+name):
					depth--
					if depth == 0 {
						if g := strings.IndexByte(s[j:], '>'); g >= 0 {
							end = j + g + 1
						}
						j = len(s) // done
						continue
					}
					j += len("</" + name)
				default:
					j++
				}
			}
			s = s[:i] + " " + s[end:]
		}
	}
	return s
}

// stripTags removes every <...> span from s, wherever it appears.
//
// POSITION-INDEPENDENT, which is the point: the shape-by-shape filters above it each guard one
// placement, and review found a new placement each round. This one cannot be flanked.
//
// Unbalanced or nested markup is handled by construction, since it only ever deletes from a "<" to
// the next ">" and leaves a lone "<" alone. That last part is deliberate: "is 3 < 5 in Go?" and
// "List<String>" are the shapes a real prompt carries, and the first must survive. The second is
// sacrificed — a prompt that genuinely quotes a tag loses it — which is the trade this asks for:
// titles are plain, and a plain title cannot also faithfully quote markup.
func stripTags(s string) string {
	if !strings.ContainsRune(s, '<') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			break
		}
		// A TAG-NAME-SHAPED SPAN, not just anything between angle brackets. Two defects came from
		// treating every "<" as an opener and counting depth:
		//
		//   - two balanced comparison operators cancelled out, so the depth-0 fallback never fired
		//     and everything between them was eaten: "is 3 < 5 and 6 > 2 in Go?" became
		//     "is 3 2 in Go?";
		//   - one unbalanced "<" disabled stripping for the WHOLE string, including balanced tags
		//     before it, so normalizeTitle's no-markup contract was simply false for such inputs.
		//
		// Deciding per span fixes both: a "<" that does not begin a plausible tag is ordinary text
		// and is kept, and each span is judged on its own.
		if n := tagSpanLen(s[i:]); n > 0 {
			b.WriteString(s[:i])
			s = s[i+n:]
			continue
		}
		b.WriteString(s[:i+1])
		s = s[i+1:]
	}
	return b.String()
}

// tagSpanLen returns the length of the tag starting at s[0], or 0 if s does not start with one.
//
// A tag is "<", an optional "/", a name of letters, digits, "-" or "_", then anything up to the
// first ">". That is deliberately narrow: "< 5" and "<" at end-of-string are not tags, so a
// comparison operator in a real prompt survives, while "<div>", "</system-reminder>" and
// "<a href=\"x\">" do not.
func tagSpanLen(s string) int {
	if len(s) < 2 || s[0] != '<' {
		return 0
	}
	i := 1
	if s[i] == '/' {
		i++
	}
	nameStart := i
	for i < len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			i++
			continue
		}
		break
	}
	if i == nameStart {
		return 0 // no name: "< 5", "<>", "<="
	}
	if j := strings.IndexByte(s[i:], '>'); j >= 0 {
		return i + j + 1
	}
	// An opening tag with no ">" anywhere. Not a span, so the "<" is kept as text — the alternative
	// is swallowing the rest of the string on a stray bracket.
	return 0
}

// promptFromMessage returns the text a person typed in one user turn, and whether the content was
// a plain STRING rather than an array of blocks.
//
// Two shapes, because Claude Code writes both: a bare string, or an array of blocks of which only
// `text` is human input. A tool_result array yields "" — it is tool output, and titling a session
// with a grep dump was the thing this function exists to prevent.
//
// The shape is returned because it grades the result. Every one of the 640 attributed human turns
// on the measured tree had string content and none had an array, while the arrays are where the
// harness injects — a "Base directory for this skill: …" title came from one. So a string is
// evidence about authorship, not just an encoding detail, and the caller ranks on it.
func promptFromMessage(raw json.RawMessage) (text string, wasString bool) {
	if len(raw) == 0 {
		return "", false
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, true
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type != "text" || blk.Text == "" {
			continue
		}
		// FILTERED PER BLOCK, not after joining. A harness block can arrive ALONGSIDE the user's
		// real text — "my question" and "<system-reminder>…" as two text blocks of one turn — and
		// testing only the joined string let the markup through, because the guard is anchored at
		// the start and the join begins with the prose.
		if isSyntheticPrompt(blk.Text) {
			continue
		}
		// AND WITHIN the block. isSyntheticPrompt is anchored, so a block holding prose followed by
		// markup — "my question\n<system-reminder>…" as ONE block — passed the check and reached
		// the title intact. Cut here rather than after the join, so a later block cannot be
		// mistaken for the appended markup of an earlier one.
		text := cutTrailingHarness(blk.Text)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(text)
	}
	return b.String(), false
}

// unwrapCommandEnvelope renders a slash-command turn as the command the user typed, or returns s
// unchanged when it is not one.
//
// The turn is genuinely the user's — origin.kind is "human" — but the text is an envelope:
// "<command-message>review</command-message><command-name>/review</command-name>
// <command-args>some/path.md</command-args>". Measured, 28 of 128 transcripts ended with one, so
// left alone they are the commonest title shape and every one reads as markup. Worse, 27 of those 28
// share the same command, so they would all carry an identical title.
//
// Yields "/review some/path.md": the line the person would recognise, and one that tells the 28
// apart by their arguments. <command-message> is dropped as a duplicate of the name.
//
// Hand-parsed rather than by regexp, and deliberately: the obvious pattern for a tag pair wants a
// BACKREFERENCE to match the closing tag, which RE2 does not support — `regexp.MustCompile` panics
// at init on `\1`, which compiles clean and dies on first use. Two literal scans need no such
// trick.
func unwrapCommandEnvelope(s string) string {
	if !strings.Contains(s, "<command-name>") {
		return s
	}
	name := between(s, "<command-name>", "</command-name>")
	if name == "" {
		return s
	}
	if args := between(s, "<command-args>", "</command-args>"); args != "" {
		return name + " " + args
	}
	return name
}

// between returns the text bracketed by open and closing, trimmed, or "".
//
// The second parameter is `closing`, not `close`: that would shadow the predeclared builtin, which
// go vet does not report and this module's lint step (go fmt + go vet) would therefore never catch.
func between(s, open, closing string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// contentBearingWrappers are the harness tags whose BODY is the user's own text.
//
// The distinction that matters for stripWrapperTag: a `<pasted_content>` block holds something the
// user pasted, so its body is a prompt and unwrapping it recovers a real title. A `<bash-stdout>`,
// `<system-reminder>` or `<task-notification>` block holds the harness's own output, so its body is
// not a prompt at any depth and the turn must fall through instead.
//
// An allowlist rather than a denylist because the failure directions are not symmetric: omitting a
// content-bearing tag costs one fallback to the previous prompt, while omitting a harness tag puts
// tool output in the title.
var contentBearingWrappers = []string{
	"pasted_content",
}

// isContentBearingWrapper reports whether tag — a full opening tag, "<name …>" — is one whose body
// is the user's own text.
func isContentBearingWrapper(tag string) bool {
	name := strings.TrimPrefix(strings.TrimSpace(tag), "<")
	if j := strings.IndexAny(name, " \t/>"); j >= 0 {
		name = name[:j]
	}
	name = strings.ToLower(name)
	for _, want := range contentBearingWrappers {
		if name == want {
			return true
		}
	}
	return false
}

// stripWrapperTag removes a leading CONTENT-BEARING wrapper from text the user really typed, keeping
// what is inside it.
//
// Content-bearing means the body belongs to the user — see contentBearingWrappers. A wrapper holding
// the harness's own output is left alone, so the caller's synthetic test rejects it and the turn
// falls through rather than titling a session with tool stdout.
//
// Pasted input arrives as `<pasted_content id="2e21">\n…the actual text…`, on a turn whose
// origin.kind is "human" — so it must not be filtered like injected traffic, but the wrapper is
// still markup and left alone it became the title. Unlike a slash command there is nothing to
// reconstruct: the content after the tag IS the prompt.
//
// Only a LEADING tag, and only one. A "<" further in is ordinary prose ("is 3 < 5 in Go?"), and
// stripping repeatedly would start eating text that merely looks like markup.
func stripWrapperTag(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<") {
		return s
	}
	i := strings.IndexByte(t, '>')
	if i < 0 {
		return s
	}
	// AN ALLOWLIST, not the structural test. Unwrapping anything tag-shaped promoted the body of
	// whatever the harness had wrapped: `<bash-stdout>total 40</bash-stdout>` became the title
	// "total 40", and a `<system-reminder>` body became a title — the grep-dump outcome
	// isSyntheticPrompt's own doc says it exists to prevent, and a contradiction of this PR's claim
	// that markup all the way through falls through.
	//
	// Only wrappers whose CONTENT IS THE USER'S qualify. `<pasted_content>` is the one: the user
	// pasted what is inside it, so the body is a prompt. Everything else the harness emits is its
	// own output — tool stdout, a reminder, a notification — and belongs to the next tier down, not
	// in the title.
	if !isContentBearingWrapper(t[:i+1]) {
		return s
	}
	inner := strings.TrimSpace(t[i+1:])
	if inner == "" {
		return s
	}
	// A closing tag at the end is dropped too, so "<x>body</x>" yields "body".
	//
	// `j >= 0`, not `j > 0`: an EMPTY-bodied wrapper leaves the closing tag at index 0, so the
	// stricter guard skipped the trim and this returned a bare "</pasted_content>".
	//
	// REDUNDANT for the caller as it stands — the call site re-tests the result against
	// isSyntheticPrompt, which rejects a bare closing tag either way, and reverting this to `j > 0`
	// breaks no test. Kept so the helper is right on its own terms rather than only in the company
	// of that check: a second caller would otherwise inherit the bug.
	if j := strings.LastIndex(inner, "</"); j >= 0 && strings.HasSuffix(inner, ">") {
		inner = strings.TrimSpace(inner[:j])
	}
	// RE-TESTED against the same guard, which is what closes the whole family rather than one
	// shape of it. Three ways markup survived a single leading strip:
	//
	//   - an empty body, leaving a bare closing tag;
	//   - a malformed command envelope ("<command-name></command-name> real text"), where the
	//     unwrapper bails on the empty name and control falls through to here;
	//   - a nested wrapper ("<a><b>x</b></a>"), where only the outer tag is removed.
	//
	// Any of them leaves something that still opens with a tag, so asking the guard again is both
	// the narrowest fix and the one that does not need a list of shapes. Returning s unchanged on
	// a still-markup result hands the caller a value its own synthetic filter will reject, so the
	// turn falls through to the next tier instead of titling a session with markup.
	if inner == "" || isSyntheticPrompt(inner) {
		return s
	}
	return inner
}

// promptCandidate turns one raw prompt string into a usable title, reporting whether anything
// survived.
//
// ONE implementation for both callers — the recorded last-prompt line and the attributed human turn
// — because they want exactly the same thing and used to say so in two different shapes. Verified
// behaviourally identical at the time, which is precisely the state in which a later fix lands in
// only one of them.
//
// Order is load-bearing:
//
//  1. the command envelope first, because it is a multi-tag structure whose <command-args> sit past
//     the first tag — stripping a leading wrapper beforehand threw the arguments away;
//  2. otherwise a leading wrapper, keeping what it contains;
//  3. then trailing harness markup, which neither of the above sees since both are anchored;
//  4. then the synthetic test, because every helper returns its input UNCHANGED when it cannot make
//     sense of it, and assigning that verbatim is what put raw markup in titles.
func promptCandidate(text string) (string, bool) {
	candidate := unwrapCommandEnvelope(text)
	if candidate == text {
		candidate = stripWrapperTag(text)
	}
	candidate = cutTrailingHarness(candidate)
	if isSyntheticPrompt(candidate) {
		return "", false
	}
	return candidate, true
}

// harnessTagNames are the wrapper tags observed on real transcripts, for the trailing-markup cut.
//
// A NAMED SET here, unlike isSyntheticPrompt's structural test, and deliberately: that one asks
// "does this text BEGIN as markup", where a false positive costs one fallback. This one cuts text
// off mid-prompt, so it may only fire on tags known to be the harness's. Matching any "<word>"
// would truncate a prompt that quotes HTML or generics.
var harnessTagNames = []string{
	"<system-reminder",
	"<task-notification",
	"<pasted_content",
	"<bash-stdout",
	"<bash-stderr",
	"<local-command-caveat",
	"<command-message",
	"<command-name",
	"<command-args",
	"<agent-message",
	"<user-prompt-submit-hook",
}

// cutTrailingHarness drops a harness block appended AFTER the user's prose.
//
// Measured, 7 of 130 transcripts had a string turn shaped "…real question…<system-reminder>…", which
// the anchored guard cannot see because the line starts with prose. Left alone the tag reached the
// title and was clipped mid-tag at 80 runes.
//
// Cuts at the first known tag THAT STARTS A LINE, and keeps what precedes it — the prompt is the part
// the user typed, and everything the harness appends comes after, on its own line. The positional
// constraint is what stops prose being truncated for merely mentioning a tag: the named set alone was
// not enough, and "how do I use <command-args> in a skill?" came out as "how do I use".
//
// Returns s unchanged when nothing precedes the tag, so a turn that is markup all the way through
// still falls to isSyntheticPrompt rather than becoming "".
func cutTrailingHarness(s string) string {
	cut := -1
	for _, tag := range harnessTagNames {
		for from := 0; ; {
			i := strings.Index(s[from:], tag)
			if i < 0 {
				break
			}
			i += from
			from = i + len(tag)
			// POSITIONAL. The harness appends its block on a line of its own, so a known tag only
			// counts when it STARTS A LINE. Without this, prose that merely mentions a tag was
			// truncated mid-sentence — "how do I use <command-args> in a skill?" became "how do I
			// use" — which is the very outcome the named set was chosen to avoid, and the comment
			// above claimed it did.
			//
			// LEADING WHITESPACE STILL COUNTS AS LINE-LEADING. Requiring s[i-1] to be exactly a
			// newline let an INDENTED block through: "my question\n   <system-reminder>…" kept the
			// tag and was clipped mid-tag at 80 runes, which is the failure this function's own doc
			// describes. Only whitespace is skipped, so the constraint still holds against anything
			// with real text before it on the line.
			//
			// A known tag mid-line on a LATER line — "line one\nline two <system-reminder>x" — is
			// deliberately left alone: it is genuinely ambiguous between prose and appended markup,
			// and cutting it would risk the mid-sentence truncation the positional rule exists to
			// prevent.
			//
			// Every occurrence is examined, not just the first: a prompt may mention a tag inline
			// and still have a real appended block after it.
			if i > 0 {
				// ANY whitespace counts as indentation, not just space and tab — and it is decoded as
				// a RUNE, which is the part the previous version got wrong. `unicode.IsSpace(rune(s[j]))`
				// converts one BYTE, so a multi-byte space (NBSP, U+3000, U+2028, ogham) never matched:
				// its continuation bytes are not IsSpace, so the scan stopped on them and the block was
				// treated as mid-line. The comment claimed those very shapes were covered.
				//
				// Not a leak end to end — normalizeTitle removes the tag either way — but a defence that
				// only appears to cover a case is worse than one that admits it does not.
				j := i
				for j > 0 {
					r, size := utf8.DecodeLastRuneInString(s[:j])
					if r == '\n' || r == '\r' || !unicode.IsSpace(r) {
						break
					}
					j -= size
				}
				if j > 0 {
					if r, _ := utf8.DecodeLastRuneInString(s[:j]); r != '\n' && r != '\r' {
						continue
					}
				}
			}
			if cut < 0 || i < cut {
				cut = i
			}
			break
		}
	}
	if cut <= 0 {
		return s
	}
	if head := strings.TrimSpace(s[:cut]); head != "" {
		return head
	}
	return s
}

// isSyntheticPrompt reports whether s READS AS markup Claude Code inserted rather than as something
// the user typed.
//
// A TEST, not a policy. What callers do with a positive answer differs, and the distinction matters
// because an earlier version of this comment claimed harness blocks are "dropped": some are, but a
// wrapper around text the user really pasted has its body kept (stripWrapperTag), and a slash command
// is re-rendered rather than discarded (unwrapCommandEnvelope). This function only answers the
// question; promptCandidate decides.
//
// Both observed families open with a bracket and are machine-written: "[Request interrupted...]"
// (46 occurrences on the measured tree) and "[Image: original 2100x200, displayed at...]" (11).
// Neither describes what a session is about, and the interrupt one is especially misleading as a
// LAST prompt, since it is exactly what a transcript ends with when someone stopped a tool call.
//
// Matched by prefix rather than by an exact set: these strings are Claude Code's, not ours, and a
// reworded variant should keep being skipped. The cost of the loose rule is that a genuine prompt
// beginning "[" is skipped too, which is rare and costs a fallback to the previous prompt.
func isSyntheticPrompt(s string) bool {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "[Request interrupted") || strings.HasPrefix(t, "[Image:") {
		return true
	}
	// A HARNESS BLOCK, which arrives on the user turn because that is the channel the tool
	// harness speaks on, but which nobody typed: <task-notification>, <bash-stdout>,
	// <system-reminder>, <command-name> and friends. Measured, 27 of 128 transcripts ended with
	// one, so without this the commonest title on a real tree is a notification envelope.
	//
	// Detected structurally — opens with an XML-ish tag — rather than by listing the tags, which
	// are the harness's vocabulary and grow. A prompt that genuinely opens with "<" is skipped
	// too; that costs a fallback to the previous prompt, which is the safe direction.
	if strings.HasPrefix(t, "<") {
		// A CLOSING tag counts too. A malformed command envelope —
		// "<command-name></command-name> real text" — bails out of the unwrapper on the empty
		// name, falls through to the wrapper strip, and leaves "</command-name> real text": a
		// dangling close that is still markup and still not a title. Skipping the slash here means
		// one guard recognises both halves of a tag pair.
		if strings.HasPrefix(t, "</") {
			t = "<" + t[2:]
		}
		if i := strings.IndexByte(t, '>'); i > 1 {
			// THE TAG NAME ONLY, cut at the first space or slash. An opening tag may carry
			// attributes — a real transcript produced a title of raw
			// `<pasted_content id="...">` markup — and the space, `=` and `"` all failed the
			// character check below, so every attributed tag went undetected. Self-closing
			// tags are cut the same way.
			tag := t[1:i]
			if j := strings.IndexAny(tag, " \t/"); j >= 0 {
				tag = tag[:j]
			}
			// Lowercased before checking: every tag observed on a real tree is lowercase, so
			// this is latent rather than a live bug, but it costs one call and the alternative
			// is a filter that a capitalised variant walks straight through.
			tag = strings.ToLower(tag)
			if tag != "" && strings.IndexFunc(tag, func(r rune) bool {
				return !(r >= 'a' && r <= 'z') && r != '-' && r != '_'
			}) == -1 {
				return true
			}
		}
	}
	return false
}

// transcriptMeta is the minimum shape needed from a transcript line. Decoding only
// these fields keeps the parse cheap on multi-megabyte lines.
type transcriptMeta struct {
	Type    string `json:"type"`
	AiTitle string `json:"aiTitle"`
	// AgentName is the title Claude Code records as {"type":"agent-name","agentName":…}.
	//
	// A SECOND SPELLING of the same idea, not a different field: some installs write ai-title
	// lines, some write agent-name, and some write both. Measured locally: of 128 transcripts
	// under one config dir, 16 carried agent-name and 2 carried ai-title with no overlap, while
	// another dir had 26 ai-title and 16 agent-name — and in all 16 of those the two values were
	// IDENTICAL. So this is a naming variant to accept, not a competing claim to arbitrate.
	AgentName string `json:"agentName"`
	// LastPrompt is Claude Code's own record of the session's last prompt, written as
	// {"type":"last-prompt","lastPrompt":…}.
	//
	// The most direct answer available: it is the agent stating what the prompt WAS, rather than
	// this package reconstructing it from user turns and then filtering harness traffic back out.
	// Measured, 129 of 130 transcripts carry one, so it is the usual case rather than a bonus.
	LastPrompt string `json:"lastPrompt"`
	Cwd        string `json:"cwd"`
	// Message is the turn body, decoded only far enough to recover a typed prompt.
	//
	// Content is json.RawMessage because Claude Code writes it two ways: a plain string for a
	// simple prompt, and an array of blocks otherwise. Decoding it as either concrete type would
	// silently drop the other — measured on one config dir, 1026 strings against 9718 arrays.
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// Origin says who produced the turn, and it is the only authoritative answer available.
	//
	// Measured across 128 transcripts: kind is "human" on 640 turns, absent on 9940, and
	// "task-notification" (151) or "peer" (33) on the rest. Those last two are precisely the
	// harness-injected traffic that heuristics here used to guess at from the text — so where the
	// field is present it replaces the guessing rather than supplementing it.
	Origin *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
}

// contentBlock is one element of a message's content array.
//
// Only text blocks carry anything a person typed. The overwhelming majority are tool_result —
// 9576 of 9718 arrays on the measured tree — which is tool OUTPUT: grep hits, build logs, a
// CSpell summary. Titling a session with those was the failure mode this shape exists to avoid.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
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
