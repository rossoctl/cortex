package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
)

// Once Claude Code's settings point at the proxy, every request goes there — so a
// proxy that is not running means Claude Code does not work. `nohup … &` survives
// neither a crash nor a logout, which makes "it worked yesterday" the normal
// experience after a reboot. These commands hand the process to the OS supervisor
// instead.
//
// User-scoped only, never a system daemon: this process holds the bridge CA's
// private key, and running it as root to gain "robustness" would be a bad trade.
const (
	launchdLabel = "io.rossoctl.cortex"
	systemdUnit  = "cortex.service"
	// maxLogBytes bounds one generation of proxy.log; rotateLog keeps one previous
	// file, so the pair tops out near twice this.
	maxLogBytes = 8 << 20
	// exitNoSupervisor means this environment cannot manage services at all — a
	// restricted sandbox, or a shell without a usable launchd session. Distinct from a
	// failure, so install.sh can offer the unsupervised path instead of a dead end.
	exitNoSupervisor = 4
	// serviceBootoutTimeout bounds the wait for a previous job to leave the domain.
	// Longer than the supervisor's own teardown: it SIGTERMs the proxy, allows its 15s
	// graceful shutdown, then insists at 20s.
	serviceBootoutTimeout = 30 * time.Second
	serviceUsage          = `abctl service — keep Cortex running across crashes and logins

Usage:
  abctl service install   [--yes] [--restart] [--config PATH]
  abctl service uninstall [--yes]
  abctl service status
  abctl service stop | start | restart

install hands the proxy to the OS supervisor — a launchd user agent on macOS, a
systemd user unit on Linux — so it restarts on failure and comes back at login.
Claude Code depends on the proxy being up once "abctl configure claude-code enable"
has run,
and nothing else keeps it up.

stop/start/restart exist so there is never a reason to reach for launchctl,
systemctl, kill or pkill: under a supervisor a plain kill is undone within seconds,
which looks like the process refusing to die.

install is free to re-run: with nothing to change it reports "Already current" and
leaves the proxy alone, so it never cuts attached sessions for no reason. --restart
overrides that and restarts regardless, which is what a source rebuild wants — see
"make dev-install".

If a proxy you started by hand is already running, install stops it first and
supervises a fresh one: two copies cannot share the ports, and the supervised one
would otherwise crash-loop while the old one kept serving — broken in a way that
only shows up at your next reboot.

Never installed as root. The proxy holds a CA private key; this stays a user
service.
`
	// serviceReadyTimeout bounds the post-start health probe. The supervisor
	// reports "loaded", not "serving", and the difference is where a bad config
	// hides.
	serviceReadyTimeout = 15 * time.Second
)

// servicePaths is everything the platform-specific bits need, gathered so tests
// can point all of it at a temp dir instead of the real ~/Library/LaunchAgents.
type servicePaths struct {
	unitFile   string // plist or .service
	binary     string // absolute path to authbridge-proxy
	configFile string // ~/.cortex/config.yaml
	logFile    string
	pidFile    string
	// stampFile records the SHA-256 of the proxy binary the supervisor last launched.
	//
	// A sidecar beside proxy.pid rather than a field in the unit, and for the same
	// reason: it is LAUNCH state, not unit content. The unit's AbctlVersion says which
	// abctl wrote it, which is a fact about the file; this says which bytes are running,
	// which changes without the file changing. Writing it into the unit also meant
	// touching the unit on every restart, and systemd reports a unit whose mtime moved
	// as "changed on disk, run daemon-reload" — a message this whole command exists so
	// nobody has to see.
	stampFile string
	healthURL string
	// forwardAddr is the proxy port clients point at, used only to count attached
	// sessions before a stop.
	forwardAddr string
	// configErr is why the config would not load, if it would not. Carried rather
	// than returned so uninstall and status still work on a broken config.
	configErr error
	home      string // set explicitly: a supervisor's environment is minimal
}

func runService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, serviceUsage)
		return 2
	}
	action := args[0]
	// `abctl service --help` used to fail with "unknown service action", which sends
	// someone looking for the command list to the one place that refuses to print it.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, serviceUsage)
		return 0
	}

	fs := newFlagSet("service "+action, stderr)
	yes := fs.Bool("yes", false, "do not prompt for confirmation")
	cortexCfg := fs.String("config", "", "Cortex config file")
	unitOverride := fs.String("unit-file", "", "unit file path (testing)")
	proxyPath := fs.String("proxy", "", "authbridge-proxy binary to supervise (default: the one installed beside abctl)")
	printUnit := fs.Bool("print-unit", false, "print the unit file and exit, installing nothing")
	forceRestart := fs.Bool("restart", false, "with install: restart even when nothing changed")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	p, err := resolveServicePaths(*cortexCfg, *unitOverride, *proxyPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}

	if *printUnit {
		// Inspect before installing. Also the only way to exercise rendering without
		// registering a real agent in the caller's session.
		fmt.Fprint(stdout, renderUnit(p))
		return 0
	}

	switch action {
	case "install":
		return serviceInstall(p, *yes, *forceRestart, stdout, stderr)
	case "uninstall":
		return serviceUninstall(p, *yes, stdout, stderr)
	case "status":
		return serviceStatus(p, stdout)
	case "stop", "start", "restart":
		return serviceControl(action, p, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "abctl: unknown service action %q "+
			"(install, uninstall, status, stop, start, restart)\n", action)
		return 2
	}
}

