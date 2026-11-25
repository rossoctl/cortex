package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

func TestConfigMigrationPending_AgreesWithTheMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:47600\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	pending, err := configMigrationPending(path)
	if err != nil || !pending {
		t.Fatalf("unpinned config: pending=%v err=%v, want true", pending, err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("the dry run changed the file")
	}
	for _, f := range []string{".before-agentop-migrate", ".before-agentop-pricing"} {
		if _, err := os.Stat(path + f); err == nil {
			t.Errorf("the dry run wrote %s", f)
		}
	}

	if _, err := migrateConfig(path, io.Discard); err != nil {
		t.Fatal(err)
	}
	pending, err = configMigrationPending(path)
	if err != nil || !pending {
		t.Fatalf("pinned but Bob unpriced: pending=%v err=%v, want true", pending, err)
	}

	if _, err := migrateBobPricing(path, io.Discard); err != nil {
		t.Fatal(err)
	}
	pending, err = configMigrationPending(path)
	if err != nil || !pending {
		t.Fatalf("priced but no metrics_push: pending=%v err=%v, want true", pending, err)
	}

	if _, err := migrateMetricsPush(path, io.Discard); err != nil {
		t.Fatal(err)
	}
	pending, err = configMigrationPending(path)
	if err != nil || pending {
		t.Fatalf("fully migrated: pending=%v err=%v, want false", pending, err)
	}
}

// The config above lacks the pins AND leaves Bob unpriced, so its first answer is
// true whichever check gives it. Pricing Bob first leaves only the pins pending.
func TestConfigMigrationPending_SeesPinsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:47600\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateBobPricing(path, io.Discard); err != nil {
		t.Fatal(err)
	}
	pending, err := configMigrationPending(path)
	if err != nil || !pending {
		t.Fatalf("Bob priced but unpinned: pending=%v err=%v, want true", pending, err)
	}
	if changed, err := migrateConfig(path, io.Discard); err != nil || !changed {
		t.Fatalf("migrateConfig: changed=%v err=%v, want a change", changed, err)
	}
}

// Each config is valid YAML that only one of the dry run's two checks refuses, so
// each check is the one under test; migrateConfig refuses both.
func TestConfigMigrationPending_RefusesWhatMigrateConfigRefuses(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		// listenerKeys reads it; the typed config.Load does not.
		{"will not load", "mode: [proxy-sidecar]\nlistener:\n  roles: [forward]\n"},
		// config.Load reads it; listenerKeys finds no mapping to add keys to.
		{"listener not a mapping", "mode: proxy-sidecar\nlistener:\n"},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := configMigrationPending(path); err == nil {
			t.Errorf("%s: the dry run reported no error", tc.name)
		}
		if _, err := migrateConfig(path, io.Discard); err == nil {
			t.Errorf("%s: migrateConfig accepted it, so refusing it disagrees", tc.name)
		}
	}
}

// Configs both migrations leave alone, each for a different reason, so each of the
// dry run's refusals is the one under test. Pending for any of them would put a
// consent screen in front of a step that then changes nothing.
func TestConfigMigrationPending_NotPendingWhenNoMigrationWouldApply(t *testing.T) {
	pins := "listener:\n  bind_loopback_only: true\n  health_addr: 127.0.0.1:47604\n" +
		"  transparent_proxy_addr: 127.0.0.1:47603\n"
	for _, tc := range []struct{ name, body string }{
		// insertBobEndpoint refuses a flow-style pricing block.
		{"flow-style pricing", "mode: proxy-sidecar\n" + pins + "pricing: {endpoints: []}\n" + pushOff},
		// insertListenerKeys refuses a flow-style listener block.
		{"flow-style listener", "mode: proxy-sidecar\nlistener: {roles: [forward]}\npricing: {endpoints: []}\n" + pushOff},
		// A comment indented past the keys sets the inserted keys' indentation, and
		// the result does not parse.
		{"pins would not parse", "mode: proxy-sidecar\nlistener:\n    # note\n  roles: [forward]\npricing: {endpoints: []}\n" + pushOff},
		// An explicit null on the line after the key: the entry goes in above it, and
		// the result does not parse.
		{"Bob entry would not parse", "mode: proxy-sidecar\n" + pins + "pricing:\n  ~\n" + pushOff},
		// The operator prices Bob's host in USD for another model, so premium-ide is
		// unpriced, and a Bobcoins entry for the same host would not build.
		{"Bob entry would not price", "mode: proxy-sidecar\n" + pins + "pricing:\n  endpoints:\n" +
			"  - hosts: [\"" + bobHost + "\"]\n    models:\n      \"other-model\":\n" +
			"        input_cost_per_million: 1.00\n        output_cost_per_million: 1.00\n" + pushOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			pending, err := configMigrationPending(path)
			if err != nil || pending {
				t.Errorf("%s: pending=%v err=%v, want false", tc.name, pending, err)
			}
			if names := dirNames(t, dir); len(names) != 1 {
				t.Errorf("%s: the dry run left %v beside the config", tc.name, names)
			}
			if names := dirNames(t, tmp); len(names) != 0 {
				t.Errorf("%s: the dry run left %v in os.TempDir()", tc.name, names)
			}
			_, _ = migrateConfig(path, io.Discard)
			_, _ = migrateBobPricing(path, io.Discard)
			if after, _ := os.ReadFile(path); string(after) != tc.body {
				t.Errorf("%s: the migrations changed the file, so it was pending", tc.name)
			}
		})
	}
}

