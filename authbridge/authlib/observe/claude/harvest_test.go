package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// writeSessionTranscript writes one transcript with caller-supplied lines.
//
// A local copy rather than a shared helper: the equivalent in cmd/abctl's tests serves
// the command-layer tests that stayed there, and the two packages cannot share test code.
func writeSessionTranscript(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readMetadataFile decodes what a harvest wrote.
func readMetadataFile(t *testing.T, path string) map[string]SessionMetadata {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var got map[string]SessionMetadata
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return got
}

// metadataHome points $HOME at a temp dir so SessionMetadataPath resolves into it.
// Same shape as cmd/abctl's prefsHome, which is in package main and out of reach here.
func metadataHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// touch sets a file's mtime, so a test can say "this transcript is older/newer than its
// entry" without sleeping.
func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// Harvest reports what it read, kept and wrote — the numbers the command layer prints.
func TestHarvest_ReportsCounts(t *testing.T) {
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"first"}`)
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s2.jsonl",
		`{"type":"user","cwd":"/w"}`)

	res, err := Harvest(Options{ConfigDir: cfg, Merge: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Harvested != 2 || res.Total != 2 || res.Kept != 0 {
		t.Errorf("harvested/total/kept = %d/%d/%d, want 2/2/0", res.Harvested, res.Total, res.Kept)
	}
	if res.ConfigDir != cfg {
		t.Errorf("ConfigDir = %q, want %q", res.ConfigDir, cfg)
	}
	if res.Skipped != 0 {
		t.Errorf("Skipped = %d on a non-incremental harvest, want 0", res.Skipped)
	}
	got := readMetadataFile(t, res.Path)
	if got["s1"].Title != "first" {
		t.Errorf("s1 title = %q, want %q", got["s1"].Title, "first")
	}
	if got["s2"].Title != "/w" {
		t.Errorf("s2 title = %q, want the cwd fallback %q", got["s2"].Title, "/w")
	}
}

// An unchanged transcript is skipped, and the title it already had survives.
//
// The whole point of the incremental mode: `abctl observe` runs on every launch, and a
// full re-parse measured 0.73-1.18s over 207MB. Skipping must not cost the entry.
func TestHarvest_IncrementalSkipsUnchangedAndKeepsTheTitle(t *testing.T) {
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")
	writeSessionTranscript(t, proj, "s1.jsonl", `{"type":"ai-title","aiTitle":"harvested"}`)

	first, err := Harvest(Options{ConfigDir: cfg, Merge: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Harvested != 1 {
		t.Fatalf("first harvest read %d, want 1", first.Harvested)
	}

	// Backdate the transcript so it is older than the entry that now names it.
	touch(t, filepath.Join(proj, "s1.jsonl"), time.Now().Add(-time.Hour))

	second, err := Harvest(Options{ConfigDir: cfg, Merge: true, Incremental: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Skipped != 1 || second.Harvested != 0 {
		t.Errorf("skipped/harvested = %d/%d, want 1/0", second.Skipped, second.Harvested)
	}
	if second.Total != 1 {
		t.Errorf("Total = %d, want 1 — a skipped session must be kept, not dropped", second.Total)
	}
	if got := readMetadataFile(t, second.Path)["s1"].Title; got != "harvested" {
		t.Errorf("title after an incremental skip = %q, want %q", got, "harvested")
	}
}

// A transcript touched since its entry was written IS re-parsed, and the new title wins.
// The skip must not be sticky, or a renamed session keeps its old name forever.
func TestHarvest_IncrementalReparsesATouchedTranscript(t *testing.T) {
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")
	writeSessionTranscript(t, proj, "s1.jsonl", `{"type":"ai-title","aiTitle":"old name"}`)
	if _, err := Harvest(Options{ConfigDir: cfg, Merge: true}); err != nil {
		t.Fatal(err)
	}

	// Rewrite with a later title and a future mtime, so it is unambiguously newer.
	writeSessionTranscript(t, proj, "s1.jsonl",
		`{"type":"ai-title","aiTitle":"old name"}`,
		`{"type":"ai-title","aiTitle":"new name"}`)
	touch(t, filepath.Join(proj, "s1.jsonl"), time.Now().Add(time.Hour))

	res, err := Harvest(Options{ConfigDir: cfg, Merge: true, Incremental: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Harvested != 1 || res.Skipped != 0 {
		t.Errorf("harvested/skipped = %d/%d, want 1/0", res.Harvested, res.Skipped)
	}
	if got := readMetadataFile(t, res.Path)["s1"].Title; got != "new name" {
		t.Errorf("title = %q, want the fresher %q", got, "new name")
	}
}

// An entry that cannot vouch for the transcript is parsed, not skipped.
//
// Measured on a real file: 27 of 192 entries carried no LogFile at all. Skipping on a
// missing or mismatched LogFile would leave those sessions permanently unnamed.
func TestHarvest_IncrementalParsesWhatItCannotValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry SessionMetadata
	}{
		{"no entry at all", SessionMetadata{}},
		{"entry without a LogFile", SessionMetadata{Title: "stale"}},
		{"LogFile naming another file", SessionMetadata{Title: "stale", LogFile: "/nonexistent/elsewhere.jsonl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadataHome(t)
			cfg := filepath.Join(t.TempDir(), "claude")
			proj := filepath.Join(cfg, "projects", "-p")
			writeSessionTranscript(t, proj, "s1.jsonl", `{"type":"ai-title","aiTitle":"real"}`)
			// Backdate, so an over-eager skip would trigger on mtime alone.
			touch(t, filepath.Join(proj, "s1.jsonl"), time.Now().Add(-time.Hour))

			path, err := SessionMetadataPath()
			if err != nil {
				t.Fatal(err)
			}
			seed := map[string]SessionMetadata{}
			if tc.entry != (SessionMetadata{}) {
				seed["s1"] = tc.entry
			}
			if err := SaveMetadata(path, seed); err != nil {
				t.Fatal(err)
			}

			res, err := Harvest(Options{ConfigDir: cfg, Merge: true, Incremental: true})
			if err != nil {
				t.Fatal(err)
			}
			if res.Harvested != 1 {
				t.Errorf("harvested = %d, want 1: an unvalidatable entry must be re-parsed", res.Harvested)
			}
			if got := readMetadataFile(t, res.Path)["s1"].Title; got != "real" {
				t.Errorf("title = %q, want %q", got, "real")
			}
		})
	}
}

// Incremental without Merge is refused rather than honoured.
//
// An incremental harvest's map holds only what it parsed, so writing it without merging
// would drop every entry it skipped — which is most of them, that being the point.
func TestHarvest_IncrementalRequiresMerge(t *testing.T) {
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"t"}`)

	if _, err := Harvest(Options{ConfigDir: cfg, Incremental: true}); err == nil {
		t.Fatal("an incremental harvest with Merge=false was accepted; it would drop every skipped entry")
	}
}

