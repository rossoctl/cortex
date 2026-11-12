package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/updatestatus"
	"github.com/rossoctl/cortex/core/config"
)

// releaseAssetURL is a file of each release. Its Last-Modified is the release
// time, and the request uses no API quota. A var so tests can serve it.
var releaseAssetURL = "https://github.com/rossoctl/cortex/releases/download/%s/checksums.txt"

// updateNow is a var so tests can set the clock.
var updateNow = time.Now

// updateWait is how long a release must be Latest before --auto installs it.
// It gives the maintainers time to stop a bad release.
const updateWait = 72 * time.Hour

// releaseTime is the Last-Modified time of tag's checksums.txt.
func releaseTime(tag string) (time.Time, error) {
	url := fmt.Sprintf(releaseAssetURL, tag)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Head(url)
	if err != nil {
		return time.Time{}, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("%s: %s", url, resp.Status)
	}
	t, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: no release time: %w", url, err)
	}
	return t, nil
}

// lockUpdate takes the update lock with no wait. held is true when another
// run has it.
func lockUpdate(cortexDir string) (unlock func(), held bool, err error) {
	if err := os.MkdirAll(cortexDir, 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(cortexDir, "update.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // our own dotdir
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return func() { _ = f.Close() }, false, nil
}

func writeUpdateStatus(cortexDir string, s updatestatus.Status) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(cortexDir, updatestatus.FileName), b, 0o600)
}

// runAutoUpdate is what the daily updater runs. It always returns 0: the
// result is in the status file, and the updater job has no one to tell.
func runAutoUpdate(env *setupEnv, stdout, stderr io.Writer) int {
	s := autoUpdate(env, stdout, stderr)
	s.CheckedAt = updateNow().UTC()
	// Read again: the install replaced the binary that env read.
	s.Installed = installedVersion(filepath.Join(env.binDir, "agentop"))
	if err := writeUpdateStatus(env.cortexDir, s); err != nil {
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
	}
	return 0
}

func autoUpdate(env *setupEnv, stdout, stderr io.Writer) updatestatus.Status {
	tag := latestRelease()
	if tag == "" {
		return updatestatus.Status{Result: updatestatus.ResultCheckFailed, Error: "could not read the newest release"}
	}
	s := updatestatus.Status{Latest: tag}
	env.readVersions()
	agentop, cortex := env.installedVersion, installedVersion(filepath.Join(env.binDir, proxyBinary))
	// A config that does not load leaves auto on: a broken config must not stop
	// the update that can repair it.
	if cfg, err := config.Load(env.configPath()); err == nil && !cfg.UpdateAutoEnabled() {
		s.Result = updatestatus.ResultDisabled
		return s
	}
	if !updatestatus.IsRelease(agentop) || !updatestatus.IsRelease(cortex) {
		s.Result = updatestatus.ResultNotARelease
		return s
	}
	if agentop == tag && cortex == tag {
		s.Result = updatestatus.ResultCurrent
		return s
	}
	released, err := releaseTime(tag)
	if err != nil {
		s.Result, s.Error = updatestatus.ResultCheckFailed, err.Error()
		return s
	}
	if updateNow().Sub(released) < updateWait {
		s.Result, s.InstallAfter = updatestatus.ResultWaiting, released.Add(updateWait).UTC()
		return s
	}
	if err := installReleaseFn(env, tag, updateFlags(env, "", true), stdout, stderr); err != nil {
		s.Result, s.Error = updatestatus.ResultFailed, strings.Join(append([]string{err.Error()},
			lastLines(filepath.Join(env.cortexDir, "update.log"), 5)...), "\n")
		return s
	}
	s.Result = updatestatus.ResultUpdated
	return s
}
