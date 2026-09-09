package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
)

// localProbeTimeout bounds the "is a local Cortex actually up?" check. It runs
// before the TUI starts, on the happy path of every bare `abctl`, so it has to be
// short enough not to feel like a hang.
const localProbeTimeout = 400 * time.Millisecond

// localSessionEndpoint returns the session API URL of the Cortex installed on
// this machine, or "" if there isn't one.
//
// Read from the config rather than hardcoded so it follows a port the operator
// changed. The in-cluster default is 9094 and a local install uses 47601, which
// is exactly the kind of difference a constant gets wrong.
func localSessionEndpoint() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	cfg, err := config.Load(filepath.Join(home, ".cortex", "config.yaml"))
	if err != nil {
		return ""
	}
	addr := cfg.Listener.SessionAPIAddr
	if addr == "" {
		return ""
	}
	// SplitHostPort rather than strings.Cut, which splits at the first colon and
	// mangles "[::1]:9094" into host="[" — producing a URL that simply fails to
	// connect, after which abctl falls silently through to the cluster picker.
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return ""
	}
	// A bind address is not a dial address: ":9094", "0.0.0.0:9094" and
	// "[::]:9094" all need a host a client can connect to.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// localSessionAPIUp reports whether something is listening and answering there.
//
// Checked before choosing it over the cluster picker: a stale ~/.cortex/config.yaml
// left by an install that is no longer running must not hijack `abctl` away from
// the picker for someone working against a cluster.
func localSessionAPIUp(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	c := &http.Client{Timeout: localProbeTimeout}
	resp, err := c.Get(endpoint + "/v1/sessions") //nolint:noctx // bounded by Timeout
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Only 2xx. The session API answers GET /v1/sessions with 200, so anything
	// else — a 404 from an unrelated service that happens to hold the port — is
	// not ours, and selecting it would send abctl somewhere useless instead of to
	// the cluster picker.
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// dialable is a cheap pre-check used only to keep the error message useful when
// the config exists but nothing is running.
func dialable(endpoint string) bool {
	hostport := strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	conn, err := net.DialTimeout("tcp", hostport, localProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// runningConfigTimeout bounds the fetch of a running proxy's live config. Longer
// than localProbeTimeout because this one is not a speculative "is anything
// there?" on a hot path — the user has explicitly asked to run something through
// the proxy, so a slow answer beats a wrong one.
const runningConfigTimeout = 2 * time.Second

// defaultCortexStatsURL is where a local Cortex serves its stats endpoints.
//
// A fixed default rather than a value read from ~/.cortex/config.yaml. The file
// would only ever tell us where to ask, and reading it for that reintroduces the
// staleness the live fetch exists to avoid: an edited stats address would send
// abctl to the wrong port and have it report "nothing running" about a proxy that
// is running fine. One well-known port, with --cortex-stats-url for a non-default
// install.
const defaultCortexStatsURL = "http://localhost:47602/"

// errNoRunningCortex means nothing answered at the stats URL.
var errNoRunningCortex = errors.New("no Cortex is running on this machine")

// runningConfig returns the config of the Cortex actually running, fetched from
// the stats server's /config endpoint under base.
//
// Read from the running process rather than from ~/.cortex/config.yaml on purpose.
// `abctl exec` hands its child the addresses of a proxy that must be up for the
// child to work at all, so the file is the wrong source of truth twice over: it can
// have been edited since the proxy started (listener addresses are not hot-reloaded
// at all), and it says nothing about whether anything is listening. Asking the
// process means the values describe the thing that will actually serve the request,
// and a proxy that is down is reported as down rather than yielding an environment
// that points at nothing.
func runningConfig(base string) (*config.Config, error) {
	u, perr := url.Parse(base)
	if perr != nil || u.Host == "" {
		return nil, fmt.Errorf("%q is not a URL like %s", base, defaultCortexStatsURL)
	}
	// JoinPath rather than concatenation, so a base with or without a trailing
	// slash — and the documented default has one — yields exactly one "/config".
	cfgURL := u.JoinPath("config").String()

	c := &http.Client{Timeout: runningConfigTimeout}
	resp, err := c.Get(cfgURL) //nolint:noctx // bounded by Timeout
	if err != nil {
		return nil, fmt.Errorf("%w (nothing answered at %s)", errNoRunningCortex, cfgURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w (%s returned %s)", errNoRunningCortex, cfgURL, resp.Status)
	}

	// Capped read: a local endpoint, but a wrong service holding the port should not
	// stream unbounded JSON into abctl.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", cfgURL, err)
	}
	var cfg config.Config
	if uerr := json.Unmarshal(body, &cfg); uerr != nil {
		return nil, fmt.Errorf("%s did not return a Cortex config: %w", cfgURL, uerr)
	}
	// /config redacts values whose keys end in secret/password/token/key/credential.
	// Nothing exec reads is redacted — forward_proxy_addr, tls_bridge.mode and
	// tls_bridge.ca_dir all survive — but an empty proxy address would otherwise
	// surface as a confusing downstream error, so it is named here.
	if cfg.Listener.ForwardProxyAddr == "" {
		return nil, fmt.Errorf("the Cortex at %s reports no listener.forward_proxy_addr", cfgURL)
	}
	return &cfg, nil
}