// A corrupt existing file is refused under Merge, distinguishably, so the command layer
// can name --merge=false as the way past.
func TestHarvest_CorruptMetadataIsDistinguishable(t *testing.T) {
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"t"}`)
	path, err := SessionMetadataPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Harvest(Options{ConfigDir: cfg, Merge: true})
	if !errors.Is(err, ErrCorruptMetadata) {
		t.Fatalf("err = %v, want it to wrap ErrCorruptMetadata", err)
	}
}

// An entry written concurrently by another run survives this run's save.
//
// The lost-update the review named: two --merge runs both read the file, both merge their own
// harvest, and the second rename replaces the first's entries with a map that never contained
// them. Unrecoverable once Claude Code prunes the transcript they came from, which is why it
// is worth closing rather than documenting.
//
// Driven through recoverConcurrentEntries rather than through the command, because the window
// being tested is INSIDE one run — between its read and its rename. Planting the competing
// entry before invoking the command tests nothing: the ordinary merge picks it up, and the
// test passes with the recovery deleted (confirmed by mutation). Here the map argument stands
// for "what this run decided to write", the file stands for "what the other run left behind",
// and the two deliberately disagree.
func TestRecoverConcurrentEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")
	const theirs = "99999999-0000-0000-0000-000000000009"
	const ours = "11111111-0000-0000-0000-000000000001"

	// On disk: the other run's write, which landed after this run had already read.
	onDisk, err := json.Marshal(map[string]SessionMetadata{
		theirs: {Title: "From another run", AgentType: "Claude Code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, onDisk, 0o600); err != nil {
		t.Fatal(err)
	}

	// In hand: what this run was about to save, which knows nothing of theirs.
	meta := map[string]SessionMetadata{ours: {Title: "Ours"}}

	recovered, replaced, err := recoverConcurrentEntries(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Errorf("recovered = %d, want 1", recovered)
	}
	if replaced != 0 {
		t.Errorf("replaced = %d, want 0 — this shape ADDS an id, it does not overwrite one", replaced)
	}
	got := readMetadataFile(t, path)
	if w := got[theirs].Title; w != "From another run" {
		t.Errorf("the concurrent entry was lost: %q", w)
	}
	if w := got[ours].Title; w != "Ours" {
		t.Errorf("this run's own entry is missing: %q", w)
	}

	// Idempotent: a second pass finds nothing to do and must not rewrite the file.
	again, againReplaced, err := recoverConcurrentEntries(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 || againReplaced != 0 {
		t.Errorf("second pass recovered %d/replaced %d, want 0/0 — the merge is not idempotent",
			again, againReplaced)
	}
}

// A corrupt file at recovery time is not fatal: our own save already succeeded.
//
// The asymmetry is deliberate. Before the save, a corrupt file is refused, because merging
// into it would discard entries. Here the valid file is already on disk and this is a
// best-effort attempt to also rescue someone else's entries — failing the command now would
// report an error for a harvest that in fact succeeded.
func TestRecoverConcurrentEntries_CorruptFileIsNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, replaced, err := recoverConcurrentEntries(path, map[string]SessionMetadata{"a": {}})
	if err != nil {
		t.Errorf("err = %v, want nil — the harvest already succeeded", err)
	}
	if recovered != 0 || replaced != 0 {
		t.Errorf("recovered = %d, replaced = %d, want 0/0", recovered, replaced)
	}
}

// A duplicated session id keeps the NEWEST transcript's title even when the newer one is
// skipped as unchanged.
//
// The skip used to `continue` before recording the id's claim, so a skipped transcript left
// `won` empty for that id and the next directory's OLDER transcript looked like a first
// sighting and overwrote it. Two project directories holding one id is rare (0 duplicates in
// 109 local sessions) but the failure is worse than rare: the title alternates between the
// two names on consecutive launches instead of converging, which is exactly the "stale title
// that looks authoritative" the newest-wins rule exists to prevent.
//
// "-a" sorts before "-b", so ReadDir hands over the directories in that order and the fix
// has to hold the claim across the skip rather than rely on iteration order.
func TestReadSessions_IncrementalSkipStillHonoursNewestWins(t *testing.T) {
	cfg := t.TempDir()
	const id = "11111111-0000-0000-0000-000000000001"
	older := filepath.Join(cfg, "projects", "-a")
	newer := filepath.Join(cfg, "projects", "-b")
	writeSessionTranscript(t, older, id+".jsonl", `{"type":"ai-title","aiTitle":"older"}`)
	writeSessionTranscript(t, newer, id+".jsonl", `{"type":"ai-title","aiTitle":"newer"}`)

	base := time.Now().Add(-24 * time.Hour)
	touch(t, filepath.Join(older, id+".jsonl"), base)
	touch(t, filepath.Join(newer, id+".jsonl"), base.Add(time.Hour))

	// since holds the NEWER transcript at its current mtime, so that one is skipped as
	// unchanged while the older one, which since cannot vouch for, is parsed.
	since := map[string]SessionMetadata{id: {
		Title:      "newer",
		LogFile:    filepath.Join(newer, id+".jsonl"),
		LogModTime: base.Add(time.Hour),
	}}

	out, _, skipped, err := ReadSessions(cfg, since)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the newer transcript)", skipped)
	}
	// The older transcript must NOT claim the id: the newer one already holds it, and
	// leaving it out is what lets Harvest's merge keep the newer title from `since`.
	if got, ok := out[id]; ok && got.Title == "older" {
		t.Errorf("the older transcript overwrote the newer title: got %q", got.Title)
	}
}

// The same guarantee in the other direction: the SKIPPED transcript is visited first, so
// the older duplicate arrives afterwards and must not displace it.
//
// Both orders are tested because ReadDir is alphabetical, not chronological — which of the
// two a real tree hands over first is an accident of directory naming, and the rule has to
// hold either way. "-a" holds the newer file here, the reverse of the test above.
func TestReadSessions_IncrementalSkipWinsWhenSeenFirst(t *testing.T) {
	cfg := t.TempDir()
	const id = "22222222-0000-0000-0000-000000000002"
	newer := filepath.Join(cfg, "projects", "-a")
	older := filepath.Join(cfg, "projects", "-b")
	writeSessionTranscript(t, newer, id+".jsonl", `{"type":"ai-title","aiTitle":"newer"}`)
	writeSessionTranscript(t, older, id+".jsonl", `{"type":"ai-title","aiTitle":"older"}`)

	base := time.Now().Add(-24 * time.Hour)
	touch(t, filepath.Join(older, id+".jsonl"), base)
	touch(t, filepath.Join(newer, id+".jsonl"), base.Add(time.Hour))

	since := map[string]SessionMetadata{id: {
		Title:      "newer",
		LogFile:    filepath.Join(newer, id+".jsonl"),
		LogModTime: base.Add(time.Hour),
	}}

	out, _, skipped, err := ReadSessions(cfg, since)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the newer transcript)", skipped)
	}
	if got, ok := out[id]; ok && got.Title == "older" {
		t.Errorf("the older transcript overwrote the newer title: got %q", got.Title)
	}
}

// The reported numbers balance: total == harvested + kept, even on a run that recovers
// entries another process wrote concurrently.
//
// Kept used to be computed before the recovery step while Total was refreshed after it, so any
// run that recovered anything reported a breakdown that did not add up — and the command
// prints all three on one line ("Wrote N ... (H from DIR, K kept ...)"), so the arithmetic is
// visible to whoever reads it.
//
// Driven through recoverConcurrentEntries rather than through Harvest, for the reason
// TestRecoverConcurrentEntries gives: the window is INSIDE one run, between its read and its
// rename, and planting the entry beforehand tests nothing — the ordinary merge picks it up and
// Recovered stays 0 (confirmed: a fixture that seeds the file first reports recovered=0). Here
// the map stands for "what this run decided to write" and the file for "what the other run
// left behind".
//
// HONEST LIMIT: with no seam inside Harvest there is no way to open that window from a test,
// so the last two assertions recompute the arithmetic rather than reading it off a Result.
// This pins that recovery grows the map — which is what made Kept stale — and documents the
// invariant Harvest must preserve; it does not by itself fail if someone moves Kept back
// above the recovery step. Reordering that line is guarded by review and by the comment on
// it, not by this test. An injectable post-save hook would close the gap and is not worth a
// production seam for one arithmetic line.
func TestHarvest_CountsBalanceAfterRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")

	// What this run harvested (1) plus what it kept from the file (1).
	harvested := 1
	meta := map[string]SessionMetadata{
		"ours":           {Title: "ours"},
		"theirs-earlier": {Title: "from an earlier run"},
	}
	if err := SaveMetadata(path, meta); err != nil {
		t.Fatal(err)
	}

	// Another run lands an entry this one never saw.
	onDisk := map[string]SessionMetadata{
		"ours":              {Title: "ours"},
		"theirs-earlier":    {Title: "from an earlier run"},
		"theirs-concurrent": {Title: "written by another run"},
	}
	if err := SaveMetadata(path, onDisk); err != nil {
		t.Fatal(err)
	}

	recovered, replaced, err := recoverConcurrentEntries(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1 — the fixture is not exercising recovery", recovered)
	}
	if replaced != 0 {
		t.Errorf("replaced = %d, want 0 — this fixture only ADDS an id", replaced)
	}

	// Harvest derives these from the final map, in this order.
	total := len(meta)
	kept := total - harvested - recovered
	// THREE terms, not two. Kept excludes Recovered so that it means what the caller prints it
	// as — "kept from the existing file" — since an entry another run wrote while this one worked
	// was never in the file this run read. Folding it in balanced the arithmetic while
	// attributing the entry to the wrong source, which is why the drift went unnoticed.
	if total != harvested+kept+recovered {
		t.Errorf("total %d != harvested %d + kept %d + recovered %d", total, harvested, kept, recovered)
	}
	if kept != 1 {
		t.Errorf("kept = %d, want 1 (the earlier-run entry only, not the concurrent one)", kept)
	}
	if total != len(readMetadataFile(t, path)) {
		t.Errorf("total = %d but the file holds %d entries", total, len(readMetadataFile(t, path)))
	}
}

// An entry from the previous on-disk format — no recorded mtime — is re-parsed rather than
// trusted, so the upgrade path cannot pin a stale title.
//
// The zero time is what every such entry decodes to, and harvestedAt reads it as "cannot
// tell". That is the whole upgrade story, and it had no test: a regression here would be
// invisible until someone's titles silently stopped updating after an abctl upgrade.
func TestHarvest_IncrementalReparsesPreUpgradeEntries(t *testing.T) {
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")
	writeSessionTranscript(t, proj, "s1.jsonl", `{"type":"ai-title","aiTitle":"current"}`)
	// Backdated, so a skip keyed on mtime alone would fire.
	touch(t, filepath.Join(proj, "s1.jsonl"), time.Now().Add(-time.Hour))

	path, err := SessionMetadataPath()
	if err != nil {
		t.Fatal(err)
	}
	// Written as the OLD format did: the three original fields, no logModTime at all.
	old := `{"s1":{"title":"stale","agentType":"Claude Code","logFile":"` +
		filepath.Join(proj, "s1.jsonl") + `"}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Harvest(Options{ConfigDir: cfg, Merge: true, Incremental: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Harvested != 1 || res.Skipped != 0 {
		t.Errorf("harvested/skipped = %d/%d, want 1/0: an entry with no recorded time must be re-parsed",
			res.Harvested, res.Skipped)
	}
	if got := readMetadataFile(t, res.Path)["s1"].Title; got != "current" {
		t.Errorf("title = %q, want the freshly-parsed %q", got, "current")
	}
}

// ReadMetadata is bounded, like the viewer's own reader of the same file.
//
// `abctl observe` calls this synchronously before the TUI starts, to check the file is readable,
// so an unbounded read would stall startup on a stray large file with nothing on screen to say
// why. A truncated read surfaces as a JSON error, which is the right outcome: the caller refuses
// to merge over a file it cannot parse, so "too large" behaves like any other unreadable file
// rather than silently becoming a smaller map.
func TestReadMetadata_IsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")
	// One entry, then padding past the 16 MiB cap inside a string value so the file stays valid
	// JSON right to its end — a file that is only malformed would prove nothing about the bound.
	f, err := os.Create(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"s1":{"title":"real","agentType":"pad-`); err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 1<<20)
	for i := 0; i < 17; i++ { // 17 MiB of padding, past the 16 MiB cap
		if _, err := f.WriteString(pad); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString(`"}}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadMetadata(path); err == nil {
		t.Error("a file past the cap was accepted; the read is unbounded")
	}
}

// childTimeout bounds a harvester subprocess.
//
// Its own constant rather than a shared one, since cmd_observe_test.go's namesake is in a different
// module. Generous enough that a loaded CI machine running six children against one lock does not
// flake, short enough that a wedge fails the test instead of the package.
const childTimeout = 60 * time.Second

// A concurrent run's FRESHER title for a shared id survives this run's recovery pass.
//
// recoverConcurrentEntries only pulled in ids this run did not have (`if _, ours := meta[id];
// !ours`), so a shared id kept OUR value and the save wrote it back over the other run's. Under
// Incremental that is the common shape rather than an exotic one: the run that skipped a
// transcript holds the OLD title for it, the run that re-read it holds the NEW one, and whichever
// finishes second used to win regardless of which was fresher — so a renamed session's title
// silently reverted and stayed reverted until the transcript changed again.
//
// Resolved by LogModTime, the same field the incremental skip uses, so "fresher" means the same
// thing in both places. An entry that cannot be compared (no recorded time on either side) keeps
// ours, since this run at least knows its own provenance.
func TestRecoverConcurrentEntries_KeepsTheFresherTitleForASharedID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")
	base := time.Now().Add(-time.Hour)

	// What this run decided to write: a stale title for s1 (it skipped that transcript), plus one
	// entry only it has.
	ours := map[string]SessionMetadata{
		"s1": {Title: "old name", LogFile: "/t/s1.jsonl", LogModTime: base},
		"s2": {Title: "ours only", LogFile: "/t/s2.jsonl", LogModTime: base},
	}
	// What another run left on disk: a fresher title for s1, plus one entry only it has. The
	// second entry matters — without an id to recover, the pass never saves and the revert cannot
	// be observed, which is how this hid.
	onDisk := map[string]SessionMetadata{
		"s1": {Title: "NEW name", LogFile: "/t/s1.jsonl", LogModTime: base.Add(30 * time.Minute)},
		"s3": {Title: "theirs only", LogFile: "/t/s3.jsonl", LogModTime: base},
	}
	if err := SaveMetadata(path, onDisk); err != nil {
		t.Fatal(err)
	}

	added, replaced, err := recoverConcurrentEntries(path, ours)
	if err != nil {
		t.Fatal(err)
	}
	// THE COUNTS, not just the file. This test discarded them (`if _, err := ...`), which is
	// exactly how a shared-id adoption came to be counted as a recovery: it replaces a key rather
	// than adding one, so folding it into Recovered drove the caller's
	// Kept = Total - Harvested - Recovered negative. Asserting the shapes separately here is what
	// stops that returning.
	if added != 1 {
		t.Errorf("added = %d, want 1 (s3, an id only they have)", added)
	}
	if replaced != 1 {
		t.Errorf("replaced = %d, want 1 (s1, adopted because theirs is fresher)", replaced)
	}
	// And the caller's arithmetic stays non-negative on this shape, which is the bug's own symptom:
	// one harvested entry plus one adopted title used to report Kept = -1.
	harvested := 2 // ours held s1 and s2
	if kept := len(ours) - harvested - added; kept < 0 {
		t.Errorf("Kept = %d, negative: the subcommand would print %q",
			kept, "-1 kept from the existing file")
	}

	got := readMetadataFile(t, path)
	if got["s1"].Title != "NEW name" {
		t.Errorf("s1 = %q, want the fresher %q — the concurrent run's rename was reverted",
			got["s1"].Title, "NEW name")
	}
	// And nothing is lost in either direction.
	if got["s2"].Title != "ours only" {
		t.Errorf("s2 = %q, want this run's own entry kept", got["s2"].Title)
	}
	if got["s3"].Title != "theirs only" {
		t.Errorf("s3 = %q, want the other run's entry recovered", got["s3"].Title)
	}
}

// Ours wins when neither side can prove which is fresher.
//
// No recorded time on either entry means no basis for preferring theirs, and this run at least
// knows where its own value came from. Also the pre-upgrade shape: entries written before
// LogModTime existed carry the zero time.
func TestRecoverConcurrentEntries_KeepsOursWhenNeitherIsComparable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")
	ours := map[string]SessionMetadata{
		"s1": {Title: "ours"},
		"s2": {Title: "ours only"},
	}
	onDisk := map[string]SessionMetadata{
		"s1": {Title: "theirs"},
		"s3": {Title: "theirs only"},
	}
	if err := SaveMetadata(path, onDisk); err != nil {
		t.Fatal(err)
	}
	added, replaced, err := recoverConcurrentEntries(path, ours)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Errorf("added = %d, want 1 (s3)", added)
	}
	if replaced != 0 {
		t.Errorf("replaced = %d, want 0 — an incomparable pair keeps ours", replaced)
	}
	got := readMetadataFile(t, path)
	if got["s1"].Title != "ours" {
		t.Errorf("s1 = %q, want %q when neither entry carries a time", got["s1"].Title, "ours")
	}
	if got["s3"].Title != "theirs only" {
		t.Errorf("s3 = %q, want the other run's entry recovered anyway", got["s3"].Title)
	}
}

