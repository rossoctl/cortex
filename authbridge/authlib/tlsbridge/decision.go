package tlsbridge

import (
	"sync"
)

type Verdict int

const (
	Passthrough Verdict = iota
	Terminate
)

type DecisionOpts struct {
	Ports     map[int]bool
	SkipHosts []string
}

type Decision struct {
	ports map[int]bool
	skip  map[string]bool
}

func NewDecision(o DecisionOpts) *Decision {
	d := &Decision{ports: o.Ports, skip: map[string]bool{}}
	if d.ports == nil {
		d.ports = map[int]bool{443: true, 8443: true}
	}
	for _, h := range o.SkipHosts {
		d.skip[h] = true
	}
	return d
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
func (d *Decision) Classify(host string, port int, first []byte) (Verdict, string) {
	if !d.ports[port] {
		return Passthrough, "port"
	}
	if !looksLikeTLSRecord(first) {
		return Passthrough, "non-tls"
	}
	if d.skip[host] {
		return Passthrough, "skip"
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

// SkipSet is the runtime auto-skip set (hosts whose minted leaf the client
// rejected). Concurrent-safe; augments the static skip list.
type SkipSet struct {
	mu sync.RWMutex
	m  map[string]bool
}

func NewSkipSet() *SkipSet { return &SkipSet{m: map[string]bool{}} }
func (s *SkipSet) Add(host string) {
	s.mu.Lock()
	s.m[host] = true
	s.mu.Unlock()
}
func (s *SkipSet) Contains(host string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[host]
}
