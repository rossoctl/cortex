// Package catalog is the script plugins Cortex ships: each folder under entries/
// is one entry, which installed is one slot. It imports nothing from the script
// plugin, so agentop can read it without linking the plugin in.
package catalog

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// catalogFS holds the scripts Cortex ships. `all:` keeps the _-prefixed helper
// files an entry may hold, which embed would otherwise leave out.
//
//go:embed all:entries
var catalogFS embed.FS

// entryFile is each catalog entry's metadata file. It is not a script: a slot
// skips it because it is not executable, and installing an entry does not copy it.
const entryFile = "entry.yaml"

// CatalogEntry is one script plugin Cortex ships. Installed, it is one slot: its
// files go to ~/.cortex/plugins/<Name>/, and the slot's settings are its own.
type CatalogEntry struct {
	Name        string
	Description string
	FailClosed  bool
	// Timeout is the slot's timeout, "" for the plugin's default.
	Timeout string
	// Files is every file of the entry but entry.yaml, by name.
	Files map[string][]byte
}

// Scripts is the names of the files a slot would run, in the order it would run
// them: the ones a slot does not skip by name.
func (e CatalogEntry) Scripts() []string {
	var out []string
	for name := range e.Files {
		if !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

type entryMeta struct {
	Description string `yaml:"description"`
	FailClosed  bool   `yaml:"fail_closed"`
	Timeout     string `yaml:"timeout"`
}

// generated reports whether name is something Python leaves beside a script it
// imported or tested, rather than part of the entry. `all:` embeds whatever is in
// the folder, and a pytest run in a checkout writes __pycache__ there.
func generated(name string) bool {
	return name == "__pycache__" || strings.HasSuffix(name, ".pyc")
}

// validName is a folder name a slot's dir accepts: one plain path element. It is
// the script plugin's SlotDir rule, restated so this package needs nothing from it;
// TestCatalog_NamesAreSlotDirs in the plugin's package holds the two together.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) &&
		!strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

// Catalog is every entry Cortex ships, sorted by name. An error means the binary
// was built with a broken catalog, which the catalog's own test refuses.
func Catalog() ([]CatalogEntry, error) {
	dirs, err := fs.ReadDir(catalogFS, "entries")
	if err != nil {
		return nil, err
	}
	var out []CatalogEntry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		e, err := readEntry(d.Name())
		if err != nil {
			return nil, fmt.Errorf("script catalog: %s: %w", d.Name(), err)
		}
		out = append(out, e)
	}
	return out, nil
}

func readEntry(name string) (CatalogEntry, error) {
	e := CatalogEntry{Name: name, Files: map[string][]byte{}}
	if !validName(name) {
		return e, fmt.Errorf("%q is not a name a slot's dir accepts", name)
	}
	dir := path.Join("entries", name)
	files, err := fs.ReadDir(catalogFS, dir)
	if err != nil {
		return e, err
	}
	var meta entryMeta
	haveMeta := false
	for _, f := range files {
		if generated(f.Name()) {
			continue
		}
		if f.IsDir() {
			return e, fmt.Errorf("%s is a folder; a slot runs only the files directly in its own", f.Name())
		}
		b, err := catalogFS.ReadFile(path.Join(dir, f.Name()))
		if err != nil {
			return e, err
		}
		if f.Name() == entryFile {
			dec := yaml.NewDecoder(strings.NewReader(string(b)))
			dec.KnownFields(true)
			if err := dec.Decode(&meta); err != nil {
				return e, fmt.Errorf("%s: %w", entryFile, err)
			}
			haveMeta = true
			continue
		}
		e.Files[f.Name()] = b
	}
	switch {
	case !haveMeta:
		return e, fmt.Errorf("no %s", entryFile)
	case strings.TrimSpace(meta.Description) == "":
		return e, fmt.Errorf("%s has no description", entryFile)
	}
	if meta.Timeout != "" {
		if d, err := time.ParseDuration(meta.Timeout); err != nil || d <= 0 {
			return e, fmt.Errorf("%s: timeout %q is not a positive duration", entryFile, meta.Timeout)
		}
	}
	e.Description, e.FailClosed, e.Timeout = meta.Description, meta.FailClosed, meta.Timeout
	if len(e.Scripts()) == 0 {
		return e, fmt.Errorf("no script: every file is a helper")
	}
	return e, nil
}
