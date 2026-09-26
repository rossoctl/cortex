package tls

import (
	"sync/atomic"
)

// Metrics holds the counters surfaced through the existing /stats
// endpoint on :9093. Atomic counters because the listener and the
// dialer call into them concurrently.
//
// Outbound: only present when the workload is in strict mode (the
// only mode the forward proxy attempts TLS-or-fail outbound).
// Permissive mode dials plaintext directly — no TLS attempt, no
// fallback, no counter — matching envoy-sidecar's permissive
// semantics.
type Metrics struct {
	// Inbound: which path did each accepted connection take?
	InboundTLSAccepted   atomic.Uint64 // TLS handshake succeeded + verified
	InboundPlainAccepted atomic.Uint64 // Plain HTTP served (permissive only)
	InboundPlainRejected atomic.Uint64 // Plain HTTP rejected (strict only)
	InboundTLSFailed     atomic.Uint64 // TLS handshake / verification failed

	// Outbound: only counted in strict mode (TLS-or-fail). Permissive
	// is plaintext outbound and bypasses these.
	OutboundTLSSucceeded atomic.Uint64 // TLS handshake succeeded + verified
	OutboundFailed       atomic.Uint64 // TLS handshake / verification failed
}

// Snapshot is a point-in-time copy of the metrics for serialization
// to the /stats endpoint. Uses non-atomic uint64s so the JSON payload
// renders cleanly without sync/atomic exposure.
type Snapshot struct {
	InboundTLSAccepted   uint64 `json:"inbound_tls_accepted"`
	InboundPlainAccepted uint64 `json:"inbound_plain_accepted"`
	InboundPlainRejected uint64 `json:"inbound_plain_rejected"`
	InboundTLSFailed     uint64 `json:"inbound_tls_failed"`

	OutboundTLSSucceeded uint64 `json:"outbound_tls_succeeded"`
	OutboundFailed       uint64 `json:"outbound_failed"`
}

// Snapshot returns the current counter values. Each Load is atomic
// independently; the snapshot may straddle a counter increment, which
// is fine for an observability surface.
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		InboundTLSAccepted:   m.InboundTLSAccepted.Load(),
		InboundPlainAccepted: m.InboundPlainAccepted.Load(),
		InboundPlainRejected: m.InboundPlainRejected.Load(),
		InboundTLSFailed:     m.InboundTLSFailed.Load(),
		OutboundTLSSucceeded: m.OutboundTLSSucceeded.Load(),
		OutboundFailed:       m.OutboundFailed.Load(),
	}
}

// NewMetrics constructs a Metrics with all counters at zero.
func NewMetrics() *Metrics {
	return &Metrics{}
}
