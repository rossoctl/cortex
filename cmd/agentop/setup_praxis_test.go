package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// praxisBase is a config that praxis mode accepts once the block is added.
const praxisBase = "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  bind_loopback_only: true\n"

func loadText(t *testing.T, src string) *config.Config {
	t.Helper()
	cfg, loaded, err := loadCandidate(src)
	if err != nil || !loaded {
		t.Fatalf("does not load (err %v):\n%s", err, src)
	}
	return cfg
}

func TestEnablePraxisText_AppendsBlock(t *testing.T) {
	out, changed, err := enablePraxisText(praxisBase)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if out != praxisBase+"praxis:\n  enabled: true\n" {
		t.Errorf("out =\n%s", out)
	}
	if !loadText(t, out).PraxisEnabled() {
		t.Error("praxis mode is not on")
	}
}

func TestEnablePraxisText_NoTrailingNewline(t *testing.T) {
	out, _, err := enablePraxisText(strings.TrimSuffix(praxisBase, "\n"))
	if err != nil || out != praxisBase+"praxis:\n  enabled: true\n" {
		t.Errorf("err %v, out =\n%s", err, out)
	}
}

func TestEnablePraxisText_IndentedDocument(t *testing.T) {
	src := "  mode: proxy-sidecar\n  listener:\n    roles: [forward]\n    bind_loopback_only: true\n"
	out, _, err := enablePraxisText(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "\n  praxis:\n    enabled: true\n") {
		t.Errorf("out =\n%s", out)
	}
	if !loadText(t, out).PraxisEnabled() {
		t.Error("praxis mode is not on")
	}
}

func TestEnablePraxisText_InsertsEnabled(t *testing.T) {
	src := praxisBase + "praxis:\n  listen: 127.0.0.1:47605\n"
	out, changed, err := enablePraxisText(src)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	cfg := loadText(t, out)
	if !cfg.PraxisEnabled() || cfg.Praxis.Listen != "127.0.0.1:47605" {
		t.Errorf("praxis = %+v, out =\n%s", cfg.Praxis, out)
	}
	if strings.Contains(out, "# ") {
		t.Errorf("the insert added a comment line:\n%s", out)
	}
}

func TestEnablePraxis_ReplacesFalseKeepsComment(t *testing.T) {
	src := praxisBase + "praxis:\n  enabled: false # off for now\n"
	out, changed, err := enablePraxisText(src)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(out, "\n  enabled: true # off for now\n") {
		t.Errorf("out =\n%s", out)
	}
	if !loadText(t, out).PraxisEnabled() {
		t.Error("praxis mode is not on")
	}
}

// An enabled: key nested deeper than the block's own children is not the block's.
func TestEnablePraxisText_IgnoresNestedEnabled(t *testing.T) {
	src := praxisBase + "session:\n  enabled: false\npraxis:\n  listen: 127.0.0.1:47605\n"
	out, _, err := enablePraxisText(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session:\n  enabled: false\n") || !loadText(t, out).PraxisEnabled() {
		t.Errorf("out =\n%s", out)
	}
}

func TestEnablePraxisText_AlreadyOn(t *testing.T) {
	src := praxisBase + "praxis:\n  enabled: true\n"
	out, changed, err := enablePraxisText(src)
	if err != nil || changed || out != src {
		t.Errorf("changed=%v err=%v out=\n%s", changed, err, out)
	}
}

func TestEnablePraxisText_FlowStyleRefused(t *testing.T) {
	if _, _, err := enablePraxisText(praxisBase + "praxis: {enabled: false}\n"); err == nil {
		t.Error("a flow-style praxis block raised no error")
	}
}

func TestEnablePraxis_InvalidPraxisConfigLeavesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	src := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := enablePraxis(path); err == nil || !strings.Contains(err.Error(), "bind_loopback_only") {
		t.Errorf("err = %v, want the bind_loopback_only refusal", err)
	}
	if got, _ := os.ReadFile(path); string(got) != src {
		t.Errorf("the file changed:\n%s", got)
	}
}

func TestEnablePraxis_WritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(praxisBase), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := enablePraxis(path); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if changed, err := enablePraxis(path); err != nil || changed {
		t.Errorf("second run: changed=%v err=%v", changed, err)
	}
}

