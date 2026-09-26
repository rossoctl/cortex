package tlsbridge

import (
	"fmt"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/listener/skiphost"
)

type Verdict int

const (
	Passthrough Verdict = iota
	Terminate
)

type DecisionOpts struct {
	Ports map[int]bool
	// SkipHosts are host patterns to tunnel. NIL means "unset", and
	// DefaultPassthroughHosts is used; an EMPTY non-nil slice means the operator
	// explicitly wants nothing skipped. YAML distinguishes the two (absent key
	// vs `passthrough_hosts: []`), which is what makes the default overridable
	// without a second config field.
	SkipHosts []string
}

type Decision struct {
	ports map[int]bool
	skip  *skiphost.Matcher
}

// DefaultPassthroughHosts are the hosts the bridge does not intercept unless the
// operator sets passthrough_hosts explicitly.
//
// Every entry is developer tooling, and the reasoning is the same for all of
// them: no plugin can read this traffic. inference-parser, the MCP/A2A parsers
// and tool-prune all act on agent↔LLM and agent↔tool messages, and none of them
// has anything to say about a module download or a REST call to a forge. So
// forging a leaf for these hosts buys nothing — and costs a great deal, because
// the tools that talk to them are overwhelmingly Go binaries, and a Go binary
// cannot be pointed at a CA file by environment on macOS (no SSL_CERT_FILE
// support in root_darwin.go). `gh` in particular has no CA option at all
// (cli/cli#1735, open since 2020), so intercepting api.github.com breaks it with
// no configuration available to fix it.
//
// The list is grounded in observed failures — every host here appeared in a real
// proxy log as `reason=handshake-fail` — plus the sibling registries that fail
// the same way for the same reason.
//
// What this costs, stated plainly because "no plugin can read it" is only the
// upside half of the ledger: several of these hosts accept arbitrary uploads.
// *.github.com globs gist. and uploads.; *.githubusercontent.com is user
// content; ghcr.io takes blob pushes; storage.googleapis.com is object storage.
// Those are exactly where an agent would put data it wanted to exfiltrate, and
// terminating TLS was the only point at which such a payload was ever visible.
// After this change they are opaque tunnels: a session event still records the
// CONNECT host and byte counts, but not one byte of the body.
//
// That is accepted rather than overlooked. The traffic could not be inspected
// reliably anyway — before this list, an untrusting client's first request to any
// of these hosts failed and the host was then auto-skipped for ten minutes (see
// SkipSet below), so interception was intermittent and its absence silent. Both
// halves of that have since been addressed: the absence now carries a reason
// (SessionEvent.TunnelReason), and the intermittency is bounded by a window that
// starts short and is cleared outright by any client that does trust the CA. The
// list stays, because a tunnel chosen on purpose still beats one arrived at by
// failure. The
// control that actually remains for egress is allow-listing which hosts a
// workload may reach at all — iptables / NetworkPolicy in-cluster, and
// listener.skip_hosts plus the egress gate for the proxy — not TLS inspection of
// hosts whose bodies no plugin parses. A deployment that needs payload
// visibility on a specific host should set passthrough_hosts explicitly and
// arrange for its clients to trust the CA.
//
// NEVER add an inference or tool endpoint here. api.anthropic.com,
// api.openai.com, generativelanguage.googleapis.com, a LiteLLM gateway, an MCP
// server: those are the entire point of the bridge, and skipping one silently
// removes the parsing and the tool-prune savings with no error anywhere. Note
// this is why the Go-module hosts are listed one family at a time rather than as
// a blanket `*.googleapis.com` — Gemini lives under that domain.
var DefaultPassthroughHosts = []string{
	// GitHub: gh (no CA option), git, Actions, container pulls.
	"github.com",
	"*.github.com", // api., codeload., uploads.
	"*.githubusercontent.com",
	"ghcr.io",
	// Go toolchain: module proxy, checksum DB, VCS, common module hosts.
	"golang.org",
	"*.golang.org", // proxy., sum., google.
	"go.dev",       // golang.org redirects here; the toolchain's version check hits it
	"*.go.dev",
	"go.googlesource.com",
	// proxy.golang.org redirects module zips here, so omitting it left `go mod
	// download` still failing on one host after everything else was skipped.
	// Named explicitly rather than as *.googleapis.com: Gemini
	// (generativelanguage.googleapis.com) and Vertex live under that domain and
	// must stay bridged.
	"storage.googleapis.com",
	// GOTOOLCHAIN fetches a newer toolchain from here when go.mod asks for one.
	"dl.google.com",
	"go.opentelemetry.io",
	"go.yaml.in",
	"gopkg.in",
	// Package registries.
	"pypi.org",
	"*.pythonhosted.org",
	"registry.npmjs.org",
	// crates.io is the exact host; the glob covers index. (the sparse index, the
	// default registry protocol since Rust 1.70 — so `cargo build` hits it on
	// every resolve) and static. (the .crate downloads).
	"crates.io",
	"*.crates.io",
	// Docker Hub, for parity with ghcr.io above: registry-1. and auth. under the
	// glob, and the blob CDN, which is a separate domain in the same way
	// storage.googleapis.com is for Go modules.
	"docker.io",
	"*.docker.io",
	"production.cloudflare.docker.com",
}

