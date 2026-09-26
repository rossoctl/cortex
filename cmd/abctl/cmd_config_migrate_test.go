package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// oldStyleConfig is shaped like a config written before the pins existed.
const oldStyleConfig = `mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
stats:
  address: 127.0.0.1:47602
tls_bridge:
  mode: enabled
  ca_dir: "CADIR"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: tool-prune
        config:
          remove: [CronCreate, WebSearch]
`

// indentedConfig is a REAL shape found on a machine: the whole document indented
// two spaces. Valid YAML, and fatal to any migration that hardcodes indentation.
const indentedConfig = `  mode: proxy-sidecar
  listener:
    roles: [forward]
    forward_proxy_addr: 127.0.0.1:47600
    session_api_addr: 127.0.0.1:47601
  stats:
    address: 127.0.0.1:47602
  tls_bridge:
    mode: enabled
    ca_dir: "CADIR"
    generate_ca: true
  pipeline:
    outbound:
      plugins:
        - name: inference-parser
`

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(strings.Replace(body, "CADIR", filepath.Join(dir, "ca"), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMigrateConfig_AddsThePins is the point: an old config leaves health and the
// transparent listener on preset defaults that bind EVERY interface.
func TestMigrateConfig_AddsThePins(t *testing.T) {
	p := writeCfg(t, oldStyleConfig)

	before, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if before.Listener.HealthAddr != "" || before.Listener.TransparentProxyAddr != "" {
		t.Fatal("fixture already has the pins")
	}

	var out bytes.Buffer
	changed, err := migrateConfig(p, &out)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !changed {
		t.Fatal("reported no change")
	}

	after, err := config.Load(p)
	if err != nil {
		t.Fatalf("migrated config does not parse: %v", err)
	}
	if after.Listener.HealthAddr != "127.0.0.1:47604" {
		t.Errorf("health_addr = %q", after.Listener.HealthAddr)
	}
	if after.Listener.TransparentProxyAddr != "127.0.0.1:47603" {
		t.Errorf("transparent_proxy_addr = %q", after.Listener.TransparentProxyAddr)
	}
	// The durable half: covers listeners this migration does not name.
	if !after.Listener.BindLoopbackOnly {
		t.Error("bind_loopback_only was not added; a future listener would bind wide")
	}
	// Untouched: everything the user had.
	if after.Listener.ForwardProxyAddr != before.Listener.ForwardProxyAddr {
		t.Error("forward_proxy_addr changed")
	}
	if after.Stats.StatsAddress != before.Stats.StatsAddress {
		t.Error("stats address changed")
	}
	if !strings.Contains(readFile(t, p), "remove: [CronCreate, WebSearch]") {
		t.Error("the prune list was lost")
	}
	if _, serr := os.Stat(p + ".before-abctl-migrate"); serr != nil {
		t.Errorf("no backup kept: %v", serr)
	}
}

// TestMigrateConfig_HandlesAnIndentedDocument covers the real-world shape that
// would break a hardcoded indent.
func TestMigrateConfig_HandlesAnIndentedDocument(t *testing.T) {
	p := writeCfg(t, indentedConfig)
	var out bytes.Buffer
	if _, err := migrateConfig(p, &out); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("migrated config does not parse: %v\n%s", err, readFile(t, p))
	}
	if cfg.Listener.HealthAddr != "127.0.0.1:47604" {
		t.Errorf("health_addr = %q\n%s", cfg.Listener.HealthAddr, readFile(t, p))
	}
	// The inserted lines must match the document's own indentation, or the YAML is
	// valid-but-wrong (a key landing in the wrong block) or invalid outright.
	for _, line := range strings.Split(readFile(t, p), "\n") {
		if strings.Contains(line, "health_addr:") {
			if indent := len(line) - len(strings.TrimLeft(line, " ")); indent != 4 {
				t.Errorf("health_addr indented %d spaces, want 4 to match the document", indent)
			}
		}
	}
}

// TestMigrateConfig_Idempotent: this runs on every service install.
func TestMigrateConfig_Idempotent(t *testing.T) {
	p := writeCfg(t, oldStyleConfig)
	var out bytes.Buffer
	if changed, err := migrateConfig(p, &out); err != nil || !changed {
		t.Fatalf("first: changed=%v err=%v", changed, err)
	}
	first := readFile(t, p)
	changed, err := migrateConfig(p, &out)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second run changed the file again")
	}
	if readFile(t, p) != first {
		t.Error("second run modified the file")
	}
}