// resolveServicePaths refuses early on an unsupported platform rather than
// writing a unit file that will never be honoured.
func resolveServicePaths(cortexCfg, unitOverride, proxyOverride string) (servicePaths, error) {
	var p servicePaths
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p, fmt.Errorf("cannot determine your home directory (is $HOME set?)")
	}
	if cortexCfg == "" {
		cortexCfg = filepath.Join(home, ".cortex", "config.yaml")
	}
	p.home = home
	p.configFile = cortexCfg
	p.logFile = filepath.Join(filepath.Dir(cortexCfg), "proxy.log")
	p.pidFile = filepath.Join(filepath.Dir(cortexCfg), "proxy.pid")
	p.stampFile = filepath.Join(filepath.Dir(cortexCfg), "proxy.sha256")

	// An absolute binary path: a supervisor has no shell PATH to search.
	//
	// Resolution order matters. PATH first was wrong: an end-to-end run found the
	// plist pointing at an OLDER authbridge-proxy that happened to sit earlier on
	// PATH, which then rejected --supervise and exited, so the service never came up.
	// abctl and the proxy are installed together, so the sibling of the running abctl
	// is the one that matches it; PATH is only a fallback. An explicit --proxy wins
	// over both, which is what install.sh passes so the service uses the binary it
	// just installed.
	bin := proxyOverride
	if bin == "" {
		if self, serr := os.Executable(); serr == nil {
			if sib := filepath.Join(filepath.Dir(self), "authbridge-proxy"); fileExists(sib) {
				bin = sib
			}
		}
	}
	if bin == "" {
		if found, lerr := exec.LookPath("authbridge-proxy"); lerr == nil {
			bin = found
		} else {
			bin = filepath.Join(home, ".local", "bin", "authbridge-proxy")
		}
	}
	if abs, aerr := filepath.Abs(bin); aerr == nil {
		bin = abs
	}
	// Deliberately NOT an error here. resolveServicePaths runs for every action, and
	// uninstall/status are exactly what you reach for when the binary is gone or
	// renamed — refusing them at that point leaves a loaded unit with no way to
	// remove or diagnose it. serviceInstall validates the binary itself, since it is
	// the only action that needs one.
	p.binary = bin

	switch unitOverride {
	case "":
		switch runtime.GOOS {
		case "darwin":
			p.unitFile = filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		case "linux":
			p.unitFile = filepath.Join(home, ".config", "systemd", "user", systemdUnit)
		default:
			return p, fmt.Errorf("no supervisor integration for %s; start the proxy yourself:\n"+
				"  authbridge-proxy --config %s &", runtime.GOOS, cortexCfg)
		}
	default:
		p.unitFile = unitOverride
	}

	p.healthURL = resolveHealthURL(cortexCfg)
	// Kept, not discarded: install must refuse a config that cannot load, or it
	// writes the unit, gets an empty healthURL, skips the probe and calls a proxy
	// that cannot start "running". uninstall and status stay usable regardless,
	// which is the whole point of not returning it as an error here.
	if cfg, cerr := config.Load(cortexCfg); cerr != nil {
		p.configErr = cerr
	} else {
		config.ApplyPreset(cfg)
		p.forwardAddr = cfg.Listener.ForwardProxyAddr
	}
	return p, nil
}

// resolveHealthURL reads the health endpoint out of the config.
//
// ApplyPreset is applied because the binaries do: without it an unpinned
// health_addr reads as empty, so a config that never received the health pin
// produced no URL at all — and install then reported "Running under launchd" having
// probed nothing, for exactly the older configs that most need checking.
func resolveHealthURL(cortexCfg string) string {
	cfg, err := config.Load(cortexCfg)
	if err != nil {
		return ""
	}
	config.ApplyPreset(cfg)
	if cfg.Listener.HealthAddr == "" {
		return ""
	}
	return "http://" + dialableAddr(cfg.Listener.HealthAddr) + "/healthz"
}

func serviceInstalled(p servicePaths) bool {
	_, err := os.Stat(p.unitFile)
	return err == nil
}

