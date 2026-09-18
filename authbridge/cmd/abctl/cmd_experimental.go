package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// claudeConfigDirEnv is the variable Claude Code itself honours for relocating its
// config tree. Read here so a user who has moved it is not told there are no
// sessions; nothing else in abctl consults it today, which is why the two commands
// that read ~/.claude resolve it differently — see the follow-up in the PR.
const claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"

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
~/.cortex/session-metadata.json, keyed by the same UUID Cortex uses — so a later
reader can put a name next to a row.

Each entry carries a title, the agent type ("Claude Code"), the config directory it
came from, and the transcript it was read from. The title is the transcript's
model-generated one when it has one, and otherwise the working directory: most
sessions never get a title, so most entries name a directory.

The config directory is ` + claudeConfigDirEnv + ` when set, and ~/.claude otherwise.
Only transcripts one level down (projects/<project>/<id>.jsonl) are read: deeper
files are subagent transcripts, whose names are not session ids.

By default a run UPSERTS: entries for the sessions it finds are added or updated, and
entries already in the file are left alone. That matters because a harvest only sees
the sessions one config directory holds, so replacing the file would silently drop
metadata for anything else it had — another config directory, or a session whose
transcript Claude Code has since pruned. --merge=false rebuilds the file from this
harvest alone, which is the way to drop stale entries on purpose.

Nothing in Cortex reads the file yet.

Flags:
  --dir PATH     config directory to read instead of ` + claudeConfigDirEnv + ` / ~/.claude
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

	configDir := *dir
	if configDir == "" {
		var err error
		if configDir, err = defaultClaudeConfigDir(); err != nil {
			fmt.Fprintf(stderr, "abctl: %v\n", err)
			return 1
		}
	}

	meta, partial, err := readClaudeSessions(configDir)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: reading %s: %v\n", configDir, err)
		return 1
	}
	// Warned about, and not fatal: these sessions have a title, it just may be an older one
	// than the transcript's last claim. Capped at three names plus a count, so a systemic
	// problem is as visible as a single bad file without burying the result.
	if len(partial) > 0 {
		fmt.Fprintf(stderr, "abctl: %d transcript(s) read incompletely; their titles may be stale:\n", len(partial))
		for i, msg := range partial {
			if i == 3 {
				fmt.Fprintf(stderr, "  ... and %d more\n", len(partial)-3)
				break
			}
			fmt.Fprintf(stderr, "  %s\n", msg)
		}
	}

	path, err := tui.SessionMetadataPath()
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}

	harvested := len(meta)
	if *merge {
		existing, err := readSessionMetadata(path)
		if err != nil {
			// Refused rather than treated as empty. A corrupt file read as absent would
			// silently rebuild from scratch under the flag whose whole purpose is not
			// losing entries — the same trap readState exists to close for
			// claude-code-state.json. Name the repair, and name the way past it.
			fmt.Fprintf(stderr, "abctl: %v\n"+
				"  Fix or move the file, or re-run with --merge=false to rebuild it.\n", err)
			return 1
		}
		// This harvest wins per key: it just read the transcripts, so where both have a
		// session the fresher title is here. Keys only the file has are kept — that is
		// what merging is for.
		for id, m := range meta {
			existing[id] = m
		}
		meta = existing
	}

	if err := saveSessionMetadata(path, meta); err != nil {
		fmt.Fprintf(stderr, "abctl: writing %s: %v\n", path, err)
		return 1
	}

	// A concurrent --merge run could have written between our read and our rename, and
	// os.Rename would have replaced its entries with a map that never contained them —
	// entries that are unrecoverable once Claude Code prunes the transcript they came from.
	//
	// Closed by re-reading and re-merging rather than by an interprocess lock. A lock is the
	// textbook answer and is what the review asked for, but there is no file locking anywhere
	// in this repo and golang.org/x/sys is only an indirect dependency, so it would add a
	// primitive and promote a dependency for a race that needs two harvests running at once.
	// This costs one extra read of a small file on every run and needs neither.
	if *merge {
		recovered, err := recoverConcurrentEntries(path, meta)
		if err != nil {
			fmt.Fprintf(stderr, "abctl: writing %s: %v\n", path, err)
			return 1
		}
		if recovered > 0 {
			fmt.Fprintf(stderr, "abctl: merged %d entry(s) written concurrently by another run\n", recovered)
		}
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
			len(meta), path, harvested, configDir, len(meta)-harvested)
	} else {
		fmt.Fprintf(stdout, "Wrote %d session(s) to %s\n", len(meta), path)
	}
	if harvested == 0 {
		fmt.Fprintf(stdout, "No transcripts under %s/projects — is that the right config directory?\n", configDir)
	}
	return 0
}

