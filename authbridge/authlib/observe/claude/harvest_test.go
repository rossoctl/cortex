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
