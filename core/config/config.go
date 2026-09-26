// Package config provides YAML-based configuration with mode presets
// and startup validation for the AuthBridge auth layer.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
	"github.com/rossoctl/cortex/core/session"
	"gopkg.in/yaml.v3"
)

// Config is the top-level AuthBridge configuration.
//
// Plugin-specific settings (inbound JWT validation, outbound token
// exchange, identity, bypass paths, routes) live inside their
// respective entries under Pipeline.* now — see the plugin reference at
// docs/plugin-reference.md for how each plugin
// declares its own config schema and defaults.
type Config struct {
	Mode     string         `yaml:"mode" json:"mode"` // "envoy-sidecar", "proxy-sidecar"
	Listener ListenerConfig `yaml:"listener" json:"listener"`
	Pipeline PipelineConfig `yaml:"pipeline" json:"pipeline"`
	Session  SessionConfig  `yaml:"session" json:"session"`
	Stats    StatsConfig    `yaml:"stats" json:"stats"`
	// MTLS, when non-nil, enables transport-level mTLS using SPIRE
	// X.509 SVIDs. Applies symmetrically to inbound (reverse-proxy)
	// and outbound (forward-proxy) traffic in proxy-sidecar mode;
	// envoy-sidecar mode is unaffected (Envoy handles its own TLS via
	// SDS). Pointer so absent block = today's plaintext behavior.
	MTLS *MTLSConfig `yaml:"mtls,omitempty" json:"mtls,omitempty"`
	// SPIFFE, when non-nil, configures the in-process Provider that
	// supplies X.509-SVIDs to the mTLS listeners and a JWT-SVID to the
	// token-exchange plugin (when configured). Pointer so absent block
	// = today's spiffe-helper-driven behavior (until the chart/operator
	// follow-ups land and start populating the block).
	SPIFFE *SPIFFEConfig `yaml:"spiffe,omitempty" json:"spiffe,omitempty"`
	// TLSBridge, when non-nil and Enabled, terminates agent outbound TLS so the
	// outbound pipeline sees decrypted HTTPS. See docs/.../tlsbridge-design.md.
	TLSBridge *TLSBridgeConfig `yaml:"tls_bridge,omitempty" json:"tls_bridge,omitempty"`
	// Pricing configures model rates for the whole process — one section rather
	// than a knob per plugin, so cost is consistent wherever it is reported.
	//
	// Absent means the bundled price table alone, which is deliberate: covering
	// internal usage with no manual setup is the point. Set `pricing.bundled:
	// false` to price only what you configure. See core/pricing.
	Pricing *pricing.Config `yaml:"pricing,omitempty" json:"pricing,omitempty"`
	// CostLedger configures the durable per-minute cost ledger (core/costledger),
	// which persists closed minutes so "what did today cost" survives a restart.
	//
	// Absent means the caller's default, and the callers differ deliberately: a local
	// install turns it ON (a laptop has a home directory and a developer who wants
	// yesterday's number), Kubernetes leaves it OFF (writing files in a pod is the
	// wrong sink; a central collector is the right one). Set `cost_ledger.enabled:
	// false` to turn it off locally.
	CostLedger *CostLedgerConfig `yaml:"cost_ledger,omitempty" json:"cost_ledger,omitempty"`
}

// CostLedgerConfig configures the durable cost ledger.
//
// NOT HOT-RELOADABLE, unlike most of this file. The reloader swaps the plugin pipeline
// and per-plugin config in place, but the ledger is constructed once at startup and
// handed to the session store as a recorder, so a running proxy holds whichever writer
// it opened. Editing anything here — enabled, dir, retention_days — takes effect on
// RESTART.
//
// The edit is REFUSED rather than ignored: reloader.validateReloadable compares this
// block the way it compares mode and listener.*, so a live edit fails the reload,
// leaves LastError naming cost_ledger on /reload/status, and asks for a pod restart.
//
// It did not always. This doc used to warn that "an operator who edits this to stop
// writing cost history has every reason to believe it stopped" — and they did, because
// the edit was ACCEPTED: ReloadsOK incremented, ActiveConfigSHA256 moved, and /config
// served the new values while the startup writer kept appending under the old
// retention. Documenting that was the weaker of the two options available; the guard
// is three lines and the precedent for it was already in the same function. Pinned by
// reloader.TestReloader_RefusesCostLedgerChange.
type CostLedgerConfig struct {
	// Enabled is a POINTER so "unset" and "explicitly false" are different states.
	// The local default is on, and an operator has to be able to turn it off; with a
	// plain bool an absent block and `enabled: false` would be the same value, so the
	// only way to disable it would be to delete the whole block — which also discards
	// the retention setting beside it.
	//
	// Takes effect on restart. See the type doc.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Dir is where day files are written. Empty means the caller's default, which for
	// a local install is ~/.cortex/cost — kept out of this struct so the config does
	// not pin a $HOME-derived absolute path into a file that may be copied between
	// machines.
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty"`
	// RetentionDays is how many day files survive. Zero means the package default of
	// 31, which is roughly 10 MB.
	//
	// THIRTY-ONE because that is what window=month needs: a month-to-date total on the 31st
	// of a 31-day month opens 31 day files, and costledger's prune keeps exactly
	// retention_days distinct dates. See costledger.defaultRetentionDays, which is pinned to
	// usage.WindowMonthLocalDays.
	//
	// A non-zero value must be at least minCostLedgerRetentionDays; see there. Note that the
	// FLOOR is lower than the default: a deployment may legitimately keep less history than
	// window=month needs, and one that does will answer that window short.
	//
	// THE SHORTFALL IS DISCLOSED, which an earlier version of this comment denied: a pruned day
	// file is absent rather than unreadable, so it produces no Caveats entry — but
	// usage.Snapshot.DaysOutsideRetention counts the days a REQUEST asks for beyond this setting,
	// and the band marks such a total as a floor while `abctl cost` prints a coverage line. What
	// is still silent is a day pruned INSIDE the current horizon, from a setting that used to be
	// shorter; see that field. The default covers the longest shipped window so that the ordinary
	// case needs no disclosure at all.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty"`
}

