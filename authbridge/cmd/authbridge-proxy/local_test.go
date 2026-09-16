package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
)

// writeBuiltinConfig must produce a config file in cortexDir that loads, presets,
// and validates cleanly and describes a forward-only TLS-bridge observe
// pipeline pointed at caDir — otherwise --local would fail at boot instead of
// giving users a working, hot-reloadable local setup.
func TestDemoConfig_WriteLoadsAndValidates(t *testing.T) {
	cortexDir := t.TempDir()
	caDir := filepath.Join(cortexDir, "ca")

	p, err := writeBuiltinConfig(cortexDir, caDir)
	if err != nil {
		t.Fatalf("writeBuiltinConfig: %v", err)
	}
	if filepath.Dir(p) != cortexDir {
		t.Errorf("config written to %q, want inside %q", p, cortexDir)
	}

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	config.ApplyPreset(cfg)
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.Mode != config.ModeProxySidecar {
		t.Errorf("Mode = %q, want %q", cfg.Mode, config.ModeProxySidecar)
	}

	roles := cfg.Listener.ActiveRoles()
	if !roles[config.RoleForward] || roles[config.RoleReverse] {
		t.Errorf("expected forward-only roles, got %v", roles)
	}

	// The listeners the demo uses must bind loopback on the uncommon ports the
	// installer probes/prints, never a wildcard that would expose an open forward
	// proxy, the stats endpoint, or the unauthenticated session API (decrypted
	// bodies + injected tokens) to the LAN. The transparent listener isn't started
	// under --local (main.go gates it), so it's not asserted here.
	if got := cfg.Listener.ForwardProxyAddr; got != "127.0.0.1:47600" {
		t.Errorf("ForwardProxyAddr = %q, want loopback 127.0.0.1:47600", got)
	}
	if got := cfg.Listener.SessionAPIAddr; got != "127.0.0.1:47601" {
		t.Errorf("SessionAPIAddr = %q, want loopback 127.0.0.1:47601", got)
	}
	if got := cfg.Stats.StatsAddress; got != "127.0.0.1:47602" {
		t.Errorf("Stats.StatsAddress = %q, want loopback 127.0.0.1:47602", got)
	}

	if cfg.TLSBridge == nil {
		t.Fatalf("expected tls_bridge config, got nil")
	}
	if cfg.TLSBridge.Mode != "enabled" || !cfg.TLSBridge.GenerateCA {
		t.Errorf("expected tls_bridge enabled with generate_ca, got %+v", cfg.TLSBridge)
	}
	if cfg.TLSBridge.CADir != caDir {
		t.Errorf("CADir = %q, want %q", cfg.TLSBridge.CADir, caDir)
	}

	// Assert the exact parser set and order, not just the count — a swapped or
	// renamed plugin would otherwise pass silently.
	gotPlugins := make([]string, len(cfg.Pipeline.Outbound.Plugins))
	for i, p := range cfg.Pipeline.Outbound.Plugins {
		gotPlugins[i] = p.Name
	}
	// tool-prune must come last: it is the request-body mutator, and the
	// pipeline refuses to build a chain where a body reader follows it.
	wantPlugins := []string{"inference-parser", "mcp-parser", "a2a-parser", "tool-prune"}
	if !slices.Equal(gotPlugins, wantPlugins) {
		t.Errorf("outbound plugins = %v, want %v", gotPlugins, wantPlugins)
	}

	// tool-prune ships inert, and that is a property worth pinning: the demo
	// must never silently start rewriting a user's traffic. The empty remove
	// list is the guard — with no tool named there is nothing to remove, whatever
	// the policy — so filling the list is the single, deliberate act that
	// enables it. Asserting the policy too would just pin a default that is
	// meant to be edited.
	var tp *config.PluginEntry
	for i := range cfg.Pipeline.Outbound.Plugins {
		if cfg.Pipeline.Outbound.Plugins[i].Name == "tool-prune" {
			tp = &cfg.Pipeline.Outbound.Plugins[i]
		}
	}
	if tp == nil {
		t.Fatal("tool-prune entry not found")
	}
	if !strings.Contains(string(tp.Config), "\"remove\":[]") &&
		!strings.Contains(string(tp.Config), "\"remove\": []") {
		t.Errorf("tool-prune must ship with an empty remove list, got %s", tp.Config)
	}
}

// TestWriteDemoConfig_PreservesAnExistingFile: the config's own header invites
// editing it, and `abctl tools scan --write` writes a prune list into it. This
// function also runs before any port is bound, so an unconditional overwrite
// meant a --local start that then failed on a port clash silently destroyed those
// edits — which is exactly how a populated remove list was lost in practice.
func TestWriteDemoConfig_PreservesAnExistingFile(t *testing.T) {
	cortexDir := t.TempDir()
	caDir := filepath.Join(cortexDir, "ca")
	p, err := writeBuiltinConfig(cortexDir, caDir)
	if err != nil {
		t.Fatal(err)
	}
	edited := "# operator edit\nmode: proxy-sidecar\n"
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	// A second call — a restart — must not clobber it.
	p2, err := writeBuiltinConfig(cortexDir, caDir)
	if err != nil {
		t.Fatal(err)
	}
	if p2 != p {
		t.Errorf("path changed: %q vs %q", p2, p)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Errorf("edits were overwritten:\n%s", got)
	}
}

