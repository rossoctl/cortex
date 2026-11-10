package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/core/config"
)

// Supervision exists because launchd cannot be relied on for it.
//
// Measured on macOS 26.5.2, in a gui/<uid> domain with an active Aqua session, for
// an agent bootstrapped mid-session: launchd logs "pending spawn, domain in
// on-demand-only mode" and then honours NOTHING that would restart the process —
// KeepAlive=true, KeepAlive={SuccessfulExit:false}, StartInterval, and RunAtLoad all
// fail to fire, via both `launchctl bootstrap` and legacy `launchctl load -w`, with
// and without ProcessType, with the executable approved in Login Items. Bootstrapping
// into user/<uid> instead is refused outright. Only an explicit `launchctl kickstart`
// starts the job.
//
// So the process that restarts the proxy has to be one launchd already started. This
// is that process: launchd starts the supervisor (at login, which does work, or via
// kickstart at install), and the supervisor restarts the proxy for as long as it
// lives. A crash of the proxy — the failure this feature is for — is then covered
// without depending on launchd's autonomous spawn at all.
//
// What this does NOT cover: the supervisor itself being killed. Nothing restarts it
// until the next login. That is a far narrower gap than every crash going unrecovered,
// and it is stated rather than papered over.
//
// That same gap is why this never gives up on a child that cannot start. An address
// another process holds can be freed at any moment — the other install is stopped, the
// other user logs out — and if the supervisor had exited on a condition outside its
// control, a clash that fixes itself would instead need a login to recover from. So a
// child that cannot bind slows the loop down and says why, once, rather than either
// spinning silently or quitting. See portconflict.go for the other half: the child naming
// the address and the process holding it.
const (
	// superviseRestartDelay is the pause after a child exits before starting another.
	superviseRestartDelay = 2 * time.Second
	// superviseMaxDelay caps the backoff when the child keeps dying immediately, so a
	// config that cannot start does not spin.
	superviseMaxDelay = 30 * time.Second
	// superviseHealthyRun is how long a child must survive for its run to count as
	// healthy, resetting the backoff.
	superviseHealthyRun = 10 * time.Second
	// superviseBlockedMaxDelay replaces superviseMaxDelay as the ceiling once the child
	// has failed to bind an address several times running. Nothing the supervisor does
	// frees an address somebody else holds, so retrying every 30s only writes the same
	// line 2,880 times a day; at 10 minutes it is 144, and a proxy still comes back
	// within ten minutes of the address being freed, unattended.
	superviseBlockedMaxDelay = 10 * time.Minute
	// superviseBlockedEscalate is how many bind failures in a row it takes to move to
	// superviseBlockedMaxDelay. More than one, because a restart racing the old proxy's
	// exit meets a briefly-held address and recovers on the next attempt; few enough
	// that a genuinely held address slows down inside a minute.
	superviseBlockedEscalate = 3
	// superviseStopGrace is how long a stop waits for the proxy before killing it. It must
	// fit inside launchd's exit timeout — 5s, pinned as ExitTimeOut in the plist agentop
	// writes — because launchd SIGKILLs the supervisor when that runs out and only SIGTERMs
	// the rest of the process group, which a proxy that has already taken its one SIGTERM
	// ignores. Waiting 20s here is how a restart left the old proxy orphaned on its ports
	// while the new one crash-looped on the bind. A local install's proxy stops in well
	// under a second (it closes rather than drains), so this only ever ends a stuck one.
	superviseStopGrace = 3 * time.Second
)

// restartPolicy decides how long to wait before the next child, and how loudly to say
// why. It is kept apart from the loop so those decisions can be tested without forking
// anything: the policy records and returns, the loop logs.
type restartPolicy struct {
	delay   time.Duration // the wait before the next child
	blocked int           // consecutive exits that could not bind an address
	// explained and slowed make the two loud lines fire once per run of bind failures
	// rather than once per attempt, which is the whole point of counting them.
	explained bool
	slowed    bool
}