// minCostLedgerRetentionDays is the floor a NON-ZERO retention_days has to clear, so
// window=7d cannot be answered over a partial week without saying so.
//
// NINE, not seven. window=7d is a rolling 7x24h, so it starts part-way through a date
// and reads EIGHT local day files — and nine in a spring-forward week, which is 167
// hours long and so reaches an hour further back. Both corrections had the same shape:
// counting days instead of counting the files the span opens.
//
// Refused at load rather than clamped: an operator who chose the number should hear that
// it is wrong, and a silent clamp makes /config disagree with what was written.
//
// A literal rather than derived from usage.Window7dLocalDays, because this package is the
// leaf every binary loads to parse its config and must not import the aggregator.
// TestMinCostLedgerRetentionDays_MatchesTheWindowItProtects keeps the two equal, which is
// the right shape for a cross-package invariant that is agreement rather than a
// dependency.
const minCostLedgerRetentionDays = 9

// maxCostLedgerRetentionDays is the ceiling a retention_days has to stay under, and the
// reason there is one at all is that the failure INVERTS.
//
// prune counts back with ref.AddDate(0, 0, -(retainDays-1)). AddDate normalises, so a large
// enough retention does not merely reach further back — it wraps. Measured at
// retainDays = 1<<62-1 against a reference of 2026-09-15: the cutoff came out as
// 2026-09-17, TWO DAYS IN THE FUTURE, which makes every day file including today older than
// the cutoff. So the largest number an operator can type, meaning "keep everything", deletes
// the entire ledger on the first prune. A validator that bounded only the floor let that
// through while carefully explaining why 7 was too small.
//
// TEN YEARS, which is far past any use for a per-minute cost file on a laptop (3,650 days is
// roughly 1.2 GB at the measured ~10 MB per 30 days) and far below the region where the
// arithmetic misbehaves. It is deliberately not derived from math.MaxInt: a bound chosen to
// be "just safe" would need a reader to verify the overflow arithmetic to know it is safe,
// where a bound this far away is obviously so.
const maxCostLedgerRetentionDays = 3650

// DirSet reports whether an operator named a directory for the ledger.
//
// IT IS THE STRONGEST AVAILABLE SIGNAL THAT THE LEDGER CAN DELIVER, which is why the caller
// uses it to pick the default. The ledger's whole purpose is surviving a restart; writing to a
// path nobody chose achieves the opposite in a container, where the only place left is the
// image's writable layer — wiped on every restart and counted against the pod's
// ephemeral-storage limit, which is an eviction rather than a lost figure. An operator who
// names a path has mounted something to put it on, and that is a fact this package can see
// where "is there a volume here" is not.
//
// A method rather than a field read, and nil-safe, because the block is absent in the common
// Kubernetes case and every caller would otherwise repeat the same check.
func (c *CostLedgerConfig) DirSet() bool {
	return c != nil && c.Dir != ""
}

// LedgerEnabled reports whether the ledger should run, given the default for this
// deployment shape.
//
// WHAT defaultOn USED TO MEAN, AND WHY IT CHANGED. It was localMode — true under --local,
// false otherwise — and that made the default a property of WHICH FLAG STARTED THE BINARY
// rather than of whether the ledger could do its job. Every service install runs --config, so
// the documented "on by default" was false for every installed laptop. The caller now derives
// defaultOn from whether a durable location exists at all (see DirSet), so the answer no
// longer depends on the command line.
//
// A method on the pointer receiver so a nil block — the common case in Kubernetes —
// answers without every caller writing the same nil check and one of them getting it
// backwards.
func (c *CostLedgerConfig) LedgerEnabled(defaultOn bool) bool {
	if c == nil || c.Enabled == nil {
		return defaultOn
	}
	return *c.Enabled
}

// Validate is called from the loader when CostLedger != nil.
func (c *CostLedgerConfig) Validate() error {
	if c.RetentionDays < 0 {
		return fmt.Errorf("cost_ledger.retention_days must not be negative, got %d", c.RetentionDays)
	}
	if c.RetentionDays > maxCostLedgerRetentionDays {
		// Named as the inversion it is rather than as a preference about disk. An operator who
		// typed a huge number meant "keep everything", and the honest message is that this one
		// would keep nothing.
		return fmt.Errorf("cost_ledger.retention_days must be at most %d (ten years), got %d: "+
			"retention is counted back with AddDate, which NORMALISES rather than saturating, so a "+
			"large enough value wraps the cutoff into the FUTURE and the first prune deletes every "+
			"day file including today — the opposite of what the number says",
			maxCostLedgerRetentionDays, c.RetentionDays)
	}
	if c.RetentionDays > 0 && c.RetentionDays < minCostLedgerRetentionDays {
		// Says NINE and says why it is not seven: an operator who typed 7 for a seven-day
		// window is not making a careless mistake, and a message that only quoted the floor
		// would read as an off-by-one in the software rather than as the rolling span it is.
		// It names both increments, because 8 is as reasonable a guess as 7 and was the floor
		// until a spring-forward week was measured against it.
		// The numbers are spelled out rather than interpolated from core/usage, for the
		// reason minCostLedgerRetentionDays is a literal: this package must not import the
		// aggregator. Every one of them is pinned against usage's own constants by
		// TestMinCostLedgerRetentionDays_MatchesTheWindowItProtects, so a window change
		// makes this message wrong in a test rather than wrong in front of an operator.
		return fmt.Errorf("cost_ledger.retention_days must be at least %d, or 0 for the default: "+
			"the usage API serves window=7d from these day files, and that window is a ROLLING "+
			"7x24h, so it reads 8 local day files rather than 7 — and 9 in a spring-forward "+
			"week, which is only 167 hours long — a shorter retention reports a partial week "+
			"as a full one, got %d",
			minCostLedgerRetentionDays, c.RetentionDays)
	}
	return nil
}