func serviceInstall(p servicePaths, yes, forceRestart bool, stdout, stderr io.Writer) int {
	if _, err := os.Stat(p.configFile); err != nil {
		// Not "run the installer first": the installer is what calls this, so that
		// advice sent people in a circle. Name the command that creates the file.
		fmt.Fprintf(stderr, "abctl: no config at %s. Create it with:\n"+
			"  authbridge-proxy --local --write-config\n", p.configFile)
		return 1
	}

	// Only when there is actually a decision to make. With --yes this text explained a
	// choice nobody was being offered, and it said "restarts on failure and starts at
	// login" a second time right after the caller had already said it — three sentences
	// of the installer's output for one fact.
	if !yes {
		fmt.Fprintf(stdout, "This will install a %s that runs:\n  %s --config %s\n\n",
			supervisorName(), p.binary, p.configFile)
		fmt.Fprintf(stdout, "It restarts on failure and starts at login, so Claude Code keeps working\n"+
			"after a crash or a reboot. Unit file: %s\n\n", p.unitFile)
	}

	// Adopt rather than collide. Two copies cannot share the ports, and the
	// supervised one would lose the race and crash-loop while the hand-started one
	// kept serving — a break that only surfaces at the next reboot.
	adopt := runningPID(p.pidFile)
	if adopt > 0 && !yes {
		// Preview only. The connection count is printed once, at the point of action
		// below, so it cannot be announced for a run that turns out to change nothing.
		fmt.Fprintf(stdout, "A Cortex you started by hand is running (pid %d). It will be stopped\n"+
			"so the supervised one can take the ports.\n\n", adopt)
	}
	if !yes {
		fmt.Fprintf(stdout, "Undo with: abctl service uninstall\n\n")
	}
	if !yes && !confirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	// Probed before anything is written. Without this the first sign of trouble was
	// launchctl's "Bootstrap failed: 5: Input/output error" after the unit was already on
	// disk — a message that names neither the cause nor a way forward. Reported from a
	// restricted sandbox environment.
	if ok, why := launchdUsable(); !ok {
		fmt.Fprintf(stderr, "abctl: this environment cannot manage %ss (%s).\n\n"+
			"  Cortex still runs, just not supervised — start it yourself:\n"+
			"    %s --local\n\n"+
			"  It will not restart after a crash or come back at login while running that\n"+
			"  way. To stop it: kill that process.\n", supervisorName(), why, p.binary)
		return exitNoSupervisor
	}

	if _, serr := os.Stat(p.binary); serr != nil {
		fmt.Fprintf(stderr, "abctl: authbridge-proxy not found at %s; install it first\n", p.binary)
		return 1
	}
	// launchd loads the LOGIN home's ~/Library/LaunchAgents, taken from the user
	// record — not $HOME. With $HOME pointed at a project directory (sandboxes do this)
	// the unit is written somewhere launchd will never scan, so bootstrap succeeds and
	// "comes back at login" is quietly false. Warn rather than refuse: it is a real way
	// to run, just not a persistent one.
	if lh := loginHome(); lh != "" && lh != p.home {
		fmt.Fprintf(stderr, "abctl: $HOME is %s but your login home is %s.\n"+
			"  The unit goes to $HOME/Library/LaunchAgents, which launchd does not scan at\n"+
			"  login, so Cortex will NOT come back after a logout. Crash recovery still\n"+
			"  works while you are logged in.\n\n", p.home, lh)
	}

	if p.configErr != nil {
		fmt.Fprintf(stderr, "abctl: %s will not load, so a supervised proxy could not start:\n  %v\n"+
			"  Fix it (or delete it and run: authbridge-proxy --local --write-config), then re-run.\n",
			p.configFile, p.configErr)
		return 1
	}

	// Taking ownership of how the proxy runs is the natural moment to bring an
	// older config up to date: the service starts it with --config, which honours
	// listeners that --local skipped, so an unpinned transparent_proxy_addr would
	// start binding every interface precisely now.
	configChanged := false
	if changed, mErr := migrateConfig(p.configFile, stdout); mErr != nil {
		// Refuse only if continuing would actually expose something. A migration that
		// fails on a config already bound to loopback costs nothing to skip; one that
		// fails on a config with wildcard listeners would supervise a proxy publishing
		// on every interface, which is not a thing to do quietly.
		if exposed := wildcardListeners(p.configFile); len(exposed) > 0 {
			fmt.Fprintf(stderr, "abctl: could not update %s (%v),\n"+
				"  and as it stands it binds %s on every interface.\n"+
				"  Refusing to supervise that. Add `bind_loopback_only: true` under listener:,\n"+
				"  or delete the file and run: authbridge-proxy --local --write-config\n",
				p.configFile, mErr, strings.Join(exposed, ", "))
			return 1
		}
		fmt.Fprintf(stderr, "abctl: could not update %s (%v); it already binds loopback only, continuing\n",
			p.configFile, mErr)
	} else if changed {
		configChanged = true
		// The paths were resolved from the pre-migration config, so anything the
		// migration added — health_addr above all — is not in them yet. Without this
		// the probe below is skipped for precisely the configs that were just fixed.
		p.healthURL = resolveHealthURL(p.configFile)
	}

	// Nothing to do is a valid outcome, and the common one: this command is what the
	// one-liner runs every time, including when everything is already current. Without
	// this it went ahead with bootout + bootstrap, which restarts the proxy and cuts every
	// attached connection — costing each client the request it had in flight — for no
	// change at all. Re-running an installer should be free.
	//
	// Deliberately AFTER the migration: a config that still needs pins is a change, so
	// it must not be short-circuited. `changed` above is false only when the config was
	// already up to date.
	//
	// --restart opts out. `make dev-install` wants one command that ends with the built
	// bytes serving, and it must not have to reason about whether they differ from the
	// running ones — a rebuild that happens to be reproducible would otherwise silently
	// skip the restart, making the target's effect depend on whether the compiler was
	// deterministic that day.
	if installCanSkip(configChanged, forceRestart, func() bool { return serviceIsCurrent(p) }) {
		fmt.Fprintf(stdout, "Already current: %s is running under %s and healthy.\n"+
			"  Nothing to change. Use `abctl service restart` to restart it anyway.\n",
			filepath.Base(p.binary), supervisorName())
		return 0
	}

	// Warned HERE, not earlier: past the no-op check we know a restart is really going
	// to happen. Announcing "connections will be cut" and then changing nothing would be
	// its own small lie, and it printed on every re-run.
	//
	// Replacing a running Cortex cuts whatever is talking to it. What that costs is one
	// in-flight request per connection, not the session: HTTPS_PROXY is fixed in each
	// client's environment so it cannot fall back to a direct connection, but it does
	// reconnect through the proxy on its next request. The measurements are in
	// docs/laptop-service.md, "Re-running the installer is safe" — kept in one place so a
	// re-measurement does not leave stale copies in three others.
	//
	// This used to read "cannot reconnect on its own — restart any session that starts
	// failing", which is what it looked like to me when a mid-stream request died and I
	// restarted the session that reported it. The session had not needed restarting.
	reportSessionInterruption(p, stdout)
	// Whether anything is SERVING, captured before the replacement and reported after it
	// succeeds — see reportHistoryCleared for why neither half of that is optional.
	wasServing := adopt > 0 ||
		(p.healthURL != "" && waitHealthy(p.healthURL, historyProbeBudget))

	if adopt > 0 {
		fmt.Fprintf(stdout, "Stopping pid %d...\n", adopt)
		if err := stopPID(adopt); err != nil {
			fmt.Fprintf(stderr, "abctl: could not stop pid %d (%v); stop it yourself and re-run\n", adopt, err)
			return 1
		}
		_ = os.Remove(p.pidFile)
	}

	rotateLog(p.logFile, maxLogBytes)
	tightenLog(p.logFile, stderr)

	if err := os.MkdirAll(filepath.Dir(p.unitFile), 0o755); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	if err := os.WriteFile(p.unitFile, []byte(renderUnit(p)), 0o644); err != nil {
		fmt.Fprintf(stderr, "abctl: writing %s: %v\n", p.unitFile, err)
		return 1
	}
	// Recorded here as well as on start/restart. loadService below may fail after this
	// point, and a stamp naming the binary we are about to launch is still the right
	// answer for the next install: it reflects the unit that was just written.
	writeProxyStamp(p)
	// The path is not printed on the happy path: it is one more line of output on an
	// install that already says what happened, and `abctl service status` reports it
	// whenever someone actually needs it.
	if !yes {
		fmt.Fprintf(stdout, "Wrote %s\n", p.unitFile)
	}

	if err := loadService(p, stdout); errors.Is(err, errLingerUnavailable) {
		// The unit IS loaded, so this is a caveat rather than a failure: keep going,
		// but never claim it survives a logout.
		fmt.Fprintf(stderr, "abctl: %v\n", err)
	} else if err != nil {
		// Leaving the unit behind would make serviceInstalled() true for something
		// that never loaded, so `service status` would report it installed and
		// `service stop` would act on a job the supervisor does not have.
		if rmErr := os.Remove(p.unitFile); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Fprintf(stderr, "abctl: also could not remove %s: %v\n", p.unitFile, rmErr)
		}
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}

	// "Loaded" is not "serving". A bad config is fatal at startup and a supervisor
	// turns that into a restart loop, so probe the health endpoint before claiming
	// success.
	// Health alone is not proof: an unadopted proxy (started by hand, no pidfile)
	// keeps the ports, the supervised copy loses the bind race and crash-loops, and
	// the probe cheerfully succeeds against the survivor. Ask the supervisor whether
	// OUR job is actually up before believing the probe.
	if running, why := supervisorRunning(p); !running {
		fmt.Fprintf(stderr, "abctl: the unit loaded but the supervisor does not report it running (%s).\n"+
			"  Something else may hold the ports — check for a Cortex you started by hand:\n"+
			"    pgrep -fl authbridge-prox\n"+
			"  Last log lines:\n", why)
		for _, line := range lastLines(p.logFile, 5) {
			fmt.Fprintf(stderr, "    %s\n", line)
		}
		return 1
	}

	if p.healthURL != "" {
		if waitHealthy(p.healthURL, serviceReadyTimeout) {
			reportInstallSuccess(true, stdout)
			reportHistoryCleared(wasServing, stdout)
			return 0
		}
		fmt.Fprintf(stderr, "\nabctl: installed, but nothing answered %s within %s.\n"+
			"  Check %s — a config error is fatal at startup and the supervisor will keep retrying.\n"+
			"  abctl service status shows the current state.\n", p.healthURL, serviceReadyTimeout, p.logFile)
		return 1
	}
	reportInstallSuccess(false, stdout)
	reportHistoryCleared(wasServing, stdout)
	return 0
}

