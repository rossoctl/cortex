package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
)

// The policy is the whole of #1122's behaviour change, and it is deliberately separate
// from the loop so it can be driven through a run of failures in a test rather than in
// an hour of wall clock. Each step asserts both halves: the wait, and what gets said.
func TestRestartPolicy_BindFailuresExplainOnceThenSlowDown(t *testing.T) {
	p := &restartPolicy{delay: superviseRestartDelay}

	// The first bind failure is the one that has to speak. Before this change the log
	// said only "proxy exited; restarting", which is what left #1122's reporter with
	// 2,880 lines a day and no cause among them.
	first := p.exited(true, false)
	if !first.explain {
		t.Error("the first bind failure says nothing; the condition would never be stated")
	}
	if first.quiet || first.slowed {
		t.Errorf("first = %+v, want explain alone", first)
	}
	if first.delay != superviseRestartDelay {
		t.Errorf("first delay = %v, want %v — a clash racing the old proxy's exit must retry fast", first.delay, superviseRestartDelay)
	}

	// The second is the same condition. Saying it again adds nothing, and the retry
	// still has to be quick: this is where a restart racing a stop recovers.
	second := p.exited(true, false)
	switch {
	case second.explain:
		t.Error("the condition is explained twice")
	case !second.quiet:
		t.Error("second = not quiet; the per-attempt line stays at WARN and buries the ERROR")
	case second.slowed:
		t.Error("slowed on attempt 2; a brief clash would be treated as a held address")
	case second.delay != 2*superviseRestartDelay:
		t.Errorf("second delay = %v, want %v", second.delay, 2*superviseRestartDelay)
	}

	// The third is a held address rather than a race, so the ceiling moves — announced
	// once, because the log is about to go quiet for ten minutes at a time.
	third := p.exited(true, false)
	if !third.slowed {
		t.Fatalf("third = %+v, want slowed at %d attempts", third, superviseBlockedEscalate)
	}
	if third.attempts != superviseBlockedEscalate {
		t.Errorf("attempts = %d, want %d", third.attempts, superviseBlockedEscalate)
	}

	// From here the backoff climbs past the ordinary ceiling to the blocked one, and
	// nothing more is said. The old ceiling is what made this a 30-second heartbeat.
	for i := 0; i < 12; i++ {
		n := p.exited(true, false)
		if n.explain || n.slowed {
			t.Fatalf("attempt %d = %+v, want silence", n.attempts, n)
		}
		if !n.quiet {
			t.Fatalf("attempt %d is not quiet", n.attempts)
		}
		if n.delay > superviseBlockedMaxDelay {
			t.Fatalf("attempt %d waits %v, past the ceiling of %v", n.attempts, n.delay, superviseBlockedMaxDelay)
		}
	}
	if p.delay != superviseBlockedMaxDelay {
		t.Errorf("delay settled at %v, want the blocked ceiling %v", p.delay, superviseBlockedMaxDelay)
	}
}

// The address being freed is the transition nobody can read off the log, because after
// the slowdown there is nothing in it for ten minutes at a stretch. A child that
// survives long enough has to both reset the backoff and say so.
func TestRestartPolicy_HealthyRunRecoversAndResets(t *testing.T) {
	p := &restartPolicy{delay: superviseRestartDelay}
	for i := 0; i < superviseBlockedEscalate; i++ {
		p.exited(true, false)
	}

	if !p.healthy() {
		t.Error("healthy() = false after a run of bind failures; the recovery goes unannounced")
	}
	if p.delay != superviseRestartDelay {
		t.Errorf("delay = %v after recovery, want %v", p.delay, superviseRestartDelay)
	}
	// A healthy run that was never blocked is the ordinary case and says nothing.
	if p.healthy() {
		t.Error("healthy() = true with nothing to recover from")
	}

	// And the next bind failure is a new run, which explains itself again: the user who
	// starts a second install a week later gets the line, not the silence.
	if n := p.exited(true, false); !n.explain || n.attempts != 1 {
		t.Errorf("after recovery, exited = %+v, want a fresh explained failure", n)
	}
}