// TLSBridgeConfig configures the outbound TLS bridge (TLS termination of
// agent egress — formerly "MITM" — so the outbound plugin pipeline sees
// decrypted HTTPS).
type TLSBridgeConfig struct {
	// Mode is the bridge posture: "disabled" (off) or "enabled" (intercept all
	// eligible egress on the configured Ports). Empty == disabled.
	Mode string `yaml:"mode" json:"mode"` // disabled | enabled
	// CADir holds the per-agent signing CA, mounted from the operator's
	// cert-manager Secret. The bridge reads tls.crt + tls.key (to sign leaves);
	// ca.crt is the trust cert handed to the agent. cert-manager Secret key
	// conventions, so only the directory is configured.
	CADir string `yaml:"ca_dir" json:"ca_dir"`
	// GenerateCA, when true, makes the bridge mint and persist a self-signed CA
	// into CADir (tls.crt/tls.key/ca.crt) if those files are absent, instead of
	// failing at startup. For standalone / demo use only — in-cluster the CA is
	// a mounted cert-manager Secret and this stays false so a missing Secret
	// fails loudly rather than silently forging leaves under an untrusted CA.
	// CADir should persist across restarts: on ephemeral storage (e.g. an
	// emptyDir) a fresh CA is minted each boot, so clients must re-trust the
	// new ca.crt.
	GenerateCA bool `yaml:"generate_ca" json:"generate_ca"`
	// UpstreamCABundle is an extra-roots PEM file for re-origination (private-CA
	// origins the agent trusts); empty == system roots only.
	UpstreamCABundle string `yaml:"upstream_ca_bundle" json:"upstream_ca_bundle"`
	// UpstreamInsecure disables origin cert verification on re-origination
	// (InsecureSkipVerify). For internal/self-signed upstreams when the origin CA
	// is unavailable. Prefer UpstreamCABundle when possible.
	UpstreamInsecure bool `yaml:"upstream_insecure" json:"upstream_insecure"`
	// PassthroughHosts are hosts to tunnel (never intercept). Distinct from
	// listener.skip_hosts, which bypasses the whole pipeline; these still run the
	// egress gate, they just aren't TLS-terminated.
	PassthroughHosts []string `yaml:"passthrough_hosts" json:"passthrough_hosts"`
	// Ports is the set of TCP ports to intercept as TLS. Empty => {443, 8443}.
	// Only HTTP(S)-bearing ports belong here: the bridge serves the decrypted
	// stream as HTTP/1.1 or h2, so terminating a non-HTTP TLS protocol (LDAPS,
	// SMTPS, DB-over-TLS, …) would break it.
	Ports []int `yaml:"ports" json:"ports"`
}

// Validate is called from the loader when TLSBridge != nil.
func (b *TLSBridgeConfig) Validate() error {
	if b.Mode != "" && b.Mode != "disabled" && b.Mode != "enabled" {
		return fmt.Errorf("tls_bridge.mode must be 'disabled' or 'enabled', got %q", b.Mode)
	}
	if b.Mode == "enabled" && b.CADir == "" {
		return fmt.Errorf("tls_bridge.mode=enabled requires ca_dir")
	}
	for _, p := range b.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("tls_bridge.ports: %d is out of range 1-65535", p)
		}
	}
	return nil
}

// MTLSMode names the inbound + outbound TLS posture. Vocabulary
// borrows from Istio's PeerAuthentication.mtls.mode for familiarity.
type MTLSMode string

const (
	// MTLSModePermissive accepts both TLS and plaintext on the
	// inbound side (byte-peek listener) and tries TLS on the outbound
	// side, falling back to plain TCP on handshake failure. The
	// rollout-friendly default; when an operator omits the mode
	// field, this is what they get.
	MTLSModePermissive MTLSMode = "permissive"
	// MTLSModeStrict accepts only TLS on the inbound side (byte-peek
	// closes non-TLS connections) and treats outbound TLS handshake
	// failures as hard errors with no fallback. Production posture
	// once the cluster is fully mTLS-capable.
	MTLSModeStrict MTLSMode = "strict"
)

// MTLSConfig is the top-level mTLS schema. One mode applies to both
// directions; if asymmetric needs surface later, this can split into
// separate Inbound / Outbound sub-blocks without breaking the
// existing flat shape.
//
// X.509-SVID material is supplied by the in-process Provider (see
// SPIFFEConfig) and no longer configured here. Legacy chart configs may
// still ship cert_file / key_file / bundle_file keys; the YAML loader
// silently drops them (pinned by TestLoad_UnknownMTLSFields_Ignored).
type MTLSConfig struct {
	// Mode controls the inbound + outbound TLS posture. Defaults to
	// permissive when empty.
	Mode MTLSMode `yaml:"mode" json:"mode"`
}

// ResolvedMode returns Mode with the empty-string default applied.
func (m *MTLSConfig) ResolvedMode() MTLSMode {
	if m == nil || m.Mode == "" {
		return MTLSModePermissive
	}
	return m.Mode
}

