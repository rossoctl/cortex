package edit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureLocalConfig mirrors the shape of ~/.cortex/config.yaml: comments the
// user cares about, top-level keys both before and after pipeline:, and a
// trailing key so FindPipelineRange has a real "next sibling" to stop at.
const fixtureLocalConfig = `# Built-in config for: authbridge-proxy --local
mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
stats:
  address: 127.0.0.1:47602
pipeline:
  outbound:
    - name: inference-parser
    - name: tool-prune
      config:
        on_error: observe
session:
  enabled: true
`

// fixtureLocalConfigPipelineLast is the shape the REAL ~/.cortex/config.yaml
// has: mode, listener, stats, tls_bridge, then pipeline: with nothing after it.
// That makes FindPipelineRange take its nextKeyLine == 0 branch (end =
// len(innerYAML)) — the branch production always takes, and the one
// fixtureLocalConfig never reaches because it has a trailing session: key.
const fixtureLocalConfigPipelineLast = `# Built-in config for: authbridge-proxy --local
mode: proxy-sidecar
stats:
  address: 127.0.0.1:47602
pipeline:
  outbound:
    - name: inference-parser
    - name: tool-prune
`

func writeFixture(t *testing.T, mode os.FileMode) string {
	t.Helper()
	return writeFixtureContent(t, fixtureLocalConfig, mode)
}