// The pins migration refuses each of these and leaves the file alone, but the Bob
// migration still edits it, so the dry run must fall through to Bob rather than
// stop at the refusal.
func TestConfigMigrationPending_FallsThroughARefusedPinsMigrationToBob(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		// insertListenerKeys refuses: there is no listener: block to add keys to.
		{"no listener block", "mode: proxy-sidecar\n"},
		// insertListenerKeys refuses a flow-style listener block.
		{"flow-style listener", "mode: proxy-sidecar\nlistener: {bind_loopback_only: true}\n"},
		// insertListenerKeys succeeds, and the result does not parse.
		{"pins would not parse", "mode: proxy-sidecar\nlistener:\n    # note\n  bind_loopback_only: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			pending, err := configMigrationPending(path)
			if err != nil || !pending {
				t.Errorf("%s: pending=%v err=%v, want true", tc.name, pending, err)
			}
			if changed, _ := migrateConfig(path, io.Discard); changed {
				t.Errorf("%s: migrateConfig changed the file, so Bob is not what made it pending", tc.name)
			}
			if changed, err := migrateBobPricing(path, io.Discard); err != nil || !changed {
				t.Errorf("%s: migrateBobPricing: changed=%v err=%v, want a change", tc.name, changed, err)
			}
		})
	}
}

// config.Load accepts this pricing block but pricing.Build does not: one host
// priced in two units. migrateBobPricing refuses it and changes nothing. Setup asks
// the dry run before consent, so an error here would stop setup outright over a
// step that would only have failed.
func TestConfigMigrationPending_APricingBlockThatWillNotBuildIsNotPending(t *testing.T) {
	model := "    models:\n      \"m\":\n        input_cost_per_million: 1.00\n        output_cost_per_million: 1.00\n"
	body := "mode: proxy-sidecar\nlistener:\n  bind_loopback_only: true\n  health_addr: 127.0.0.1:47604\n" +
		"  transparent_proxy_addr: 127.0.0.1:47603\npricing:\n  endpoints:\n" +
		"  - hosts: [\"gw.example.com\"]\n" + model +
		"  - hosts: [\"gw.example.com\"]\n    unit: Credits\n" + model +
		"metrics_push:\n  enabled: false\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the fixture must load: %v", err)
	}
	if _, err := bobPriced(cfg); err == nil {
		t.Fatal("the fixture's pricing builds, so it tests nothing")
	}
	pending, err := configMigrationPending(path)
	if err != nil || pending {
		t.Errorf("a pricing block that will not build: pending=%v err=%v, want false and no error", pending, err)
	}
	_, _ = migrateConfig(path, io.Discard)
	_, _ = migrateBobPricing(path, io.Discard)
	if after, _ := os.ReadFile(path); string(after) != body {
		t.Error("the migrations changed a file whose pricing will not build, so it was pending")
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestConfigMigrationPending_RefusesABrokenConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listener:\n  roles: [forward\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configMigrationPending(path); err == nil {
		t.Error("a config that will not parse reported no error")
	}
}

// Each of the context-guru and metrics push migrations is pending alone, and a
// config that has both is not. configRestartPending is true for a change that a
// running proxy takes only on a restart, and false for a moved tool-prune list.
func TestConfigMigrationPending_SeesContextGuruAndMetricsPushAlone(t *testing.T) {
	cg := "      - name: context-guru\n"
	toolPrune := "      - name: tool-prune\n        on_error: enforce\n        config:\n          remove: [Bash]\n"
	for _, tc := range []struct {
		name, tail, push string
		pending, restart bool
	}{
		{"current", cg, pushOff, false, false},
		{"no metrics_push", cg, "", true, true},
		{"no context operator", "", pushOff, true, true},
		{"tool-prune to move", toolPrune, pushOff, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			b := beforeContextGuru(t, path, tc.tail)
			if err := os.WriteFile(path, append(b, tc.push...), 0o600); err != nil {
				t.Fatal(err)
			}
			if pending, err := configMigrationPending(path); err != nil || pending != tc.pending {
				t.Errorf("pending=%v err=%v, want %v", pending, err, tc.pending)
			}
			if got := configRestartPending(path); got != tc.restart {
				t.Errorf("configRestartPending = %v, want %v", got, tc.restart)
			}
		})
	}
}