// NewDecision compiles the interception policy. It returns an error for an
// unusable skip pattern rather than ignoring it: a pattern that silently fails
// to match presents as "the bridge broke my tool", with nothing connecting the
// symptom to the typo.
func NewDecision(o DecisionOpts) (*Decision, error) {
	d := &Decision{ports: o.Ports}
	if d.ports == nil {
		d.ports = map[int]bool{443: true, 8443: true}
	}
	patterns := o.SkipHosts
	if patterns == nil {
		patterns = DefaultPassthroughHosts
	}
	m, err := skiphost.New(patterns)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: passthrough_hosts: %w", err)
	}
	d.skip = m
	return d, nil
}

// HandlesPort reports whether port is in the bridge's interception set. It is
// the single source of truth for "which ports the bridge cares about" — the
// transparent listener consults it so it sniffs (and thus can bridge) exactly
// the configured ports, never drifting from Classify's port gate.
func (d *Decision) HandlesPort(port int) bool { return d.ports[port] }

// Classify decides whether to bridge. first is the peeked client bytes. The
// bridge intercepts everything eligible on the configured ports (no in-cluster
// vs external distinction): a port + valid-TLS-record + not-skip-listed
// connection is terminated; anything else passes through.
// Reasons Classify gives for declining to intercept. Exported and enumerated in
// ClassifyReasons so a consumer that maps them can assert it covers every one —
// otherwise adding a reason here silently produces an unmapped value downstream, and
// an unmapped tunnel reason renders as "bridged", the opposite of the truth.
const (
	ReasonPort   = "port"
	ReasonNonTLS = "non-tls"
	ReasonSkip   = "skip"
)

// ClassifyReasons is every non-empty reason Classify can return. Keep in step with
// the switch below; the mapping tests in dependent packages derive their coverage
// from this slice rather than restating it.
var ClassifyReasons = []string{ReasonPort, ReasonNonTLS, ReasonSkip}

func (d *Decision) Classify(host string, port int, first []byte) (Verdict, string) {
	if !d.ports[port] {
		return Passthrough, ReasonPort
	}
	if !looksLikeTLSRecord(first) {
		return Passthrough, ReasonNonTLS
	}
	// Glob, not exact match: the tooling hosts this skips come in families
	// (api./codeload./uploads.github.com), and Match strips the port so a caller
	// may pass either host or host:port.
	if d.skip.Match(host) {
		return Passthrough, ReasonSkip
	}
	return Terminate, ""
}

// looksLikeTLSRecord validates the 5-byte TLS record header (not just 0x16):
// content type 22 (handshake), legacy record version 0x03 with minor 0x01-0x04
// (TLS 1.0–1.3; SSLv3's 0x0300 is rejected). It checks the record layer, not the
// handshake message type.
func looksLikeTLSRecord(b []byte) bool {
	if len(b) < 5 {
		return false
	}
	return b[0] == 0x16 && b[1] == 0x03 && b[2] >= 0x01 && b[2] <= 0x04
}

const (
	// skipBackoffBase is the FIRST skip window after a host's leaf is rejected, and
	// skipTTL is the ceiling it doubles up to. Both matter, for opposite cases.
	//
	// The window is collateral: it suppresses interception for every client of that
	// host, not just the one that rejected us — the set is keyed by host, and there is
	// no durable client identity to key it by (a source port changes per connection,
	// and peer-PID has no portable API over TCP). So on a machine where one agent
	// holds a stale CA and the rest are fine, a long window costs the healthy ones all
	// their observability. Ten minutes of it, re-armed on every attempt, is how a
	// laptop lost 99% of the visibility on one host for two hours.
	//
	// Starting short and doubling separates the two situations without needing to tell
	// the clients apart:
	//
	//   mixed clients   a success CLEARS the entry, so the counter never climbs and
	//                   windows stay near the base — the healthy client is visible
	//                   again seconds after a stale one trips it.
	//   pinned host     nothing ever succeeds, so it escalates to skipTTL and settles
	//                   at today's behaviour. That is the case the skip was built for
	//                   and it must not regress: a fixed short window would break such
	//                   a client's handshake every 30s forever.
	//
	// The cost is borne by the misconfigured client — its forged handshake fails once
	// per window rather than once per ten minutes — which is the right way round, and
	// it now gets a warning naming itself and the CA.
	skipBackoffBase = 30 * time.Second
	skipTTL         = 10 * time.Minute
	skipMax         = 4096
)

