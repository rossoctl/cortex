package script

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// file is one script a slot runs.
type file struct {
	// name is what the timeline, metrics and errors call it: the file name without
	// a leading order prefix or extension.
	name string
	path string
	hash string
}

// orderPrefix is the leading "10-" a file name uses to choose its position. It is
// not part of the plugin's name, so moving a file earlier keeps its identity.
var orderPrefix = regexp.MustCompile(`^[0-9]+-`)

// pluginName is base without its order prefix and extension: 10-mask.py is mask.
func pluginName(base string) string {
	n := orderPrefix.ReplaceAllString(base, "")
	if ext := filepath.Ext(n); ext != "" && ext != n {
		n = strings.TrimSuffix(n, ext)
	}
	return n
}

// ignored reports whether a directory entry is not a script: hidden and
// underscored names (where a shared helper or a switched-off script lives), editor
// leftovers, and anything that is not a regular executable file.
func ignored(e os.DirEntry, info os.FileInfo) bool {
	n := e.Name()
	switch {
	case strings.HasPrefix(n, "."), strings.HasPrefix(n, "_"):
		return true
	case strings.HasSuffix(n, "~"), strings.HasSuffix(n, ".swp"), strings.HasSuffix(n, ".tmp"):
		return true
	case !info.Mode().IsRegular():
		return true
	case info.Mode().Perm()&0o111 == 0:
		return true
	}
	return false
}

// scan returns the scripts directly in dir, in the order they run: byte order of
// their file names. A missing directory is an empty slot, not an error. A second
// file reducing to a name already taken is skipped with a WARN, since two
// timeline rows of the same name would be impossible to tell apart.
func scan(dir string) ([]file, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []file
	taken := map[string]string{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || ignored(e, info) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		name := pluginName(e.Name())
		if prev, ok := taken[name]; ok {
			slog.Warn("script plugin: two files have the same name; the later one is not loaded",
				"name", name, "loaded", prev, "skipped", e.Name())
			continue
		}
		hash, err := hashFile(path)
		if err != nil {
			slog.Warn("script plugin: cannot read script", "path", path, "error", err)
			continue
		}
		taken[name] = e.Name()
		out = append(out, file{name: name, path: path, hash: hash})
	}
	return out, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Fingerprint is a digest of every script directly in dir — names, order and
// contents — so the reloader can tell that a slot changed even when the config
// file did not. Empty for an empty or missing directory.
func Fingerprint(dir string) string {
	files, err := scan(dir)
	if err != nil || len(files) == 0 {
		return ""
	}
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\x00", f.path, f.hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}