// Two Harvests running at once lose nothing, driven as real OS processes.
//
// A SUBPROCESS test because the race is between processes, not goroutines: the window is one run's
// read-modify-rename interleaving with another's, and -race sees nothing because there is no shared
// memory to instrument. In-process goroutines would exercise the same functions but not the same
// hazard — os.Rename is what makes the last writer total.
//
// Each child harvests a config dir only IT has, so a correct outcome is the union: every session
// from both trees present afterwards. Any entry missing means one rename clobbered the other's work.
//
// Deliberately more children than a single pair, and repeated, because one pair passes by luck often
// enough to be a useless regression test.
//
// The child branch is in TestMain, not here: it os.Exit()s before m.Run(), so a child never reaches
// a test body at all. A guard at the top of this function would be dead code.
func TestHarvest_ConcurrentRunsLoseNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns subprocesses")
	}

	// THE WINDOW HAS TO BE WIDE ENOUGH TO HIT. A harvest of a one-file tree takes ~3ms while
	// spawning a process takes tens of ms, so children released "together" in fact run one after
	// another and every entry survives no matter what the recovery pass does — the first version of
	// this test passed with recovery deleted outright. Each child therefore gets a tree big enough
	// that its read-modify-rename spans the others' spawn latency: the scan is the slow part, so
	// bulk in the transcripts is what buys the overlap.
	const (
		children   = 6
		padLines   = 400
		padPerLine = 4 << 10
	)
	pad := strings.Repeat("y", padPerLine)

	for attempt := 0; attempt < 3; attempt++ {
		home := t.TempDir()
		dirs := make([]string, children)
		for i := range dirs {
			dirs[i] = filepath.Join(t.TempDir(), fmt.Sprintf("claude-%d", i))
			lines := make([]string, 0, padLines+1)
			for j := 0; j < padLines; j++ {
				// Lines the title scan must read past. "cwd" makes each one a line
				// titleFromTranscript actually parses rather than skips on the byte test.
				lines = append(lines, fmt.Sprintf(`{"type":"user","cwd":"/w/%s"}`, pad))
			}
			lines = append(lines, fmt.Sprintf(`{"type":"ai-title","aiTitle":"title %d"}`, i))
			writeSessionTranscript(t, filepath.Join(dirs[i], "projects", "-p"),
				fmt.Sprintf("sess-%d.jsonl", i), lines...)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range dirs {
			wg.Add(1)
			go func(dir string) {
				defer wg.Done()
				// BOUNDED, and it matters more here than the usual "tests should have timeouts".
				// Every child shares one HOME, so they contend on a single blocking
				// syscall.Flock(LOCK_EX) — lock_unix.go takes it with no timeout, deliberately. A
				// child wedged holding it would block cmd.Wait(), then wg.Wait(), and with three
				// attempts times six children the package would hit its own timeout with nothing
				// saying which child stuck. Same two lines cmd_observe_test.go's runAbctlChild uses.
				ctx, cancel := context.WithTimeout(context.Background(), childTimeout)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHarvest_ConcurrentRunsLoseNothing")
				cmd.Env = append(os.Environ(),
					"CLAUDE_HARVEST_CHILD="+dir,
					"HOME="+home, "USERPROFILE="+home,
				)
				// Started BEFORE the barrier so the spawn cost is paid in parallel; the barrier
				// then releases the harvests themselves as close together as this can manage.
				if err := cmd.Start(); err != nil {
					t.Errorf("spawning child for %s: %v", dir, err)
					return
				}
				<-start
				if err := cmd.Wait(); err != nil {
					if ctx.Err() != nil {
						t.Errorf("child for %s did not exit within %s — likely wedged on the metadata lock",
							dir, childTimeout)
						return
					}
					t.Errorf("child for %s failed: %v", dir, err)
				}
			}(dirs[i])
		}
		close(start)
		wg.Wait()

		path := filepath.Join(home, SessionMetadataRel)
		got := readMetadataFile(t, path)
		for i := range dirs {
			id := fmt.Sprintf("sess-%d", i)
			if got[id].Title != fmt.Sprintf("title %d", i) {
				t.Fatalf("attempt %d: %s missing or wrong after %d concurrent harvests: %q (file holds %d of %d)",
					attempt, id, children, got[id].Title, len(got), children)
			}
		}
	}
}

