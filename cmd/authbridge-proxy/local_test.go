package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/usage"
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

// The moved-CA warning (issue #1033). `--local` derives ca_dir from $HOME via
// defaultCortexDir, so a redirected $HOME — a sandbox, a per-project home, a
// wrapper that sets HOME=$PWD — silently gives every environment a CA of its own.
// All of them are spelled ~/.cortex/ca and all carry CN=authbridge-tls-bridge-ca,
// so nothing in a log line or a directory listing says which one a client holds.
//
// The comparison is on CERTIFICATES, not paths, and these tests pin why. Comparing
// directories looked equivalent and was not: `prior` holds whatever the user's
// settings had before `abctl claude-code enable`, which is often not a Cortex CA at
// all, and abctl freezes that record on first write — so a path comparison both
// fired on innocent setups and could never go quiet again.

// writeTestCA persists a self-signed CA at path with the given CN and returns its
// PEM, so a test can build both "one of ours" and "somebody else's" anchors.
func writeTestCA(t *testing.T, path, commonName string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()), // distinct per call
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if path != "" {
		if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return pemBytes
}

// TestStaleClientCAWarning_FiresOnADifferentBridgeCA is the real-world case from the
// issue: the client's anchor is one of ours, but not the one now in force.
func TestStaleClientCAWarning_FiresOnADifferentBridgeCA(t *testing.T) {
	dir := t.TempDir()
	clientCA := filepath.Join(dir, "client-ca.crt")
	writeTestCA(t, clientCA, "authbridge-tls-bridge-ca")
	inForce := writeTestCA(t, "", "authbridge-tls-bridge-ca")

	got := staleClientCAWarning("/Users/dev/sandbox/proj/.cortex/ca", clientCA, inForce)

	if got == nil {
		t.Fatal("no warning for a client holding a different bridge CA; this is the state " +
			"that produces 'Self-signed certificate detected' against a healthy proxy")
	}
	joined := fmt.Sprint(got...)
	// Both fingerprints must appear, since telling the two CAs apart is the entire
	// point — their subjects are identical.
	for _, want := range []string{clientCA, "client_ca_fingerprint", "ca_fingerprint"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warning omitted %q: %v", want, got)
		}
	}
}

// TestStaleClientCAWarning_SilentWhenClientHoldsTheCAInForce is the case a path
// comparison could never reach. abctl records `prior` on the FIRST enable only and
// refuses to overwrite it, so the recorded path stays pointing at the old location
// forever. Once the client actually trusts the current CA the warning must stop,
// or it fires on every boot for the rest of the install's life and teaches the user
// to ignore it.
func TestStaleClientCAWarning_SilentWhenClientHoldsTheCAInForce(t *testing.T) {
	dir := t.TempDir()
	clientCA := filepath.Join(dir, "ca.crt")
	// Same bytes on both sides: the recorded path is stale, the CONTENT is current.
	inForce := writeTestCA(t, clientCA, "authbridge-tls-bridge-ca")

	if got := staleClientCAWarning("/some/other/.cortex/ca", clientCA, inForce); got != nil {
		t.Errorf("warned about a client that already trusts the CA in force, from a stale "+
			"recorded path — this warning can never go quiet: %v", got)
	}
}