// crashRecoveryNote explains why there are two authbridge-proxy processes on macOS.
//
// launchd does not restart these agents — see renderUnitFor and
// cmd/authbridge-proxy/supervise.go — so crash recovery belongs to the supervisor, not
// to KeepAlive. A package-level const so its content can be asserted on any platform,
// not only when the test happens to run on darwin.
const crashRecoveryNote = "A supervisor process handles crashes (launchd will not restart these agents)."

// reportInstallSuccess prints the outcome of an install that worked.
//
// ONE function for both outcomes, deliberately. This was previously two call sites
// that each had to remember to print the crash-recovery note, and one of them did not
// — so the note appeared only when the install could verify least, never on a normal
// healthy install. A test could have caught that; not being able to express it is
// better. There is now no "which path prints what" to get wrong.
func reportInstallSuccess(healthy bool, stdout io.Writer) {
	if healthy {
		fmt.Fprintf(stdout, "Running as a %s, healthy.\n", supervisorName())
	} else {
		fmt.Fprintf(stdout, "Running as a %s.\n", supervisorName())
	}
	if runtime.GOOS == "darwin" {
		fmt.Fprintln(stdout, crashRecoveryNote)
	}
}

func serviceUninstall(p servicePaths, yes bool, stdout, stderr io.Writer) int {
	if !serviceInstalled(p) {
		fmt.Fprintf(stdout, "Nothing to do: no unit at %s\n", p.unitFile)
		return 0
	}
	fmt.Fprintf(stdout, "This will stop and remove the %s at:\n  %s\n\n", supervisorName(), p.unitFile)
	fmt.Fprintf(stdout, "Cortex will no longer start at login. Claude Code stops working whenever\n"+
		"the proxy is not running — `abctl configure claude-code disable` removes that\n"+
		"dependency.\n\n")
	if !yes && !confirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	if err := unloadService(p); err != nil {
		// Report but keep going: leaving the unit file behind would make a
		// reinstall look installed-but-dead.
		fmt.Fprintf(stderr, "abctl: %v\n", err)
	}
	if err := os.Remove(p.unitFile); err != nil {
		fmt.Fprintf(stderr, "abctl: removing %s: %v\n", p.unitFile, err)
		return 1
	}
	// Launch state, so it goes with the unit rather than surviving in ~/.cortex beside
	// the config and CA the message below promises are untouched. Left behind, it would
	// describe a service that no longer exists.
	if err := os.Remove(p.stampFile); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "abctl: could not remove %s: %v\n", p.stampFile, err)
	}
	// Not "start it yourself with a backgrounded proxy": running unsupervised is no
	// longer a mode this tool offers, and on macOS a hand-started proxy gets no crash
	// recovery at all. Point back at the supported path.
	fmt.Fprintf(stdout, "\nRemoved. Cortex is stopped; Claude Code will fail until it runs again.\n"+
		"  Set it up again with:  abctl service install\n"+
		"  Or unwire Claude Code: abctl configure claude-code disable\n"+
		"  The config and CA are untouched in %s\n", filepath.Dir(p.configFile))
	return 0
}

