package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/core/config"
	"gopkg.in/yaml.v3"
)

// A config written by an older build keeps its own shape forever:
// writeBuiltinConfig deliberately never overwrites an existing file, so that a
// hand-edited prune list survives a restart. The cost is that fixes to the
// built-in config never reach anyone who already has one — a config from before
// the health/transparent pins leaves those listeners on the preset defaults,
// which bind EVERY interface. On a laptop on an untrusted network that is a
// listener nobody asked for.
//
// So the pins are added to configs that lack them: only ever ADDING keys, never
// changing a value the user set, with the previous file kept alongside.
type pinnedListener struct {
	key     string
	value   string
	comment string
	// isFlag marks a boolean key, reported differently from an address.
	isFlag bool
	// unpinnedDefault is what the preset fills in when the key is absent, and
	// what makes the omission worth fixing rather than tidy.
	unpinnedDefault string
}

var listenerPins = []pinnedListener{
	{
		// The durable half of this migration. The two address pins below fix the two
		// listeners that were known to be wildcard; this covers every other listener
		// in the config, and any added later, without a further migration.
		key:   "bind_loopback_only",
		value: "true",
		comment: "Added by agentop: bind every listener to 127.0.0.1, including any not named\n" +
			"here. Without it a listener falls back to a preset default that binds every\n" +
			"interface — on a laptop, the Wi-Fi.",
		isFlag:          true,
		unpinnedDefault: "wildcard binds for anything unpinned",
	},
	{
		key:             "health_addr",
		value:           "127.0.0.1:47604",
		comment:         "Added by agentop: unpinned, the preset binds health on :9091 — every interface.",
		unpinnedDefault: ":9091",
	},
	{
		key:   "transparent_proxy_addr",
		value: "127.0.0.1:47603",
		comment: "Added by agentop: unpinned, the preset binds :8082 on every interface. A proxy\n" +
			"started from ~/.cortex never opens this listener; one started from elsewhere does.",
		unpinnedDefault: ":8082",
	},
}

// migrateConfig adds any missing listener pins. It reports what it changed and
// whether anything did.
func migrateConfig(path string, stdout io.Writer) (changed bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return false, err
	}
	// Parsed only to establish the config is valid before editing it; the keys
	// themselves come from listenerKeys below.
	if _, err := config.Load(path); err != nil {
		return false, fmt.Errorf("%s does not parse (%w); not touching it", path, err)
	}
	missing, err := missingListenerPins(raw)
	if err != nil {
		return false, err
	}
	if len(missing) == 0 {
		return false, nil
	}

	updated, err := insertListenerKeys(string(raw), missing)
	if err != nil {
		return false, err
	}

	// Keep the previous file once, under a distinct name so it cannot be confused
	// with the config itself.
	bak := path + ".before-agentop-migrate"
	if _, serr := os.Stat(bak); os.IsNotExist(serr) {
		if werr := os.WriteFile(bak, raw, 0o600); werr != nil {
			return false, fmt.Errorf("writing %s: %w", bak, werr)
		}
	}
	if err := replaceConfig(path, updated, nil); err != nil {
		return false, err
	}

	fmt.Fprintf(stdout, "Updated %s (previous kept as %s):\n", path, bak)
	for _, pin := range missing {
		if pin.isFlag {
			fmt.Fprintf(stdout, "  + %s: %s   (was: %s)\n", pin.key, pin.value, pin.unpinnedDefault)
			continue
		}
		fmt.Fprintf(stdout, "  + %s: %s   (was defaulting to %s — every interface)\n",
			pin.key, pin.value, pin.unpinnedDefault)
	}
	return true, nil
}

// missingListenerPins returns the pins raw's listener block lacks.
func missingListenerPins(raw []byte) ([]pinnedListener, error) {
	// Decide what is missing from the PARSED config, not from a text search: a key
	// could appear in a comment, and a commented-out key is still absent.
	//
	// Presence is decided against the DOCUMENT, not the parsed value. An explicit
	// `health_addr: ""` or `bind_loopback_only: false` is present but parses as the
	// zero value, so a value-based check appended a second copy of the key — and
	// config.Load then rejected the duplicate, failing the migration on a config that
	// was perfectly valid. Reading the keys also removes the need for a per-pin
	// predicate, so there is no second list to keep in sync.
	present, err := listenerKeys(raw)
	if err != nil {
		return nil, err
	}
	missing := make([]pinnedListener, 0, len(listenerPins))
	for _, pin := range listenerPins {
		if _, ok := present[pin.key]; !ok {
			missing = append(missing, pin)
		}
	}
	return missing, nil
}

// configMigrationPending reports true exactly when migrateConfig or
// migrateBobPricing would change the file at path. agentop setup asks it before
// consent, so a re-run with an up-to-date config can report "nothing to do" while
// one with an older config still gets its migration.
//
// Each migration is decided by the steps it takes before it writes, with the same
// helpers: for the pins, missingListenerPins, insertListenerKeys and loading the
// result; for Bob, bobPriced, insertBobEndpoint, loading the result and
// bobPricedVerify of that. A config a migration would refuse is not pending from
// it, and a refused pins migration falls through to Bob, which may still apply.
// Pricing that will not build is one such refusal: it reads as not pending, not as
// an error, so setup is not stopped by a step that would only have failed.
//
// It writes nothing beside the user's config. Each candidate is loaded from a temp
// file in os.TempDir(), removed before it returns. It returns an error, not an
// answer, for a config migrateConfig refuses before editing (one that does not
// load, or whose listener: is not a mapping) and when the temp file fails.
func configMigrationPending(path string) (bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return false, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return false, fmt.Errorf("%s does not parse (%w)", path, err)
	}
	if pins, err := pinsMigrationPending(raw); err != nil || pins {
		return pins, err
	}
	// migrateConfig would leave the file as it is, so migrateBobPricing reads raw.
	if priced, perr := bobPriced(cfg); perr != nil || priced {
		return false, nil
	}
	updated, ierr := insertBobEndpoint(string(raw))
	if ierr != nil {
		return false, nil
	}
	c, loaded, lerr := loadCandidate(updated)
	if lerr != nil || !loaded {
		return false, lerr
	}
	return bobPricedVerify(c) == nil, nil
}