// Validate rejects unknown mode values at startup. SVID material is
// supplied by the SPIFFE Provider (see SPIFFEConfig); validation of
// that material is the Provider's responsibility, not this struct's.
func (m *MTLSConfig) Validate() error {
	if m == nil {
		return nil
	}
	switch m.Mode {
	case "", MTLSModePermissive, MTLSModeStrict:
		return nil
	default:
		return fmt.Errorf("mtls.mode: %q is not a recognized value (use %q or %q)",
			m.Mode, MTLSModePermissive, MTLSModeStrict)
	}
}

// SPIFFEConfig is the top-level SPIFFE provider configuration. One block
// drives the in-process Provider that supplies X.509-SVIDs to the mTLS
// listeners and a JWT-SVID to the token-exchange plugin (when configured).
//
// Defaults match today's spiffe-helper-driven setup so existing
// deployments boot without changes once chart/operator follow-ups land.
//
// The audience for the JWT-SVID used by token-exchange as a client
// assertion is per-plugin (tokenexchange.identity.jwt_audience) and is
// no longer carried here — only the tokenexchange plugin's spiffe
// identity path consumes it, so it lives in that plugin's config.
type SPIFFEConfig struct {
	// Socket is the SPIRE agent socket URL. Defaults to
	// "unix:///spiffe-workload-api/spire-agent.sock" — the same path
	// spiffe-helper used to talk to.
	Socket string `yaml:"socket" json:"socket"`

	// MirrorFiles, when true, runs an in-process goroutine that writes
	// /opt/svid.pem, /opt/svid_key.pem, /opt/svid_bundle.pem, and
	// /opt/jwt_svid.token on every rotation — preserving today's
	// external-reader contract (Envoy filesystem SDS, e2e probes,
	// debugging shells). Pointer so we can distinguish unset
	// ("apply default true") from explicit false ("operator opted out").
	MirrorFiles *bool `yaml:"mirror_files" json:"mirror_files"`

	// MirrorDir is the directory where mirror files are written.
	// Defaults to "/opt". Only used when MirrorFiles is true.
	MirrorDir string `yaml:"mirror_dir" json:"mirror_dir"`
}

// Validate rejects sockets that aren't unix:// URLs. The Workload API
// only speaks over a unix domain socket in our deployment model; a
// tcp:// or http:// scheme is almost certainly an operator typo and
// should fail at startup rather than at first dial.
func (s *SPIFFEConfig) Validate() error {
	if s == nil {
		return nil
	}
	if !strings.HasPrefix(s.Socket, "unix://") {
		return fmt.Errorf("spiffe.socket must be a unix:// URL, got %q", s.Socket)
	}
	return nil
}

// SessionConfig controls in-memory session tracking for cross-request correlation.
// When enabled, the framework records inbound intents and outbound tool calls so
// that guardrail plugins can evaluate sequences across request boundaries.
//
// Enabled is a pointer so the loader can distinguish "unset" (apply default)
// from "explicitly false" (user opted out). Default when unset: enabled.
type SessionConfig struct {
	// Enabled: nil means "unset → default on". Explicit `false` opts out.
	// Do not change to a plain bool — losing the nil sentinel would collapse
	// "user didn't say" with "user said false" and silently flip the default.
	Enabled *bool `yaml:"enabled" json:"enabled"`
	// TTL bounds how long an IDLE session is kept. Empty or "0" means never, which is
	// the default: time-based expiry read as data loss — traffic vanished because
	// someone stepped away, not because anything overflowed. Set it to a duration
	// ("30m") where limiting how long raw prompts sit in memory is worth the surprise,
	// or where one long-lived session would otherwise grow without limit — with
	// MaxEvents unset, a ttl is the only thing that bounds that shape. See Limits for
	// how the three resolve together and why the default is what it is.
	TTL string `yaml:"ttl" json:"ttl"` // duration string; default: never

	// MaxEvents bounds how many events ONE session keeps, oldest evicted first.
	// Unset (0) means unlimited, which is the default: a trimmed store is lossy on
	// exactly the sessions worth reading, and the trim point is invisible from the
	// timeline. Set it where one long-lived chatty session would otherwise grow
	// without limit — MaxSessions bounds how MANY sessions are kept, not how big any
	// one of them gets, so it is no help against a single session that never ends.
	MaxEvents int `yaml:"max_events" json:"max_events"` // max events per session; default: unlimited
	// MaxSessions bounds how many sessions are kept at once, least-recently-updated
	// evicted first. Unset means 100.
	//
	// NOT "0 = unlimited", which is what this said and what the store would do with a
	// zero it was handed: Limits substitutes 100 for any value <= 0, so the documented
	// unlimited was unreachable through every binary. It cannot be honoured either,
	// because MaxSessions is a plain int — an operator who never mentions max_sessions
	// yields the same 0 as one who writes it, so honouring 0 would delete the default
	// for everyone who left it alone, and with MaxEvents now unset that is the last
	// default bound standing. Distinguishing the two would take a *int, the way Enabled
	// above uses a *bool for exactly this reason. Not worth it for a value nobody has
	// asked for: to lift the cap, set it high.
	MaxSessions int `yaml:"max_sessions" json:"max_sessions"` // max concurrent sessions; default: 100 (<= 0 means default)

	// IDHeaders names the request headers consulted, in order, for a
	// client-supplied session id to bucket events under. Unset means the built-in
	// list of supported-agent headers (see SessionIDHeaders); an explicit list
	// REPLACES that default rather than extending it, and an explicit empty list
	// turns header bucketing off and puts every session back in one shared bucket.
	// Like Enabled above, the nil-versus-empty distinction is load-bearing — do
	// not collapse it by assigning a default at load time.
	//
	// The ids these headers carry are client-asserted and unauthenticated: a
	// client can name any bucket, including another session's. The store is
	// in-process, so the reach of that is every client that can reach this proxy
	// — one workload for a sidecar, but all of them for a shared or standalone
	// forward proxy, which binds a wildcard address by default. Set this to an
	// empty list in any deployment where telemetry attribution is a trust
	// boundary rather than a convenience. See session.IDFromHeaders for the full
	// reasoning.
	IDHeaders []string `yaml:"id_headers" json:"id_headers"`
}