// TestStaleClientCAWarning_SilentForAForeignCA is the corporate-proxy false positive.
// `prior` is whatever was in the user's settings before enable, which behind a
// corporate proxy is routinely a system root bundle. A directory comparison fired on
// that — a setup that was never broken — and then advised `--ca-dir /etc/ssl/certs`,
// which would point the bridge's generate path at a system directory.
func TestStaleClientCAWarning_SilentForAForeignCA(t *testing.T) {
	dir := t.TempDir()
	inForce := writeTestCA(t, "", "authbridge-tls-bridge-ca")

	corporate := filepath.Join(dir, "corp-root.pem")
	writeTestCA(t, corporate, "ACME Corporate Root CA")
	if got := staleClientCAWarning("/Users/dev/.cortex/ca", corporate, inForce); got != nil {
		t.Errorf("warned about a corporate root CA, and would advise pointing --ca-dir at "+
			"a system directory: %v", got)
	}

	// Not a certificate at all, and a path that does not exist: silence, not a claim.
	garbage := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(garbage, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{garbage, filepath.Join(dir, "absent.crt")} {
		if got := staleClientCAWarning("/Users/dev/.cortex/ca", p, inForce); got != nil {
			t.Errorf("staleClientCAWarning(%q) = %v, want nil", p, got)
		}
	}
}

// TestStaleClientCAWarning_SilentWithoutInputs: no record and no loaded CA are both
// ordinary states (a first install, a bridge that is off). Neither is evidence.
func TestStaleClientCAWarning_SilentWithoutInputs(t *testing.T) {
	inForce := writeTestCA(t, "", "authbridge-tls-bridge-ca")
	if got := staleClientCAWarning("/Users/dev/.cortex/ca", "", inForce); got != nil {
		t.Errorf("warned with no prior record: %v", got)
	}
	dir := t.TempDir()
	clientCA := filepath.Join(dir, "ca.crt")
	writeTestCA(t, clientCA, "authbridge-tls-bridge-ca")
	if got := staleClientCAWarning("/Users/dev/.cortex/ca", clientCA, nil); got != nil {
		t.Errorf("warned with no CA in force: %v", got)
	}
}

// The openssl-parity test for this rendering lives with the implementation, in
// core/tlsbridge (TestFingerprintSHA256_MatchesOpenSSL). It used to be duplicated
// here against a local copy of the byte loop; both collapsed into the shared helper.

// clientCAFromState reads the CA the client is configured with RIGHT NOW, which is
// the only value that can justify this warning. These tests pin why `prior` cannot:
// abctl snapshots `prior` from the settings env block BEFORE overwriting it in the
// same run (cmd_claudecode.go:468-478), so it is what abctl displaced, never what
// the client holds. In the actual #1033 repro that makes it doubly wrong — the run
// that put a foreign CA into `prior` also pointed the client at the correct one.

// writeStateAndSettings lays down abctl's two files: the state record naming a
// settings path, and that settings file with the given current CA value. Passing
// currentCA == "" writes an env block with no NODE_EXTRA_CA_CERTS at all.
func writeStateAndSettings(t *testing.T, dir, priorCA, currentCA string) string {
	t.Helper()
	settingsPath := filepath.Join(dir, "settings.json")
	env := map[string]any{"HTTPS_PROXY": "http://127.0.0.1:47600"}
	if currentCA != "" {
		env["NODE_EXTRA_CA_CERTS"] = currentCA
	}
	settings, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
		t.Fatal(err)
	}
	prior := map[string]any{"HTTPS_PROXY": nil}
	if priorCA != "" {
		prior["NODE_EXTRA_CA_CERTS"] = priorCA
	} else {
		prior["NODE_EXTRA_CA_CERTS"] = nil
	}
	state, err := json.Marshal(map[string]any{"settings": settingsPath, "prior": prior})
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "claude-code-state.json")
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	return statePath
}

// TestClientCAFromState_ReadsTheCurrentValueNotPrior is the correctness of the whole
// warning. `prior` and the live value differ on every enable; reading the wrong one
// makes the warning fire when the client is already right.
func TestClientCAFromState_ReadsTheCurrentValueNotPrior(t *testing.T) {
	dir := t.TempDir()
	statePath := writeStateAndSettings(t, dir,
		"/old/sandbox/.cortex/ca/ca.crt", // what abctl displaced
		"/current/.cortex/ca/ca.crt")     // what the client actually loads now

	got := clientCAFromState(statePath)

	if got == "/old/sandbox/.cortex/ca/ca.crt" {
		t.Fatal("read `prior` — abctl's record of what it DISPLACED. The same enable that " +
			"wrote that also pointed the client at the current CA, so this warns about a " +
			"client that is already correct, and can never go quiet")
	}
	if got != "/current/.cortex/ca/ca.crt" {
		t.Errorf("clientCAFromState = %q, want the live NODE_EXTRA_CA_CERTS", got)
	}
}