// TestMain lets the test above re-enter this binary as a harvester.
func TestMain(m *testing.M) {
	if dir := os.Getenv("CLAUDE_HARVEST_CHILD"); dir != "" {
		if _, err := Harvest(Options{ConfigDir: dir, Merge: true}); err != nil {
			fmt.Fprintln(os.Stderr, "child harvest:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Both title line kinds are read: ai-title and agent-name.
//
// Claude Code writes one or the other depending on the install, and reading only ai-title left
// whole config dirs named by working directory instead — measured, 16 of 128 transcripts under one
// dir carried agent-name and only 2 carried ai-title, with no overlap.
func TestTitleFromTranscript_AcceptsBothTitleLineKinds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{
			"agent-name only",
			[]string{`{"type":"agent-name","agentName":"sept-15-rossoctl","sessionId":"ee85f9ac"}`},
			"sept-15-rossoctl",
		},
		{
			"ai-title only",
			[]string{`{"type":"ai-title","aiTitle":"a generated title"}`},
			"a generated title",
		},
		{
			// Last-wins across the two kinds, the same rule that holds between two ai-title
			// lines. Where real transcripts carry both they agree, so this only pins that
			// neither kind is silently preferred over a later claim.
			"both, agent-name later",
			[]string{
				`{"type":"ai-title","aiTitle":"earlier"}`,
				`{"type":"agent-name","agentName":"later"}`,
			},
			"later",
		},
		{
			"both, ai-title later",
			[]string{
				`{"type":"agent-name","agentName":"earlier"}`,
				`{"type":"ai-title","aiTitle":"later"}`,
			},
			"later",
		},
		{
			// A cwd is the fallback, not a competitor: a titled session keeps its title.
			"agent-name beats the cwd fallback",
			[]string{
				`{"type":"user","cwd":"/some/dir"}`,
				`{"type":"agent-name","agentName":"named"}`,
			},
			"named",
		},
		{
			// The field on the wrong line kind is not a claim, same guard ai-title has.
			"agentName on another line kind is ignored",
			[]string{
				`{"type":"user","agentName":"not a title","cwd":"/w"}`,
			},
			"/w",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSessionTranscript(t, dir, "s.jsonl", tc.lines...)
			got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// The user's last typed prompt names a session when no title line does.
//
// Third tier, below both title kinds: measured on a real config dir, 110 of 128 transcripts carried
// neither an ai-title nor an agent-name and fell back to a directory path, which made most rows in
// the sessions table indistinguishable from each other.
func TestTitleFromTranscript_FallsBackToTheLastUserPrompt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{
			"plain string content",
			[]string{`{"type":"user","message":{"role":"user","content":"how do I build abctl?"}}`},
			"how do I build abctl?",
		},
		{
			"text blocks are joined",
			[]string{`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}}`},
			"first second",
		},
		{
			// LAST-wins, like the title tiers above it.
			"last prompt wins",
			[]string{
				`{"type":"user","message":{"role":"user","content":"earlier ask"}}`,
				`{"type":"user","message":{"role":"user","content":"later ask"}}`,
			},
			"later ask",
		},
		{
			// tool_result is tool OUTPUT. 9576 of 9718 content arrays on the measured tree were
			// this, so taking it would have titled 54 of 128 sessions with grep hits and build logs.
			"tool_result is not a prompt",
			[]string{
				`{"type":"user","message":{"role":"user","content":"the real ask"}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"1\tMakefile\n2\tbuild:"}]}}`,
			},
			"the real ask",
		},
		{
			"interrupt markers are skipped",
			[]string{
				`{"type":"user","message":{"role":"user","content":"the real ask"}}`,
				`{"type":"user","message":{"role":"user","content":"[Request interrupted by user for tool use]"}}`,
			},
			"the real ask",
		},
		{
			"image markers are skipped",
			[]string{
				`{"type":"user","message":{"role":"user","content":"the real ask"}}`,
				`{"type":"user","message":{"role":"user","content":"[Image: original 2100x200, displayed at 2000x190]"}}`,
			},
			"the real ask",
		},
		{
			// 27 of 128 transcripts ended with one of these, so without the filter the commonest
			// title on a real tree is a notification envelope.
			"harness blocks are skipped",
			[]string{
				`{"type":"user","message":{"role":"user","content":"the real ask"}}`,
				`{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>abc</task-id>\n</task-notification>"}}`,
			},
			"the real ask",
		},
		{
			"an assistant turn is not a prompt",
			[]string{
				`{"type":"user","message":{"role":"user","content":"the real ask"}}`,
				`{"type":"assistant","message":{"role":"assistant","content":"my reply"}}`,
			},
			"the real ask",
		},
		{
			// Both title kinds outrank a prompt, so a titled session is never renamed.
			"a title line outranks a prompt",
			[]string{
				`{"type":"user","message":{"role":"user","content":"a long rambling ask"}}`,
				`{"type":"agent-name","agentName":"sept-15-rossoctl"}`,
			},
			"sept-15-rossoctl",
		},
		{
			"cwd is still the last resort",
			[]string{
				`{"type":"user","cwd":"/w/project"}`,
				`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"}}`,
			},
			"/w/project",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSessionTranscript(t, dir, "s.jsonl", tc.lines...)
			got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// Titles are clipped to 80 runes and flattened to one line.
//
// A prompt is unbounded free text and a title is a table cell. Runes rather than bytes so a
// multi-byte prompt is not cut mid-character, and whitespace is collapsed because the viewer turns
// control characters into U+FFFD rather than dropping them — a raw newline would reach the cell as
// a visible glyph.
func TestTitleFromTranscript_ClipsAndFlattens(t *testing.T) {
	long := strings.Repeat("a", 200)
	cjk := strings.Repeat("日", 200)
	for _, tc := range []struct {
		name        string
		content     string
		wantRunes   int
		wantOneLine bool
	}{
		{"long ascii", long, MaxTitleLen, true},
		{"long CJK is cut by rune, not byte", cjk, MaxTitleLen, true},
		{"newlines collapse", "first line\n\nsecond line\twith a tab", -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body, err := json.Marshal(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			writeSessionTranscript(t, dir, "s.jsonl",
				`{"type":"user","message":{"role":"user","content":`+string(body)+`}}`)
			got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if gerr != nil {
				t.Fatal(gerr)
			}
			if n := len([]rune(got)); n > MaxTitleLen {
				t.Errorf("title is %d runes, over the %d cap: %q", n, MaxTitleLen, got)
			}
			if tc.wantRunes > 0 && len([]rune(got)) != tc.wantRunes {
				t.Errorf("title is %d runes, want %d", len([]rune(got)), tc.wantRunes)
			}
			if tc.wantOneLine && strings.ContainsAny(got, "\n\t") {
				t.Errorf("title carries a control character: %q", got)
			}
			if !utf8.ValidString(got) {
				t.Errorf("title is not valid UTF-8 — cut mid-character: %q", got)
			}
		})
	}
}

// An attributed human turn outranks everything below it, and a string outranks an array.
//
// origin.kind is the only authoritative statement of who produced a turn, and Claude Code supplies
// it: measured across 128 transcripts, 640 user turns were "human", 151 "task-notification", 33
// "peer". Every human turn carried STRING content and none carried an array — which is why the shape
// grades the result too. The array path is where the harness injects, and it produced a
// "Base directory for this skill: /Users/…/plugins/cache/…" title before this ranking existed.
func TestTitleFromTranscript_PrefersAttributedHumanStrings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{
			// The shape of the reported file: one human turn early, harness arrays after it.
			"human string beats a later array",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"what I actually asked"}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /Users/x/.claude/plugins/cache/y"}]}}`,
			},
			"what I actually asked",
		},
		{
			"human string beats an unattributed string",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"attributed"}}`,
				`{"type":"user","message":{"role":"user","content":"unattributed but later"}}`,
			},
			"attributed",
		},
		{
			// Explicitly not human: discarded rather than ranked.
			"task-notification is discarded",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"the real ask"}}`,
				`{"type":"user","origin":{"kind":"task-notification"},"message":{"role":"user","content":"a notification body"}}`,
			},
			"the real ask",
		},
		{
			"peer is discarded",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"the real ask"}}`,
				`{"type":"user","origin":{"kind":"peer"},"message":{"role":"user","content":"a peer message"}}`,
			},
			"the real ask",
		},
		{
			// Last-wins WITHIN the human grade.
			"the latest human turn wins",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"first ask"}}`,
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"second ask"}}`,
			},
			"second ask",
		},
		{
			// An unattributed string still beats an array, for transcripts with no origin field.
			"unattributed string beats an array",
			[]string{
				`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"from an array"}]}}`,
				`{"type":"user","message":{"role":"user","content":"a plain string"}}`,
			},
			"a plain string",
		},
		{
			// A human turn is taken as typed, with no synthetic-text guessing over the top.
			"a bracketed human prompt is kept",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"[note] this is mine"}}`,
			},
			"[note] this is mine",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSessionTranscript(t, dir, "s.jsonl", tc.lines...)
			got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// A slash command is rendered as the line the user typed, not as its envelope.
