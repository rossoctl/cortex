package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func mkdirs(t *testing.T, base string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRoot(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, b string)
		dir   string // relative to the temp base; "/" means the filesystem root
		home  string // relative to the temp base; "" means no home
		want  string // relative to the temp base; "" means ""
	}{
		{"plain repo", func(t *testing.T, b string) { mkdirs(t, b, "r/.git") }, "r", "", "r"},
		{"subdirectory", func(t *testing.T, b string) { mkdirs(t, b, "r/.git", "r/a/b") }, "r/a/b", "", "r"},
		{"linked worktree", func(t *testing.T, b string) {
			mkdirs(t, b, "wt/sub")
			writeFile(t, filepath.Join(b, "main/.git/worktrees/w/commondir"), "../..\n")
			writeFile(t, filepath.Join(b, "wt/.git"), "gitdir: "+filepath.Join(b, "main/.git/worktrees/w")+"\n")
		}, "wt/sub", "", "main"},
		{"relative gitdir", func(t *testing.T, b string) {
			writeFile(t, filepath.Join(b, "main/.git/worktrees/w/commondir"), "../..\n")
			writeFile(t, filepath.Join(b, "wt/.git"), "gitdir: ../main/.git/worktrees/w\n")
		}, "wt", "", "main"},
		{"bare common dir", func(t *testing.T, b string) {
			writeFile(t, filepath.Join(b, "x/repo.git/worktrees/w/commondir"), "../..\n")
			writeFile(t, filepath.Join(b, "wt/.git"), "gitdir: "+filepath.Join(b, "x/repo.git/worktrees/w")+"\n")
		}, "wt", "", "wt"},
		{"submodule", func(t *testing.T, b string) {
			mkdirs(t, b, "sup/.git/modules/m")
			writeFile(t, filepath.Join(b, "sup/m/.git"), "gitdir: ../.git/modules/m\n")
		}, "sup/m", "", "sup/m"},
		{"no repo", func(t *testing.T, b string) { mkdirs(t, b, "n/a") }, "n/a", "", "n/a"},
		{"home-dir repo", func(t *testing.T, b string) { mkdirs(t, b, "home/.git", "home/x") }, "home/x", "home", "home/x"},
		{"cwd /", func(t *testing.T, b string) {}, "/", "", ""},
		{"cwd home no repo", func(t *testing.T, b string) { mkdirs(t, b, "home") }, "home", "home", "home"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := tempDir(t)
			c.build(t, b)
			abs := func(p string) string {
				if p == "" {
					return ""
				}
				return filepath.Join(b, p)
			}
			dir := abs(c.dir)
			if c.dir == "/" {
				dir = "/"
			}
			if got, want := Root(dir, abs(c.home)), abs(c.want); got != want {
				t.Fatalf("Root(%q) = %q; want %q", dir, got, want)
			}
		})
	}
}

func TestLabel(t *testing.T) {
	h := "/Users/alice"
	long := "/" + strings.Repeat("abc/", 75) // 301 runes; the kept tail starts mid-element
	oneElem := "/" + strings.Repeat("x", 300)
	cases := []struct {
		name, root, want string
	}{
		{"empty", "", ""},
		{"home", h, "~"},
		{"under home", h + "/p", "~/p"},
		{"outside home", "/opt/p", "/opt/p"},
		{"sibling prefix", h + "x", h + "x"},
		{"control rune", "/opt/a\x1bb", "/opt/a�b"},
	}
	for _, c := range cases {
		if got := Label(c.root, h); got != c.want {
			t.Errorf("%s: Label(%q) = %q; want %q", c.name, c.root, got, c.want)
		}
	}
	if got := Label(h+"/p", ""); got != h+"/p" {
		t.Errorf("no home: Label = %q", got)
	}

	got := Label(long, h)
	if n := utf8.RuneCountInString(got); n > MaxLabelLen {
		t.Errorf("long: %d runes", n)
	}
	if !strings.HasSuffix(long, got) || !strings.HasPrefix(got, "/") {
		t.Errorf("long: Label = %q; want a tail of the input that starts with /", got)
	}

	got = Label(oneElem, h)
	if utf8.RuneCountInString(got) != MaxLabelLen || !strings.HasPrefix(got, "/") || !strings.HasSuffix(oneElem, got[1:]) {
		t.Errorf("one element: Label = %q (%d runes)", got, utf8.RuneCountInString(got))
	}
}