func serviceStatus(p servicePaths, stdout io.Writer) int {
	if !serviceInstalled(p) {
		fmt.Fprintf(stdout, "not installed (%s)\n", p.unitFile)
		if pid := runningPID(p.pidFile); pid > 0 {
			fmt.Fprintf(stdout, "  a hand-started Cortex is running (pid %d); nothing restarts it\n", pid)
		}
		return 0
	}
	fmt.Fprintf(stdout, "installed: %s\n", p.unitFile)
	// Skew is worth naming before anything else: if the unit was written by a
	// different abctl, the rest of this output describes a job this binary may not be
	// able to manage.
	if w := unitWriterVersion(p.unitFile); w != "" && w != version {
		fmt.Fprintf(stdout, "WARNING: this unit was written by abctl %s; you are running %s.\n"+
			"  The unit also pins a fixed authbridge-proxy path, which that abctl chose.\n"+
			"  Reinstall with this build to bring them back in step:  abctl service install\n",
			w, version)
	}

	// The CA cutoff, because "installed and healthy" is not the same as "seeing
	// anything". A client reads its CA file once at startup, so an agent older than
	// this CA is being tunnelled rather than observed — and nothing on its side says
	// so, because tunnelling is not an error. This is the only place that fact is
	// discoverable without reading the proxy log.
	if nb, caFile := bridgeCANotBefore(p.configFile); nb != "" {
		fmt.Fprintf(stdout, "TLS bridge CA: %s (minted %s)\n", caFile, nb)
		fmt.Fprintf(stdout, "  Agents started before that cannot be observed until restarted.\n")
		if p.forwardAddr != "" {
			// Filtered to the CLIENT side and paired with start times, because the
			// bare `lsof -nP -iTCP:<port>` this used to print does not answer the
			// question: it lists the proxy's own listening socket and its server-side
			// half of every connection, and shows no start times at all — so nothing
			// in it can be compared against the cutoff printed one line above.
			//
			// Matching the destination suffix rather than an IP literal keeps it
			// correct whichever loopback address the listener is on.
			fmt.Fprintf(stdout, "  Find them:  lsof -nP -iTCP -sTCP:ESTABLISHED"+
				" | awk '$9 ~ /->.*:%s$/ {print $2}' | sort -u"+
				" | xargs ps -o pid,lstart,comm -p\n", portOfAddr(p.forwardAddr))
		}
	}

	if p.healthURL == "" {
		return 0
	}
	if waitHealthy(p.healthURL, 2*time.Second) {
		fmt.Fprintf(stdout, "healthy: %s\n", p.healthURL)
		return 0
	}
	// Installed but not serving is the state worth naming loudly: Claude Code is
	// pointed at a proxy that is not answering.
	fmt.Fprintf(stdout, "NOT answering %s\n", p.healthURL)
	fmt.Fprintf(stdout, "  Claude Code will fail while this is true. Last log lines:\n")
	for _, line := range lastLines(p.logFile, 5) {
		fmt.Fprintf(stdout, "    %s\n", line)
	}
	return 1
}