// A ten-minute wait is justified only for the one condition the supervisor cannot fix.
// Any other failure has to drop back under the ordinary ceiling, or a crash loop that
// happens to follow a port clash would be retried once an hour.
func TestRestartPolicy_OtherFailuresLeaveTheSlowdown(t *testing.T) {
	p := &restartPolicy{delay: superviseRestartDelay}
	for i := 0; i < 12; i++ {
		p.exited(true, false)
	}
	if p.delay <= superviseMaxDelay {
		t.Fatalf("delay = %v, want the blocked ceiling before this case means anything", p.delay)
	}

	n := p.exited(false, false)
	if n.delay > superviseMaxDelay {
		t.Errorf("a crash after a port clash waits %v, want at most %v", n.delay, superviseMaxDelay)
	}
	if n.explain || n.slowed || n.quiet {
		t.Errorf("n = %+v, want an ordinary restart line", n)
	}
	if n.attempts != 0 {
		t.Errorf("attempts = %d, want 0 — this exit was not a bind failure", n.attempts)
	}
	// The run of failures is over, so the next bind failure explains itself.
	if again := p.exited(true, false); !again.explain {
		t.Error("the condition is not re-explained after an unrelated exit")
	}
}

// A proxy that serves for hours between crashes is not what a backoff is for: the
// healthy timer has already reset the delay, and doubling it again from the exit would
// put a once-a-day crash on a growing wait for no reason.
func TestRestartPolicy_HealthyRunIsNotBackedOff(t *testing.T) {
	p := &restartPolicy{delay: superviseRestartDelay}
	p.healthy()
	if n := p.exited(false, true); n.delay != superviseRestartDelay {
		t.Errorf("delay = %v, want %v", n.delay, superviseRestartDelay)
	}
	if p.delay != superviseRestartDelay {
		t.Errorf("delay advanced to %v after a healthy run", p.delay)
	}
}

// The exit status is the entire protocol between the child and the supervisor. Reading
// it wrong in either direction is a real failure: a missed 3 restores the silent loop,
// and a false positive would dampen a genuine crash loop to one attempt per ten minutes.
func TestPortInUseExit(t *testing.T) {
	cases := []struct {
		name string
		code int
		want bool
	}{
		{"the status reportListenFailure leaves with", exitPortInUse, true},
		{"an ordinary failure", 1, false},
		{"a usage error", 2, false},
		{"a clean exit", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(tc.code)).Run()
			if tc.code == 0 && err != nil {
				t.Fatalf("sh exited %v for a clean run", err)
			}
			if tc.code != 0 {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("err = %v (%T), want an *exec.ExitError", err, err)
				}
			}
			if got := portInUseExit(err); got != tc.want {
				t.Errorf("portInUseExit(exit %d) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
	// A failure to start, or a signal, is not a status at all.
	if portInUseExit(errors.New("fork/exec: resource temporarily unavailable")) {
		t.Error("a non-exit error reads as a bind failure")
	}
	if portInUseExit(nil) {
		t.Error("a child that exited cleanly reads as a bind failure")
	}
}

// The ceilings are the numbers #1122 is about: 30s was 2,880 identical lines a day, and
// the slow one has to be long enough to matter yet short enough that an unattended
// machine recovers on its own.
func TestSuperviseDelays_AreOrdered(t *testing.T) {
	switch {
	case superviseRestartDelay >= superviseMaxDelay:
		t.Error("the first retry is not faster than the ceiling")
	case superviseMaxDelay >= superviseBlockedMaxDelay:
		t.Error("the blocked ceiling does not slow anything down")
	case superviseBlockedMaxDelay > 15*time.Minute:
		t.Errorf("superviseBlockedMaxDelay = %v; a freed address should not go unnoticed this long", superviseBlockedMaxDelay)
	case superviseBlockedEscalate < 2:
		t.Error("one bind failure escalates, so a restart racing a stop is treated as a held address")
	case superviseHealthyRun <= superviseRestartDelay:
		t.Error("a child counts as healthy sooner than the wait before starting it")
	}
}

// Direct mode is the default, and it must not change: one child, today's args and
// env, no gateway file and no lookup of the gateway binary. A praxis config that
// does not validate also runs cortex alone.
func TestSupervisorChildren_DirectModeHasOnlyCortex(t *testing.T) {
	args := []string{"--config", "/x/config.yaml", "-local"}
	noWrite := func(string, *config.Config) (string, string, error) {
		t.Error("direct mode wrote the gateway files")
		return "", "", nil
	}
	// Praxis on, but cortex refuses the config: a gateway would serve on 0.0.0.0.
	notValid := praxisConfig()
	notValid.Praxis.Listen = "0.0.0.0:47605"
	for name, cfg := range map[string]*config.Config{
		"praxis off":           {Praxis: &config.PraxisConfig{Enabled: false}},
		"no config":            nil,
		"praxis on, not valid": notValid,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			specs := supervisorChildren(cfg, filepath.Join(dir, "config.yaml"), "/bin/cortex", args, noWrite)
			if len(specs) != 1 || specs[0].name != "cortex" {
				t.Fatalf("specs = %+v, want only cortex", specs)
			}
			s := specs[0]
			if s.path != "/bin/cortex" || !slices.Equal(s.args, args) || s.env != nil || s.stdinPipe {
				t.Errorf("cortex spec = %+v, want today's path, args and inherited env", s)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Errorf("files in the config dir: %v", ents)
			}
		})
	}
}