//
// Claude Code wraps a typed slash command in <command-message>/<command-name>/<command-args>. The
// turn IS the user's, so filtering it would lose a real prompt — but 28 of 128 measured transcripts
// ended with one, and 27 of those shared the same command, so left as markup they would all carry an
// identical unreadable title. The arguments are what tell them apart.
func TestTitleFromTranscript_UnwrapsSlashCommands(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{
			"name and args",
			`<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>some/path.md</command-args>`,
			"/review some/path.md",
		},
		{
			"name only",
			`<command-message>clear</command-message>\n<command-name>/clear</command-name>\n<command-args></command-args>`,
			"/clear",
		},
		{
			"ordinary prose is untouched",
			`just a normal question`,
			"just a normal question",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body, err := json.Marshal(strings.ReplaceAll(tc.content, `\n`, "\n"))
			if err != nil {
				t.Fatal(err)
			}
			writeSessionTranscript(t, dir, "s.jsonl",
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":`+string(body)+`}}`)
			got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// The cwd fallback is NOT clipped, so sibling directories stay distinguishable.
//
// A path's distinguishing end is its leaf and clipTitle keeps the head, so two worktrees under a
// prefix of 80 runes or more clipped to byte-identical titles — the column stopped telling apart
// exactly the sessions it exists to name. Also a regression against the behaviour before titles
// were clipped at all.
func TestTitleFromTranscript_DoesNotClipTheCwdFallback(t *testing.T) {
	// 81 runes, so the leaf is entirely past the cap.
	const prefix = "/Users/somebody/go/src/github.com/some-organisation/some-repository/worktrees/wt/"
	if len([]rune(prefix)) <= MaxTitleLen {
		t.Fatalf("fixture prefix is %d runes, needs to exceed %d to exercise the cut",
			len([]rune(prefix)), MaxTitleLen)
	}
	titleFor := func(cwd string) string {
		dir := t.TempDir()
		body, err := json.Marshal(cwd)
		if err != nil {
			t.Fatal(err)
		}
		writeSessionTranscript(t, dir, "s.jsonl", `{"type":"user","cwd":`+string(body)+`}`)
		got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
		if gerr != nil {
			t.Fatal(gerr)
		}
		return got
	}
	a, b := titleFor(prefix+"alpha"), titleFor(prefix+"beta")
	if a == b {
		t.Errorf("sibling paths produced the same title %q — the leaf was clipped away", a)
	}
	if !strings.HasSuffix(a, "alpha") || !strings.HasSuffix(b, "beta") {
		t.Errorf("the leaf did not survive: %q / %q", a, b)
	}
}

// A whitespace-only candidate falls through to the next tier instead of rendering blank.
//
// It passed a bare `!= ""` guard and clipTitle then emptied it, so the tier below was skipped and
// the cell came out empty. Each candidate is normalised before the switch now.
func TestTitleFromTranscript_WhitespaceOnlyCandidateFallsThrough(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{
			"whitespace ai-title falls through to the cwd",
			[]string{
				`{"type":"user","cwd":"/w/real"}`,
				`{"type":"ai-title","aiTitle":"   "}`,
			},
			"/w/real",
		},
		{
			"whitespace agent-name falls through to a prompt",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"the real ask"}}`,
				`{"type":"agent-name","agentName":"\t\n "}`,
			},
			"the real ask",
		},
		{
			"whitespace prompt falls through to the cwd",
			[]string{
				`{"type":"user","cwd":"/w/real"}`,
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"   \t "}}`,
			},
			"/w/real",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSessionTranscript(t, dir, "s.jsonl", tc.lines...)
			got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// Harness tags are detected with attributes, and whatever their case.
//
// The tag-name check ran over everything up to ">", so a space, "=" or quote failed it and every
// attributed tag slipped through — a real transcript produced a title of raw
// `<pasted_content id="...">` markup. Case folding is latent by comparison: every tag observed on a
// real tree is lowercase.
func TestIsSyntheticPrompt_HandlesAttributesAndCase(t *testing.T) {
	for _, s := range []string{
		`<pasted_content id="abc123">some pasted text</pasted_content>`,
		`<task-notification id="1" kind="x">body</task-notification>`,
		`<TASK-NOTIFICATION>body</TASK-NOTIFICATION>`,
		`<Task-Notification id="2">body</Task-Notification>`,
		`<system-reminder/>`,
		`<bash-stdout>out</bash-stdout>`,
		`[Request interrupted by user for tool use]`,
		`[Image: original 2100x200, displayed at 2000x190]`,
	} {
		if !isSyntheticPrompt(s) {
			t.Errorf("not detected as synthetic: %q", s)
		}
	}
	// And real prose is still a prompt, including text that merely contains a "<".
	for _, s := range []string{
		"how do I build abctl?",
		"is 3 < 5 in Go?",
		"/review some/path.md",
		"日本語の質問です",
	} {
		if isSyntheticPrompt(s) {
			t.Errorf("real prompt rejected as synthetic: %q", s)
		}
	}
}

// MaxTitleLen is a RUNE cap, and the renderer is what bounds display width.
//
// 80 runes of CJK occupy 160 columns, so nothing may read this constant as a width budget. This
// pins ONLY the half that lives in this package: that the cap counts runes rather than bytes or
// columns. The other half — that the sessions pane re-truncates by lipgloss.Width — is in
// cmd/abctl/tui and tested there; no test in either module fails if that truncation is removed while
// this constant stays, so the cross-module invariant rests on the comments, not on this test.
func TestMaxTitleLen_IsARuneCapNotAWidthBudget(t *testing.T) {
	dir := t.TempDir()
	body, err := json.Marshal(strings.Repeat("日", 200))
	if err != nil {
		t.Fatal(err)
	}
	writeSessionTranscript(t, dir, "s.jsonl",
		`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":`+string(body)+`}}`)
	got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
	if gerr != nil {
		t.Fatal(gerr)
	}
	if n := len([]rune(got)); n != MaxTitleLen {
		t.Errorf("clipped to %d runes, want %d", n, MaxTitleLen)
	}
	// The point of the test: runes are capped, BYTES AND COLUMNS ARE NOT. authlib has no width
	// library — lipgloss and go-runewidth are cmd/abctl dependencies, and adding one here for a
	// single assertion is not worth it — so byte length stands in as the observable proxy: a
	// three-byte-per-rune title is 240 bytes at 80 runes, and anything laying out by width will
	// likewise see more than 80. What this pins is that the cap is NOT a width, which is the
	// mistake the comment on MaxTitleLen warns against.
	if len(got) <= MaxTitleLen {
		t.Errorf("CJK title is %d bytes for %d runes; if that is now <= the cap then MaxTitleLen "+
			"is being applied as a width or byte bound, and the renderers' own truncation must be "+
			"revisited", len(got), MaxTitleLen)
	}
}

// A pasted-input wrapper is stripped from a human turn, keeping what was pasted.
//
// `<pasted_content id="2e21">…` arrives with origin.kind "human" — the user really did paste it — so
// filtering it would lose a real prompt, but the wrapper is markup and left alone it became the
// title. A real transcript produced exactly that.
func TestTitleFromTranscript_StripsPastedContentWrapper(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{
			"pasted wrapper is stripped",
			`<pasted_content id="2e21">` + "\n" + `When considering each agent node, consult the first`,
			"When considering each agent node, consult the first",
		},
		{
			"closing tag is dropped too",
			`<pasted_content id="x">the pasted body</pasted_content>`,
			"the pasted body",
		},
		{
			// A command envelope must NOT lose its arguments to the wrapper strip: it is a
			// multi-tag structure and <command-args> sits past the first tag.
			"a command envelope keeps its args",
			`<command-message>review</command-message>` + "\n" + `<command-name>/review</command-name>` + "\n" + `<command-args>some/path.md</command-args>`,
			"/review some/path.md",
		},
		{
			"prose containing a less-than is untouched",
			"is 3 < 5 in Go?",
			"is 3 < 5 in Go?",
		},
		{
			// Falls through rather than titling with markup. An earlier version of this test
			// expected the raw tag back, on the reasoning that returning the input unchanged is
			// the safe default — but the caller now re-tests the result against the synthetic
			// guard, so "unchanged" means "rejected" and the tier below is used instead. With no
			// cwd in this fixture that leaves "", which is the honest answer for a turn whose
			// entire content was a wrapper.
			"a wrapper with nothing inside it yields no title",
			`<pasted_content id="x">`,
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body, err := json.Marshal(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			writeSessionTranscript(t, dir, "s.jsonl",
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":`+string(body)+`}}`)
			got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// Claude Code's own last-prompt record outranks anything reconstructed from user turns.
//
// {"type":"last-prompt","lastPrompt":…} is the agent stating what the prompt was, rather than this
// package inferring it and then filtering harness traffic back out. Measured, 129 of 130 transcripts
// carry one.
func TestTitleFromTranscript_PrefersLastPromptRecord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{
			"last-prompt beats a reconstructed human turn",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"a reconstructed ask"}}`,
				`{"type":"last-prompt","lastPrompt":"the recorded ask"}`,
			},
			"the recorded ask",
		},
		{
			// Both title kinds still outrank it: a generated title is a summary, this is raw input.
			"a title line still outranks last-prompt",
			[]string{
				`{"type":"last-prompt","lastPrompt":"the recorded ask"}`,
				`{"type":"agent-name","agentName":"sept-15-rossoctl"}`,
			},
			"sept-15-rossoctl",
		},
		{
			"last-wins among last-prompt lines",
			[]string{
				`{"type":"last-prompt","lastPrompt":"earlier"}`,
				`{"type":"last-prompt","lastPrompt":"later"}`,
			},
			"later",
		},
		{
			// A slash command is recorded in its envelope form here too.
			"a recorded slash command is unwrapped",
			[]string{
				`{"type":"last-prompt","lastPrompt":"<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>some/path.md</command-args>"}`,
			},
			"/review some/path.md",
		},
		{
			// Three of 2223 real lines carried a null, which decodes to "".
			"a null lastPrompt falls through",
			[]string{
				`{"type":"user","cwd":"/w/real"}`,
				`{"type":"last-prompt","lastPrompt":null}`,
			},
			"/w/real",
		},
		{
			// A HARNESS-OUTPUT wrapper falls through: its body is the harness's own text, not the
			// user's, so promoting it would put a notification in the title. An earlier version of
			// this test asserted the opposite — that the body becomes the title — which
			// contradicted isSyntheticPrompt's own stated purpose and let
			// `<bash-stdout>total 40</bash-stdout>` render as "total 40".
			"a harness-output lastPrompt falls through",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"the real ask"}}`,
				`{"type":"last-prompt","lastPrompt":"<task-notification>body</task-notification>"}`,
			},
			"the real ask",
		},
		{
			// A CONTENT-BEARING wrapper is still unwrapped: the user pasted what is inside it.
			"a pasted-content lastPrompt is stripped to its body",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"the real ask"}}`,
				`{"type":"last-prompt","lastPrompt":"<pasted_content id=\"x\">the pasted body</pasted_content>"}`,
			},
			"the pasted body",
		},
		{
			"a lastPrompt that is markup all the way down falls through",
			[]string{
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"the real ask"}}`,
				`{"type":"last-prompt","lastPrompt":"<a><b>x</b></a>"}`,
			},
			"the real ask",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSessionTranscript(t, dir, "s.jsonl", tc.lines...)
			got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// Markup that survives one leading-tag strip falls through instead of becoming the title.
//
// Three shapes, one root cause — a single strip is not enough, so the result is re-tested against
// the synthetic guard:
//
//   - an EMPTY body leaves the closing tag at index 0, which a `j > 0` guard skipped, yielding a
//     bare "</pasted_content>";
//   - a MALFORMED command envelope makes the unwrapper bail on the empty name, so control reaches
//     the wrapper strip and leaves "</command-name> real text";
//   - a NESTED wrapper has only its outer tag removed, leaving "<b>real prompt</b>".
//
// The nested case has no occurrence on real data (0 of 645 human string turns), but all three are the
// same defect and the fix is one check.
func TestTitleFromTranscript_MarkupSurvivingOneStripFallsThrough(t *testing.T) {
	for _, tc := range []struct {
		name, content string
	}{
		{"empty-bodied wrapper", `<pasted_content id="x"></pasted_content>`},
		{"malformed command envelope", `<command-name></command-name> real text`},
		{"nested wrapper", `<a><b>real prompt</b></a>`},
		{"bare closing tag", `</pasted_content>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body, err := json.Marshal(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			// A cwd is present, so falling through has somewhere to land and the assertion
			// distinguishes "fell through" from "returned empty".
			writeSessionTranscript(t, dir, "s.jsonl",
				`{"type":"user","cwd":"/w/real"}`,
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":`+string(body)+`}}`)
			got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got != "/w/real" {
				t.Errorf("title = %q, want the cwd fallback — markup reached the title", got)
			}
			if strings.ContainsAny(got, "<>") {
				t.Errorf("title carries markup: %q", got)
			}
		})
	}
}