// SessionIDHeaders returns the headers to consult for a client-supplied session
// id, defaulting to the known coding-agent session headers when unset: bucketing
// per coding-agent session is the point of running this on a laptop, and an
// operator should not have to learn a header name to get it. An explicit empty
// list disables the lookup.
//
// The order is a precedence rule over clients, not a ranking — a request
// carrying both headers buckets under the Claude Code id. See
// session.IDFromHeaders.
//
// Whatever this returns, a request that carries none of the named headers
// buckets exactly as it did before this option existed.
func (s SessionConfig) SessionIDHeaders() []string {
	if s.IDHeaders == nil {
		return []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader}
	}
	return s.IDHeaders
}

// SessionEnabled returns true when session tracking should run. Defaults to true
// when Enabled is unset, so operators need to explicitly opt out.
func (s SessionConfig) SessionEnabled() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// SessionLimits is the resolved form of the three store parameters: what
// session.New should be called with, after defaults.
type SessionLimits struct {
	TTL         time.Duration
	MaxEvents   int
	MaxSessions int
}

// Limits resolves the session store's parameters from config.
//
// One home for what used to be the same twenty lines in three main packages, kept in
// step by review alone. It is also the only way to assert the part that matters most
// and is easiest to get wrong in one copy: MaxEvents passes through UNMODIFIED, so an
// operator who has not set session.max_events gets a store that never evicts an event
// from a live session.
//
// The defaults, and why they are not symmetrical:
//
//   - TTL 0 (never expire on time). Time-based expiry read as data loss — traffic
//     vanished because someone stepped away, not because anything overflowed. Note
//     what this does NOT rest on any more: it used to be justified by "the size caps
//     still bound memory", and with MaxEvents unset by default that is no longer true
//     for a single long-lived session. TTL stays 0 because a clock is the wrong tool
//     for bounding memory, not because something else is bounding it.
//   - MaxEvents 0 (unlimited). A trimmed store is lossy on exactly the sessions worth
//     reading, and FIFO eviction takes the BEGINNING of a session — on a long agent
//     run, the inbound request that started it — with no mark left in the timeline.
//   - MaxSessions 100, for any configured value <= 0 including unset. This one keeps a
//     default because it bounds how MANY sessions are kept, which is the dimension that
//     grows without an operator doing anything unusual, and evicting a whole idle
//     session costs less than truncating a live one. So the resolved MaxSessions is
//     always positive, which is why LogAttrs does not render an "unlimited" for it the
//     way it does for MaxEvents.
//
// So on a single session that never ends, nothing here is a ceiling: set max_events or
// a ttl for that shape. A negative max_events means unlimited, the same as unset — it
// used to fall back to 500, and the startup log reports the resolved value either way.
//
// A malformed session.ttl yields the zero TTL and is returned as an error for the
// caller to log; resolution stays here, logging stays at the edge.
func (s SessionConfig) Limits() (SessionLimits, error) {
	lim := SessionLimits{MaxEvents: s.MaxEvents, MaxSessions: 100}
	if s.MaxSessions > 0 {
		lim.MaxSessions = s.MaxSessions
	}
	if s.TTL == "" {
		return lim, nil
	}
	d, err := time.ParseDuration(s.TTL)
	if err != nil {
		return lim, err
	}
	lim.TTL = d
	return lim, nil
}

// LogAttrs renders the resolved limits for a startup line, spelling out the two zeros
// that would otherwise read as misconfiguration: "ttl=0s" as if sessions expired
// instantly, "maxEvents=0" as if the store kept nothing. Takes limits as Limits
// produces them, where MaxSessions is always positive.
func (l SessionLimits) LogAttrs() []any {
	ttl := "never"
	if l.TTL > 0 {
		ttl = l.TTL.String()
	}
	maxEvents := "unlimited"
	if l.MaxEvents > 0 {
		maxEvents = strconv.Itoa(l.MaxEvents)
	}
	// No "unlimited" case for MaxSessions: Limits resolves every value <= 0 to 100, so
	// a zero cannot reach here, and a branch for it would document a semantic no config
	// can produce.
	return []any{"expiry", ttl, "maxEvents", maxEvents, "maxSessions", strconv.Itoa(l.MaxSessions)}
}

// PipelineConfig holds the plugin pipeline composition. Required:
// the runtime YAML must populate both inbound and outbound lists, or
// plugins.Build will produce empty pipelines and the listener will
// have nothing to invoke. There are no implicit defaults.
type PipelineConfig struct {
	Inbound  PipelineStageConfig `yaml:"inbound" json:"inbound"`
	Outbound PipelineStageConfig `yaml:"outbound" json:"outbound"`
}

// PipelineStageConfig lists the plugins for a pipeline stage in execution order.
type PipelineStageConfig struct {
	Plugins []PluginEntry `yaml:"plugins" json:"plugins"`
}

