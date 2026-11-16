package main

import (
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/cmd/agentop/updatestatus"
	"github.com/rossoctl/cortex/core/config"
)

// The checks the spec's doctor column names that the step plans do not make
// themselves. The rest are the plans': present and executable (binaries), loads
// (config), and unit installed, supervisor running and the stamp matching the
// installed cortex (service: its supervised done state is serviceIsCurrent, whose
// proxyBinaryUnchanged is the stamp check). /healthz is asked here as well: the
// unsupervised done state asks only whether the pid is alive.

// backgroundOnly reports whether Cortex is set up here as a background proxy with no
// unit, as setup --no-service leaves it, so doctor plans it as one: otherwise the
// service plan reads it as not started, and its fix would put a service in place
// of the mode the user chose.
func backgroundOnly(env *setupEnv) bool {
	sp, err := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, proxyBinary))
	if err != nil || serviceInstalled(sp) {
		return false
	}
	return fileExists(filepath.Join(env.cortexDir, proxyPIDFile))
}

// doctorRefine adds those checks to a step's done plan: a failed one makes the
// plan a problem, and a doubtful one gives it advice, unless it has its own. A
// plan with changes or a problem is left as it is, as setup is its fix either way;
// but the PATH lines setup would add get a second fix, the line to add by hand,
// for whoever set up with --no-modify-path to keep setup out of their dotfiles.
func doctorRefine(env *setupEnv, s step, p stepPlan, prob *problem, fix string) (stepPlan, *problem) {
	if _, ok := s.(pathStep); ok && prob == nil && !p.done {
		return p, &problem{reason: wouldChange(p), fix: []string{fix,
			"or add it yourself, in " + env.tilde(env.pathProfile) + ":  " + exportLine(env.binDir)}}
	}
	if prob != nil || !p.done || p.hidden {
		return p, prob
	}
	var bad, doubt *problem
	switch s.(type) {
	case binariesStep:
		bad, doubt = checkBinaries(env)
	case pathStep:
		if !env.binOnPath && p.advice == nil { // the marker, and so new terminals only
			doubt = &problem{reason: p.doneMsg}
		}
	case configStep:
		doubt = checkBridgeOn(env)
	case serviceStep:
		bad, doubt = checkServing(env)
	case claudeCodeStep:
		bad = checkTrustFiles(env, filepath.Join(env.home, settingsRel), fix)
	}
	if bad != nil {
		return p, bad
	}
	if doubt != nil && p.advice == nil {
		p.advice = doubt
	}
	return p, nil
}

// checkBinaries asks agentop and cortex for their versions, which must agree, as
// one install puts both there; and which agentop this shell runs.
func checkBinaries(env *setupEnv) (bad, doubt *problem) {
	cortex := filepath.Join(env.binDir, proxyBinary)
	a, c := env.installedVersion, installedVersion(cortex)
	switch {
	case a == "":
		bad = &problem{reason: "agentop did not report its version"}
	case c == "":
		bad = &problem{reason: "cortex did not report its version"}
	case a != c:
		bad = &problem{reason: "agentop is " + a + ", cortex is " + c}
	}
	if bad != nil {
		bad.fix = []string{installerOneLiner}
		return bad, nil
	}
	// Found nowhere is the PATH step's to report.
	self := filepath.Join(env.binDir, "agentop")
	if found, err := exec.LookPath("agentop"); err == nil && !samePath(found, self) {
		doubt = &problem{reason: "the agentop on PATH is " + env.tilde(found) + ", not " + env.tilde(self),
			fix: []string{"put " + env.tilde(env.binDir) + " ahead of " + env.tilde(filepath.Dir(found)) + " on PATH, or remove that agentop"}}
	}
	return nil, doubt
}

// checkBridgeOn advises when the config's TLS bridge is off: a deliberate
// setting, but Cortex then reads no HTTPS, so nothing it routes is seen.
func checkBridgeOn(env *setupEnv) *problem {
	cfg, err := config.Load(env.configPath())
	if err != nil || bridgeEnabled(cfg) {
		return nil
	}
	_, restart := restartCommands(env)
	return &problem{reason: "the TLS bridge is off, so Cortex reads no HTTPS traffic",
		fix: []string{"set tls_bridge.mode: enabled, with a ca_dir, in " + env.tilde(env.configPath()) + "; then " + restart}}
}

// checkServing asks the health endpoint the service step uses: /healthz must
// answer 2xx, as a background proxy's done state asks only that its pid is alive;
// and once it does, /readyz too, as a proxy that is up can still have a plugin
// waiting on a dependency, which /readyz names. A config that gives no health
// endpoint has nothing to ask.
func checkServing(env *setupEnv) (bad, doubt *problem) {
	health := resolveHealthURL(env.configPath())
	if health == "" {
		return nil, nil
	}
	if ok, _ := probe(health); !ok {
		setup, _ := restartCommands(env)
		return &problem{reason: "not answering " + health, fix: []string{setup}}, nil
	}
	ok, body := probe(strings.TrimSuffix(health, "/healthz") + "/readyz")
	if ok {
		return nil, nil
	}
	reason := "plugins not ready"
	if body != "" {
		reason += " — " + body
	}
	return nil, &problem{reason: reason, fix: []string{"see " + env.tilde(filepath.Join(env.cortexDir, "proxy.log")) + " for what it waits on"}}
}