// defaultClaudeConfigDir resolves Claude Code's config directory: CLAUDE_CONFIG_DIR
// when set, else ~/.claude.
func defaultClaudeConfigDir() (string, error) {
	if d := os.Getenv(claudeConfigDirEnv); d != "" {
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

// readClaudeSessions harvests every session transcript under configDir/projects.
//
// One level down only — projects/<project>/<id>.jsonl. Deliberately not
// filepath.WalkDir, which the rest of this module uses: deeper files are subagent
// transcripts (<id>/subagents/agent-*.jsonl) whose basenames are agent ids, not
// session ids, so walking would key 92 non-sessions into the map on the machine this
// was written against.
//
// A missing projects directory is not an error: a machine that has never run Claude
// Code has none, and an empty result already says so.
// The second return names transcripts whose title may be incomplete — a read that stopped
// early. Not an error: those sessions are still in the map with the best title found. The
// caller reports them, because a truncated read is otherwise indistinguishable from a
// session that has no title.
func readClaudeSessions(configDir string) (map[string]tui.SessionMetadata, []string, error) {
	projects := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]tui.SessionMetadata{}, nil, nil
		}
		return nil, nil, err
	}
	var partial []string

	out := make(map[string]tui.SessionMetadata)
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
			won[id] = st.ModTime()
			title, terr := titleFromTranscript(path)
			if terr != nil {
				// Collected rather than returned: one bad transcript must not cost the
				// other hundred names, which is the whole reason this is best-effort. The
				// caller prints a bounded summary so it is visible without turning a
				// 109-session harvest into 109 lines of stderr.
				partial = append(partial, fmt.Sprintf("%s: %v", path, terr))
			}
			out[id] = tui.SessionMetadata{
				Title:          title,
				AgentType:      "Claude Code",
				AgentConfigDir: configDir,
				LogFile:        path,
			}
		}
	}
	return out, partial, nil
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
	defer f.Close()

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

// readSessionMetadata reads the existing file, distinguishing absent from unreadable.
//
// An empty map with a nil error means genuinely no file yet — the first run, which is
// not a problem. A non-nil error means a file was there and could not be trusted, and
// the caller must say so out loud rather than proceeding: under --merge, treating a
// corrupt file as empty would discard exactly the entries the flag exists to keep.
// Same distinction, for the same reason, as readState in cmd_claudecode.go.
//
// A file holding JSON `null` decodes to a nil map, which is indistinguishable from an
// empty object for merging purposes, so it is normalised rather than refused.
func readSessionMetadata(path string) (map[string]tui.SessionMetadata, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]tui.SessionMetadata{}, nil
		}
		return nil, err
	}
	var m map[string]tui.SessionMetadata
	if uerr := json.Unmarshal(b, &m); uerr != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, uerr)
	}
	if m == nil {
		return map[string]tui.SessionMetadata{}, nil
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
func recoverConcurrentEntries(path string, meta map[string]tui.SessionMetadata) (int, error) {
	after, err := readSessionMetadata(path)
	if err != nil {
		return 0, nil
	}
	recovered := 0
	for id, m := range after {
		if _, ours := meta[id]; !ours {
			meta[id] = m
			recovered++
		}
	}
	if recovered == 0 {
		return 0, nil
	}
	if err := saveSessionMetadata(path, meta); err != nil {
		return 0, err
	}
	return recovered, nil
}

// saveSessionMetadata writes the map atomically, creating ~/.cortex if needed.
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
// is the follow-up noted in the PR rather than a silent omission.
//
// What would change this calculus outright: harvesting an agent whose config lives somewhere
// the user cannot already read, or adding any field carrying transcript CONTENT rather than a
// path. Either makes this file carry something its reader could not otherwise obtain.
func saveSessionMetadata(path string, meta map[string]tui.SessionMetadata) error {
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
	_, err = writeAll(f, body)
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
