package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/tokenexchange"
)

func identityConfig(idType string) json.RawMessage {
	return json.RawMessage(`{"identity":{"type":"` + idType + `"}}`)
}

// TestSpiffeIdentityTypeMatchesTokenExchange guards against drift between
// main.go's local spiffeIdentityType constant and the token-exchange plugin's
// canonical SpiffeIdentity value. main.go intentionally does NOT import the
// plugin package (so token-exchange stays build-tag excludable via
// plugins_tokenexchange.go); this test re-couples the two at test time — a
// test file may import the package unconditionally without pulling it into the
// tag-gated production binary.
func TestSpiffeIdentityTypeMatchesTokenExchange(t *testing.T) {
	if spiffeIdentityType != tokenexchange.SpiffeIdentity {
		t.Errorf("spiffeIdentityType = %q but tokenexchange.SpiffeIdentity = %q; "+
			"the identity.type=spiffe convention has drifted — update spiffeIdentityType in main.go",
			spiffeIdentityType, tokenexchange.SpiffeIdentity)
	}
}

// TestSpiffeProviderNeeded pins the need-driven gate: the SPIFFE provider is
// built only when mTLS is on or a plugin selects the spiffe identity scheme.
// A bare `spiffe: {}` block with neither must NOT trigger it — that is what
// lets the TLS bridge (cert-manager CA, no SVID) boot without SPIRE.
func TestSpiffeProviderNeeded(t *testing.T) {
	outbound := func(entries ...config.PluginEntry) *config.Config {
		return &config.Config{Pipeline: config.PipelineConfig{
			Outbound: config.PipelineStageConfig{Plugins: entries},
		}}
	}
	inbound := func(entries ...config.PluginEntry) *config.Config {
		return &config.Config{Pipeline: config.PipelineConfig{
			Inbound: config.PipelineStageConfig{Plugins: entries},
		}}
	}

	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"empty config", &config.Config{}, false},
		{"mtls present", &config.Config{MTLS: &config.MTLSConfig{}}, true},
		{"token-exchange client-secret", outbound(config.PluginEntry{
			Name: "token-exchange", Config: identityConfig("client-secret"),
		}), false},
		{"token-exchange spiffe (outbound)", outbound(config.PluginEntry{
			Name: "token-exchange", Config: identityConfig("spiffe"),
		}), true},
		{"spiffe identity (inbound)", inbound(config.PluginEntry{
			Name: "some-plugin", Config: identityConfig("spiffe"),
		}), true},
		{"plugin with no config", outbound(config.PluginEntry{Name: "jwt-validation"}), false},
		{"malformed plugin config", outbound(config.PluginEntry{
			Name: "bad-plugin", Config: json.RawMessage(`{not valid json`),
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spiffeProviderNeeded(tt.cfg); got != tt.want {
				t.Errorf("spiffeProviderNeeded() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestProviderConsumersCoveredByPredicate enforces the invariant that ties
// spiffeProviderNeeded to plugins.BuildWithSPIFFE: the predicate detects a
// plugin's need via identity.type=spiffe, which covers token-exchange — the
// only spiffe.ProviderConsumer today. If a new ProviderConsumer is registered,
// this fails so the author confirms spiffeProviderNeeded detects its need;
// otherwise that plugin would silently receive a nil Provider on a SPIRE-less
// cluster. The main package blank-imports every plugin, so the registry here is
// the full production set.
func TestProviderConsumersCoveredByPredicate(t *testing.T) {
	// Plugins whose SPIFFE need spiffeProviderNeeded is known to detect.
	covered := map[string]bool{"token-exchange": true}
	for _, name := range plugins.SPIFFEConsumerPlugins() {
		// Tripwire: a new consumer must be consciously reviewed and listed.
		if !covered[name] {
			t.Errorf("plugin %q implements spiffe.ProviderConsumer but is not covered by "+
				"spiffeProviderNeeded; make it signal its need via identity.type=spiffe (or "+
				"extend the predicate), then add %q to this set", name, name)
		}
		// Functional: the predicate must actually fire for that consumer's
		// spiffe config, so it never receives a nil Provider.
		cfg := &config.Config{Pipeline: config.PipelineConfig{
			Outbound: config.PipelineStageConfig{Plugins: []config.PluginEntry{
				{Name: name, Config: identityConfig("spiffe")},
			}},
		}}
		if !spiffeProviderNeeded(cfg) {
			t.Errorf("spiffeProviderNeeded must return true for consumer %q with identity.type=spiffe", name)
		}
	}
}

// captureWarns runs fn with a logger recording WARN records as JSON lines.
func captureWarns(t *testing.T, fn func(*slog.Logger)) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	fn(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// A cost ledger that is on by DEFAULT and unreachable because sessions are off must
// say so. This is the residue of the same defect config.Validate now refuses in its
// explicit form: the whole ledger block in main is nested inside
// `if cfg.Session.SessionEnabled()`, so with sessions off the operator got no error,
// no warning, and no log line naming the ledger at all — an empty `abctl usage`
// history with nothing anywhere to explain it.
//
// Local mode is the case that matters: --local turns the ledger on without anyone
// writing cost_ledger into the config, so this is the one combination that is
// legitimate, silent, and wrong.
func TestWarnCostLedgerNeedsSessions_LocalDefaultOn(t *testing.T) {
	off := false
	recs := captureWarns(t, func(l *slog.Logger) {
		warnCostLedgerNeedsSessions(&config.Config{
			Mode:    config.ModeProxySidecar,
			Session: config.SessionConfig{Enabled: &off},
		}, true, l)
	})
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 warn, got %d: %#v", len(recs), recs)
	}
	msg, _ := recs[0]["msg"].(string)
	if !strings.Contains(msg, "cost ledger") {
		t.Errorf("warn must name the ledger, got %q", msg)
	}
	// Both settings, because the fix is a decision between them and a message naming
	// one would send the reader to the wrong file.
	for _, key := range []string{"cost_ledger.enabled", "session.enabled"} {
		if _, ok := recs[0][key]; !ok {
			t.Errorf("warn must name %q; got keys %v", key, recs[0])
		}
	}
}

// It must stay quiet when the ledger was not going to run anyway — otherwise every
// in-cluster deployment with sessions off logs a warning about a feature it never
// asked for, and the line stops meaning anything.
func TestWarnCostLedgerNeedsSessions_SilentWhenLedgerIsOff(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name      string
		cfg       *config.Config
		defaultOn bool
	}{
		{"no durable location, so the ledger was never going to run", &config.Config{Mode: config.ModeProxySidecar, Session: config.SessionConfig{Enabled: &off}}, false},
		{"explicitly off, so its absence is a choice", &config.Config{Mode: config.ModeProxySidecar, Session: config.SessionConfig{Enabled: &off}, CostLedger: &config.CostLedgerConfig{Enabled: &off}}, true},
		// Sessions on is the normal --local shape: the ledger runs, so there is
		// nothing to warn about. This is what makes the call site in main safe to
		// make unconditional instead of hiding it down one arm of an if.
		{"sessions on, which is the ordinary shape", &config.Config{Mode: config.ModeProxySidecar}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if recs := captureWarns(t, func(l *slog.Logger) {
				warnCostLedgerNeedsSessions(tc.cfg, tc.defaultOn, l)
			}); len(recs) != 0 {
				t.Errorf("expected silence, got %#v", recs)
			}
		})
	}
}

// THE DEFAULT IS KEYED ON DURABILITY, NOT ON WHICH FLAG STARTED THE BINARY.
//
// It used to be localMode: true under --local, false otherwise. That is a property of the
// command line, not of whether the ledger can do its job, and the consequence was measured —
// `abctl service install` runs `--config`, never `--local`, so the documented "on by default"
// was false for every installed laptop and cost history was silently never written.
//
// The rule now asks whether anything survives a restart. The third row is the one that has to
// stay OFF: a container with no volume can only write to a layer that is discarded on restart,
// so the ledger would pay its whole cost and keep nothing — and at a measured 36 MB to 1.2 GB
// per 30 days it would do that against an ephemeral-storage limit, where exceeding it evicts
// the pod rather than losing a figure.
func TestLedgerDefaultOn_NeedsEvidenceOfSomewhereDurable(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
		home string
		// configIn names where this process was started from, relative to $HOME: ".cortex" is a
		// local install, anything else is not. Empty means no --config at all.
		configIn string
		// installedDir creates <home>/.cortex without starting from it, which is the leftover case.
		installedDir bool
		want         bool
		wantWhy      string
	}{
		{
			// The case that regressed: a service install, no dir named, real home. It runs with
			// --config pointing into ~/.cortex, which is what identifies it — localMode is false.
			// --local reaches this same row, because it writes that file and points --config at it.
			name: "started from a local install's config", home: t.TempDir(), configIn: ".cortex",
			want: true, wantWhy: "inside a local install",
		},
		{
			// The case the HOME-only rule got wrong: os.UserHomeDir succeeds in almost every
			// container, so this row was ON and writing day files to ephemeral storage. The
			// directory-exists rule got it wrong too — see the next row.
			name: "container with a home and a config elsewhere", home: t.TempDir(), configIn: "etc",
			want: false, wantWhy: "not inside",
		},
		{
			// A LEFTOVER ~/.cortex is not a decision. The directory-exists rule turned this on,
			// which is 30-day disk writes bought by a stale directory in an image.
			name: "leftover .cortex but started from elsewhere", home: t.TempDir(),
			configIn: "etc", installedDir: true,
			want: false, wantWhy: "not inside",
		},
		{
			// Kubernetes done properly: the operator mounted a volume and named it.
			name: "explicit dir and no home", dir: "/var/lib/cortex/cost",
			want: true, wantWhy: "cost_ledger.dir",
		},
		{
			// A container with neither. The only row that must be off.
			name: "no dir and no home", want: false, wantWhy: "discarded on restart",
		},
		{
			name: "both, dir decides", dir: "/var/lib/cortex/cost", home: t.TempDir(),
			want: true, wantWhy: "cost_ledger.dir",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// HOME drives os.UserHomeDir on the platforms this runs on; empty makes it
			// fail, which is the container-with-no-home case.
			t.Setenv("HOME", tc.home)
			var configPath string
			switch tc.configIn {
			case ".cortex":
				configPath = filepath.Join(tc.home, cortexDirName, localConfigName)
			case "etc":
				configPath = filepath.Join(t.TempDir(), "authbridge", "config.yaml")
			}
			if tc.installedDir {
				if merr := os.MkdirAll(filepath.Join(tc.home, cortexDirName), 0o700); merr != nil {
					t.Fatalf("seed the leftover directory: %v", merr)
				}
			}
			cfg := &config.Config{Mode: config.ModeProxySidecar}
			if tc.dir != "" {
				cfg.CostLedger = &config.CostLedgerConfig{Dir: tc.dir}
			}

			got, why := ledgerDefaultOn(cfg, configPath)
			if got != tc.want {
				t.Errorf("ledgerDefaultOn() = %v, want %v (reason given: %q)", got, tc.want, why)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("reason = %q, want it to mention %q — the log line quotes this, so a "+
					"vague reason sends an operator to main.go", why, tc.wantWhy)
			}
		})
	}
}

// An explicit cost_ledger.enabled still wins in BOTH directions, so the derived default is a
// default and not a policy. The false case is the escape hatch for a full or read-only disk;
// the true case is a filesystem the operator knows persists even though this process cannot
// tell (a container whose HOME is on a volume mounted over it, say).
func TestLedgerEnabled_ExplicitSettingOverridesTheDerivedDefault(t *testing.T) {
	on, off := true, false
	t.Setenv("HOME", "") // derived default would be OFF here
	cfg := &config.Config{Mode: config.ModeProxySidecar, CostLedger: &config.CostLedgerConfig{Enabled: &on}}
	if !cfg.CostLedger.LedgerEnabled(ledgerDefaultOnValue(cfg, "")) {
		t.Error("enabled: true did not override a derived default of off")
	}
	home := t.TempDir()
	t.Setenv("HOME", home) // derived default would be ON here
	cfg = &config.Config{Mode: config.ModeProxySidecar, CostLedger: &config.CostLedgerConfig{Enabled: &off}}
	if cfg.CostLedger.LedgerEnabled(ledgerDefaultOnValue(cfg, "")) {
		t.Error("enabled: false did not override a derived default of on")
	}
}

// TestMain_WiresTheLedgerAndFlushesItOnFatalPaths is a source-shaped guard, and says so.
//
// Nothing here runs main(), so review is right that the wiring has no behavioural test: deleting the
// whole ledger block from main.go left this package green. What CAN be checked cheaply is that the
// four things the wiring consists of are present, so a deletion or a half-revert fails rather than
// passing silently:
//
//   - the ledger is constructed (costledger.New),
//   - it is registered on the session store, which is the only way events reach it,
//   - a fatal exit flushes it (closeLedgerOnFatal assigned),
//   - and no fatal site after that point still calls log.Fatalf directly, which would skip the flush
//     because os.Exit runs no deferred functions.
//
// A behavioural test would need main() split into a runnable unit; that is worth doing and is not
// this change.
func TestMain_WiresTheLedgerAndFlushesItOnFatalPaths(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var newLedger, addRecorder bool
	var flushPos token.Pos
	var directFatal []string
	// inFatalf skips the one log.Fatalf that is allowed: the one inside fatalf itself.
	var fatalfDecl *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "fatalf" {
			fatalfDecl = fd
		}
	}
	if fatalfDecl == nil {
		t.Fatal("no fatalf function: the fatal paths cannot be flushing the ledger")
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "closeLedgerOnFatal" && flushPos == 0 {
					flushPos = v.Pos()
				}
			}
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, _ := sel.X.(*ast.Ident)
			switch {
			case pkg != nil && pkg.Name == "costledger" && sel.Sel.Name == "New":
				newLedger = true
			case sel.Sel.Name == "AddRecorder":
				for _, a := range v.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == "costLedger" {
						addRecorder = true
					}
				}
			case pkg != nil && pkg.Name == "log" && (sel.Sel.Name == "Fatalf" ||
				sel.Sel.Name == "Fatal" || sel.Sel.Name == "Fatalln"),
				pkg != nil && pkg.Name == "os" && sel.Sel.Name == "Exit":
				// EVERY SPELLING, not just Fatalf. The first version of this guard matched Fatalf
				// alone and passed with a live log.Fatal sitting after the ledger opened — the exact
				// half-revert it was written to catch, already present in the file it guards.
				//
				// Only after the ledger can be open: before that there is nothing to flush, and main
				// runs top to bottom, so source order is execution order for this question.
				inFatalf := v.Pos() >= fatalfDecl.Pos() && v.Pos() <= fatalfDecl.End()
				if !inFatalf && flushPos != 0 && v.Pos() > flushPos {
					directFatal = append(directFatal,
						fset.Position(v.Pos()).String()+" ("+pkg.Name+"."+sel.Sel.Name+")")
				}
			}
		}
		return true
	})

	if !newLedger {
		t.Error("main.go never calls costledger.New: the ledger is not constructed")
	}
	if !addRecorder {
		t.Error("main.go never passes costLedger to AddRecorder: nothing would reach the ledger, and every window=today would read an empty file")
	}
	if flushPos == 0 {
		t.Fatal("closeLedgerOnFatal is never assigned: a fatal startup error would discard the open minute, and the check below has no anchor")
	}
	for _, pos := range directFatal {
		t.Errorf("%s exits without flushing the ledger: os.Exit runs no deferred functions, so the open minute is lost — use fatalf", pos)
	}

	// AND fatalf HAS TO ACTUALLY FLUSH, which this guard did not check: gutting it to a bare
	// log.Fatalf, or assigning closeLedgerOnFatal = func(){}, kept every assertion above green.
	var callsFlush bool
	ast.Inspect(fatalfDecl, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "closeLedgerOnFatal" {
				callsFlush = true
			}
		}
		return true
	})
	if !callsFlush {
		t.Error("fatalf never calls closeLedgerOnFatal: it is log.Fatalf under another name, and every site above loses its minute")
	}

	// And the closure assigned to it has to close the ledger, not merely exist.
	var closesLedger bool
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); !ok || id.Name != "closeLedgerOnFatal" {
			return true
		}
		ast.Inspect(as.Rhs[0], func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" {
					closesLedger = true
				}
			}
			return true
		})
		return true
	})
	if !closesLedger {
		t.Error("the closure assigned to closeLedgerOnFatal never calls Close: the flush is a no-op")
	}
}