// PluginEntry names a plugin and optionally carries per-instance config.
//
// The YAML accepts both the bare-name form ("jwt-validation") and the
// full form ({name, id, on_error, config}). The short form keeps
// existing pipeline configs parsing unchanged; the long form is what
// plugins that implement pipeline.Configurable actually need. See
// docs/plugin-reference.md for the convention plugins
// follow when decoding Config.
//
// Config is captured as a raw subtree via json.RawMessage so the plugin
// can do its own DisallowUnknownFields decode against a typed struct —
// the framework does not interpret it.
//
// OnError is the framework-owned wrapper policy (see ErrorPolicy).
// Plugin authors do not consume it — it lives outside the plugin's
// own config block so all plugins get the same rollout story without
// each one re-implementing shadow mode.
type PluginEntry struct {
	Name    string               `yaml:"name" json:"name"`
	ID      string               `yaml:"id,omitempty" json:"id,omitempty"`
	OnError pipeline.ErrorPolicy `yaml:"on_error,omitempty" json:"on_error,omitempty"`
	Config  json.RawMessage      `yaml:"-" json:"config,omitempty"`
}

// UnmarshalYAML accepts either a bare string or a map. The string form
// is equivalent to {name: <string>} with no config.
func (p *PluginEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		p.Name = node.Value
		return nil
	case yaml.MappingNode:
		// Walk the mapping's Content pairs directly so we can preserve
		// the config subtree as raw bytes. yaml.v3's struct decode into
		// a *yaml.Node field produces nil in this version; iterating
		// Content is the reliable path.
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode {
				return fmt.Errorf("plugin entry: non-scalar key %q", key.Value)
			}
			switch key.Value {
			case "name":
				if err := val.Decode(&p.Name); err != nil {
					return fmt.Errorf("plugin entry name: %w", err)
				}
			case "id":
				if err := val.Decode(&p.ID); err != nil {
					return fmt.Errorf("plugin entry id: %w", err)
				}
			case "on_error":
				var raw string
				if err := val.Decode(&raw); err != nil {
					return fmt.Errorf("plugin entry on_error: %w", err)
				}
				policy := pipeline.ErrorPolicy(raw)
				if !policy.Valid() {
					return fmt.Errorf("plugin entry on_error: %q is not a valid policy (expected: enforce, observe, off)", raw)
				}
				p.OnError = policy
			case "config":
				// Explicit `config: null` (or `config:` with no value)
				// decodes to a null-tagged scalar node. Normalize to
				// nil here — otherwise yamlNodeToJSON would emit the
				// literal bytes "null" and the Build-time "plugin does
				// not accept configuration" gate would fire
				// spuriously on non-Configurable plugins that a user
				// explicitly declared with a null config block.
				if val.Kind == yaml.ScalarNode && val.Tag == "!!null" {
					p.Config = nil
					continue
				}
				raw, err := yamlNodeToJSON(val)
				if err != nil {
					return fmt.Errorf("plugin %q config: %w", p.Name, err)
				}
				p.Config = raw
			default:
				return fmt.Errorf("plugin entry: unknown field %q", key.Value)
			}
		}
		return nil
	default:
		return fmt.Errorf("plugin entry: expected string or map, got kind %d", node.Kind)
	}
}

// yamlNodeToJSON converts a YAML node to JSON bytes by round-tripping
// through a generic Go value. Sufficient for config sub-trees, which
// only contain scalars, sequences, and maps.
func yamlNodeToJSON(n *yaml.Node) ([]byte, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(normalizeYAMLMaps(v))
}

// normalizeYAMLMaps converts map[any]any (which yaml.v3 can produce when
// decoding into an untyped `any`) into map[string]any so json.Marshal
// accepts it. YAML allows non-string keys but config files never use them.
func normalizeYAMLMaps(v any) any {
	switch x := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			ks, ok := k.(string)
			if !ok {
				ks = fmt.Sprintf("%v", k)
			}
			out[ks] = normalizeYAMLMaps(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeYAMLMaps(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizeYAMLMaps(val)
		}
		return out
	default:
		return v
	}
}