// SkipSet is the runtime auto-skip set (hosts whose minted leaf the client
// rejected). Concurrent-safe; augments the static skip list. Entries expire
// after skipTTL and the set is bounded to skipMax (oldest-expiry eviction).
type SkipSet struct {
	mu sync.RWMutex
	// base is the first window and ttl the ceiling it doubles up to. base is a field
	// rather than only a package const so a same-package test can drive the actual
	// doubling: with base fixed at 30s, tightening ttl alone caps every window to ttl
	// regardless of the failure count, which exercises cap-and-reset and not escalation.
	base time.Duration
	ttl  time.Duration
	max  int
	m    map[string]skipEntry
}

// skipEntry is one skipped host: when the window ends, and how many consecutive
// rejections have set it. failures drives the backoff and is what a success discards.
type skipEntry struct {
	expiry   time.Time
	failures int
}

func NewSkipSet() *SkipSet {
	return &SkipSet{base: skipBackoffBase, ttl: skipTTL, max: skipMax, m: map[string]skipEntry{}}
}

// backoffFor is the window after n consecutive failures: base, doubling, capped at ttl.
// Shifting past 63 would wrap, so the cap is applied on the count first — a host that
// has failed sixty times is at the ceiling either way.
func (s *SkipSet) backoffFor(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > 32 {
		return s.ttl
	}
	d := s.base << (n - 1)
	if d > s.ttl || d <= 0 {
		return s.ttl
	}
	return d
}

// Fail records a failure that IS evidence of a persistent trust problem — the client
// rejected the minted leaf — and lengthens the window for each consecutive one. A
// Succeed in between resets the count.
func (s *SkipSet) Fail(host string) { s.fail(host, true) }

// FailTransient records a failure that is NOT evidence about trust: a client that hung
// up, a version or cipher mismatch, or our own minting failing. It still seeds a skip,
// because the forged handshake killed that connection either way and the retry needs a
// tunnel — but it does not escalate, so a client that merely cancels a lot cannot walk
// the host up to the ceiling and take every other client's observability with it.
//
// Which one to call is the CALLER's decision, deliberately: it holds the classified
// reason, and passing that vocabulary down here would make this package depend on the
// session-event types it has no other business knowing about.
func (s *SkipSet) FailTransient(host string) { s.fail(host, false) }

func (s *SkipSet) fail(host string, escalate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.m) >= s.max {
		// Purge expired entries; if still full, drop the earliest-expiring one.
		// Fail is cold (only fires when a minted leaf is rejected), so an O(n)
		// sweep here is cheap.
		var oldestK string
		var oldestT time.Time
		for k, e := range s.m {
			if !e.expiry.After(now) {
				delete(s.m, k)
				continue
			}
			if oldestK == "" || e.expiry.Before(oldestT) {
				oldestK, oldestT = k, e.expiry
			}
		}
		if len(s.m) >= s.max && oldestK != "" {
			delete(s.m, oldestK)
		}
	}
	// An expired entry starts over rather than continuing to escalate: the window
	// having elapsed with no further rejection is evidence the problem may be gone.
	n := 1
	if e, ok := s.m[host]; ok && e.expiry.After(now) {
		if escalate {
			n = e.failures + 1
		} else {
			// Hold the count where it is: a transient failure must not lengthen the
			// window, but it must not shorten one a real rejection already earned
			// either.
			n = e.failures
			if n < 1 {
				n = 1
			}
		}
	}
	// .Round(0) strips the monotonic reading so the expiry is a pure wall-clock
	// time. Contains compares it against time.Now() via the wall clock, so an
	// entry expires after its window of real time even across a suspend (where the
	// monotonic clock freezes and would otherwise keep the host skipped longer).
	s.m[host] = skipEntry{expiry: now.Add(s.backoffFor(n)).Round(0), failures: n}
}

// Succeed records that a client completed the forged handshake for this host, which
// disproves the entry outright and clears it — count included.
//
// This is the signal the set never had. It only ever added entries and waited them
// out, so a demonstrably-trusting client bridging successfully taught it nothing and
// the next connection was still tunnelled. Clearing here is what makes the collateral
// window seconds long instead of minutes, and it is why restarting a stale agent
// restores observability immediately rather than after a wait.
func (s *SkipSet) Succeed(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, host)
}

func (s *SkipSet) Contains(host string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[host]
	return ok && time.Now().Before(e.expiry) // expired entries read as absent; Fail reclaims them
}
