// Package project names the project an agent process works in: the git repo root of its cwd.
package project

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rossoctl/cortex/core/model"
)

// MaxLabelLen caps a label, in runes. Label keeps the TAIL, as a path's leaf is its distinct end.
const MaxLabelLen = 256

// maxDepth bounds the walk up from dir.
const maxDepth = 64

// Root returns the git repo root that holds dir. A linked worktree gives its main repo.
// A submodule or a bare common dir gives the level that holds the .git file. No repo, or a
// repo root equal to home (a dotfiles repo), gives dir itself. A dir of "/" gives "".
func Root(dir, home string) string {
	dir = filepath.Clean(dir)
	if dir == "/" {
		return ""
	}
	root := dir
	for level, i := dir, 0; i < maxDepth; i++ {
		fi, err := os.Lstat(filepath.Join(level, ".git"))
		if err == nil {
			root = level
			if fi.Mode().IsRegular() {
				if main := mainRepo(level); main != "" {
					root = main
				}
			}
			break
		}
		parent := filepath.Dir(level)
		if parent == level {
			break
		}
		level = parent
	}
	if home != "" && root == filepath.Clean(home) {
		return dir
	}
	return root
}

// mainRepo reads the .git file at level and returns the main repo of a linked worktree, or "".
func mainRepo(level string) string {
	gitdir := firstLine(filepath.Join(level, ".git"))
	if !strings.HasPrefix(gitdir, "gitdir: ") {
		return ""
	}
	x := strings.TrimSpace(strings.TrimPrefix(gitdir, "gitdir: "))
	if !filepath.IsAbs(x) {
		x = filepath.Join(level, x)
	}
	common := firstLine(filepath.Join(x, "commondir"))
	if common == "" {
		return "" // a submodule: no commondir
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(x, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return "" // a bare common dir has no work tree to name
	}
	return filepath.Dir(common)
}

// firstLine reads only a regular file. O_NONBLOCK keeps open(2) from blocking on a FIFO, and
// the fstat check closes the gap between a Lstat and the open.
func firstLine(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	s := bufio.NewScanner(f)
	s.Scan()
	return strings.TrimSpace(s.Text())
}

// Label turns a repo root into a display label: the home prefix becomes "~", control runes
// are replaced, and the result is capped at MaxLabelLen runes. A non-empty label starts
// with "/" or "~".
func Label(root, home string) string {
	if root == "" {
		return ""
	}
	switch {
	case home != "" && root == home:
		root = "~"
	case home != "" && strings.HasPrefix(root, home+"/"):
		root = "~" + root[len(home):]
	}
	return clip(model.SanitizeControlRunes(root))
}

// clip keeps the last MaxLabelLen runes, then drops the runes before the first "/" so the
// label starts at a path element. A tail with no "/" becomes "/" and the last MaxLabelLen-1 runes.
func clip(s string) string {
	r := []rune(s)
	if len(r) <= MaxLabelLen {
		return s
	}
	tail := string(r[len(r)-MaxLabelLen:])
	if i := strings.IndexByte(tail, '/'); i >= 0 {
		return tail[i:]
	}
	return "/" + string(r[len(r)-MaxLabelLen+1:])
}