// ListenerConfig holds per-mode listener addresses.
type ListenerConfig struct {
	ExtProcAddr         string `yaml:"ext_proc_addr" json:"ext_proc_addr"`
	ForwardProxyAddr    string `yaml:"forward_proxy_addr" json:"forward_proxy_addr"`
	ReverseProxyAddr    string `yaml:"reverse_proxy_addr" json:"reverse_proxy_addr"`
	ReverseProxyBackend string `yaml:"reverse_proxy_backend" json:"reverse_proxy_backend"`

	// Roles selects which proxies run in proxy-sidecar mode. Empty (the
	// default) runs BOTH the reverse proxy (inbound) and the forward proxy
	// (outbound) — the full pod deployment, so existing configs are unchanged.
	// Set a subset to run a single shape:
	//   roles: [forward]   # egress-only (e.g. a laptop TLS-bridge demo)
	//   roles: [reverse]   # inbound-only JWT validation
	// Valid values: "reverse", "forward". The preset fills an addr default
	// only for an active role, and reverse_proxy_backend is required only when
	// the reverse role is active. Ignored outside proxy-sidecar mode.
	Roles []string `yaml:"roles" json:"roles"`

	// InboundInterception selects HOW inbound traffic reaches the reverse
	// proxy's pipeline, in proxy-sidecar mode with the reverse role active:
	//
	//   reverse-proxy (default) — the operator binds AuthBridge on the agent's
	//                 original port and relocates the agent to a free one
	//                 ("port stealing"), so the Service needs no patching.
	//                 Forwards to the single reverse_proxy_backend URL.
	//   transparent — proxy-init PREROUTING-REDIRECTs inbound TCP to
	//                 transparent_inbound_addr; the listener recovers the port
	//                 the client actually addressed via SO_ORIGINAL_DST and
	//                 forwards there over loopback. The agent keeps its own
	//                 port, so no PORT env var is imposed on it and every port
	//                 it listens on is covered — not just the first declared
	//                 one. Requires proxy-init (NET_ADMIN) and is Linux-only.
	//
	// Defaults to reverse-proxy: transparent adds a privileged init container,
	// so it is opt-in, and leaving it unset keeps existing deployments
	// byte-identical.
	InboundInterception string `yaml:"inbound_interception" json:"inbound_interception"`

	// TransparentInboundAddr is the bind address for the inbound transparent
	// listener (inbound_interception: transparent). The proxy-sidecar preset
	// defaults it to ":8083" when that mode is selected — 8080 (reverse), 8081
	// (forward) and 8082 (transparent egress) are already taken. It MUST match
	// proxy-init's INBOUND_TRANSPARENT_PORT, or PREROUTING will REDIRECT to a
	// dead port and inbound traffic will break outright.
	TransparentInboundAddr string `yaml:"transparent_inbound_addr" json:"transparent_inbound_addr"`

	// TransparentProxyAddr is the bind address for the outbound transparent
	// listener used by proxy-sidecar enforce-redirect mode: iptables REDIRECTs
	// the agent's bypass egress here, and the listener recovers the original
	// destination via SO_ORIGINAL_DST and tunnels it through the same outbound
	// pipeline as the forward proxy. The proxy-sidecar / lite presets default it
	// to ":8082", so for those shapes the listener is effectively always on —
	// binding is harmless when nothing is redirected to it (cooperative
	// HTTP_PROXY deployments simply never receive connections on it). An empty
	// value only disables the listener for modes that have no preset default for
	// this field (envoy-sidecar); under proxy-sidecar / lite the
	// preset refills it, matching the always-on enforce-redirect design.
	TransparentProxyAddr string `yaml:"transparent_proxy_addr" json:"transparent_proxy_addr"`

	// SessionAPIAddr is the bind address for the session events HTTP server
	// (JSON snapshots + SSE stream consumed by abctl or curl). Default per
	// mode preset is ":9094". Set to empty string to disable the endpoint.
	SessionAPIAddr string `yaml:"session_api_addr" json:"session_api_addr"`

	// HealthAddr is the bind address for the liveness/readiness server
	// (/healthz, /readyz). Every mode preset defaults it to ":9091", which is
	// what Kubernetes probes expect. It is configurable because the literal was
	// previously hardcoded, and two proxies on one host could therefore never
	// coexist: the second died on a bind conflict. Local setups can pin it to
	// loopback on another port; leaving it empty keeps the preset default.
	HealthAddr string `yaml:"health_addr" json:"health_addr"`

	// BindLoopbackOnly rewrites EVERY listener address in this config (and the
	// stats address) to 127.0.0.1, keeping each port. Off by default, which
	// preserves Kubernetes behaviour exactly.
	//
	// Why a flag and not the default: in a pod, a wildcard bind is correct and
	// necessary — the network namespace is the boundary, and kubelet probes health
	// from outside the container's loopback, so forcing 127.0.0.1 there would fail
	// every liveness probe. On a laptop there is no namespace, and the same default
	// publishes the listener on Wi-Fi.
	//
	// Why a flag and not a per-listener pin: pinning each address protects only the
	// listeners someone remembered to name. A config written before a pin existed
	// never receives it (writeBuiltinConfig never overwrites, so hand edits
	// survive), and a listener added later starts wide with nothing to catch it.
	// This was not hypothetical: a laptop config predating the health and
	// transparent pins was serving *:9091 and *:8082 on every interface. One rule
	// covers listeners that do not exist yet.
	BindLoopbackOnly bool `yaml:"bind_loopback_only" json:"bind_loopback_only"`

	// SkipHosts lists outbound destination host patterns whose traffic
	// bypasses the plugin pipeline AND session recording entirely. The
	// listener forwards matched requests as a transparent proxy without
	// running plugins or appending events to any session bucket.
	//
	// Intended for high-volume infrastructure traffic that competes
	// with agent-meaningful events for session-buffer slots. The
	// canonical example: an OpenTelemetry collector sidecar that emits
	// dozens of exports per agent turn — without this gate, those
	// exports occupy the session buffer's FIFO eviction window and
	// silently push out the inbound A2A user intent that IBAC needs
	// to align tool calls against, causing IBAC to fall through to
	// the no_intent skip path on every call after the first.
	//
	// Patterns use `.`-delimited glob semantics (same library as
	// `authproxy-routes`): "otel-collector*" matches the short
	// service name, "otel-collector.rossoctl-system.svc.cluster.local"
	// matches the FQDN, "*-collector" matches any single-label name
	// ending in -collector. Port is stripped before matching, so
	// patterns must NOT include `:port`.
	//
	// Empty list (default) preserves current behavior: every outbound
	// host runs the pipeline and is eligible for session recording.
	//
	// Trust model — the value matched against SkipHosts is the
	// destination Host as observed at the listener boundary, which is
	// agent-influenceable in two of the three deployment shapes:
	//
	//   - ext_proc / envoy-sidecar: matches Envoy's `:authority`
	//     (fallback `host` header). The agent sets these; Envoy may
	//     rewrite them per its config, but ultimately the value is
	//     "what the agent told Envoy it wanted to talk to."
	//   - HTTP forward-proxy / proxy-sidecar: matches `r.Host` from
	//     the HTTP request. The request is then dialed against
	//     `r.URL`, so a forged Host that diverges from the real URL
	//     host would skip-match yet send to the actual upstream.
	//   - CONNECT-tunnel / proxy-sidecar: safer-by-construction —
	//     `r.Host` IS the dial target. A forged Host cannot
	//     skip-match while dialing elsewhere.
	//
	// Implication: do NOT list a destination here that you'd want
	// IBAC / token-exchange to deny on. Skip means "the operator
	// trusts every flow to this host enough to bypass the entire
	// outbound enforcement pipeline." Limit entries to infrastructure
	// destinations the agent should not be making policy decisions
	// against in the first place (collector sidecars, log shippers).
	//
	// Each skip is logged at INFO with the matched host and pattern
	// so an operator reviewing logs can see when a pattern fired and
	// catch unexpected matches early. The `transparentproxy` listener
	// (proxy-sidecar enforce-redirect mode) intentionally does NOT
	// consult SkipHosts — that is the hard egress guard and must not
	// be self-exemptable via the agent's outbound destination.
	//
	// Match-all patterns ("*", "**", whitespace-only) and patterns
	// containing ":port" are rejected at startup so a single
	// misconfigured entry can't silently disable all outbound
	// enforcement. Mirrors the bypass-pattern guard added to ibac
	// in #496.
	SkipHosts []string `yaml:"skip_hosts" json:"skip_hosts"`
}