// Harness markup never reaches the title, wherever in the turn it sits.
//
// Two leaks, both reaching rendered output before this:
//
//   - a harness BLOCK alongside the user's real text in a multi-block turn. promptFromMessage joined
//     every text block and the guard is anchored at the start, so the join began with prose and the
//     markup passed intact. Filtered per block now.
//   - markup APPENDED after prose in a string turn, which no anchored check can see. Measured on a
//     real tree: 7 of 130 transcripts. Cut at the first known harness tag.
func TestTitleFromTranscript_NoHarnessMarkupInTitles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{
			"a harness block beside real text is dropped",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":[{"type":"text","text":"my real question"},{"type":"text","text":"<system-reminder>hidden</system-reminder>"}]}}`},
			"my real question",
		},
		{
			"a harness block BEFORE real text is dropped",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":[{"type":"text","text":"<system-reminder>hidden</system-reminder>"},{"type":"text","text":"my real question"}]}}`},
			"my real question",
		},
		{
			"markup appended after prose is cut",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"my real question\n<system-reminder>do not mention this</system-reminder>"}}`},
			"my real question",
		},
		{
			// INDENTED. Every appended-block fixture here was unindented, so none exercised the
			// whitespace path: requiring s[i-1] to be exactly a newline let "my question\n
			// <system-reminder>…" keep its tag and get clipped mid-tag at 80 runes.
			"a space-indented appended block is cut",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"my real question\n   <system-reminder>do not mention this</system-reminder>"}}`},
			"my real question",
		},
		{
			"a tab-indented appended block is cut",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"my real question\n\t<system-reminder>do not mention this</system-reminder>"}}`},
			"my real question",
		},
		{
			"a mixed-indent appended block is cut",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"my real question\n \t  <bash-stdout>total 40</bash-stdout>"}}`},
			"my real question",
		},
		{
			// Prose and markup sharing ONE text block. isSyntheticPrompt is anchored, so the block
			// passed the per-block check and reached the title whole; the cut is applied inside the
			// join loop now.
			"markup sharing a block with prose is cut",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":[{"type":"text","text":"my real question\n<system-reminder>hidden</system-reminder>"}]}}`},
			"my real question",
		},
		{
			"markup sharing a block with prose, unattributed turn",
			[]string{`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"my real question\n<system-reminder>hidden</system-reminder>"}]}}`},
			"my real question",
		},
		{
			"a recorded lastPrompt is cut the same way",
			[]string{`{"type":"last-prompt","lastPrompt":"recorded ask\n<system-reminder>hidden</system-reminder>"}`},
			"recorded ask",
		},
		{
			// A TAG IS REMOVED WHEREVER IT APPEARS, including one a real prompt quotes. Titles are
			// plain text with nothing hidden in them, and that cannot also be "faithfully quotes
			// markup" — every shape-based filter this replaced was flanked by the next round of
			// review. The prompt stays readable, which is what the title is for.
			"a quoted tag is stripped, prose survives",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"why does <div> break my layout?"}}`},
			"why does break my layout?",
		},
		{
			"generics are stripped, prose survives",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"how do I write List<String> in Go?"}}`},
			"how do I write List in Go?",
		},
		{
			// A LONE "<" is not a tag and must survive: this is the shape a real prompt carries.
			"a comparison is not markup",
			[]string{`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"is 3 < 5 in Go?"}}`},
			"is 3 < 5 in Go?",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSessionTranscript(t, dir, "s.jsonl", tc.lines...)
			got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "<system-reminder") || strings.Contains(got, "</system-reminder") {
				t.Errorf("harness markup reached the title: %q", got)
			}
		})
	}
}

