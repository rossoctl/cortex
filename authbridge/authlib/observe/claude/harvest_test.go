package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	recovered, err := recoverConcurrentEntries(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Errorf("recovered = %d, want 1", recovered)
	}
	got := readMetadataFile(t, path)
	if w := got[theirs].Title; w != "From another run" {
		t.Errorf("the concurrent entry was lost: %q", w)
	}
	if w := got[ours].Title; w != "Ours" {
		t.Errorf("this run's own entry is missing: %q", w)
	}

	// Idempotent: a second pass finds nothing to do and must not rewrite the file.
	again, err := recoverConcurrentEntries(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("second pass recovered %d, want 0 — the merge is not idempotent", again)
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
	recovered, err := recoverConcurrentEntries(path, map[string]SessionMetadata{"a": {}})
	if err != nil {
		t.Errorf("err = %v, want nil — the harvest already succeeded", err)
	}
	if recovered != 0 {
		t.Errorf("recovered = %d, want 0", recovered)
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

	recovered, err := recoverConcurrentEntries(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1 — the fixture is not exercising recovery", recovered)
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