// restartNote is what the loop says about one exit, and how long it then waits.
type restartNote struct {
	delay    time.Duration
	attempts int  // consecutive bind failures including this one; 0 when this was not one
	explain  bool // first bind failure of a run: state the condition, at ERROR
	slowed   bool // this exit moved the ceiling to superviseBlockedMaxDelay
	quiet    bool // a bind failure already explained: log the restart at DEBUG
}

// healthy marks the running child as having survived long enough to count, and reports
// whether that ended a run of bind failures. That is the one transition worth announcing:
// after the slowdown the log goes quiet for ten minutes at a time, so "it is working now"
// cannot be read off the silence.
func (p *restartPolicy) healthy() (recovered bool) {
	p.delay = superviseRestartDelay
	recovered = p.blocked > 0
	p.blocked, p.explained, p.slowed = 0, false, false
	return recovered
}

// exited folds one child's exit into the policy and returns what to say about it. The
// delay it returns is the one to wait now and the doubling lands on the attempt after
// that, which is how the loop read before this was extracted from it.
func (p *restartPolicy) exited(portInUse, wasHealthy bool) restartNote {
	if !portInUse {
		if p.blocked > 0 && p.delay > superviseMaxDelay {
			// Leaving the slowdown: some other failure belongs under the ordinary
			// ceiling and should not inherit a ten-minute wait.
			p.delay = superviseMaxDelay
		}
		p.blocked, p.explained, p.slowed = 0, false, false
		n := restartNote{delay: p.delay}
		p.advance(superviseMaxDelay, wasHealthy)
		return n
	}

	p.blocked++
	n := restartNote{attempts: p.blocked}
	if p.explained {
		n.quiet = true
	} else {
		p.explained, n.explain = true, true
	}
	ceiling := superviseMaxDelay
	if p.blocked >= superviseBlockedEscalate {
		ceiling = superviseBlockedMaxDelay
		if !p.slowed {
			p.slowed, n.slowed = true, true
		}
	}
	n.delay = p.delay
	p.advance(ceiling, wasHealthy)
	return n
}

// advance doubles the backoff up to a ceiling, unless the run that just ended was healthy
// — healthy() has already put the delay back to superviseRestartDelay, and a proxy that
// serves for hours between crashes is not what a backoff is for.
func (p *restartPolicy) advance(ceiling time.Duration, wasHealthy bool) {
	if wasHealthy {
		return
	}
	if p.delay *= 2; p.delay > ceiling {
		p.delay = ceiling
	}
}

// portInUseExit reports whether a child exited because it could not bind an address: the
// exitPortInUse status reportListenFailure leaves with. Read off the exit status rather
// than out of the child's output, which would turn a log line into a protocol.
func portInUseExit(werr error) bool {
	var ee *exec.ExitError
	return errors.As(werr, &ee) && ee.ExitCode() == exitPortInUse
}

// cortexChild names the proxy child in the log and in its childSpec.
const cortexChild = "cortex"

// childSpec is one child the supervisor runs, each in its own restart loop, so an
// exit of one child does not touch the other.
type childSpec struct {
	name         string
	path         string
	args         []string
	env          []string // nil = inherit
	stdinPipe    bool     // the supervisor keeps the write end; closing it stops the child
	fallback     *fallbackServer
	fallbackAddr string // cortex only, with fallback
}