// The byte prefilter does not depend on how the writer spaces its JSON.
//
// Every other fixture in this file is hand-written COMPACT JSON, so the suite could not see a
// prefilter term that embedded a key-value pair: `"role":"user"` skipped a line written
// `"role": "user"` before it ever reached the decoder, silently losing the prompt. The terms are bare
// keys again, and this is the only test that writes the spaced form — which is the point of it.
func TestTitleFromTranscript_PrefilterIgnoresJSONSpacing(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{
			"spaced user turn",
			`{"type": "user", "origin": {"kind": "human"}, "message": {"role": "user", "content": "a spaced ask"}}`,
			"a spaced ask",
		},
		{
			"spaced last-prompt",
			`{"type": "last-prompt", "lastPrompt": "a spaced record"}`,
			"a spaced record",
		},
		{
			"spaced agent-name",
			`{"type": "agent-name", "agentName": "spaced-name"}`,
			"spaced-name",
		},
		{
			"spaced ai-title",
			`{"type": "ai-title", "aiTitle": "a spaced title"}`,
			"a spaced title",
		},
		{
			"spaced cwd",
			`{"type": "user", "cwd": "/w/spaced"}`,
			"/w/spaced",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSessionTranscript(t, dir, "s.jsonl", tc.line)
			got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q — the prefilter skipped a spaced line", got, tc.want)
			}
		})
	}
}

