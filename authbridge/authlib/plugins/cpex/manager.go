package cpex

import (
	"context"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// Manager is the surface plugin.go uses to talk to CPEX. It is a
// tag-free interface so unit tests can substitute a fake without
// requiring CGO_ENABLED or the libcpex_ffi.a static library.
//
// The real implementation lives in manager_cpex.go (//go:build cpex)
// and wraps the cpex.PluginManager from the
// github.com/contextforge-org/cpex/go/cpex package. The
// stub in manager_stub.go (//go:build !cpex) makes NewManager error
// out with a clear "build the cpex binary" message if anyone tries to
// configure the plugin in a binary that wasn't built with -tags cpex.
//
// Lifecycle, matching pipeline.Plugin's: LoadConfig (during plugin
// Configure) → Initialize (during plugin Init) → many Invoke calls →
// Shutdown (during plugin Shutdown). HasHook is a cheap "is this hook
// wired up at all?" check used to skip Invoke when no sub-plugin
// would run.
type Manager interface {
	// LoadConfig parses the operator's CPEX YAML and stages sub-plugin
	// factories + routes. Called once. APL DSL factories are enabled
	// before LoadConfig so the YAML may freely reference APL plugins.
	LoadConfig(yaml string) error

	// Initialize starts the CPEX runtime — binds sub-plugin handlers
	// to hooks, spawns background workers (JWKS refreshers, audit
	// sinks). Called once after LoadConfig.
	Initialize(ctx context.Context) error

	// Shutdown drains in-flight work and releases resources. Called
	// once on pipeline stop; honor ctx's deadline.
	Shutdown(ctx context.Context)

	// HasHook reports whether any loaded sub-plugin handles hookName.
	// Lets OnRequest/OnResponse skip the cgo round-trip when no
	// policy is configured for the current phase.
	HasHook(hookName string) bool

	// Invoke runs hookName through every sub-plugin registered for
	// it. The real implementation builds a CMF Message + Extensions
	// from pctx, calls the cpex FFI, and (if policy modified the
	// message) writes the changes back to pctx via pctx.SetBody /
	// pctx.SetResponseBody / header mutation. The returned Result
	// summarizes what happened for the Invocation record.
	//
	// Returning an error means CPEX itself failed (FFI error, runtime
	// panic). A policy `deny` is NOT an error — it surfaces as
	// Result.Decision == DecisionDeny with a populated Reason.
	Invoke(ctx context.Context, hookName string, pctx *pipeline.Context) (Result, error)
}

// ManagerOptions configures Manager construction. Fields here are
// process-wide knobs that don't belong in the YAML config because
// they affect runtime layout, not policy.
type ManagerOptions struct {
	// WorkerThreads sets CPEX's tokio worker pool size. 0 = auto
	// (CPEX picks based on CPU count).
	WorkerThreads int
}

// NewManager is defined in one of two build-tag-gated files:
//
//   - manager_cpex.go (//go:build cpex)  — returns a manager wrapping
//     cpex.PluginManager; pulls in libcpex_ffi via cgo.
//   - manager_stub.go (//go:build !cpex) — returns an error so a
//     misconfigured binary fails loud at Configure time instead of
//     silently registering a no-op plugin.
//
// The two files have mutually exclusive build tags, so exactly one
// NewManager symbol is defined per build.
//
// NewManager does NOT call LoadConfig or Initialize — those are the
// caller's responsibility, sequenced by the plugin's Configure /
// Init.

// (NewManager is defined in manager_cpex.go / manager_stub.go.)

// Decision is the outcome category of a CPEX Invoke. It maps onto
// the existing AuthBridge 5-value vocabulary (allow / deny / skip /
// modify / observe), plus an Error bucket for CPEX-internal failures
// that the plugin's fail_open knob governs.
type Decision int

const (
	// DecisionUnknown is the zero-value sentinel. A Result whose
	// Decision was never explicitly set fails closed in applyDecision
	// (the default arm), so an uninitialised Result never silently
	// allows traffic.
	DecisionUnknown Decision = iota

	// DecisionAllow: all sub-plugins continued. Request proceeds
	// untouched.
	DecisionAllow

	// DecisionDeny: at least one sub-plugin returned a policy
	// violation. Plugin emits pipeline.Deny.
	DecisionDeny

	// DecisionModify: policy rewrote the message / headers. Plugin
	// applies Modifications and continues.
	DecisionModify

	// DecisionObserve: policy emitted observation-only outcomes
	// (audit logged, no enforcement). Plugin records and continues.
	DecisionObserve

	// DecisionError: CPEX itself errored. The plugin's fail_open
	// config decides whether to convert this to allow or deny.
	DecisionError
)

// String returns the lowercase decision name, used in Invocation
// reasons and log fields.
func (d Decision) String() string {
	switch d {
	case DecisionUnknown:
		return "unknown"
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	case DecisionModify:
		return "modify"
	case DecisionObserve:
		return "observe"
	case DecisionError:
		return "error"
	default:
		return "unknown"
	}
}

// Result is the aggregate outcome of a CPEX Invoke. Populated by the
// Manager from PipelineResult; consumed by plugin.go to drive
// pipeline.Action and Invocation emission.
type Result struct {
	// Decision is the top-line outcome category.
	Decision Decision

	// Reason is a short human-readable summary suitable for the
	// Invocation.Reason field. For Deny, this is the deciding
	// sub-plugin's violation message; for Modify, a one-line summary
	// of what changed; for Error, the error string.
	Reason string

	// PluginsRun lists CPEX sub-plugin names that executed for this
	// hook, in declaration order. Always populated, even on Allow,
	// so audit can show "what ran."
	PluginsRun []string

	// Errors carries per-sub-plugin error records from CPEX's
	// PipelineResult.errors. Populated when at least one sub-plugin
	// errored; otherwise empty.
	Errors []SubPluginError

	// Code is an optional machine-readable identifier for the
	// outcome — e.g. "cedar.denied", "pii.redacted". Surfaces in
	// Invocation.Code when present.
	Code string
}

// SubPluginError is one CPEX sub-plugin's error report, lifted from
// PipelineResult.errors. The Plugin field is the CPEX sub-plugin
// name (not the AuthBridge plugin name — that's always "cpex").
type SubPluginError struct {
	Plugin  string
	Code    string
	Message string
}
