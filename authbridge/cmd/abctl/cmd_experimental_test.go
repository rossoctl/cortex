package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// writeSessionTranscript writes one transcript with caller-supplied lines.
//
// A distinct name from cmd_tools_test.go's writeTranscript, which cannot be reused:
// its signature takes a withCall bool and always emits a tool_use line, so it cannot
// express an ai-title or a cwd-only session. Variadic instead, after
// toolscan/scan_test.go's helper of the same shape.
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

// readMetadataFile decodes what the command wrote.
func readMetadataFile(t *testing.T, path string) map[string]tui.SessionMetadata {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var got map[string]tui.SessionMetadata
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return got
}

// Usage errors, and the stdout/stderr split they turn on: an explicit --help is a
// successful answer (stdout, 0); an incomplete or wrong invocation is an error
// (stderr, 2). A script branches on the code, so the two must not blur.
func TestExperimental_UsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no action", nil},
		{"unknown action", []string{"frobnicate"}},
		{"unknown flag", []string{"read-claude-sessions", "--nosuchflag"}},
		// --dir consumes "--help" as its VALUE, so scanning argv for it cannot tell a
		// help request from a parse failure. Keying off Parse's ErrHelp can: this must
		// not put the usage on stdout while the error goes to stderr.
		{"help eaten as a flag value", []string{"read-claude-sessions", "--dir", "--help", "--nosuchflag"}},
		{"stray positional", []string{"read-claude-sessions", "/tmp/somewhere"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runExperimental(tc.args, &out, &errb); code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if errb.Len() == 0 {
				t.Error("nothing on stderr")
			}
			if out.Len() != 0 {
				t.Errorf("stdout not empty: %q", out.String())
			}
		})
	}
}

// An unknown action must name the valid set — the difference between a refusal and a
// dead end — and quote the input, so a name carrying a stray shell character is
// legible.
func TestExperimental_UnknownActionNamesTheValidSet(t *testing.T) {
	var out, errb bytes.Buffer
	runExperimental([]string{"frobnicate"}, &out, &errb)
	got := errb.String()
	if !strings.Contains(got, `"frobnicate"`) {
		t.Errorf("stderr does not quote the input: %q", got)
	}
	if !strings.Contains(got, "read-claude-sessions") {
		t.Errorf("stderr omits the valid action: %q", got)
	}
}