// praxisEnv is a configEnv with a current config (both migrations applied) and
// cortex-gateway in the bin dir.
func praxisEnv(t *testing.T, src string) (*setupEnv, string) {
	t.Helper()
	env := configEnv(t)
	writeExe(t, filepath.Join(env.binDir, "cortex-gateway"), "#!/bin/sh\n")
	if err := os.MkdirAll(env.cortexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExe(t, env.configPath(), src)
	if _, err := migrateConfig(env.configPath(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateBobPricing(env.configPath(), io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(env.configPath())
	if err != nil {
		t.Fatal(err)
	}
	return env, string(raw)
}

func TestConfigStep_PraxisKeepsEverythingElse(t *testing.T) {
	src := praxisBase + "pipeline:\n  outbound:\n    plugins:\n      - name: inference-parser\n" + routerBlock
	env, before := praxisEnv(t, src)
	env.opts.praxis = true
	p, prob := configStep{}.plan(env)
	if prob != nil || p.done || p.verb != "update" {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	if !env.configPinsPending {
		t.Error("the plan does not say the running proxy needs a restart")
	}
	detail, _, err := configStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "praxis mode on") || !env.configPinsChanged {
		t.Errorf("detail %q, restart %v", detail, env.configPinsChanged)
	}
	after, _ := os.ReadFile(env.configPath())
	if string(after) != before+"praxis:\n  enabled: true\n" {
		t.Errorf("before:\n%s\nafter:\n%s", before, after)
	}
	// A second run finds nothing to do.
	if p, prob := (configStep{}).plan(env); prob != nil || !p.done {
		t.Errorf("re-plan = %+v %v", p, prob)
	}
}

func TestConfigStep_NoPraxisFlagLeavesBlock(t *testing.T) {
	env, before := praxisEnv(t, praxisBase+"praxis:\n  enabled: true\n")
	if p, prob := (configStep{}).plan(env); prob != nil || !p.done {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	if after, _ := os.ReadFile(env.configPath()); string(after) != before {
		t.Errorf("the file changed:\n%s", after)
	}
}

func TestConfigStep_PraxisFlowStyleIsAProblem(t *testing.T) {
	env, _ := praxisEnv(t, praxisBase+"praxis: {enabled: false}\n")
	env.opts.praxis = true
	_, prob := configStep{}.plan(env)
	if prob == nil || !strings.Contains(strings.Join(prob.fix, " "), "enabled: true") {
		t.Errorf("problem = %+v", prob)
	}
}

func TestConfigStep_PraxisNeedsGateway(t *testing.T) {
	env := configEnv(t)
	env.fromDir = t.TempDir()
	// A gateway in the bin dir does not count when --from stages the binaries.
	writeExe(t, filepath.Join(env.binDir, "cortex-gateway"), "#!/bin/sh\n")
	env.opts.praxis = true
	_, prob := configStep{}.plan(env)
	if prob == nil || prob.reason != "cortex-gateway is not installed" ||
		!strings.Contains(strings.Join(prob.fix, " "), "sh -s -- --praxis") {
		t.Fatalf("problem = %+v", prob)
	}
	writeExe(t, filepath.Join(env.fromDir, "cortex-gateway"), "#!/bin/sh\n")
	if _, prob := (configStep{}).plan(env); prob != nil {
		t.Errorf("a staged gateway still raised %+v", prob)
	}
}

func TestConfigStep_PraxisFreshConfig(t *testing.T) {
	env := configEnv(t)
	writeExe(t, filepath.Join(env.binDir, "cortex-gateway"), "#!/bin/sh\n")
	env.opts.praxis = true
	if p, prob := (configStep{}).plan(env); prob != nil || p.verb != "write" {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(env.configPath())
	if err != nil || !cfg.PraxisEnabled() {
		t.Errorf("err %v, praxis %+v", err, cfg)
	}
}

// The Global Constraint: without --praxis, setup needs no cortex-gateway and
// writes no praxis block.
func TestSetup_NoGatewayDirectModeWorks(t *testing.T) {
	skipSceneInShort(t)
	sc := newSetupScene(t, ok200)
	code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	raw, err := os.ReadFile(filepath.Join(sc.home, ".cortex", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "praxis:") {
		t.Errorf("direct-mode setup wrote a praxis block:\n%s", raw)
	}
}

func TestPreflightPorts_PraxisProbesGatewayPorts(t *testing.T) {
	fakeSupervisor(t)
	env := newTestSetupEnv(t)
	env.configFresh = true
	freePorts(t)
	saved := portInUse
	portInUse = func(p string) bool { return p == "47605" }
	t.Cleanup(func() { portInUse = saved })

	if _, prob := (serviceStep{}).preflightPorts(env); prob != nil {
		t.Errorf("without --praxis: %+v", prob)
	}
	env.opts.praxis = true
	_, prob := serviceStep{}.preflightPorts(env)
	if prob == nil || !strings.Contains(prob.reason, "47605") {
		t.Errorf("problem = %+v", prob)
	}
}