// supervisorChildren returns the children to run. Direct mode is cortex alone, with
// today's args and env, and touches no file. Praxis mode first writes the gateway
// files next to the config, then adds cortex-gateway, found next to this binary. The
// gateway gets an empty env, so no proxy variable can reach it.
//
// When the gateway cannot run, cortex runs alone: it then refuses the praxis config
// at each start and logs why, and the fallback keeps the forward address. Ending the
// supervisor instead would leave nothing until the next login.
func supervisorChildren(cfg *config.Config, configPath, self string, args []string,
	write func(dir string, cfg *config.Config) (string, string, error)) []childSpec {
	cortex := childSpec{name: cortexChild, path: self, args: args}
	if cfg == nil || !cfg.PraxisEnabled() {
		return []childSpec{cortex}
	}
	// The cortex child refuses this config at each start. Do not run a gateway for it.
	if err := config.Validate(cfg); err != nil {
		slog.Error("supervisor: the config is not valid, so cortex-gateway does not start", "error", err)
		return []childSpec{cortex}
	}
	tokenPath, yamlPath, err := write(filepath.Dir(configPath), cfg)
	if err != nil {
		slog.Error("supervisor: cannot write the gateway files, so cortex-gateway does not start", "error", err)
		return []childSpec{cortex}
	}
	cortex.env = append(os.Environ(), gatewayTokenEnv+"="+tokenPath)
	gateway := childSpec{
		name:      gatewayBinary,
		path:      filepath.Join(filepath.Dir(self), gatewayBinary),
		args:      []string{"-c", yamlPath, "--exit-on-stdin-eof"},
		env:       []string{},
		stdinPipe: true,
	}
	return []childSpec{cortex, gateway}
}

// runSupervisor re-executes this binary without the supervise flag and restarts it
// whenever it exits, until the supervisor is asked to stop. In praxis mode it runs
// cortex-gateway beside it, in a loop of its own.
func runSupervisor(flagName string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}
	// Same arguments, minus the flag that got us here, so the child runs the proxy.
	args := make([]string, 0, len(os.Args)-1)
	for _, a := range os.Args[1:] {
		if a == "-"+flagName || a == "--"+flagName {
			continue
		}
		args = append(args, a)
	}
	if slices.Contains(args, "-"+flagName) || slices.Contains(args, "--"+flagName) {
		return errors.New("internal: supervise flag survived argument filtering")
	}

	// Forward termination to the children so `launchctl bootout` / `systemctl stop`
	// takes the whole tree down; otherwise the proxy is orphaned and keeps the ports,
	// which would look exactly like the "stop that does not stop" this replaces.
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, syscall.SIGTERM, syscall.SIGINT)

	configPath := configPathFromArgs(args)
	cfg := supervisorConfig(configPath)

	// The fallback holds the forward address whenever no child does, so a routed agent
	// gets a 502 that names Cortex instead of a refused connection it cannot trace. It
	// is opened here because nothing is serving yet, and closed before every start. See
	// fallback.go. It deliberately does not cover `agentop service stop`: that stops
	// this supervisor too, and that path already warns, having been asked for.
	fallback := &fallbackServer{}
	fallbackAddr := supervisorForwardAddr(cfg)
	fallback.open(fallbackAddr)
	defer fallback.close()

	specs := supervisorChildren(cfg, configPath, self, args, writeGatewayFiles)
	specs[0].fallback, specs[0].fallbackAddr = fallback, fallbackAddr

	// Each child stops at the same time, so the whole stop stays inside launchd's 5s.
	stop := make(chan struct{})
	errs := make(chan error, len(specs))
	var wg sync.WaitGroup
	for _, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- superviseChild(spec, stop)
		}()
	}
	select {
	case sig := <-stopping:
		slog.Info("supervisor: stopping", "signal", sig.String())
	case err = <-errs: // only a cortex start failure ends a loop by itself
	}
	close(stop)
	wg.Wait()
	return err
}

