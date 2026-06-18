package tlsbridge

import (
	"testing"
	"time"
)

func TestDecision_Classify(t *testing.T) {
	d := NewDecision(DecisionOpts{
		Ports:     map[int]bool{443: true, 8443: true},
		SkipHosts: []string{"pinned.example.com"},
	})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05} // handshake, TLS1.0 record, len
	cases := []struct {
		name   string
		host   string
		port   int
		first  []byte
		expect Verdict
		reason string
	}{
		{"happy https", "api.example.com", 443, tlsHello, Terminate, ""},
		{"happy 8443", "api.example.com", 8443, tlsHello, Terminate, ""},
		{"non-tls first byte", "api.example.com", 443, []byte("GET / "), Passthrough, "non-tls"},
		{"unlisted port", "api.example.com", 9999, tlsHello, Passthrough, "port"},
		{"skip-listed host", "pinned.example.com", 443, tlsHello, Passthrough, "skip"},
		{"short record (<5 bytes)", "api.example.com", 443, []byte{0x16, 0x03}, Passthrough, "non-tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reason := d.Classify(tc.host, tc.port, tc.first)
			if v != tc.expect || reason != tc.reason {
				t.Errorf("got (%v,%q), want (%v,%q)", v, reason, tc.expect, tc.reason)
			}
		})
	}
}

func TestDecision_DefaultPortsWhenNil(t *testing.T) {
	d := NewDecision(DecisionOpts{}) // nil Ports -> {443,8443}
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	if v, reason := d.Classify("api.example.com", 443, tlsHello); v != Terminate || reason != "" {
		t.Errorf("port 443: got (%v,%q), want (%v,%q)", v, reason, Terminate, "")
	}
	if v, reason := d.Classify("api.example.com", 80, tlsHello); v != Passthrough || reason != "port" {
		t.Errorf("port 80: got (%v,%q), want (%v,%q)", v, reason, Passthrough, "port")
	}
}

func TestSkipSet_AutoSkip(t *testing.T) {
	s := NewSkipSet()
	if s.Contains("h") {
		t.Fatal("empty set should not contain h")
	}
	s.Add("h")
	if !s.Contains("h") {
		t.Error("Add then Contains failed")
	}
}

func TestSkipSet_TTLExpires(t *testing.T) {
	s := NewSkipSet()
	s.ttl = 20 * time.Millisecond // same-package test can tighten the TTL
	s.Add("h")
	if !s.Contains("h") {
		t.Fatal("should contain immediately after Add")
	}
	time.Sleep(40 * time.Millisecond)
	if s.Contains("h") {
		t.Error("entry should have expired (self-healing re-attempt)")
	}
}

func TestSkipSet_Bounded(t *testing.T) {
	s := NewSkipSet()
	s.max = 2 // cap small; a flood of distinct SNIs must not grow it unbounded
	s.Add("a")
	s.Add("b")
	s.Add("c")
	if len(s.m) > 2 {
		t.Errorf("SkipSet grew past max: len=%d, want <=2", len(s.m))
	}
}

func TestDecision_HandlesPort(t *testing.T) {
	// Default set when Ports is nil.
	d := NewDecision(DecisionOpts{})
	if !d.HandlesPort(443) || !d.HandlesPort(8443) {
		t.Error("default set must handle 443 and 8443")
	}
	if d.HandlesPort(9443) {
		t.Error("default set must not handle 9443")
	}
	// Custom set replaces the default.
	c := NewDecision(DecisionOpts{Ports: map[int]bool{9443: true}})
	if !c.HandlesPort(9443) {
		t.Error("custom set must handle 9443")
	}
	if c.HandlesPort(443) {
		t.Error("custom set must not handle 443 (it replaces, not augments, the default)")
	}
}