// A harness-output wrapper never has its body promoted to a title.
//
// stripWrapperTag used to unwrap anything tag-shaped, so `<bash-stdout>total 40</bash-stdout>` became
// the title "total 40" and a `<system-reminder>` body became a title — the grep-dump outcome
// isSyntheticPrompt's doc says it exists to prevent. Only wrappers whose CONTENT IS THE USER'S are
// unwrapped now; see contentBearingWrappers.
func TestTitleFromTranscript_HarnessOutputWrappersFallThrough(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{"bash-stdout", `<bash-stdout>total 40</bash-stdout>`, "/w/fallback"},
		{"bash-stderr", `<bash-stderr>No such file</bash-stderr>`, "/w/fallback"},
		{"system-reminder", `<system-reminder>do not mention this</system-reminder>`, "/w/fallback"},
		{"task-notification", `<task-notification>agent finished</task-notification>`, "/w/fallback"},
		{"local-command-caveat", `<local-command-caveat>Caveat: generated</local-command-caveat>`, "/w/fallback"},
		// The one wrapper whose body IS the user's, so it is still unwrapped.
		{"pasted_content is still unwrapped", `<pasted_content id="x">the pasted body</pasted_content>`, "the pasted body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body, err := json.Marshal(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			writeSessionTranscript(t, dir, "s.jsonl",
				`{"type":"user","cwd":"/w/fallback"}`,
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":`+string(body)+`}}`)
			got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// Prose that MENTIONS a known harness tag is not truncated; only a tag starting a line is a cut.
//
// THE COVERAGE GAP THIS CLOSES: every previous "untouched prose" case used an UNKNOWN tag — <div>,
// List<String>, "3 < 5" — so they all took isSyntheticPrompt's structural path and never reached
// cutTrailingHarness's named set. Nothing put a known tag inside real prose, which is exactly how
// "how do I use <command-args> in a skill?" came to be truncated to "how do I use".
func TestTitleFromTranscript_InlineHarnessTagMentionIsNotACut(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{
			// These used to assert the tag SURVIVES, on the reasoning that cutting mid-sentence was
			// worse than leaking a tag. That trade is gone: the tag is stripped and the surrounding
			// prose is kept, so neither the truncation nor the leak happens.
			"known tag mid-sentence is stripped, not cut",
			"how do I use <command-args> in a skill?",
			"how do I use in a skill?",
		},
		{
			"known tag mid-sentence, different tag",
			"why does <system-reminder> show up in my logs?",
			"why does show up in my logs?",
		},
		{
			"known tag after a space",
			"see <task-notification> for details",
			"see for details",
		},
		{
			// The real shape: the harness appends on its own line, which IS a cut.
			"a tag starting a line is still cut",
			"my real question\n<system-reminder>hidden</system-reminder>",
			"my real question",
		},
		{
			// Every occurrence is examined, so an inline mention does not mask a later real block.
			"an inline mention does not hide a later appended block",
			"how do I use <command-args>?\n<system-reminder>hidden</system-reminder>",
			"how do I use ?",
		},
		{
			// THE LAST DOCUMENTED LEAK, now closed. This was left alone deliberately, as a trade
			// against mid-sentence truncation — a known tag mid-line on a later line being
			// ambiguous between prose and appended markup. Stripping the span removes the need to
			// judge: the harness's block and its body go, the prose stays.
			"a known tag mid-line on a later line is stripped",
			"line one\nline two <system-reminder>x</system-reminder>",
			"line one line two",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body, err := json.Marshal(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			writeSessionTranscript(t, dir, "s.jsonl",
				`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":`+string(body)+`}}`)
			got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// A Unicode-escaped member name still reaches the decoder.
//
// JSON permits `"lastPrompt"`, which decodes to lastPrompt but matches no literal in the raw-byte
// prefilter, so the line was skipped and the session lost its title. Nothing observed writes keys that
// way — 0 of 46,124 real lines — so this is latent; it is taken because it is nearly free, only 0.2% of
// lines containing a `\u` escape at all.
func TestTitleFromTranscript_DecodesEscapedMemberNames(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
	}{
		// BACKTICK-CONCATENATED so the \u sequences reach the FILE as six literal characters. An
		// earlier version of this test wrote them inside a normal literal, where Go decoded them at
		// compile time: the fixture then contained a plain "lastPrompt" and the test passed with the
		// fix removed, testing nothing. Verified by mutation after the change.
		{"escaped lastPrompt", `{"type":"last-prompt","last` + `\u0050` + `rompt":"review the change"}`, "review the change"},
		{"escaped aiTitle", `{"type":"ai-title","ai` + `\u0054` + `itle":"a generated title"}`, "a generated title"},
		{"escaped agentName", `{"type":"agent-name","agent` + `\u004E` + `ame":"sept-15-rossoctl"}`, "sept-15-rossoctl"},
		{"escaped cwd", `{"type":"user","c` + `\u0077` + `d":"/w/escaped"}`, "/w/escaped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSessionTranscript(t, dir, "s.jsonl", tc.line)
			got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("title = %q, want %q — the prefilter skipped an escaped key", got, tc.want)
			}
		})
	}
}

// EVERY title is plain text: no markup, no control characters, no hidden code points.
//
// THE GUARANTEE, asserted as a property rather than as a list of shapes. Six rounds of review each
// found a new placement that the shape-based filters missed — markup inside <command-args>, mid-line
// on a later line, a block indented with a vertical tab — because enumerating placements cannot
// converge when the placements are the harness's to choose. clipTitle now normalises unconditionally,
// and this checks the outcome for every input rather than each route into it.
//
// The corpus deliberately mixes real prompts with adversarial ones: a title is LLM-generated text
// read from a file nothing authenticates, so "would a hostile transcript do this" is not the
// question — the question is what a title may contain.
func TestTitleFromTranscript_TitlesAreAlwaysPlain(t *testing.T) {
	inputs := []string{
		// Harness markup in every placement review found, plus the ones it did not.
		`<system-reminder>injected instructions</system-reminder>`,
		"my question\n<system-reminder>injected</system-reminder>",
		"my question\n   <system-reminder>injected</system-reminder>",
		"my question\n\t<system-reminder>injected</system-reminder>",
		"my question\n\v<system-reminder>injected</system-reminder>",
		"my question\n <system-reminder>injected</system-reminder>",
		"my question\n　<system-reminder>injected</system-reminder>",
		"line one\nline two <system-reminder>injected</system-reminder>",
		"<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args><system-reminder>injected</system-reminder></command-args>",
		`<pasted_content id="x"><system-reminder>injected</system-reminder></pasted_content>`,
		`<bash-stdout>total 40</bash-stdout>`,
		`<a><b>nested</b></a>`,
		// UNKNOWN tags, which only stripTags removes — the corpus had none, so a mutation deleting
		// that step passed. Same fixture blindness as the earlier rounds: the inputs excluded the
		// exact path the code was meant to cover.
		"why does <div> break my layout?",
		"how do I write List<String> in Go?",
		"a <b>bold</b> claim",
		"<unknown-tag>body</unknown-tag>",
		// Control characters and invisible code points.
		"colour \x1b[31mred\x1b[0m here",
		"osc \x1b]0;evil\x07 here",
		"nel \u0085 here",
		"csi \u009b here",
		"del \x7f here",
		"bidi ‮ reversed",
		"isolate ⁦ x ⁩ y",
		"zwsp a​b",
		"zwj a‍b",
		"vs16 a️b",
		"nul \x00 here",
		"tab\tand\nnewline",
		// Legitimate content, which must survive as readable text.
		"how do I build abctl?",
		"is 3 < 5 in Go?",
		"日本語のセッションタイトルです",
		"ship it 🎉",
		"café naïve",
		"/review some/path.md",
	}
	for _, in := range inputs {
		dir := t.TempDir()
		body, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		writeSessionTranscript(t, dir, "s.jsonl",
			`{"type":"user","cwd":"/w/fallback"}`,
			`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":`+string(body)+`}}`)
		got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
		if gerr != nil {
			t.Fatalf("input %q: %v", in, gerr)
		}

		// No angle brackets that could read as a tag. A lone "<" survives — "is 3 < 5" is a real
		// prompt — but never a matched pair.
		if strings.Contains(got, ">") {
			t.Errorf("input %q: title carries a closing bracket: %q", in, got)
		}
		// No harness tag name, in any form.
		for _, tag := range harnessTagNames {
			if strings.Contains(got, strings.TrimPrefix(tag, "<")) {
				t.Errorf("input %q: title carries %s: %q", in, tag, got)
			}
		}
		// AND NOT THE BODY EITHER. Stripping the brackets alone promoted the payload into the
		// title — "my question\n<system-reminder>injected</system-reminder>" became
		// "my question injected" — which is the whole point of removing a harness span rather than
		// just its tags. Checking only for tags left this test blind to it, and the mutation that
		// exposed the gap passed until this assertion existed.
		if strings.Contains(got, "injected") || strings.Contains(got, "total 40") {
			t.Errorf("input %q: title carries a harness BODY: %q", in, got)
		}
		// Every rune prints as itself: no controls, format characters, surrogates or unassigned.
		for _, r := range got {
			if r == ' ' {
				continue
			}
			if !unicode.IsGraphic(r) {
				t.Errorf("input %q: title carries non-graphic %U: %q", in, r, got)
			}
		}
		// One line, no runs of whitespace, no leading or trailing space.
		if got != strings.TrimSpace(got) {
			t.Errorf("input %q: title is not trimmed: %q", in, got)
		}
		if strings.Contains(got, "  ") {
			t.Errorf("input %q: title has a double space: %q", in, got)
		}
		// Bounded, and valid UTF-8 — never cut mid-character.
		if n := len([]rune(got)); n > MaxTitleLen {
			t.Errorf("input %q: title is %d runes: %q", in, n, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("input %q: title is not valid UTF-8: %q", in, got)
		}
	}
}

// Legitimate prompts survive the normalisation as readable text.
//
// The guarantee above would also be satisfied by returning "" for everything, so this is the other
// half of it: what the normalisation must NOT destroy.
func TestClipTitle_KeepsLegitimateText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"how do I build abctl?", "how do I build abctl?"},
		{"is 3 < 5 in Go?", "is 3 < 5 in Go?"},
		{"日本語のセッションタイトル", "日本語のセッションタイトル"},
		{"ship it 🎉", "ship it 🎉"},
		{"café naïve", "café naïve"},
		{"/review some/path.md", "/review some/path.md"},
		{"  leading and trailing  ", "leading and trailing"},
		{"collapses\n\n\tinner   whitespace", "collapses inner whitespace"},
		{"colour \x1b[31mred\x1b[0m here", "colour red here"},
	} {
		if got := clipTitle(tc.in); got != tc.want {
			t.Errorf("clipTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The cwd fallback is plain too, and keeps its leaf.
//
// It returned through a bare strings.Fields — whitespace collapsed and nothing else — so a directory
// name carrying an ESC sequence, a bidi override or a tag reached the file unfiltered while every
// other tier was clean. It now shares normalizeTitle with them, but NOT the length cap: a path's
// distinguishing end is its leaf, and clipping keeps the head.
func TestTitleFromTranscript_CwdFallbackIsPlainAndUnclipped(t *testing.T) {
	titleFor := func(cwd string) string {
		t.Helper()
		dir := t.TempDir()
		body, err := json.Marshal(cwd)
		if err != nil {
			t.Fatal(err)
		}
		writeSessionTranscript(t, dir, "s.jsonl", `{"type":"user","cwd":`+string(body)+`}`)
		got, gerr := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
		if gerr != nil {
			t.Fatal(gerr)
		}
		return got
	}

	for _, in := range []string{
		"/w/dir\x1b[31mred",
		"/w/‮dir",
		"/w/<system-reminder>injected</system-reminder>dir",
		"/w/áccent",
		"/w/norm​al",
	} {
		got := titleFor(in)
		for _, r := range got {
			if r != ' ' && !unicode.IsGraphic(r) {
				t.Errorf("cwd %q: title carries non-graphic %U: %q", in, r, got)
			}
		}
		if strings.Contains(got, ">") {
			t.Errorf("cwd %q: title carries markup: %q", in, got)
		}
		if strings.Contains(got, "injected") {
			t.Errorf("cwd %q: title carries a harness body: %q", in, got)
		}
	}

	// NOT clipped: a prefix past the cap must not swallow the leaf, or sibling worktrees become
	// indistinguishable.
	const prefix = "/Users/somebody/go/src/github.com/some-organisation/some-repository/worktrees/wt/"
	if len([]rune(prefix)) <= MaxTitleLen {
		t.Fatalf("fixture prefix is %d runes, needs to exceed %d", len([]rune(prefix)), MaxTitleLen)
	}
	a, b := titleFor(prefix+"alpha"), titleFor(prefix+"beta")
	if a == b {
		t.Errorf("sibling paths produced the same title %q", a)
	}
	if !strings.HasSuffix(a, "alpha") || !strings.HasSuffix(b, "beta") {
		t.Errorf("the leaf did not survive: %q / %q", a, b)
	}
}

// A truncated read is REPORTED, not silently swallowed, and the best title found still comes back.
//
// titleFromTranscript's second return says the scan ended early — a line past the 16MB buffer, or an
// I/O error partway through — and ReadSessions collects those into Result.Partial for the caller to
// warn about. The contract was documented in two places and asserted nowhere, so a regression would
// have been silent: the title of a long session would quietly become a stale one.
func TestTitleFromTranscript_ReportsTruncatedReads(t *testing.T) {
	dir := t.TempDir()
	// One line past bufio.Scanner's 16MB ceiling, after a usable title. The title found before the
	// stop must survive, since something is better than nothing — that is why the error is returned
	// ALONGSIDE it rather than instead of it.
	huge := strings.Repeat("x", 17<<20)
	writeSessionTranscript(t, dir, "s.jsonl",
		`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"a real ask"}}`,
		`{"type":"user","cwd":"/w/`+huge+`"}`)

	got, err := titleFromTranscript(filepath.Join(dir, "s.jsonl"))
	if err == nil {
		t.Error("a line past the scanner buffer was not reported")
	}
	if got != "a real ask" {
		t.Errorf("title = %q, want the title found before the stop", got)
	}
}

// ReadSessions surfaces a truncated transcript through Result.Partial rather than failing the harvest.
//
// One bad transcript must not cost the other hundred names, so the error is collected and the session
// still lands in the map. The propagation was untested.
func TestReadSessions_CollectsPartialReads(t *testing.T) {
	cfg := t.TempDir()
	proj := filepath.Join(cfg, "projects", "-p")
	huge := strings.Repeat("x", 17<<20)
	writeSessionTranscript(t, proj, "truncated.jsonl",
		`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"a real ask"}}`,
		`{"type":"user","cwd":"/w/`+huge+`"}`)
	writeSessionTranscript(t, proj, "fine.jsonl",
		`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"another ask"}}`)

	out, partial, _, err := ReadSessions(cfg, nil)
	if err != nil {
		t.Fatalf("a truncated transcript failed the whole harvest: %v", err)
	}
	if len(partial) != 1 {
		t.Errorf("partial = %d entries, want 1: %v", len(partial), partial)
	}
	if len(partial) == 1 && !strings.Contains(partial[0], "truncated.jsonl") {
		t.Errorf("the partial entry does not name the transcript: %q", partial[0])
	}
	// Both sessions are still named — the truncated one from what was found before the stop.
	if out["truncated"].Title != "a real ask" {
		t.Errorf("truncated session title = %q, want %q", out["truncated"].Title, "a real ask")
	}
	if out["fine"].Title != "another ask" {
		t.Errorf("healthy session title = %q, want %q", out["fine"].Title, "another ask")
	}
}