// probe GETs url and reports whether it answered 2xx, with the first line of
// what it said.
func probe(url string) (bool, string) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url) //nolint:noctx // bounded by Timeout
	if err != nil {
		return false, ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	first, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return resp.StatusCode/100 == 2, first
}

// checkRoutedElsewhere checks the routing of a settings file enable's record
// names other than ~/.claude/settings.json, which setup's step does not write:
// routed while its HTTPS_PROXY is Cortex's, and then only if the CA files it names
// are there. Re-routing it is enable's job, for that file.
func checkRoutedElsewhere(env *setupEnv, ui *checklist.UI, settings, fix string) (failed bool) {
	if !cortexProxyIn(env, settings) {
		ui.Fail("routed", "Claude Code is not routed through Cortex in "+env.tilde(settings))
		ui.Remedy("fix: ", "agentop configure claude-code enable --settings "+env.shellPath(settings))
		return true
	}
	if prob := checkTrustFiles(env, settings, fix); prob != nil {
		return doctorCheck(ui, "routed", stepPlan{}, prob, fix)
	}
	ui.Done("routed", "Claude Code → Cortex · "+env.tilde(settings), 0)
	return false
}

// checkTrustFiles fails routing whose CA variables, in settings, name files that
// are not there: Claude Code then trusts nothing it can read. Cortex writes them
// as it starts, so the fix restarts it.
func checkTrustFiles(env *setupEnv, settings, fix string) *problem {
	doc, err := readSettings(settings)
	if err != nil {
		return nil
	}
	vals := envStrings(doc)
	var missing []string
	seen := map[string]bool{}
	for _, k := range append([]string{envCACerts}, bundleKeys...) {
		if v := vals[k]; v != "" && !seen[v] && !fileExists(v) {
			seen[v] = true
			missing = append(missing, env.tilde(v))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &problem{reason: "Claude Code's CA files are not there: " + strings.Join(missing, ", "),
		fix: []string{fix + " --restart"}}
}

// checkPython advises when cortex-session-dump, a python3 script, is installed
// and there is no python3 on PATH to run it.
func checkPython(env *setupEnv, ui *checklist.UI) {
	if !fileExists(filepath.Join(env.binDir, "cortex-session-dump")) {
		return
	}
	if _, err := exec.LookPath("python3"); err == nil {
		return
	}
	ui.Advise("python3", "cortex-session-dump needs python3, which is not on PATH")
}

// checkUpdates draws the row for automatic updates from ~/.cortex/update.json.
// Only a failed install is a failure.
func checkUpdates(env *setupEnv, ui *checklist.UI) (failed bool) {
	const label, fix = "updates", "see ~/.cortex/update.log"
	// Check the times first: the proxy refuses a bad value also when auto is off.
	u, err := readUpdateConfig(env.configPath())
	if err == nil {
		_, _, err = u.Times()
	}
	if err != nil {
		ui.Advise(label, err.Error(), "edit "+env.tilde(env.configPath()))
		return false
	}
	if autoUpdateOff(env.configPath()) {
		ui.Already(label, "automatic updates off (update.auto: false)")
		return false
	}
	if sp, err := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, proxyBinary)); err != nil || !serviceInstalled(sp) {
		ui.Already(label, "not automatic without the service; agentop update installs a release")
		return false
	}
	at := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }
	st, err := updatestatus.Read(filepath.Join(env.cortexDir, updatestatus.FileName))
	switch {
	case err != nil:
		ui.Advise(label, "no update check yet", fix)
	case st.Result == updatestatus.ResultFailed:
		ui.Fail(label, "update to "+st.Latest+" failed at "+at(st.CheckedAt), strings.Split(st.Error, "\n")...)
		ui.Remedy("fix: ", fix)
		return true
	case doctorNow().Sub(st.CheckedAt) > 72*time.Hour:
		ui.Advise(label, "no update check since "+at(st.CheckedAt), fix)
	case st.Result == updatestatus.ResultCheckFailed:
		first, _, _ := strings.Cut(st.Error, "\n")
		ui.Advise(label, "the last check failed at "+at(st.CheckedAt)+": "+first, fix)
	default:
		detail := "auto on · " + st.Result + " at " + at(st.CheckedAt)
		if st.Result == updatestatus.ResultWaiting {
			detail += " · installs " + st.Latest + " after " + at(st.InstallAfter)
		}
		ui.Already(label, detail)
	}
	return false
}