// A relative cost_ledger.dir must be refused, not resolved.
//
// costLedgerDir's own comment says it returns an error rather than falling back to
// the working directory, "for the reason defaultCortexDir does" — and then returned
// `dir` verbatim, so `dir: cost` produced exactly that failure. The proxy's working
// directory is not a property of the config: a launchd job, a container and a shell
// in a checkout each resolve it somewhere else, so one setting scatters day files
// across three directories and a query opens one of them and reports the rest as
// absent.
func TestCostLedgerDir_RefusesARelativeDir(t *testing.T) {
	for _, dir := range []string{"cost", "./cost", "../cost", "cortex/cost"} {
		cfg := &config.Config{CostLedger: &config.CostLedgerConfig{Dir: dir}}
		got, err := costLedgerDir(cfg)
		if err == nil {
			t.Errorf("cost_ledger.dir %q accepted, resolved to %q; it would resolve against the "+
				"proxy's working directory, which is what this function's comment says it refuses", dir, got)
			continue
		}
		// The operator has to be able to tell which setting to fix.
		if !strings.Contains(err.Error(), "cost_ledger.dir") {
			t.Errorf("error for %q does not name the setting: %v", dir, err)
		}
		if got != "" {
			t.Errorf("returned %q alongside an error; the caller would write there", got)
		}
	}
}

// An absolute dir is honoured and cleaned. Cleaning is not cosmetic: the writer's
// per-day file handling is keyed on the path, so a trailing slash or a doubled
// separator naming the same directory twice is a way to get two handles on one day
// file.
func TestCostLedgerDir_HonoursAndCleansAnAbsoluteDir(t *testing.T) {
	base := t.TempDir()
	for _, in := range []string{base + "/cost", base + "/cost/", base + "//cost", base + "/./cost"} {
		cfg := &config.Config{CostLedger: &config.CostLedgerConfig{Dir: in}}
		got, err := costLedgerDir(cfg)
		if err != nil {
			t.Fatalf("cost_ledger.dir %q rejected: %v", in, err)
		}
		if want := filepath.Join(base, "cost"); got != want {
			t.Errorf("cost_ledger.dir %q resolved to %q, want the cleaned %q", in, got, want)
		}
	}
}

// With no dir set the default still applies, and it is still absolute — the floor the
// refusal above rests on. Derived from $HOME rather than stored in the config, so a
// config copied between machines does not point at someone else's home.
func TestCostLedgerDir_DefaultsUnderCortexDirAndIsAbsolute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, cfg := range []*config.Config{
		nil,
		{},
		{CostLedger: &config.CostLedgerConfig{RetentionDays: 8}},
	} {
		got, err := costLedgerDir(cfg)
		if err != nil {
			t.Fatalf("costLedgerDir(%+v): %v", cfg, err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("default ledger dir %q is not absolute", got)
		}
		if filepath.Base(got) != costLedgerDirName {
			t.Errorf("default ledger dir %q does not end in %q", got, costLedgerDirName)
		}
	}
}

// THE LEDGER MUST BE ON FOR A CONFIG LAUNCHED WITH --config, because that is how every
// INSTALLED laptop launches: `abctl service install` writes a plist/unit whose ExecStart is
// `authbridge-proxy --config ~/.cortex/config.yaml`, never --local.
//
// The default alone could not deliver that. LedgerEnabled(defaultOn) is asked with
// defaultOn = localMode, localMode is set only by --local, and a config with no cost_ledger
// block falls through to false — so "Every local install keeps a cost ledger on disk, on by
// default" (docs/laptop-service.md) was true only for a hand-run
// `authbridge-proxy --local`. The service had it off, for its whole life, silently: cost
// history is the one thing a laptop restart is supposed not to lose, and window=7d had
// nothing to read.
//
// So the assertion is deliberately made with defaultOn = FALSE. Passing true here would
// assert the binary's --local default and pass with no cost_ledger block at all, which is
// exactly the hole this closes.
func TestDemoConfig_CostLedgerIsOnWhenLaunchedWithConfigNotLocal(t *testing.T) {
	cortexDir := t.TempDir()
	p, err := writeBuiltinConfig(cortexDir, filepath.Join(cortexDir, "ca"))
	if err != nil {
		t.Fatalf("writeBuiltinConfig: %v", err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	config.ApplyPreset(cfg)
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.CostLedger == nil {
		t.Fatal("the generated config carries no cost_ledger block, so an installed service " +
			"(--config, not --local) gets the ledger OFF while the docs promise it is on")
	}
	if !cfg.CostLedger.LedgerEnabled(false) {
		t.Error("LedgerEnabled(false) = false: the block is present but does not enable the " +
			"ledger for the launch path every installed laptop uses")
	}
	// The ledger runs by registering as a Recorder on the session store, so an enabled
	// ledger over a disabled store is inert — and config.Validate refuses that combination
	// outright. Asserted here so the preset cannot start shipping a config that fails to
	// load precisely because this block was added.
	if !cfg.Session.SessionEnabled() {
		t.Error("sessions are disabled in the generated config, which makes the ledger above " +
			"unreachable; see warnCostLedgerNeedsSessions")
	}
}
