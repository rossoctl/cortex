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

	"github.com/rossoctl/cortex/core/config"
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
	cfg, _, err := localCortexConfig()
	if err != nil {
		return ""
	}
	return dialURL(cfg.Listener.SessionAPIAddr)
}

// localCortexConfig loads ~/.cortex/config.yaml and returns it with the path it
// came from. The path is what the editor needs: it is the file the local proxy
// was started with and is watching, so writing it is how a local pipeline edit
// takes effect.
func localCortexConfig() (*config.Config, string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, "", fmt.Errorf("no home directory")
	}
	path := filepath.Join(home, ".cortex", "config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, path, nil
}

// localEditTargets returns the config file a local pipeline edit writes and the
// base URL whose /reload/status confirms the proxy picked it up. Both empty when
// this machine has no usable local Cortex config, or when nothing answers where
// the config says the stats server is.
//
// The address has to be bootstrapped from the file — asking the running proxy
// where it serves /reload/status would require already knowing that address —
// but the value is then PROVEN by probing it, for a reason worth spelling out.
// config.Load defaults an absent stats.address to ":9093" (an in-cluster
// default), while a local install serves 47602. So a hand-written or
// pre-pinning config with no stats: block yields http://localhost:9093, where
// nothing is listening. That is not a harmless wrong guess: the poll would take
// 5 transport errors to a PollFailure, and RollbackCmd would then REVERT an
// edit that had already applied and hot-reloaded correctly, while reporting "is
// the local proxy still running?" about a perfectly healthy proxy.
//
// Returning empty instead means `e` declines with a message about there being
// no local target, which is a far better outcome than undoing the operator's
// work and blaming the proxy. Mirrors localSessionAPIUp, which probes for the
// same class of reason.
func localEditTargets() (configPath, statsURL string) {
	cfg, path, err := localCortexConfig()
	if err != nil {
		return "", ""
	}
	statsURL = dialURL(cfg.Stats.StatsAddress)
	if statsURL == "" || !localStatsUp(statsURL) {
		return "", ""
	}
	return path, statsURL
}

// localStatsUp reports whether a reload-status endpoint is answering at base.
//
// Probes /reload/status specifically, not just the port: that is the endpoint
// the edit flow depends on, and it is absent unless the stats server was built
// WithReloadStatus. A 200 from something else holding the port would be just as
// wrong as nothing at all.
func localStatsUp(base string) bool {
	c := &http.Client{Timeout: localProbeTimeout}
	resp, err := c.Get(base + "/reload/status") //nolint:noctx // bounded by Timeout
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	// Decode it: the poll parses this same shape, so a body it cannot read is a
	// dead end however healthy the status code looked.
	var probe struct {
		ReloadsOK *int64 `json:"reloads_ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&probe); err != nil {
		return false
	}
	return probe.ReloadsOK != nil
}

// dialURL turns a bind address from the config into a URL a client can connect
// to, or "" if it names no port.
func dialURL(addr string) string {
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
// TRUST NOTE, considered rather than missed: this moves the trust anchor from a
// file only the user can write to whatever answers on a TCP port. The response
// carries tls_bridge.ca_dir, from which exec derives a CA path it injects into four
// variables that REPLACE the child's trust store — so anything able to bind 47602
// before Cortex does (no privileges needed, any local account) could hand back a
// proxy URL and a CA of its choosing. `claude-code enable` reads a file instead, so
// this is new surface rather than a regression.
//
// Not hardened further here: on a single-user workstation a local process that can
// squat a port can generally also write ~/.cortex, so the file path is not much
// stronger, and requiring the response to "look like Cortex" is a speed bump rather
// than a boundary. Worth revisiting if abctl ever runs somewhere multi-tenant —
// checking that ca_dir is where abctl expects Cortex's own to be would be the
// cheapest first step.
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
