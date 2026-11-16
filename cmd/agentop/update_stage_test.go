package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type tarEntry struct {
	name string
	typ  byte
	body string
	link string
}

// tarGz builds a .tar.gz that holds entries in order.
func tarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o755}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer serves files at /<tag>/<name> (404 for anything else) and points
// releaseDownloadBase at it.
func releaseServer(t *testing.T, tag string, files map[string][]byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/"+tag+"/")]
		if !ok || !strings.HasPrefix(r.URL.Path, "/"+tag+"/") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	old := releaseDownloadBase
	releaseDownloadBase = srv.URL + "/"
	t.Cleanup(func() { releaseDownloadBase = old })
}

// sumsFor is a checksums.txt body for files, in the GNU "HASH  ./name" form.
func sumsFor(files map[string][]byte) []byte {
	var b strings.Builder
	for name, body := range files {
		sum := sha256.Sum256(body)
		fmt.Fprintf(&b, "%x  ./%s\n", sum, name)
	}
	return []byte(b.String())
}

// stageFixture is a two-tarball v1.2.3 release with right checksums.
func stageFixture(t *testing.T) (names []string, files map[string][]byte) {
	t.Helper()
	names = []string{"agentop_v1.2.3_darwin_arm64.tar.gz", "cortex_v1.2.3_darwin_arm64.tar.gz"}
	files = map[string][]byte{
		names[0]: tarGz(t, tarEntry{name: "agentop", typ: tar.TypeReg, body: "#!/bin/sh\necho agentop\n"}),
		names[1]: tarGz(t, tarEntry{name: "cortex", typ: tar.TypeReg, body: "#!/bin/sh\necho cortex\n"}),
	}
	return names, files
}

func withSums(files map[string][]byte) map[string][]byte {
	files["checksums.txt"] = sumsFor(files)
	return files
}

// emptyTmp sets TMPDIR to a new dir and returns it.
func emptyTmp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("%s holds %d entries, want none (first %s)", dir, len(ents), ents[0].Name())
	}
}