// serviceControl is the whole reason users never need launchctl or systemctl: a
// plain kill against a supervised process is undone in seconds, which reads as the
// process refusing to die.
func serviceControl(action string, p servicePaths, stdout, stderr io.Writer) int {
	if !serviceInstalled(p) {
		fmt.Fprintf(stderr, "abctl: no service installed (%s). Install it with:\n"+
			"  abctl service install\n", p.unitFile)
		return 1
	}
	// Counted BEFORE the stop, while the connections still exist.
	n := -1
	if action == "stop" {
		n = establishedConns(p.forwardAddr)
	}
	if action == "start" || action == "restart" {
		rotateLog(p.logFile, maxLogBytes)
		// tightenLog must follow every rotation, not just the install. Rotation renames
		// the old file away, so the supervisor creates a fresh one under its own
		// umask — measured at 0644, which silently undid the 0600 this sets.
		tightenLog(p.logFile, stderr)
	}
	if err := controlService(action, p, stdout); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	if action == "start" || action == "restart" {
		// The supervisor has just launched whatever p.binary holds now, so this is the
		// moment to record those bytes. Without it, replacing the binary and restarting
		// would leave the stamp naming something neither on disk nor running, and the
		// next install would restart a service that was already serving the right code.
		writeProxyStamp(p)
	}
	switch action {
	case "stop":
		fmt.Fprintln(stdout, "Stopped, and it will stay stopped across logins.")
		fmt.Fprintln(stdout, "  abctl service start")
		if n > 0 {
			fmt.Fprintf(stdout, "\n  %d connection(s) were attached to %s and have just been cut.\n",
				n, p.forwardAddr)
			fmt.Fprintln(stdout, "  Clients cannot fall back to a direct connection — HTTPS_PROXY is fixed")
			fmt.Fprintln(stdout, "  in their environment at startup — so they will keep failing until you run")
			fmt.Fprintln(stdout, "  abctl service start. They reconnect on their own once it is back;")
			fmt.Fprintln(stdout, "  restarting the sessions themselves is not necessary.")
		}
		return 0
	default:
		// Same gate install uses: an unadopted proxy holding the ports answers the
		// probe while OUR job crash-loops on the bind, so health alone would report a
		// restart that did not happen.
		if running, why := supervisorRunning(p); !running {
			fmt.Fprintf(stderr, "abctl: %sed, but the supervisor does not report it running (%s).\n"+
				"  Check for a Cortex started by hand holding the ports: pgrep -fl authbridge-prox\n", action, why)
			for _, line := range lastLines(p.logFile, 5) {
				fmt.Fprintf(stderr, "    %s\n", line)
			}
			return 1
		}
		if p.healthURL != "" && !waitHealthy(p.healthURL, serviceReadyTimeout) {
			fmt.Fprintf(stderr, "abctl: %sed, but nothing answered %s. Last log lines:\n", action, p.healthURL)
			for _, line := range lastLines(p.logFile, 5) {
				fmt.Fprintf(stderr, "    %s\n", line)
			}
			return 1
		}
		fmt.Fprintf(stdout, "%sed.\n", strings.ToUpper(action[:1])+action[1:])
		return 0
	}
}