// pinsMigrationPending is configMigrationPending's pins half: whether
// migrateConfig would add listener pins to raw, decided by the steps it takes
// before it writes. A result insertListenerKeys refuses, or one that will not load,
// is not pending. The error is missingListenerPins's, or loadCandidate's own temp
// file's; pending is false with it.
func pinsMigrationPending(raw []byte) (bool, error) {
	missing, err := missingListenerPins(raw)
	if err != nil || len(missing) == 0 {
		return false, err
	}
	updated, err := insertListenerKeys(string(raw), missing)
	if err != nil {
		return false, nil
	}
	_, loaded, err := loadCandidate(updated)
	return err == nil && loaded, err
}

// loadCandidate loads updated the way replaceConfig loads a migration's result
// before swapping it in, but from a temp file in os.TempDir() rather than beside
// the config, removed before it returns. loaded is false when updated does not
// load; err is the temp file's own.
func loadCandidate(updated string) (cfg *config.Config, loaded bool, err error) {
	f, err := os.CreateTemp("", "agentop-pending-*.yaml")
	if err != nil {
		return nil, false, err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	_, werr := f.WriteString(updated)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, false, werr
	}
	cfg, lerr := config.Load(name)
	if lerr != nil {
		return nil, false, nil
	}
	return cfg, true, nil
}

// replaceConfig swaps updated in for the file at path, through a temp file that
// must load first — and, when verify is set, pass it. A migration that produces an
// unparseable config would take the proxy down on next start, so the original
// survives any failure.
func replaceConfig(path, updated string, verify func(*config.Config) error) error {
	// CreateTemp, not path+".tmp": a fixed name is a symlink an attacker can
	// plant, and the file being replaced is the one the proxy boots from.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds
	if _, werr := tmp.WriteString(updated); werr != nil {
		_ = tmp.Close()
		return werr
	}
	// Sync before the rename: a crash after it must not leave the config the
	// proxy boots from empty.
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		return serr
	}
	if cerr := tmp.Close(); cerr != nil {
		return cerr
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	cfg, verr := config.Load(tmpName)
	if verr != nil {
		return fmt.Errorf("the migrated config would not parse (%w); left %s alone", verr, path)
	}
	if verify != nil {
		if verr := verify(cfg); verr != nil {
			return fmt.Errorf("%w; left %s alone", verr, path)
		}
	}
	return os.Rename(tmpName, path)
}

// insertListenerKeys adds keys to the listener block, matching whatever
// indentation the file already uses.
//
// Indentation is read from the file rather than assumed: a real config turned up
// with the whole document indented two spaces, which is valid YAML and would have
// produced a broken file from any hardcoded guess.
func insertListenerKeys(src string, pins []pinnedListener) (string, error) {
	lines := strings.Split(src, "\n")

	listenerAt := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "listener:") && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			listenerAt = i
			break
		}
	}
	if listenerAt < 0 {
		return "", fmt.Errorf("no listener: block found")
	}
	parentIndent := len(lines[listenerAt]) - len(strings.TrimLeft(lines[listenerAt], " "))

	// The block ends at the first later line whose indentation is <= the parent's
	// and which is not blank. Child indentation comes from the first child seen.
	childIndent := -1
	end := len(lines)
	for i := listenerAt + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
		if indent <= parentIndent {
			end = i
			break
		}
		if childIndent < 0 {
			childIndent = indent
		}
	}
	if childIndent < 0 {
		// No child lines at all. If the block is flow-style (`listener: {}` or
		// `listener: {roles: [forward]}`) then appending block-style children beneath it
		// is invalid YAML. That fails closed — the validation before the rename catches
		// it and the original survives — but the user would see a bare parse error
		// instead of being told the shape is unsupported.
		if rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[listenerAt]), "listener:")); rest != "" {
			return "", fmt.Errorf("listener: is written in flow style (%s); "+
				"rewrite it as an indented block, or add the keys by hand", rest)
		}
		// A genuinely empty block: indent one level in from the parent.
		childIndent = parentIndent + 2
	}
	// Insert after the last non-blank line of the block so trailing blank lines
	// stay where the author put them.
	insertAt := end
	for insertAt > listenerAt+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}

	pad := strings.Repeat(" ", childIndent)
	add := make([]string, 0, len(pins)*3)
	for _, pin := range pins {
		for _, cl := range strings.Split(pin.comment, "\n") {
			add = append(add, pad+"# "+strings.TrimSpace(cl))
		}
		add = append(add, pad+pin.key+": "+pin.value)
	}

	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:insertAt]...)
	out = append(out, add...)
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\n"), nil
}

// listenerKeys returns the keys explicitly written under `listener:`.
//
// Generic YAML rather than the typed config, because the typed struct cannot
// distinguish "absent" from "present and set to the zero value".
func listenerKeys(raw []byte) (map[string]struct{}, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reading the config's keys: %w", err)
	}
	out := map[string]struct{}{}
	l, ok := doc["listener"]
	if !ok {
		return out, nil
	}
	m, ok := l.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("listener: is not a mapping; not editing this config")
	}
	for k := range m {
		out[k] = struct{}{}
	}
	return out, nil
}