// Proxy roles selectable via ListenerConfig.Roles in proxy-sidecar mode.
const (
	RoleReverse = "reverse" // inbound reverse proxy
	RoleForward = "forward" // outbound forward proxy
)

// Valid ListenerConfig.InboundInterception values. Interception is two
// independent axes (inbound, outbound), so the inbound mechanism is a field on
// the reverse role rather than a role of its own — matching the outbound
// transparent listener, which likewise rides inside the forward role instead of
// being separately selectable.
const (
	InboundInterceptionReverseProxy = "reverse-proxy" // fixed backend (default)
	InboundInterceptionTransparent  = "transparent"   // SO_ORIGINAL_DST per connection
)

// InboundTransparent reports whether the inbound transparent listener should
// run instead of the fixed-backend reverse proxy. Empty means the default
// (reverse-proxy), so callers need no separate zero-value check.
func (l ListenerConfig) InboundTransparent() bool {
	return l.InboundInterception == InboundInterceptionTransparent
}

// ActiveRoles returns the set of proxy roles to run in proxy-sidecar mode. An
// empty Roles list defaults to BOTH roles (the full pod deployment), so
// existing configs and the operator path are unchanged; a non-empty list runs
// exactly the roles named. Unknown values are surfaced by Validate, not here.
func (l ListenerConfig) ActiveRoles() map[string]bool {
	if len(l.Roles) == 0 {
		return map[string]bool{RoleReverse: true, RoleForward: true}
	}
	set := make(map[string]bool, len(l.Roles))
	for _, r := range l.Roles {
		set[r] = true
	}
	return set
}

// StatsConfig represents the configuration for reporting config and statistics
type StatsConfig struct {
	StatsAddress string `yaml:"address" json:"address"` // for example, ":9093"
}

// Valid mode strings.
const (
	ModeEnvoySidecar = "envoy-sidecar"
	ModeProxySidecar = "proxy-sidecar"
)

// Load reads and parses a YAML config file with environment variable expansion.
// Defined env vars are expanded; undefined references like ${UNDEFINED} are left as-is
// to avoid silent empty-string substitution.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	expanded := os.Expand(string(data), func(key string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return "${" + key + "}" // preserve undefined references
	})
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Default stats server address
	if cfg.Stats.StatsAddress == "" {
		// Note that we default to an open port, not localhost 127.0.0.1:9093,
		// because the Rossoctl UI needs to see this.  (If there are concerns
		// about the data exposed, use TLS or redact fields.)
		cfg.Stats.StatsAddress = ":9093"
	}

	// mTLS validation. SVID material now comes from the SPIFFE Provider
	// (see SPIFFEConfig) — there are no per-mtls path fields to default.
	if err := cfg.MTLS.Validate(); err != nil {
		return nil, err
	}

	// SPIFFE defaults match the helper.conf-driven setup: SPIRE agent
	// socket path, mirror-files-on, /opt mirror directory. Validation
	// runs after defaults so an unset socket isn't reported as invalid.
	if cfg.SPIFFE != nil {
		if cfg.SPIFFE.Socket == "" {
			cfg.SPIFFE.Socket = "unix:///spiffe-workload-api/spire-agent.sock"
		}
		if cfg.SPIFFE.MirrorFiles == nil {
			t := true
			cfg.SPIFFE.MirrorFiles = &t
		}
		if cfg.SPIFFE.MirrorDir == "" {
			cfg.SPIFFE.MirrorDir = "/opt"
		}
	}
	if err := cfg.SPIFFE.Validate(); err != nil {
		return nil, err
	}

	if cfg.CostLedger != nil {
		if err := cfg.CostLedger.Validate(); err != nil {
			return nil, err
		}
	}

	if cfg.TLSBridge != nil {
		if err := cfg.TLSBridge.Validate(); err != nil {
			return nil, err
		}
		// With the bridge on, the session API may carry decrypted request/response
		// bodies; restrict its bind to loopback so other pods can't scrape it.
		// kubectl port-forward (abctl) still works — it targets the pod's loopback.
		if cfg.TLSBridge.Mode == "enabled" {
			cfg.Listener.SessionAPIAddr = forceLocalhost(cfg.Listener.SessionAPIAddr)
		}
	}

	return &cfg, nil
}

// forceLocalhost rewrites a bind address to 127.0.0.1, preserving the port:
// ":9094" / "0.0.0.0:9094" / "[::]:9094" -> "127.0.0.1:9094". Empty stays empty;
// a malformed address is left as-is so the bind itself surfaces the error.
func forceLocalhost(addr string) string {
	if addr == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return net.JoinHostPort("127.0.0.1", port)
}