// tightenLog makes the proxy log owner-only before the supervisor opens it.
//
// The supervisor creates it with its own umask — 0644 in practice — and it records
// every host the proxy talks to. Creating it first means the supervisor appends to a
// file that is already tight; the chmod also catches one an earlier install left
// loose. Best-effort throughout: a log mode is not worth failing an install over.
func tightenLog(path string, stderr io.Writer) {
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_ = f.Close()
	}
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "abctl: could not tighten %s (%v); continuing\n", path, err)
	}
}

// wildcardListeners names the listener addresses that are not bound to loopback,
// so a refusal can say what is actually exposed.
func wildcardListeners(cortexCfg string) []string {
	cfg, err := config.Load(cortexCfg)
	if err != nil {
		return nil // unreadable: the caller's own error already covers it
	}
	config.ApplyPreset(cfg)
	var out []string
	for name, addr := range map[string]string{
		"forward_proxy_addr":     cfg.Listener.ForwardProxyAddr,
		"session_api_addr":       cfg.Listener.SessionAPIAddr,
		"health_addr":            cfg.Listener.HealthAddr,
		"transparent_proxy_addr": cfg.Listener.TransparentProxyAddr,
		"stats address":          cfg.Stats.StatsAddress,
	} {
		if addr == "" {
			continue
		}
		if host, _, serr := net.SplitHostPort(addr); serr == nil && !isLoopbackHost(host) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// isLoopbackHost treats an empty host as a wildcard, which is exactly what ":9091"
// means.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// loginHome returns the home directory from the user record, or "" if it cannot be
// determined. Deliberately not os.UserHomeDir, which returns $HOME — the value whose
// trustworthiness is the question.
func loginHome() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	u := os.Getenv("USER")
	if u == "" {
		out, err := exec.Command("id", "-un").Output()
		if err != nil {
			return ""
		}
		u = strings.TrimSpace(string(out))
	}
	out, err := exec.Command("dscl", ".", "-read", "/Users/"+u, "NFSHomeDirectory").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return ""
	}
	return fields[len(fields)-1]
}

// reportSessionInterruption names how many clients a restart will disconnect.
//
// HTTPS_PROXY is baked into each client's environment when it starts, so none of them can
// fall back to a direct connection — but they do reconnect through the proxy on their next
// request, in well under a second (docs/laptop-service.md has the measurements). So the
// cost is the requests in flight, one per connection, and naming the count turns that into
// a five-second diagnosis instead of a bug report.
//
// Silent when there is nothing attached, or when we cannot tell — a confident "0" would be
// worse than no number.
func reportSessionInterruption(p servicePaths, stdout io.Writer) {
	if n := establishedConns(p.forwardAddr); n > 0 {
		fmt.Fprintf(stdout, "  %d connection(s) will be cut. Clients reconnect on their next\n"+
			"  request — a request in flight right now fails.\n", n)
	}
}

// historyProbeBudget is how long the pre-replacement liveness check may take.
//
// Short deliberately: on loopback a live proxy answers on the first attempt and a dead
// port refuses immediately, so this costs nothing in either direction. It is not
// supervisorRunning, which polls to serviceReadyTimeout and would therefore make every
// FIRST install — the case with nothing to find — wait out the whole budget.
const historyProbeBudget = 250 * time.Millisecond

