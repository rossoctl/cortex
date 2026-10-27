package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// proxyVars are removed from every script's environment. The proxy's own
// environment carries none of them, but a script started from a shell that routes
// through Cortex would, and then its own HTTP calls would come back through the
// pipeline that is waiting on it.
var proxyVars = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
}

// Environ returns the environment a script runs with: base without the proxy
// variables, PATH replaced by the user's login-shell PATH when one can be read,
// and PYTHONPATH led by sdkDir so `import cortex` resolves.
//
// The login PATH matters on macOS: launchd starts the service with
// /usr/bin:/bin:/usr/sbin:/sbin, where `#!/usr/bin/env python3` finds the system
// Python rather than the one the user installed and tested the script with.
func Environ(base []string, sdkDir string) []string {
	out := make([]string, 0, len(base)+2)
	var pythonPath string
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case proxyVars[k]:
			continue
		case k == "PATH":
			continue
		case k == "PYTHONPATH":
			pythonPath = v
			continue
		}
		out = append(out, kv)
	}
	path := loginPath()
	if path == "" {
		path = lookup(base, "PATH")
	}
	if path != "" {
		out = append(out, "PATH="+path)
	}
	if sdkDir != "" {
		if pythonPath != "" {
			pythonPath = sdkDir + string(os.PathListSeparator) + pythonPath
		} else {
			pythonPath = sdkDir
		}
	}
	if pythonPath != "" {
		out = append(out, "PYTHONPATH="+pythonPath)
	}
	return out
}

func lookup(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// pathMarker brackets PATH in the login shell's output, so whatever its profile
// prints on the way — a banner, nvm, conda — is not mistaken for it.
const pathMarker = "__CORTEX_LOGIN_PATH__"

var (
	loginPathMu    sync.Mutex
	loginPathVal   string
	loginPathTried time.Time
	// loginPathRetry is how long a failed probe is remembered: long enough that a
	// broken profile does not cost every reload two seconds, short enough that a
	// shell slow at boot does not leave launchd's PATH in place until a restart.
	loginPathRetry = time.Minute
	// loginPathTimeout bounds one probe. A var so a test on a loaded machine is not
	// at the mercy of the budget a real login gets.
	loginPathTimeout = 2 * time.Second
	// loginPathWaitDelay bounds the wait for stdout to close once the shell has
	// exited or been killed. A process the profile backgrounds inherits stdout, and
	// without this the probe would last as long as that process does.
	loginPathWaitDelay = 100 * time.Millisecond
)

// loginPath is the PATH a login shell of the user's own sets up. A successful
// answer is kept for the life of the process; a failed one is retried after
// loginPathRetry. Empty when there is no $SHELL or it does not answer within two
// seconds.
func loginPath() string {
	loginPathMu.Lock()
	defer loginPathMu.Unlock()
	if loginPathVal != "" || time.Since(loginPathTried) < loginPathRetry {
		return loginPathVal
	}
	loginPathTried = time.Now()
	shell := os.Getenv("SHELL")
	if shell == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginPathTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", `printf '\n'"$0"'%s'"$0"'\n' "$PATH"`, pathMarker)
	cmd.WaitDelay = loginPathWaitDelay
	out, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return ""
	}
	loginPathVal = parseLoginPath(string(out))
	return loginPathVal
}

// parseLoginPath takes PATH from between the last pair of markers in out.
func parseLoginPath(out string) string {
	end := strings.LastIndex(out, pathMarker)
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(out[:end], pathMarker)
	if start < 0 {
		return ""
	}
	return out[start+len(pathMarker) : end]
}
