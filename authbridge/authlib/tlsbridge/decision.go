package tlsbridge

import (
	"net"
	"sync"
)

type Verdict int

const (
	Passthrough Verdict = iota
	Terminate
)

type Scope int

const (
	ScopeExternal Scope = iota // default: do not bridge internal/mesh destinations
	ScopeAll                   // bridge everything eligible (no-mesh / standalone)
)

type DecisionOpts struct {
	Ports         map[int]bool
	Scope         Scope
	InternalCIDRs []string
	SkipHosts     []string
}

type Decision struct {
	ports    map[int]bool
	scope    Scope
	internal []*net.IPNet
	skip     map[string]bool
}

func NewDecision(o DecisionOpts) *Decision {
	d := &Decision{ports: o.Ports, scope: o.Scope, skip: map[string]bool{}}
	if d.ports == nil {
		d.ports = map[int]bool{443: true, 8443: true}
	}
	for _, c := range o.InternalCIDRs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			d.internal = append(d.internal, n)
		}
	}
	for _, h := range o.SkipHosts {
		d.skip[h] = true
	}
	return d
}

// Classify decides whether to bridge. first is the peeked client bytes.
func (d *Decision) Classify(host, ip string, port int, first []byte) (Verdict, string) {
	if !d.ports[port] {
		return Passthrough, "port"
	}
	if !looksLikeTLSClientHello(first) {
		return Passthrough, "non-tls"
	}
	if d.skip[host] {
		return Passthrough, "skip"
	}
	if d.scope == ScopeExternal && d.isInternal(ip) {
		return Passthrough, "in-cluster"
	}
	return Terminate, ""
}

func (d *Decision) isInternal(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false // a hostname (not an IP) is never matched as in-cluster — see CONNECT-path note
	}
	for _, n := range d.internal {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// looksLikeTLSClientHello validates the 5-byte TLS record header (not just 0x16):
// content type 22 (handshake), legacy record version 0x03 0x01-0x04.
func looksLikeTLSClientHello(b []byte) bool {
	if len(b) < 5 {
		return false
	}
	return b[0] == 0x16 && b[1] == 0x03 && b[2] <= 0x04
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