// TestClientCAFromState_ToleratesEveryFailure: this drives a diagnostic, so every
// broken shape must answer "" rather than error, panic, or guess. A state file or
// settings file that cannot be read is not a reason to fail a proxy boot.
func TestClientCAFromState_ToleratesEveryFailure(t *testing.T) {
	dir := t.TempDir()

	// A state file whose settings path does not exist.
	missing := filepath.Join(dir, "missing-settings.json")
	state, _ := json.Marshal(map[string]any{"settings": filepath.Join(dir, "nope.json")})
	if err := os.WriteFile(missing, state, 0o600); err != nil {
		t.Fatal(err)
	}

	// Settings present, but no NODE_EXTRA_CA_CERTS in it (abctl not enabled here).
	noKey := writeStateAndSettings(t, t.TempDir(), "", "")

	cases := map[string]string{
		"absent state":     filepath.Join(dir, "absent.json"),
		"settings missing": missing,
		"no CA key":        noKey,
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if got := clientCAFromState(p); got != "" {
				t.Errorf("clientCAFromState(%s) = %q, want \"\"", name, got)
			}
		})
	}

	// Malformed JSON on either side.
	for name, body := range map[string]string{
		"state not json":   "{{{",
		"no settings key":  `{"prior":{"NODE_EXTRA_CA_CERTS":"/x/ca.crt"}}`,
		"settings not str": `{"settings":42}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name+".json")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := clientCAFromState(p); got != "" {
				t.Errorf("clientCAFromState(%s) = %q, want \"\"", name, got)
			}
		})
	}

	// Settings file that is not JSON at all.
	badSettings := filepath.Join(dir, "bad-settings.json")
	if err := os.WriteFile(badSettings, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := json.Marshal(map[string]any{"settings": badSettings})
	p := filepath.Join(dir, "points-at-bad.json")
	if err := os.WriteFile(p, st, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := clientCAFromState(p); got != "" {
		t.Errorf("clientCAFromState with unparseable settings = %q, want \"\"", got)
	}
}

// TestStaleClientCAWarning_GoesQuietAfterTheClientIsFixed is the reflex-training
// failure, end to end through the real files. A client pointed at another $HOME's CA
// warns; once its settings name the CA in force, the warning stops — even though
// abctl's frozen `prior` still names the old path forever.
func TestStaleClientCAWarning_GoesQuietAfterTheClientIsFixed(t *testing.T) {
	dir := t.TempDir()
	otherCA := filepath.Join(dir, "other-ca.crt")
	writeTestCA(t, otherCA, "authbridge-tls-bridge-ca")
	currentCAPath := filepath.Join(dir, "current-ca.crt")
	inForce := writeTestCA(t, currentCAPath, "authbridge-tls-bridge-ca")

	// Misconfigured: settings point at another $HOME's bridge CA.
	statePath := writeStateAndSettings(t, dir, "/whatever/prior.crt", otherCA)
	if got := staleClientCAWarning("/current/.cortex/ca", clientCAFromState(statePath), inForce); got == nil {
		t.Fatal("no warning while the client is genuinely pointed at a different bridge CA")
	}

	// Fixed: settings now name the CA in force. `prior` is unchanged and still stale.
	statePath = writeStateAndSettings(t, dir, "/whatever/prior.crt", currentCAPath)
	if got := staleClientCAWarning("/current/.cortex/ca", clientCAFromState(statePath), inForce); got != nil {
		t.Errorf("still warning after the client was fixed — this is the every-boot-forever "+
			"failure the comparison exists to avoid: %v", got)
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
// The default alone could not deliver that WHEN THIS WAS WRITTEN, and that is now the weaker
// half of the guarantee. LedgerEnabled(defaultOn) was asked with defaultOn = localMode,
// localMode is set only by --local, and a config with no cost_ledger block fell through to
// false — so "on by default" was true only for a hand-run `authbridge-proxy --local`, and the
// installed service had it off for its whole life, silently.
//
// ledgerDefaultOn has since replaced that rule with one keyed on whether anything survives a
// restart — an explicit cost_ledger.dir, or a config inside ~/.cortex, which is where this
// generated file lives — so this block is no longer the mechanism.
// The test stays, and stays written with defaultOn = FALSE, because it now pins something
// different and still worth pinning: that the generated config states the intent explicitly
// rather than relying on a derivation, so a future change to that derivation cannot silently
// turn an installed laptop's ledger off again. Passing true here would assert the derivation
// instead and pass with no cost_ledger block at all.
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

// TestBuiltinConfig_CommentedRetentionDoesNotTruncateAMonth.
//
// The generated config carries retention_days COMMENTED OUT, as a worked example of the value
// an operator would set. That makes it a trap the type system cannot see: the line is inert
// until someone uncomments it, and if the number in it is below what window=month needs, doing
// so silently truncates month-to-date totals. A pruned day file is ABSENT rather than
// unreadable, so it produces no Caveats entry and the short answer discloses nothing.
//
// It was 30 while costledger's default moved to 31 — one day short, which is exactly the
// shortfall that default exists to prevent, sitting in the file we hand people to edit.
//
// Asserted against usage.WindowMonthLocalDays rather than against a literal, so the example and
// the window it has to satisfy cannot drift apart again.
func TestBuiltinConfig_CommentedRetentionDoesNotTruncateAMonth(t *testing.T) {
	cortexDir := t.TempDir()
	p, err := writeBuiltinConfig(cortexDir, filepath.Join(cortexDir, "ca"))
	if err != nil {
		t.Fatalf("writeBuiltinConfig: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// THE SKIP IS GATED ON THE SETTING BEING ABSENT, not on this pattern matching.
	//
	// t.Skip when the regexp misses turns any reformatting of that comment — a different indent, a
	// quoted value, the line wrapped — into a silently passing no-op, on exactly the assertion that
	// keeps the generated config from suggesting a retention that truncates window=month. So
	// absence is checked separately and coarsely: if "retention_days" appears anywhere, the example
	// exists and a pattern that cannot read it is this test's own bug, reported as a failure. Only
	// a file that never mentions it has nothing to uncomment.
	re := regexp.MustCompile(`(?m)^\s*#\s*retention_days:\s*(\d+)`)
	m := re.FindSubmatch(raw)
	if m == nil {
		if !bytes.Contains(raw, []byte("retention_days")) {
			t.Skip("the generated config carries no retention_days example at all")
		}
		t.Fatalf("the generated config mentions retention_days but %v does not match it, so this "+
			"test would have passed while checking nothing. Config:\n%s", re, raw)
	}
	got, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("unparseable retention_days example %q: %v", m[1], err)
	}
	if got < usage.WindowMonthLocalDays {
		t.Errorf("the generated config suggests retention_days: %d, but a month-to-date window "+
			"can touch %d local dates (usage.WindowMonthLocalDays). Uncommenting that line "+
			"would truncate window=month, and a pruned day file is absent rather than "+
			"unreadable — so nothing would disclose the shortfall.",
			got, usage.WindowMonthLocalDays)
	}
	// And it must clear the validator's floor, which config derives from the DATES a 7d window
	// can touch. Asserted against that constant rather than by building a Config and validating
	// it: Validate also requires listener fields this example says nothing about, so a minimal
	// fixture fails for reasons that have nothing to do with retention.
	if got < usage.Window7dLocalDays {
		t.Errorf("the generated config suggests retention_days: %d, below the %d a 7d window can "+
			"touch — config.Validate refuses a non-zero value under that floor, so uncommenting "+
			"this line would stop the proxy loading at all",
			got, usage.Window7dLocalDays)
	}
}
