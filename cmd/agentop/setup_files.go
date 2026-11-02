package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pathMarker is install.sh's PATH_MARKER, byte for byte: setup and the shell
// installer must each recognise the other's block, in either order.
const pathMarker = "# added by Cortex (rossoctl/cortex) — delete these two lines to undo"

// pathBlock is exactly what install.sh's offer_path_setup appends, leading newline
// included even for an empty file. $PATH stays literal: the shell expands it at
// startup.
func pathBlock(binDir string) string {
	return "\n" + pathMarker + "\n" + `export PATH="` + binDir + `:$PATH"` + "\n"
}

// pathProfile is the file install.sh would edit for this shell, or "" where it
// would edit none (fish, sh, an empty $SHELL): those get advice, not an edit.
func pathProfile(shell, home string) string {
	switch filepath.Base(shell) {
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "bash":
		for _, name := range []string{".bash_profile", ".bashrc", ".profile"} {
			if p := filepath.Join(home, name); fileExists(p) {
				return p
			}
		}
		return filepath.Join(home, ".bash_profile")
	}
	return ""
}

func onPath(pathEnv, dir string) bool {
	return strings.Contains(":"+pathEnv+":", ":"+dir+":")
}

func hasPathMarker(profile string) bool {
	b, err := os.ReadFile(profile) //nolint:gosec // the user's own shell profile
	return err == nil && bytes.Contains(b, []byte(pathMarker))
}

// fileSnapshot is a path's state before setup touched it, so an undo can put back
// exactly that: the same bytes and mode, the same symlink, or no file at all.
type fileSnapshot struct {
	path    string
	existed bool
	data    []byte
	mode    os.FileMode
	link    string // set when path was a symlink; data and mode then carry the target's bytes, when it had any
}

// snapshotFile returns the zero snapshot with any error, never a partly filled
// one: restoring existed-but-unread state would truncate the file.
func snapshotFile(path string) (fileSnapshot, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{path: path}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(path)
		if err != nil {
			return fileSnapshot{}, err
		}
		s := fileSnapshot{path: path, existed: true, link: link}
		// Writes follow the link since #1565, so the link alone is no longer
		// the whole state: capture the target's bytes and mode too, or undo
		// cannot put them back. A dangling link has nothing to capture.
		if tfi, err := os.Stat(path); err == nil && tfi.Mode().IsRegular() { //nolint:gosec // a path setup is about to change
			data, err := os.ReadFile(path)
			if err != nil {
				return fileSnapshot{}, err
			}
			s.data, s.mode = data, tfi.Mode().Perm()
		}
		return s, nil
	}
	if !fi.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // a path setup is about to change
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{path: path, existed: true, data: data, mode: fi.Mode().Perm()}, nil
}

func (s fileSnapshot) restore() error {
	switch {
	case !s.existed:
		return removeIfExists(s.path)
	case s.link != "":
		// Re-assert the link first — the write, or another tool, may have
		// replaced it with a regular file — then put the target's bytes back
		// through it.
		if err := symlinkAtomic(s.link, s.path); err != nil {
			return err
		}
		if s.data == nil {
			return nil
		}
		return writeFileAtomic(s.path, s.data, s.mode)
	default:
		return writeFileAtomic(s.path, s.data, s.mode)
	}
}

// symlinkAtomic points path at target by renaming a fresh link from a temp
// sibling over it, so path is never absent in between as it would be with a
// remove and then a link.
func symlinkAtomic(target, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".agentop-setup-link-" + rand.Text()
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeFileAtomic writes data to path through a temp file and a rename, so a
// crash mid-write leaves the previous contents intact (#1565: os.WriteFile
// truncates first, and a kill in that window used to leave the ownership
// record torn — after which readState refused it and every later enable was
// blocked). A SYMLINKED path is resolved first, so the write lands at the
// link's target and the link survives: a ~/.claude/settings.json managed into
// a dotfiles repo keeps tracking (renaming over a symlink replaces the link).
// A DANGLING link is replaced rather than followed — EvalSymlinks cannot
// resolve it — and setup's snapshot/undo restores the link on rollback.
// The temp file is CreateTemp'd beside the target — never a fixed name, which
// an attacker-planted symlink at path+".tmp" would redirect.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	dir := filepath.Dir(path)
	// 0700, not 0755: these are the user's private dotdirs (~/.cortex, ~/.claude,
	// ~/.codex), which hold settings, logs and pidfiles, and this may be the
	// write that first creates one.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".agentop-setup-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	// Flush before the rename, so a crash cannot leave path renamed to an empty
	// file on filesystems that reorder the two.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// backupOnce writes path's current contents to path+".bak" only if no backup
// exists yet. Overwriting on every call meant a second enable, or an
// enable/disable pair, replaced the pristine pre-Cortex file with one already
// edited — losing the only copy of settings the user actually wrote, on files
// that commonly hold API tokens. A stale-but-original backup is worth more
// than a fresh one of our own output. cur is the bytes the caller already
// read; absent callers pass nil and are skipped. The backup is itself written
// atomically and behind an Lstat: a torn .bak would corrupt the pristine copy
// forever (write-once means never rewritten), and os.Stat FOLLOWS a link, so a
// planted symlink at the .bak name pointing nowhere used to pass the
// IsNotExist check and steer the backup's contents to a chosen path.
func backupOnce(path string, cur []byte) error {
	if cur == nil {
		return nil
	}
	bak := path + ".bak"
	if _, serr := os.Lstat(bak); os.IsNotExist(serr) {
		if werr := writeFileAtomic(bak, cur, 0o600); werr != nil {
			return fmt.Errorf("writing backup %s: %w", bak, werr)
		}
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src) //nolint:gosec // setup's own staged or installed files
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, b, mode)
}