func writeFixtureContent(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// The real config ends at pipeline:, so the subtree runs to EOF. Exercised
// separately because every other test here has a key after pipeline: and so
// takes the other branch of FindPipelineRange.
func TestFileStore_PipelineLastRunsToEndOfFile(t *testing.T) {
	path := writeFixtureContent(t, fixtureLocalConfigPipelineLast, 0o600)
	s := FileStore{Path: path}
	ctx := context.Background()

	fp, err := s.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fp.PipelineEnd != len(fp.InnerYAML) {
		t.Errorf("PipelineEnd = %d, want %d (end of file)", fp.PipelineEnd, len(fp.InnerYAML))
	}
	subtree := string(fp.InnerYAML[fp.PipelineStart:fp.PipelineEnd])
	if !strings.HasPrefix(subtree, "pipeline:\n") || !strings.Contains(subtree, "tool-prune") {
		t.Errorf("subtree wrong:\n%s", subtree)
	}

	// And a round trip still keeps everything before it intact.
	newInner := Splice(fp.InnerYAML, fp.PipelineStart, fp.PipelineEnd,
		[]byte("pipeline:\n  outbound:\n    - name: mcp-parser\n"))
	payload, err := s.Build(fp, newInner)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := s.Apply(ctx, payload); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := "# Built-in config for: authbridge-proxy --local\n" +
		"mode: proxy-sidecar\n" +
		"stats:\n  address: 127.0.0.1:47602\n" +
		"pipeline:\n  outbound:\n    - name: mcp-parser\n"
	if string(got) != want {
		t.Errorf("round trip =\n%q\nwant\n%q", got, want)
	}
}

// A symlinked config — the shape you get pointing ~/.cortex/config.yaml at a
// dotfiles repo — must be written THROUGH, not over.
//
// os.Rename replaces the link itself, so without resolving, the live config
// becomes a regular file while the tracked copy silently keeps the old
// pipeline, with nothing in `git status` to reveal it. The temp also has to be
// a sibling of the real file: a rename across filesystems fails EXDEV, and a
// link can point anywhere.
func TestFileStore_WritesThroughASymlink(t *testing.T) {
	linkDir := t.TempDir()
	realDir := t.TempDir() // a separate directory, so an unresolved temp would be wrong
	real := filepath.Join(realDir, "real-config.yaml")
	if err := os.WriteFile(real, []byte(fixtureLocalConfigPipelineLast), 0o640); err != nil {
		t.Fatalf("write real: %v", err)
	}
	link := filepath.Join(linkDir, "config.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s := FileStore{Path: link}
	ctx := context.Background()
	fp, err := s.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	newInner := Splice(fp.InnerYAML, fp.PipelineStart, fp.PipelineEnd,
		[]byte("pipeline:\n  outbound:\n    - name: mcp-parser\n"))
	payload, err := s.Build(fp, newInner)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := s.Apply(ctx, payload); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The link is still a link.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("Apply replaced the symlink with a regular file")
	}
	// The real file got the edit.
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("read real: %v", err)
	}
	if !strings.Contains(string(got), "mcp-parser") {
		t.Errorf("the target did not receive the edit:\n%s", got)
	}
	// Mode came off the resolved file, not the link.
	st, err := os.Stat(real)
	if err != nil {
		t.Fatalf("stat real: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o640 {
		t.Errorf("mode = %v, want 0640 inherited from the resolved file", perm)
	}
	// No temp left behind in either directory.
	for _, d := range []string{linkDir, realDir} {
		ents, rerr := os.ReadDir(d)
		if rerr != nil {
			t.Fatalf("readdir %s: %v", d, rerr)
		}
		if len(ents) != 1 {
			var names []string
			for _, e := range ents {
				names = append(names, e.Name())
			}
			t.Errorf("%s holds %v, want one entry — a temp leaked", d, names)
		}
	}
}

// The file IS the runtime YAML, with no ConfigMap wrapper to unpick — so
// InnerYAML must be the file verbatim and the range must span exactly the
// pipeline subtree, stopping before the next top-level key.
func TestFileStore_FetchLocatesPipelineRange(t *testing.T) {
	path := writeFixture(t, 0o600)
	fp, err := FileStore{Path: path}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(fp.InnerYAML, []byte(fixtureLocalConfig)) {
		t.Error("InnerYAML is not the file verbatim")
	}
	subtree := string(fp.InnerYAML[fp.PipelineStart:fp.PipelineEnd])
	if !strings.HasPrefix(subtree, "pipeline:\n") {
		t.Errorf("subtree does not start at the pipeline key:\n%s", subtree)
	}
	if strings.Contains(subtree, "session:") {
		t.Errorf("subtree ran past pipeline into the next top-level key:\n%s", subtree)
	}
	if !strings.Contains(subtree, "tool-prune") {
		t.Errorf("subtree is missing the pipeline body:\n%s", subtree)
	}
}

// Build is identity for a file: there is no outer document to re-emit, which
// is the whole reason a file store is simpler than the ConfigMap one. If this
// ever grows a yaml round-trip it would reflow the user's comments.
func TestFileStore_BuildIsIdentity(t *testing.T) {
	path := writeFixture(t, 0o600)
	s := FileStore{Path: path}
	fp, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	newInner := []byte("whatever the caller spliced")
	got, err := s.Build(fp, newInner)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !bytes.Equal(got, newInner) {
		t.Errorf("Build returned %q, want the spliced inner unchanged", got)
	}
}

// The editor's contract is that everything outside the pipeline subtree
// survives byte-for-byte — comments, key order, the blank-line layout. A
// yaml.Marshal round-trip would quietly destroy all three.
func TestFileStore_RoundTripPreservesEverythingOutsidePipeline(t *testing.T) {
	path := writeFixture(t, 0o600)
	s := FileStore{Path: path}
	ctx := context.Background()

	fp, err := s.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	newSubtree := []byte("pipeline:\n  outbound:\n    - name: mcp-parser\n")
	newInner := Splice(fp.InnerYAML, fp.PipelineStart, fp.PipelineEnd, newSubtree)
	payload, err := s.Build(fp, newInner)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := s.Apply(ctx, payload); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(after)
	for _, keep := range []string{
		"# Built-in config for: authbridge-proxy --local",
		"mode: proxy-sidecar",
		"forward_proxy_addr: 127.0.0.1:47600",
		"address: 127.0.0.1:47602",
		"session:\n  enabled: true\n",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("round trip lost %q:\n%s", keep, got)
		}
	}
	if !strings.Contains(got, "mcp-parser") {
		t.Errorf("round trip did not apply the edit:\n%s", got)
	}
	if strings.Contains(got, "inference-parser") {
		t.Errorf("round trip left the old pipeline behind:\n%s", got)
	}
}

// Apply must land as one rename, not a truncate-then-write: the proxy is
// watching this exact path with fsnotify, and a partial file is a parse error
// that increments reloads_failed for an edit the user never made.
//
// Asserted through the observable consequences of write-then-rename — the
// original mode survives (a fresh CreateTemp would be 0600 by luck and 0644
// under a different umask) and no temp file is left in the directory.
func TestFileStore_ApplyIsAtomicAndKeepsMode(t *testing.T) {
	path := writeFixture(t, 0o600)
	dir := filepath.Dir(path)
	s := FileStore{Path: path}

	if _, err := s.Apply(context.Background(), []byte("pipeline: {}\n")); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %v, want 0600 — Apply must not widen permissions on the user's config", got)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "config.yaml" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only config.yaml — a temp file leaked", names)
	}
}