// superviseChild runs one child and restarts it whenever it exits, until stop closes.
// A start failure of cortex ends the supervisor, as before praxis mode; a start
// failure of the gateway (a missing binary, say) is retried with the normal backoff,
// and cortex keeps running.
func superviseChild(spec childSpec, stop <-chan struct{}) error {
	log := slog.With("child", spec.name)
	policy := &restartPolicy{delay: superviseRestartDelay}
	startFailing := false
	for {
		// Hand the address to the child. Nothing else frees it, so this is what lets the
		// child bind at all.
		if spec.fallback != nil {
			spec.fallback.close()
		}

		cmd := exec.Command(spec.path, spec.args...) //nolint:gosec // our own binaries, filtered args
		cmd.Env = spec.env
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, nil
		// The write end is close-on-exec, so only this process holds it, and the kernel
		// closes it however this process exits, SIGKILL included.
		var stdin io.Closer
		var err error
		if spec.stdinPipe {
			stdin, err = cmd.StdinPipe()
		}
		if err == nil {
			err = cmd.Start()
		}
		start := time.Now()
		if err != nil {
			if spec.name == cortexChild {
				return fmt.Errorf("starting the proxy: %w", err)
			}
			note := policy.exited(false, false)
			failed := log.Debug
			if !startFailing {
				failed = log.Error
			}
			startFailing = true
			failed("supervisor: cannot start the child; retrying", "path", spec.path, "err", err, "restart_in", note.delay)
			select {
			case <-stop:
				return nil
			case <-time.After(note.delay):
			}
			continue
		}
		startFailing = false
		log.Info("supervisor: child started", "pid", cmd.Process.Pid)

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		// A run counts as healthy the moment it reaches superviseHealthyRun, not once it
		// ends. A child that binds and then serves for hours would otherwise leave the
		// backoff and the bind-failure count set for its whole life, so nothing could
		// say a port conflict had cleared until the proxy next died. That timer is the
		// only reason this wait is a loop rather than the one select it used to be.
		healthy := time.NewTimer(superviseHealthyRun)
		var werr error
		var ran time.Duration
		wasHealthy := false
	wait:
		for {
			select {
			case <-stop:
				healthy.Stop()
				if stdin != nil {
					log.Info("supervisor: stopping, closing the child's stdin")
					_ = stdin.Close()
				} else {
					log.Info("supervisor: stopping, forwarding SIGTERM to the child")
					_ = cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
				}
				select {
				case <-done:
				case <-time.After(superviseStopGrace):
					log.Warn("supervisor: child did not exit in time; killing it", "waited", superviseStopGrace)
					_ = cmd.Process.Kill() //nolint:errcheck
					<-done
				}
				return nil

			case <-healthy.C:
				wasHealthy = true
				if policy.healthy() {
					log.Info("supervisor: the proxy is up; the address it could not bind is free again")
				}

			case werr = <-done:
				healthy.Stop()
				ran = time.Since(start)
				break wait
			}
		}

		// The child is gone, so the address is free and nothing would answer on it.
		if spec.fallback != nil {
			spec.fallback.open(spec.fallbackAddr)
		}

		// Only cortex uses exitPortInUse; the gateway's status gets the normal backoff.
		note := policy.exited(spec.name == cortexChild && portInUseExit(werr), wasHealthy)
		if note.explain {
			log.Error("supervisor: the proxy cannot bind an address it needs, so it exits as soon as it starts",
				"detail", "the error the proxy logged just above names the address and the process holding it",
				"doing", "retrying, so the proxy starts by itself once that address is free",
				"not_doing", "giving up — the holder may stop at any moment, and nothing restarts this supervisor before the next login")
		}
		if note.slowed {
			log.Warn("supervisor: the address is still held; retrying less often from here",
				"attempts", note.attempts, "retry_every_at_most", superviseBlockedMaxDelay)
		}
		// Once the condition has been stated and nothing has changed, the per-restart
		// line drops to DEBUG: at the old 30s ceiling it was ~2,880 identical WARNs a
		// day, which is what buried the one line that explained anything. The child still
		// logs its own ERROR on every attempt, so the log tail `agentop service status`
		// prints always carries the diagnosis.
		restarting := log.Warn
		if note.quiet {
			restarting = log.Debug
		}
		restarting("supervisor: child exited; restarting",
			"ran", ran.Round(time.Millisecond), "err", werr, "restart_in", note.delay)

		select {
		case <-stop:
			log.Info("supervisor: stopping instead of restarting")
			return nil
		case <-time.After(note.delay):
		}
	}
}