// TestMigrateConfig_LeavesUserValuesAlone: a value the operator chose is theirs.
// Migration only ever ADDS.
func TestMigrateConfig_LeavesUserValuesAlone(t *testing.T) {
	body := strings.Replace(oldStyleConfig,
		"  session_api_addr: 127.0.0.1:47601",
		"  session_api_addr: 127.0.0.1:47601\n  health_addr: 127.0.0.1:59999", 1)
	p := writeCfg(t, body)
	var out bytes.Buffer
	if _, err := migrateConfig(p, &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listener.HealthAddr != "127.0.0.1:59999" {
		t.Errorf("health_addr = %q, want the operator's 59999", cfg.Listener.HealthAddr)
	}
	if strings.Count(readFile(t, p), "health_addr:") != 1 {
		t.Error("health_addr was duplicated")
	}
}

// TestMigrateConfig_RefusesAnUnparseableConfig: rewriting a file we cannot read
// would destroy settings we never understood.
func TestMigrateConfig_RefusesAnUnparseableConfig(t *testing.T) {
	p := writeCfg(t, "listener:\n  roles: [forward\n")
	var out bytes.Buffer
	if _, err := migrateConfig(p, &out); err == nil {
		t.Fatal("accepted an unparseable config")
	}
	if !strings.Contains(readFile(t, p), "roles: [forward") {
		t.Error("the unparseable file was modified")
	}
}

// TestMigrateConfig_NoListenerBlock: nothing to migrate into, and we must not
// invent one.
func TestMigrateConfig_NoListenerBlock(t *testing.T) {
	p := writeCfg(t, "mode: proxy-sidecar\nstats:\n  address: 127.0.0.1:47602\n")
	var out bytes.Buffer
	_, err := migrateConfig(p, &out)
	if err == nil {
		t.Fatal("expected an error with no listener block")
	}
	if !strings.Contains(err.Error(), "listener") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMigrateConfig_FlagAloneIsEnoughToBeUpToDate: someone who set the flag by hand
// should not be nagged, and must not have it duplicated.
func TestMigrateConfig_FlagAlreadySet(t *testing.T) {
	body := strings.Replace(oldStyleConfig,
		"  roles: [forward]",
		"  roles: [forward]\n  bind_loopback_only: true", 1)
	p := writeCfg(t, body)
	var out bytes.Buffer
	if _, err := migrateConfig(p, &out); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readFile(t, p), "bind_loopback_only"); n != 1 {
		t.Errorf("bind_loopback_only appears %d times, want 1", n)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Listener.BindLoopbackOnly {
		t.Error("the operator's flag was lost")
	}
}

// TestMigratedConfigActuallyBindsLoopback is the end of the chain: the migration is
// only worth anything if the running proxy binds loopback afterwards. Asserts
// against the real Load+ApplyPreset sequence the binaries use.
func TestMigratedConfigActuallyBindsLoopback(t *testing.T) {
	p := writeCfg(t, oldStyleConfig)
	var out bytes.Buffer
	if _, err := migrateConfig(p, &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	config.ApplyPreset(cfg)
	for name, addr := range map[string]string{
		"forward":     cfg.Listener.ForwardProxyAddr,
		"session":     cfg.Listener.SessionAPIAddr,
		"health":      cfg.Listener.HealthAddr,
		"transparent": cfg.Listener.TransparentProxyAddr,
		"stats":       cfg.Stats.StatsAddress,
	} {
		if addr == "" {
			continue
		}
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("%s binds %q after migration — still exposed", name, addr)
		}
	}
}
