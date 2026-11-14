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
	"gopkg.in/yaml.v3"
)

// releaseAssetURL is a file of each release. Its Last-Modified is the release
// time, and the request uses no API quota. A var so tests can serve it.
var releaseAssetURL = "https://github.com/rossoctl/cortex/releases/download/%s/checksums.txt"

// updateNow is a var so tests can set the clock.
var updateNow = time.Now

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

// runAutoUpdate is what the updater runs. It always returns 0: the
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

// autoUpdateOff reports an explicit update.auto: false.
func autoUpdateOff(path string) bool {
	u, _ := readUpdateConfig(path)
	return !(&config.Config{Update: u}).UpdateAutoEnabled()
}

// updateWait is update.wait: how long after its publication (the upload time of
// its checksums.txt) --auto installs the Latest release. It gives the maintainers
// time to stop a bad release. A bad value gives the default: the proxy refuses it
// at boot, and agentop doctor names it.
func updateWait(path string) time.Duration {
	u, _ := readUpdateConfig(path)
	w, _, _ := u.Times()
	return w
}

// readUpdateConfig reads only the update block, so an error in another field
// does not cancel the opt-out. A file that does not parse gives nil, which leaves
// auto on: a broken config must not stop the update that can repair it. A field
// of the wrong type gives the block with that field empty, and the error.
func readUpdateConfig(path string) (*config.UpdateConfig, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the user's own config
	if err != nil {
		return nil, nil
	}
	var c struct {
		Update *config.UpdateConfig `yaml:"update"`
	}
	// The same ${VAR} expansion as config.Load.
	expanded := os.Expand(string(b), func(key string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return "${" + key + "}"
	})
	err = yaml.Unmarshal([]byte(expanded), &c)
	// yaml.v3 decodes the other fields after a type error.
	if te := (*yaml.TypeError)(nil); errors.As(err, &te) {
		// It also sets a wrong-type auto to false. Keep auto only when it decodes alone.
		var a struct {
			Update struct {
				Auto *bool `yaml:"auto"`
			} `yaml:"update"`
		}
		if c.Update != nil && yaml.Unmarshal([]byte(expanded), &a) != nil {
			c.Update.Auto = nil
		}
		return c.Update, fmt.Errorf("update: %s", strings.Join(te.Errors, "; "))
	}
	if err != nil {
		return nil, nil
	}
	return c.Update, nil
}

func autoUpdate(env *setupEnv, stdout, stderr io.Writer) updatestatus.Status {
	tag := latestRelease()
	if tag == "" {
		return updatestatus.Status{Result: updatestatus.ResultCheckFailed, Error: "could not read the newest release"}
	}
	s := updatestatus.Status{Latest: tag}
	env.readVersions()
	cortexPath := filepath.Join(env.binDir, proxyBinary)
	agentop, cortex := env.installedVersion, installedVersion(cortexPath)
	if autoUpdateOff(env.configPath()) {
		s.Result = updatestatus.ResultDisabled
		return s
	}
	if !updatestatus.IsRelease(agentop) || !updatestatus.IsRelease(cortex) {
		s.Result = updatestatus.ResultNotARelease
		return s
	}
	if !updatestatus.IsRelease(tag) {
		s.Result, s.Error = updatestatus.ResultCheckFailed, "Latest "+tag+" is not a plain release"
		return s
	}
	// A cpex cortex reports the tag too, and the default build must replace it.
	if agentop == tag && cortex == tag && !installedIsCpex(cortexPath) {
		s.Result = updatestatus.ResultCurrent
		return s
	}
	released, err := releaseTime(tag)
	if err != nil {
		s.Result, s.Error = updatestatus.ResultCheckFailed, err.Error()
		return s
	}
	if wait := updateWait(env.configPath()); updateNow().Sub(released) < wait {
		s.Result, s.InstallAfter = updatestatus.ResultWaiting, released.Add(wait).UTC()
		return s
	}
	// --ref pins the tag that passed the wait. Without it, install.sh reads Latest
	// again, and a release published in between skips the wait.
	if err := installReleaseFn(env, tag, updateFlags(env, tag, true), stdout, stderr); err != nil {
		s.Result, s.Error = updatestatus.ResultFailed, strings.Join(append([]string{err.Error()},
			lastLines(filepath.Join(env.cortexDir, "update.log"), 5)...), "\n")
		return s
	}
	s.Result = updatestatus.ResultUpdated
	return s
}