// PollUntilReloaded compares last_success against this timestamp with
// sub-second precision, so an applyTime captured after the reloader already
// ran would never be beaten and the poll would time out on a reload that
// actually succeeded.
func TestFileStore_ApplyTimePrecedesTheWrite(t *testing.T) {
	path := writeFixture(t, 0o600)
	before := time.Now()
	at, err := FileStore{Path: path}.Apply(context.Background(), []byte("pipeline: {}\n"))
	after := time.Now()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if at.Before(before) || at.After(after) {
		t.Errorf("applyTime %v outside [%v, %v]", at, before, after)
	}
}

// Rollback re-Applies the bytes Fetch returned, so that path is only safe if
// it restores the file exactly. Anything less and a failed reload leaves the
// user's config subtly rewritten.
func TestFileStore_ApplyingTheFetchedBytesRestoresTheFile(t *testing.T) {
	path := writeFixture(t, 0o600)
	s := FileStore{Path: path}
	ctx := context.Background()

	fp, err := s.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := s.Apply(ctx, []byte("pipeline: {}\n")); err != nil {
		t.Fatalf("Apply edit: %v", err)
	}
	if _, err := s.Apply(ctx, fp.InnerYAML); err != nil {
		t.Fatalf("Apply rollback: %v", err)
	}

	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(back, []byte(fixtureLocalConfig)) {
		t.Errorf("rollback did not restore the file byte-for-byte:\n%s", back)
	}
}

// The operator sits in $EDITOR while something else writes the same file —
// `abctl tools scan --write`, `abctl service install`'s config migration, a
// second session. Apply
// renames a whole file built from the Fetch-time bytes, so without a check that
// write vanishes silently, including the parts outside the pipeline subtree.
func TestFileStore_ApplyRefusesAConcurrentWrite(t *testing.T) {
	path := writeFixture(t, 0o600)
	s := FileStore{Path: path}
	ctx := context.Background()

	fp, err := s.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := s.CheckUnchanged(ctx, fp); err != nil {
		t.Fatalf("CheckUnchanged on an untouched file: %v", err)
	}

	// Somebody else writes while $EDITOR is open.
	sneaky := fixtureLocalConfig + "\n# added by another writer\n"
	if err := os.WriteFile(path, []byte(sneaky), 0o600); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	err = s.CheckUnchanged(ctx, fp)
	if err == nil {
		t.Fatal("want a conflict error after a concurrent write")
	}
	if !strings.Contains(err.Error(), "changed since the edit began") {
		t.Errorf("error %q should say the file moved under the edit", err)
	}

	// ApplyCmd must surface it and leave the other writer's bytes in place.
	msg := ApplyCmd(ctx, s, fp, []byte("pipeline: {}\n"))().(AppliedMsg)
	if msg.Err == nil {
		t.Fatal("ApplyCmd should refuse when the store reports a conflict")
	}
	if !msg.ApplyTime.IsZero() {
		t.Error("a refused apply must not report an apply time")
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read back: %v", rerr)
	}
	if string(after) != sneaky {
		t.Errorf("the concurrent write was clobbered:\n%s", after)
	}
}

// ConfigMapStore deliberately does NOT implement ConflictChecker: it applies
// with --force-conflicts=true and takes field-manager ownership, which is what
// makes editing an operator-owned ConfigMap work at all. Pinned so the
// asymmetry stays a decision.
func TestConflictChecker_OnlyTheFileStoreImplementsIt(t *testing.T) {
	if _, ok := any(FileStore{Path: "x"}).(ConflictChecker); !ok {
		t.Error("FileStore should implement ConflictChecker")
	}
	if _, ok := any(ConfigMapStore{}).(ConflictChecker); ok {
		t.Error("ConfigMapStore should not implement ConflictChecker — it force-conflicts on purpose")
	}
}

func TestFileStore_FetchErrors(t *testing.T) {
	dir := t.TempDir()

	noPipeline := filepath.Join(dir, "no-pipeline.yaml")
	if err := os.WriteFile(noPipeline, []byte("mode: proxy-sidecar\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"missing file", filepath.Join(dir, "absent.yaml"), "absent.yaml"},
		{"no pipeline key", noPipeline, "pipeline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FileStore{Path: tc.path}.Fetch(context.Background())
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