func TestReleaseTarballs(t *testing.T) {
	cases := []struct {
		name   string
		praxis bool
		want   []string
	}{
		{"default", false, []string{"agentop_v1.2.3_darwin_arm64.tar.gz", "cortex_v1.2.3_darwin_arm64.tar.gz"}},
		{"praxis", true, []string{"agentop_v1.2.3_darwin_arm64.tar.gz", "cortex_v1.2.3_darwin_arm64.tar.gz", "cortex-gateway_v1.2.3_darwin_arm64.tar.gz"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := releaseTarballs("v1.2.3", "darwin", "arm64", c.praxis)
			if !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestChecksumFor(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	upper := strings.ToUpper(hash)
	for _, line := range []string{upper + "  ./n", upper + "  n", upper + " *n"} {
		t.Run("accept "+line[64:], func(t *testing.T) {
			got, err := checksumFor([]byte("ffff  other\n"+line+"\n"), "n")
			if err != nil || got != hash {
				t.Errorf("got %q, %v; want %q", got, err, hash)
			}
		})
	}
	for _, line := range []string{hash + "  ../n", hash + "  /etc/n", hash + "  xn", hash[:63] + "  n"} {
		t.Run("refuse "+line[60:], func(t *testing.T) {
			_, err := checksumFor([]byte(line+"\n"), "n")
			if err == nil || !strings.Contains(err.Error(), "no usable entry for n") {
				t.Errorf("got %v, want no usable entry", err)
			}
		})
	}
	t.Run("no line", func(t *testing.T) {
		_, err := checksumFor(nil, "n")
		if err == nil || !strings.Contains(err.Error(), "no usable entry") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("twice", func(t *testing.T) {
		_, err := checksumFor([]byte(hash+"  n\n"+hash+"  ./n\n"), "n")
		if err == nil || !strings.Contains(err.Error(), "more than once") {
			t.Errorf("got %v", err)
		}
	})
}

func TestStageRelease(t *testing.T) {
	tmp := emptyTmp(t)
	names, files := stageFixture(t)
	releaseServer(t, "v1.2.3", withSums(files))
	var called []string
	stage, size, err := stageRelease(context.Background(), "v1.2.3", names, func(name string, digest []byte) error {
		sum := sha256.Sum256(files[name])
		if !bytes.Equal(digest, sum[:]) {
			t.Errorf("digest for %s = %x, want %x", name, digest, sum)
		}
		called = append(called, name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stage) })
	if !slices.Equal(called, names) {
		t.Errorf("check called for %v, want %v", called, names)
	}
	if filepath.Dir(stage) != filepath.Clean(tmp) {
		t.Errorf("stage %s is not under %s", stage, tmp)
	}
	if fi, err := os.Stat(stage); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("stage mode: %v, %v", fi, err)
	}
	for _, bin := range []string{"agentop", "cortex"} {
		fi, err := os.Lstat(filepath.Join(stage, bin))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: %v, %v", bin, fi, err)
		}
	}
	if want := int64(len(files[names[0]]) + len(files[names[1]])); size != want {
		t.Errorf("size = %d, want %d", size, want)
	}
}

func TestStageReleaseChecksumMismatch(t *testing.T) {
	tmp := emptyTmp(t)
	names, files := stageFixture(t)
	withSums(files)
	files["checksums.txt"] = bytes.Replace(files["checksums.txt"],
		[]byte(hex.EncodeToString(func() []byte { s := sha256.Sum256(files[names[1]]); return s[:] }())),
		[]byte(strings.Repeat("0", 64)), 1)
	releaseServer(t, "v1.2.3", files)
	stage, _, err := stageRelease(context.Background(), "v1.2.3", names, func(string, []byte) error {
		t.Error("check called after a checksum mismatch")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch for cortex_v1.2.3_darwin_arm64.tar.gz") || stage != "" {
		t.Errorf("got %q, %v", stage, err)
	}
	assertEmptyDir(t, tmp)
}

func TestStageReleaseCheckFails(t *testing.T) {
	tmp := emptyTmp(t)
	names, files := stageFixture(t)
	releaseServer(t, "v1.2.3", withSums(files))
	bad := errors.New("bad provenance")
	stage, _, err := stageRelease(context.Background(), "v1.2.3", names, func(name string, _ []byte) error {
		if name == names[1] {
			return bad
		}
		return nil
	})
	if !errors.Is(err, bad) || stage != "" {
		t.Errorf("got %q, %v", stage, err)
	}
	assertEmptyDir(t, tmp)
}

func TestStageReleaseMissingFile(t *testing.T) {
	tmp := emptyTmp(t)
	names, files := stageFixture(t)
	withSums(files)
	delete(files, names[1])
	releaseServer(t, "v1.2.3", files)
	_, _, err := stageRelease(context.Background(), "v1.2.3", names, func(string, []byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "download failed: "+names[1]) {
		t.Errorf("got %v", err)
	}
	assertEmptyDir(t, tmp)
}

func TestStageReleaseCancelled(t *testing.T) {
	tmp := emptyTmp(t)
	names, files := stageFixture(t)
	releaseServer(t, "v1.2.3", withSums(files))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stage, _, err := stageRelease(ctx, "v1.2.3", names, func(string, []byte) error { return nil })
	if err == nil || stage != "" {
		t.Errorf("got %q, %v", stage, err)
	}
	assertEmptyDir(t, tmp)
}

func TestStageReleaseMissingBinary(t *testing.T) {
	emptyTmp(t)
	names, files := stageFixture(t)
	files[names[0]] = tarGz(t, tarEntry{name: "other", typ: tar.TypeReg, body: "x"})
	releaseServer(t, "v1.2.3", withSums(files))
	_, _, err := stageRelease(context.Background(), "v1.2.3", names, func(string, []byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "archive did not contain expected binary: agentop") {
		t.Errorf("got %v", err)
	}
}

func TestExtractTarballRefusesUnsafeEntries(t *testing.T) {
	cases := map[string]tarEntry{
		"absolute": {name: "/abs", typ: tar.TypeReg, body: "x"},
		"dotdot":   {name: "../up", typ: tar.TypeReg, body: "x"},
		"deep up":  {name: "a/../../up", typ: tar.TypeReg, body: "x"},
		"symlink":  {name: "link", typ: tar.TypeSymlink, link: "/etc/passwd"},
		"hardlink": {name: "hard", typ: tar.TypeLink, link: "../up"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			outer := t.TempDir()
			dir := filepath.Join(outer, "stage")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(outer, "x.tar.gz")
			if err := os.WriteFile(path, tarGz(t, e), 0o600); err != nil {
				t.Fatal(err)
			}
			err := extractTarball(path, dir)
			if err == nil || !strings.Contains(err.Error(), "unsafe entry") {
				t.Errorf("got %v, want unsafe entry", err)
			}
			if _, err := os.Lstat(filepath.Join(outer, "up")); err == nil {
				t.Error("an entry landed outside dir")
			}
			assertEmptyDir(t, dir)
		})
	}
}

func TestStageReleaseRefusesALargeChecksums(t *testing.T) {
	tmp := emptyTmp(t)
	names, files := stageFixture(t)
	withSums(files)
	old := maxChecksumsBytes
	t.Cleanup(func() { maxChecksumsBytes = old })
	maxChecksumsBytes = int64(len(files["checksums.txt"])) - 1
	releaseServer(t, "v1.2.3", files)
	stage, _, err := stageRelease(context.Background(), "v1.2.3", names, func(string, []byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "checksums.txt is larger than") || stage != "" {
		t.Errorf("got %q, %v", stage, err)
	}
	assertEmptyDir(t, tmp)
}
