package toolscan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	toolPruneEntry   = regexp.MustCompile(`^(\s*)-\s+name:\s*tool-prune\s*$`)
	contextGuruEntry = regexp.MustCompile(`^(\s*)-\s+name:\s*context-guru\s*$`)
	toolfilterKey    = regexp.MustCompile(`^(\s*)toolfilter:\s*(#.*)?$`)
	listItem         = regexp.MustCompile(`^\s*-\s`)
	removeKey        = regexp.MustCompile(`^(\s*)remove:\s*.*$`)
)

// PatchConfig rewrites the prune list in the YAML at path, in place, and
// reports whether the file changed. The list is engine.components.toolfilter.remove
// of the context-guru entry. When the config has no context-guru entry, it is
// the remove: list of the tool-prune entry.
//
// Line-based on purpose. Round-tripping through a YAML library would reformat
// the whole document — dropping the comments that explain each plugin and
// reflowing entries the operator hand-tuned. The edit here touches exactly one
// line, so everything else in the file survives byte-for-byte, and re-running
// with the same candidates is a no-op.
func PatchConfig(path string, candidates []string) (changed bool, err error) {
	orig, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The bare os error ("open config.yaml: no such file or directory")
			// is technically complete and practically useless when the caller
			// passed a relative path. Say where we looked and where the config
			// actually is. This message exists to rescue a lost user, so a wrong
			// path here is worse than no path at all.
			abs, aerr := filepath.Abs(path)
			if aerr != nil {
				abs = path
			}
			return false, fmt.Errorf("no config at %s\n"+
				"  cortex --local writes ~/.cortex/config.yaml,\n"+
				"  so pass that, or an absolute path. To find it:\n"+
				"    curl -s localhost:47602/config | grep ca_dir", abs)
		}
		return false, err
	}
	lines := strings.Split(string(orig), "\n")

	if start, end := entrySpan(lines, contextGuruEntry); start >= 0 {
		tf := -1
		for i := start + 1; i < end; i++ {
			if toolfilterKey.MatchString(lines[i]) {
				tf = i
				break
			}
		}
		if tf >= 0 {
			// The toolfilter block ends at the next content line indented no
			// deeper than toolfilter:, so a remove: of another component is
			// never changed.
			tfEnd := end
			for i := tf + 1; i < end; i++ {
				if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") &&
					leadingSpaces(lines[i]) <= leadingSpaces(lines[tf]) {
					tfEnd = i
					break
				}
			}
			if changed, found, err := patchRemove(path, lines, tf+1, tfEnd, "context-guru", candidates); found || err != nil {
				return changed, err
			}
		}
		return false, fmt.Errorf("context-guru entry in %s has no remove: key under engine.components.toolfilter — add remove: [] there and re-run", path)
	}

	start, end := entrySpan(lines, toolPruneEntry)
	if start < 0 {
		return false, fmt.Errorf("no `- name: context-guru` or `- name: tool-prune` entry in %s — add the plugin to a pipeline first", path)
	}
	if changed, found, err := patchRemove(path, lines, start+1, end, "tool-prune", candidates); found || err != nil {
		return changed, err
	}
	return false, fmt.Errorf("tool-prune entry in %s has no `remove:` key under config: — add `remove: []` and re-run", path)
}

// entrySpan returns the first line of the list item that entry matches, and
// the index after its last line. start is -1 when no line matches.
func entrySpan(lines []string, entry *regexp.Regexp) (start, end int) {
	start = -1
	var entryIndent string
	for i, l := range lines {
		if m := entry.FindStringSubmatch(l); m != nil {
			start, entryIndent = i, m[1]
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	// The entry ends at the next list item indented no deeper than this one.
	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		if listItem.MatchString(lines[i]) && leadingSpaces(lines[i]) <= len(entryIndent) {
			end = i
			break
		}
	}
	return start, end
}

// patchRemove replaces the first remove: line in lines[from:end] and writes
// the file. found is false when that span has no remove: line.
func patchRemove(path string, lines []string, from, end int, plugin string, candidates []string) (changed, found bool, err error) {
	want := "remove: []"
	if len(candidates) > 0 {
		want = fmt.Sprintf("remove: [%s]", strings.Join(candidates, ", "))
	}
	for i := from; i < end; i++ {
		m := removeKey.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		// Refuse the block-list form. Replacing just the `remove:` line would
		// leave its `- item` children dangling under a now-inline value, which
		// is invalid YAML — the proxy would reject the config on reload and the
		// operator would be left with a file this tool broke.
		if isBlockList(lines, i, end) {
			return false, true, fmt.Errorf("%s: the %s `remove:` list is in block form (one `- item` per line);\n"+
				"  this tool only rewrites the inline form. Replace those lines with `remove: []` and re-run,\n"+
				"  or paste the block this command prints without --write", path, plugin)
		}
		replacement := m[1] + want
		if lines[i] == replacement {
			return false, true, nil // already current — idempotent
		}
		lines[i] = replacement
		if err := writeFileAtomic(path, []byte(strings.Join(lines, "\n"))); err != nil {
			return false, true, err
		}
		return true, true, nil
	}
	return false, false, nil
}

func leadingSpaces(s string) int {
	for i, r := range s {
		if r != ' ' && r != '\t' {
			return i
		}
	}
	return len(s)
}

// writeFileAtomic replaces path's contents via a temp file and a rename.
//
// os.WriteFile truncates in place, which has two failure modes on a live config:
// a crash mid-write leaves a truncated file with no copy to recover from, and
// even on the success path the proxy's fsnotify reloader can wake on the
// truncated intermediate state and reject its own config. A rename is atomic, so
// a reader sees either the old file or the new one.
//
// The temp file is created in the same directory so the rename stays within one
// filesystem, and the destination's existing mode is preserved — the file already
// exists (PatchConfig read it), so its permissions are the operator's to keep.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Sync before rename: without it a crash after the rename can leave the
	// new name pointing at unflushed (zero-length) content.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// isBlockList reports whether the `remove:` at lines[i] is followed by YAML
// block-sequence items rather than carrying an inline value.
func isBlockList(lines []string, i, end int) bool {
	if strings.TrimSpace(strings.SplitN(lines[i], ":", 2)[1]) != "" {
		return false // has an inline value on the same line
	}
	for j := i + 1; j < end && j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return strings.HasPrefix(t, "- ")
	}
	return false
}