// Gateway files that cannot be written leave cortex alone, with today's env. It must
// not end the supervisor: nothing would then restart anything before the next login.
func TestSupervisorChildren_WriteFailureRunsCortexAlone(t *testing.T) {
	failWrite := func(string, *config.Config) (string, string, error) {
		return "", "", errors.New("read-only dir")
	}
	specs := supervisorChildren(praxisConfig(), "/x/config.yaml", "/bin/cortex", nil, failWrite)
	if len(specs) != 1 || specs[0].name != "cortex" || specs[0].env != nil {
		t.Fatalf("specs = %+v, want only cortex with the inherited env", specs)
	}
}

func TestSupervisorChildren_PraxisAddsGateway(t *testing.T) {
	dir := t.TempDir()
	cfg := praxisConfig()
	specs := supervisorChildren(cfg, filepath.Join(dir, "config.yaml"), "/opt/bin/cortex", []string{"--config", "c"}, writeGatewayFiles)
	if len(specs) != 2 {
		t.Fatalf("specs = %+v, want cortex and the gateway", specs)
	}
	c, g := specs[0], specs[1]
	tokenEnv := gatewayTokenEnv + "=" + filepath.Join(dir, gatewayTokenName)
	if c.name != "cortex" || len(c.env) == 0 || c.env[len(c.env)-1] != tokenEnv {
		t.Errorf("cortex spec = %+v, want env ending in %s", c, tokenEnv)
	}
	wantArgs := []string{"-c", filepath.Join(dir, gatewayYAMLName), "--exit-on-stdin-eof"}
	if g.name != gatewayBinary || g.path != "/opt/bin/cortex-gateway" || !slices.Equal(g.args, wantArgs) {
		t.Errorf("gateway spec = %+v, want path /opt/bin/cortex-gateway and args %q", g, wantArgs)
	}
	if g.env == nil || len(g.env) != 0 || !g.stdinPipe {
		t.Errorf("gateway env = %#v, stdinPipe = %v; want an empty non-nil env and a stdin pipe", g.env, g.stdinPipe)
	}
	for _, n := range []string{gatewayTokenName, gatewayYAMLName} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

// The supervisor must read the config file the cortex child reads. With --local (or its
// --demo alias) and no --config, that is ~/.cortex/config.yaml.
func TestSupervisorConfigPath_LocalReadsTheChildsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := filepath.Join(home, cortexDirName, localConfigName)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-local"}, local},
		{[]string{"--local"}, local},
		{[]string{"--local=true"}, local},
		{[]string{"-local=1"}, local},
		{[]string{"--demo"}, local},
		{[]string{"-demo=true"}, local},
		{[]string{"--local=false"}, ""},
		{[]string{"--local", "--local=false"}, ""},
		{[]string{"--local=false", "-local"}, local},
		{[]string{"--config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{[]string{"-ca-dir", "/d"}, ""},
		{[]string{"local"}, ""},
		{nil, ""},
	} {
		if got := supervisorConfigPath(tc.args); got != tc.want {
			t.Errorf("supervisorConfigPath(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// The reviewer's repro: `cortex --local --supervise` with praxis.enabled in
// ~/.cortex/config.yaml runs the gateway beside cortex.
func TestSupervisorChildren_LocalPraxisAddsGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, cortexDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "mode: proxy-sidecar\nlistener:\n  session_api_addr: 127.0.0.1:47601\n  bind_loopback_only: true\n  reverse_proxy_backend: 127.0.0.1:8000\npraxis:\n  enabled: true\n"
	if err := os.WriteFile(filepath.Join(dir, localConfigName), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--local"}
	path := supervisorConfigPath(args)
	specs := supervisorChildren(supervisorConfig(path), path, "/opt/bin/cortex", args, writeGatewayFiles)
	if len(specs) != 2 || specs[1].name != gatewayBinary {
		t.Fatalf("specs = %+v, want cortex and the gateway", specs)
	}
	if _, err := os.Stat(filepath.Join(dir, gatewayTokenName)); err != nil {
		t.Errorf("gateway token not in ~/.cortex: %v", err)
	}
}

// The gateway must report the version of the cortex that runs it. Cortex checks no version.
func TestSupervisorChildren_GatewayWantsCortexVersion(t *testing.T) {
	specs := supervisorChildren(praxisConfig(), filepath.Join(t.TempDir(), "config.yaml"), "/opt/bin/cortex", nil, writeGatewayFiles)
	if len(specs) != 2 {
		t.Fatalf("specs = %+v, want cortex and the gateway", specs)
	}
	if specs[1].wantVersion != version || specs[0].wantVersion != "" {
		t.Errorf("wantVersion: cortex %q, gateway %q; want \"\" and %q", specs[0].wantVersion, specs[1].wantVersion, version)
	}
}

// runGatewaySpec runs superviseChild for 200 ms on a script that writes a marker file,
// with gatewayVersionOf replaced by one that returns got. It returns true when the
// script ran.
func runGatewaySpec(t *testing.T, got, want string) bool {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	script := filepath.Join(dir, "gateway.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\nexec cat >/dev/null\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := gatewayVersionOf
	gatewayVersionOf = func(string) string { return got }
	t.Cleanup(func() { gatewayVersionOf = orig })

	stop := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, func() { close(stop) })
	spec := childSpec{name: gatewayBinary, path: script, env: []string{}, stdinPipe: true, wantVersion: want}
	if err := superviseChild(spec, stop); err != nil {
		t.Fatalf("superviseChild: %v", err)
	}
	_, err := os.Stat(marker)
	return err == nil
}

func TestSuperviseChild_GatewayVersionMismatchDoesNotStart(t *testing.T) {
	if runGatewaySpec(t, "v0.0.1", "v9.9.9") {
		t.Error("the gateway started, but its version differs from cortex")
	}
}

func TestSuperviseChild_GatewayVersionMatchStarts(t *testing.T) {
	if !runGatewaySpec(t, "v9.9.9", "v9.9.9") {
		t.Error("the gateway did not start, but its version is the same as cortex")
	}
}