// reportHistoryCleared names the one consequence of a restart that nothing else reports.
//
// wasServing must mean SERVING, and must have been sampled before the replacement:
//
//   - not "installed". That was the first version of this, and serviceInstalled is
//     os.Stat on the unit file. A service that is installed and stopped — which `service
//     stop` makes persist across logouts, deliberately — has a unit on disk and no store,
//     so it got told a timeline it was not reading had been cleared. Precisely the false
//     line this commit removed from the Makefile, one path over.
//   - not sampled after. The probe cannot run once the replacement has started, because
//     by then the answer is about the new process.
//
// And it is emitted only on a success path, because it asserts an accomplished fact where
// its neighbour reportSessionInterruption makes a prediction ("will be cut"). A prediction
// is safe to print early; this is not — printed before the replacement, an install that
// then failed had already claimed the history was gone.
//
// Extracted rather than inlined so the gate is assertable: serviceInstall reaches these
// lines only after writing a unit file and calling launchctl, which a unit test cannot.
func reportHistoryCleared(wasServing bool, stdout io.Writer) {
	if !wasServing {
		return
	}
	fmt.Fprintln(stdout, "  Captured session history is cleared: the store is in memory, so any")
	fmt.Fprintln(stdout, "  timeline you were reading in abctl starts over.")
}

// serviceIsCurrent reports whether the installed service already matches what
// serviceInstall would write, and is actually serving.
//
// Every clause is necessary, because each one is a way for "installed" to be a lie:
// a unit written by a different abctl pins whatever binary THAT one chose; a unit naming
// a different binary or config is stale; a job the supervisor does not report running is
// the installed-but-dead state; and "loaded" is not "serving", which is where a bad
// config hides.
func serviceIsCurrent(p servicePaths) bool {
	if !serviceInstalled(p) {
		return false
	}
	if unitWriterVersion(p.unitFile) != version {
		return false
	}
	unit, err := os.ReadFile(p.unitFile) //nolint:gosec // path we wrote
	if err != nil {
		return false
	}
	body := string(unit)
	if !strings.Contains(body, p.binary) || !strings.Contains(body, p.configFile) {
		return false
	}
	if !proxyBinaryUnchanged(p) {
		return false
	}
	// On macOS the proxy must be supervised; a unit that lost --supervise would leave
	// crashes unrecovered, which is the whole point of the feature.
	if runtime.GOOS == "darwin" && !strings.Contains(body, "--supervise") {
		return false
	}
	if running, _ := supervisorRunning(p); !running {
		return false
	}
	if p.healthURL == "" {
		return false // cannot confirm it is serving, so do not claim it is
	}
	return waitHealthy(p.healthURL, 2*time.Second)
}

// proxyBinaryUnchanged reports whether the binary on disk is still the one the unit
// was written for.
//
// Separate from serviceIsCurrent because it is the one clause there that can be
// checked without a live supervisor and a serving port, and because it is the clause
// that was missing: the unit naming the right binary PATH is not the same as that
// path holding the bytes we installed, and every OTHER clause is satisfied when it
// does not. Replacing the binary in place — a source rebuild, or an installer
// overwriting it — left install reporting "Already current: authbridge-proxy is
// running under launchd and healthy" while the running process was the previous
// build. True of the unit, false of the service, and silent either way.
//
// A missing stamp counts as changed, which costs exactly one restart on the upgrade
// that introduces it and then never again. The opposite default would make the first
// dev install after upgrading silently skip its restart.
func proxyBinaryUnchanged(p servicePaths) bool {
	want := readProxyStamp(p)
	return want != "" && want == binarySHA256(p.binary)
}

// installCanSkip decides whether install may return having touched nothing.
//
// current is a FUNC, not a bool, and that is the point: serviceIsCurrent probes the
// health endpoint with a 2s budget, so evaluating it eagerly would spend that wait on
// every run that had already decided to restart. Both early exits must stay lazy.
func installCanSkip(configChanged, forceRestart bool, current func() bool) bool {
	if configChanged || forceRestart {
		return false
	}
	return current()
}

// bridgeCANotBefore returns the bridge CA's NotBefore in RFC3339 and the file it
// came from, or empty strings when there is no bridge, no CA yet, or the config
// will not load. Every failure is silent: this is one advisory line in `status`,
// and a broken config must still let status report the things it can.
func bridgeCANotBefore(cortexCfg string) (notBefore, caFile string) {
	cfg, err := config.Load(cortexCfg)
	if err != nil || cfg.TLSBridge == nil || cfg.TLSBridge.CADir == "" {
		return "", ""
	}
	caFile = filepath.Join(cfg.TLSBridge.CADir, "ca.crt")
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return "", ""
	}
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return "", ""
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return "", ""
	}
	return crt.NotBefore.Local().Format(time.RFC3339), caFile
}

// portOfAddr is the port of a host:port, or the input unchanged when it has no
// colon — so the suggested lsof command is still recognisable rather than empty.
func portOfAddr(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return addr
}