// Both levels of --help answer on stdout at exit 0. All three spellings are pinned:
// -h is what a habit produces, `help` what someone copies from another tool.
func TestExperimental_HelpOnStdout(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run("experimental "+arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runExperimental([]string{arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "abctl experimental —") {
				t.Errorf("usage not on stdout:\n%s", out.String())
			}
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
	// The action's own help, which is where the detail lives.
	for _, arg := range []string{"-h", "--help"} {
		t.Run("read-claude-sessions "+arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runExperimental([]string{"read-claude-sessions", arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "read-claude-sessions —") {
				t.Errorf("usage not on stdout:\n%s", out.String())
			}
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
}

// The two title mechanisms, and the fallback between them. The fallback is not an
// edge case: measured against a real ~/.claude, 107 of 109 sessions had no ai-title,
// so the cwd path is the one most entries take.
func TestReadClaudeSessions_TitlePrefersAiTitleThenCWD(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-Users-someone-src-thing")

	writeSessionTranscript(t, proj, "aaaaaaaa-0000-0000-0000-000000000001.jsonl",
		`{"type":"user","cwd":"/Users/someone/src/thing"}`,
		`{"type":"ai-title","aiTitle":"First title"}`,
		// Last ai-title wins: a session can be re-titled, and the newest claim is
		// the current one.
		`{"type":"ai-title","aiTitle":"Refined title"}`,
	)
	writeSessionTranscript(t, proj, "aaaaaaaa-0000-0000-0000-000000000002.jsonl",
		`{"type":"user","cwd":"/Users/someone/src/other"}`,
		// Later cwd wins too.
		`{"type":"user","cwd":"/Users/someone/src/moved"}`,
	)
	writeSessionTranscript(t, proj, "aaaaaaaa-0000-0000-0000-000000000003.jsonl",
		`{"type":"user","message":{"role":"user"}}`,
	)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}

	got := readMetadataFile(t, filepath.Join(home, tui.SessionMetadataRel))
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(got), got)
	}
	if w := got["aaaaaaaa-0000-0000-0000-000000000001"].Title; w != "Refined title" {
		t.Errorf("ai-title session title = %q, want the LAST ai-title", w)
	}
	if w := got["aaaaaaaa-0000-0000-0000-000000000002"].Title; w != "/Users/someone/src/moved" {
		t.Errorf("untitled session title = %q, want the LAST cwd", w)
	}
	// Neither title nor cwd: the entry still exists, because the session did.
	third := got["aaaaaaaa-0000-0000-0000-000000000003"]
	if third.Title != "" {
		t.Errorf("title = %q, want empty when the transcript has neither", third.Title)
	}
	if third.AgentType != "Claude Code" {
		t.Errorf("agentType = %q, want %q even with no title", third.AgentType, "Claude Code")
	}
}

// The three non-title fields, which a consumer needs to trace an entry back.
func TestReadClaudeSessions_PopulatesAgentFields(t *testing.T) {
	prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")
	writeSessionTranscript(t, proj, "sid-1.jsonl", `{"type":"user","cwd":"/w"}`)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	path, err := tui.SessionMetadataPath()
	if err != nil {
		t.Fatal(err)
	}
	e := readMetadataFile(t, path)["sid-1"]
	if e.AgentType != "Claude Code" {
		t.Errorf("agentType = %q", e.AgentType)
	}
	// The config dir the command used — the parent of projects, not projects itself.
	if e.AgentConfigDir != cfg {
		t.Errorf("agentConfigDir = %q, want %q", e.AgentConfigDir, cfg)
	}
	if want := filepath.Join(proj, "sid-1.jsonl"); e.LogFile != want {
		t.Errorf("logFile = %q, want %q", e.LogFile, want)
	}
}

// Subagent transcripts live one level deeper, at <session-id>/subagents/agent-*.jsonl,
// and their basenames are agent ids rather than session ids. Reading them would key
// non-sessions into the map — 92 of them on the machine this was written against — so
// the harvest is deliberately one level deep. This is the test that pins that.
func TestReadClaudeSessions_IgnoresSubagentTranscripts(t *testing.T) {
	prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")

	writeSessionTranscript(t, proj, "real-session.jsonl", `{"type":"user","cwd":"/w"}`)
	writeSessionTranscript(t, filepath.Join(proj, "real-session", "subagents"),
		"agent-deadbeef.jsonl", `{"type":"user","cwd":"/w"}`)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	path, err := tui.SessionMetadataPath()
	if err != nil {
		t.Fatal(err)
	}
	got := readMetadataFile(t, path)
	if _, ok := got["agent-deadbeef"]; ok {
		t.Error("a subagent transcript was keyed as a session")
	}
	if len(got) != 1 {
		t.Errorf("got %d entries, want only the real session: %v", len(got), got)
	}
}

// A transcript line far past bufio.Scanner's 64KB default must not stop the scan: a
// Scanner over its limit stops SILENTLY, so without a raised buffer the title of
// exactly the longest sessions would vanish. The largest line measured on a real
// ~/.claude was 1.36MB.
func TestReadClaudeSessions_HandlesALineOverTheScannerDefault(t *testing.T) {
	prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")

	huge := `{"type":"user","text":"` + strings.Repeat("x", 200*1024) + `"}`
	writeSessionTranscript(t, proj, "big.jsonl",
		`{"type":"user","cwd":"/w"}`,
		huge,
		// The title is AFTER the huge line, so it is only reachable if the scan got
		// past it.
		`{"type":"ai-title","aiTitle":"Found past the big line"}`,
	)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	path, err := tui.SessionMetadataPath()
	if err != nil {
		t.Fatal(err)
	}
	if got := readMetadataFile(t, path)["big"].Title; got != "Found past the big line" {
		t.Errorf("title = %q — the scan stopped at the oversized line", got)
	}
}

// A malformed line is skipped, not fatal: one bad line must not cost the other names
// in the file, nor the rest of the harvest.
func TestReadClaudeSessions_SkipsMalformedLines(t *testing.T) {
	prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")
	writeSessionTranscript(t, proj, "s.jsonl",
		`not json at all`,
		`{"type":"user","cwd":"/w"}`,
		`{"broken":`,
		`{"type":"ai-title","aiTitle":"Survived"}`,
	)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	path, err := tui.SessionMetadataPath()
	if err != nil {
		t.Fatal(err)
	}
	if got := readMetadataFile(t, path)["s"].Title; got != "Survived" {
		t.Errorf("title = %q, want the title after the malformed lines", got)
	}
}

// A config dir with no projects/ is not an error: a machine that has never run Claude
// Code has none, and exiting 1 would make a first run look broken. The file is still
// written, so a consumer always has something to read.
func TestReadClaudeSessions_MissingProjectsDirIsNotAnError(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "no-claude-here")

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	// Said out loud, because a wrong --dir is otherwise indistinguishable from an
	// empty one.
	if !strings.Contains(out.String(), "No transcripts") {
		t.Errorf("stdout does not say the directory was empty:\n%s", out.String())
	}
	got := readMetadataFile(t, filepath.Join(home, tui.SessionMetadataRel))
	if len(got) != 0 {
		t.Errorf("got %d entries, want none: %v", len(got), got)
	}
}

// --merge (the default) upserts, so an entry the file already held survives a harvest
// that no longer sees it — a second config dir, or a session Claude Code has pruned.
// --merge=false is the explicit rebuild.
func TestReadClaudeSessions_MergeUpsertsAndFalseRebuilds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantStale bool
	}{
		{"default merges", nil, true},
		{"--merge=true merges", []string{"--merge=true"}, true},
		{"--merge=false rebuilds", []string{"--merge=false"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := prefsHome(t)
			path := filepath.Join(home, tui.SessionMetadataRel)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path,
				[]byte(`{"stale-session":{"title":"from an earlier run"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg := filepath.Join(t.TempDir(), "claude")
			writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "fresh.jsonl",
				`{"type":"user","cwd":"/w"}`)

			args := append([]string{"read-claude-sessions", "--dir", cfg}, tc.args...)
			var out, errb bytes.Buffer
			if code := runExperimental(args, &out, &errb); code != 0 {
				t.Fatalf("exit = %d: %s", code, errb.String())
			}

			got := readMetadataFile(t, path)
			if _, ok := got["fresh"]; !ok {
				t.Errorf("the harvested session is missing: %v", got)
			}
			if _, ok := got["stale-session"]; ok != tc.wantStale {
				t.Errorf("stale entry present = %v, want %v", ok, tc.wantStale)
			}
		})
	}
}

// A harvest wins over the file for a key both hold: it just read the transcript, so
// its title is the current one.
func TestReadClaudeSessions_MergePrefersTheFreshHarvest(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, tui.SessionMetadataRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"s":{"title":"stale title"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s.jsonl",
		`{"type":"ai-title","aiTitle":"current title"}`)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	if got := readMetadataFile(t, path)["s"].Title; got != "current title" {
		t.Errorf("title = %q, want the freshly harvested one", got)
	}
}

// A corrupt existing file must not be read as empty under --merge: that would rebuild
// from scratch under the flag whose purpose is not losing entries. Refuse, and name
// the way past it.
func TestReadClaudeSessions_MergeRefusesACorruptFile(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, tui.SessionMetadataRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "fresh.jsonl",
		`{"type":"user","cwd":"/w"}`)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "--merge=false") {
		t.Errorf("stderr does not name the way past it: %q", errb.String())
	}
	// The bad file is left alone rather than overwritten, so it can still be repaired.
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "{not json" {
		t.Errorf("the corrupt file was modified: %q, %v", b, err)
	}

	// --merge=false is that way past it.
	var out2, errb2 bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg, "--merge=false"}, &out2, &errb2); code != 0 {
		t.Fatalf("--merge=false exit = %d: %s", code, errb2.String())
	}
	if _, ok := readMetadataFile(t, path)["fresh"]; !ok {
		t.Error("--merge=false did not rebuild the file")
	}
}

// CLAUDE_CONFIG_DIR is honoured when --dir is absent, and --dir wins over it.
func TestReadClaudeSessions_ConfigDirResolution(t *testing.T) {
	t.Run("env var is used", func(t *testing.T) {
		prefsHome(t)
		cfg := filepath.Join(t.TempDir(), "moved-claude")
		writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "from-env.jsonl",
			`{"type":"user","cwd":"/w"}`)
		t.Setenv("CLAUDE_CONFIG_DIR", cfg)

		var out, errb bytes.Buffer
		if code := runExperimental([]string{"read-claude-sessions"}, &out, &errb); code != 0 {
			t.Fatalf("exit = %d: %s", code, errb.String())
		}
		path, err := tui.SessionMetadataPath()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := readMetadataFile(t, path)["from-env"]; !ok {
			t.Error("CLAUDE_CONFIG_DIR was not consulted")
		}
	})

	t.Run("--dir overrides the env var", func(t *testing.T) {
		prefsHome(t)
		envCfg := filepath.Join(t.TempDir(), "env-claude")
		writeSessionTranscript(t, filepath.Join(envCfg, "projects", "-p"), "from-env.jsonl",
			`{"type":"user","cwd":"/w"}`)
		flagCfg := filepath.Join(t.TempDir(), "flag-claude")
		writeSessionTranscript(t, filepath.Join(flagCfg, "projects", "-p"), "from-flag.jsonl",
			`{"type":"user","cwd":"/w"}`)
		t.Setenv("CLAUDE_CONFIG_DIR", envCfg)

		var out, errb bytes.Buffer
		if code := runExperimental([]string{"read-claude-sessions", "--dir", flagCfg}, &out, &errb); code != 0 {
			t.Fatalf("exit = %d: %s", code, errb.String())
		}
		path, err := tui.SessionMetadataPath()
		if err != nil {
			t.Fatal(err)
		}
		got := readMetadataFile(t, path)
		if _, ok := got["from-flag"]; !ok {
			t.Error("--dir was not used")
		}
		if _, ok := got["from-env"]; ok {
			t.Error("the env var was read even though --dir was given")
		}
	})
}

// The file holds paths into the user's own tree, so it gets the same modes as the
// rest of ~/.cortex: 0700 on the directory, 0600 on the file.
func TestReadClaudeSessions_FileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s.jsonl",
		`{"type":"user","cwd":"/w"}`)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	path := filepath.Join(home, tui.SessionMetadataRel)
	dfi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dfi.Mode().Perm(); got != 0o700 {
		t.Errorf("~/.cortex mode = %v, want 0700", got)
	}
	ffi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ffi.Mode().Perm(); got != 0o600 {
		t.Errorf("session-metadata.json mode = %v, want 0600", got)
	}
}

// No tempfile debris on the success path: the write is CreateTemp + Rename, and a
// leftover .tmp would accumulate one per run.
func TestReadClaudeSessions_LeavesNoTempFile(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s.jsonl",
		`{"type":"user","cwd":"/w"}`)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	entries, err := os.ReadDir(filepath.Join(home, ".cortex"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("tempfile left behind: %s", e.Name())
		}
	}
}

// A symlinked project directory is harvested, not skipped.
//
// os.ReadDir reports Lstat semantics, so DirEntry.IsDir() is FALSE for a symlink pointing at
// a directory: the previous `if !e.IsDir() { continue }` dropped every session under one
// without a word. Relocating a project directory, or sharing one between two config trees,
// is a reasonable thing to do — and #1018 asks for multiple config directories, which is the
// same shape.
func TestReadClaudeSessions_FollowsSymlinkedProjectDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(filepath.Join(cfg, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The real directory lives outside the config tree; only a symlink is inside it.
	elsewhere := filepath.Join(t.TempDir(), "relocated")
	writeSessionTranscript(t, elsewhere, "bbbbbbbb-0000-0000-0000-000000000001.jsonl",
		`{"type":"ai-title","aiTitle":"Behind a symlink"}`,
	)
	if err := os.Symlink(elsewhere, filepath.Join(cfg, "projects", "-linked")); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	got := readMetadataFile(t, filepath.Join(home, tui.SessionMetadataRel))
	if w := got["bbbbbbbb-0000-0000-0000-000000000001"].Title; w != "Behind a symlink" {
		t.Errorf("title = %q, want %q — the symlinked project dir was skipped", w, "Behind a symlink")
	}
}

// One id in two project directories resolves to the NEWER transcript.
//
// ReadDir sorts by filename, so without an explicit rule the winner is whichever project
// directory sorts later — alphabetical order silently deciding which title is current. Here
// the newer transcript sits in the directory that sorts FIRST, so an implementation that just
// takes the last write loses.
func TestReadClaudeSessions_DuplicateIDPrefersTheNewerTranscript(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	const id = "cccccccc-0000-0000-0000-000000000001.jsonl"

	writeSessionTranscript(t, filepath.Join(cfg, "projects", "aaa-first"), id,
		`{"type":"ai-title","aiTitle":"NEWER"}`,
	)
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "zzz-last"), id,
		`{"type":"ai-title","aiTitle":"OLDER"}`,
	)
	// Make the alphabetically-first copy the newer one, so filename order and mtime order
	// disagree and only an mtime rule gets this right.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(cfg, "projects", "zzz-last", id), old, old); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	got := readMetadataFile(t, filepath.Join(home, tui.SessionMetadataRel))
	if w := got["cccccccc-0000-0000-0000-000000000001"].Title; w != "NEWER" {
		t.Errorf("title = %q, want %q — alphabetical order beat mtime", w, "NEWER")
	}
}

// A transcript with a line past the scanner buffer keeps its partial title AND is reported.
//
// Both halves matter. The title is best-effort by design, so the harvest must not fail — but
// last-wins means the name returned may be an OLD claim with newer ones unread, and the
// fallbacks make that indistinguishable from a session that simply has no title. Silence was
// the bug; the earlier code discarded sc.Err() outright.
func TestReadClaudeSessions_ReportsTruncatedTranscripts(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-huge")

	// 17MB single line: past the 16MB ceiling, so the scanner stops there and the later
	// title is never seen.
	writeSessionTranscript(t, proj, "dddddddd-0000-0000-0000-000000000001.jsonl",
		`{"type":"ai-title","aiTitle":"Seen before the wall"}`,
		`{"pad":"`+strings.Repeat("x", 17*1024*1024)+`"}`,
		`{"type":"ai-title","aiTitle":"NEVER READ"}`,
	)

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 — one bad transcript must not fail the harvest; stderr=%s",
			code, errb.String())
	}
	got := readMetadataFile(t, filepath.Join(home, tui.SessionMetadataRel))
	if w := got["dddddddd-0000-0000-0000-000000000001"].Title; w != "Seen before the wall" {
		t.Errorf("title = %q, want the partial title found before the oversized line", w)
	}
	if !strings.Contains(errb.String(), "read incompletely") {
		t.Errorf("stderr does not report the truncated read: %q", errb.String())
	}
}

// The --merge breakdown is printed whenever merging, including when nothing was kept.
//
// Gating the detailed line on "the totals differ" dropped it in exactly the case that needed
// it: when the harvest is a SUPERSET of the file, kept is 0 and the totals match, so a
// --merge run printed the same bare line as --merge=false.
func TestReadClaudeSessions_MergeSummaryWhenHarvestIsASuperset(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")
	writeSessionTranscript(t, proj, "eeeeeeee-0000-0000-0000-000000000001.jsonl",
		`{"type":"ai-title","aiTitle":"One"}`,
	)
	writeSessionTranscript(t, proj, "eeeeeeee-0000-0000-0000-000000000002.jsonl",
		`{"type":"ai-title","aiTitle":"Two"}`,
	)

	// Seed the file with a strict subset of what the harvest will find.
	path := filepath.Join(home, tui.SessionMetadataRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seed, err := json.Marshal(map[string]tui.SessionMetadata{
		"eeeeeeee-0000-0000-0000-000000000001": {Title: "stale"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "kept from the existing file") {
		t.Errorf("merge run printed no breakdown, so it reads identically to --merge=false: %q",
			out.String())
	}
	if !strings.Contains(out.String(), "0 kept") {
		t.Errorf("want an explicit 0 kept, got %q", out.String())
	}
}

// A failed write is reported and exits 1, rather than claiming success.
//
// The atomic-write path had no test at all: every failure branch removes the tempfile and
// returns, so a silent success on a failed write would have looked exactly like a working
// harvest with an empty result.
func TestReadClaudeSessions_ReportsWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block writes the same way on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"),
		"ffffffff-0000-0000-0000-000000000001.jsonl",
		`{"type":"ai-title","aiTitle":"Doomed"}`,
	)

	// ~/.cortex exists but cannot be written into, so CreateTemp fails inside it.
	dir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var out, errb bytes.Buffer
	if code := runExperimental([]string{"read-claude-sessions", "--dir", cfg}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1 on an unwritable metadata directory; stdout=%q stderr=%q",
			code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "writing") {
		t.Errorf("stderr does not name the write failure: %q", errb.String())
	}
	if strings.Contains(out.String(), "Wrote") {
		t.Errorf("stdout claims success after a failed write: %q", out.String())
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
	onDisk, err := json.Marshal(map[string]tui.SessionMetadata{
		theirs: {Title: "From another run", AgentType: "Claude Code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, onDisk, 0o600); err != nil {
		t.Fatal(err)
	}

	// In hand: what this run was about to save, which knows nothing of theirs.
	meta := map[string]tui.SessionMetadata{ours: {Title: "Ours"}}

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
	recovered, err := recoverConcurrentEntries(path, map[string]tui.SessionMetadata{"a": {}})
	if err != nil {
		t.Errorf("err = %v, want nil — the harvest already succeeded", err)
	}
	if recovered != 0 {
		t.Errorf("recovered = %d, want 0", recovered)
	}
}
