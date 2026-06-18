package tlsbridge

import "testing"

func TestDecision_Classify(t *testing.T) {
	d := NewDecision(DecisionOpts{
		Ports:         map[int]bool{443: true, 8443: true},
		Scope:         ScopeExternal,
		InternalCIDRs: []string{"10.0.0.0/8"},
		SkipHosts:     []string{"pinned.example.com"},
	})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05} // handshake, TLS1.0 record, len
	cases := []struct {
		name   string
		host   string
		ip     string
		port   int
		first  []byte
		expect Verdict
		reason string
	}{
		{"happy external https", "api.example.com", "93.184.216.34", 443, tlsHello, Terminate, ""},
		{"non-tls first byte", "api.example.com", "93.184.216.34", 443, []byte("GET / "), Passthrough, "non-tls"},
		{"unlisted port", "api.example.com", "93.184.216.34", 9999, tlsHello, Passthrough, "port"},
		{"internal under external scope", "tool.svc", "10.96.1.2", 443, tlsHello, Passthrough, "in-cluster"},
		{"skip-listed host", "pinned.example.com", "1.2.3.4", 443, tlsHello, Passthrough, "skip"},
		{"short record (<5 bytes)", "api.example.com", "1.2.3.4", 443, []byte{0x16, 0x03}, Passthrough, "non-tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reason := d.Classify(tc.host, tc.ip, tc.port, tc.first)
			if v != tc.expect || reason != tc.reason {
				t.Errorf("got (%v,%q), want (%v,%q)", v, reason, tc.expect, tc.reason)
			}
		})
	}
}

func TestDecision_ScopeAll(t *testing.T) {
	d := NewDecision(DecisionOpts{Scope: ScopeAll, InternalCIDRs: []string{"10.0.0.0/8"}})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	// scope=all does NOT passthrough internal destinations.
	if v, reason := d.Classify("tool.svc", "10.96.1.2", 443, tlsHello); v != Terminate || reason != "" {
		t.Errorf("got (%v,%q), want (%v,%q)", v, reason, Terminate, "")
	}
}

func TestDecision_DefaultPortsWhenNil(t *testing.T) {
	d := NewDecision(DecisionOpts{}) // nil Ports -> {443,8443}
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	if v, reason := d.Classify("api.example.com", "1.2.3.4", 443, tlsHello); v != Terminate || reason != "" {
		t.Errorf("port 443: got (%v,%q), want (%v,%q)", v, reason, Terminate, "")
	}
	if v, reason := d.Classify("api.example.com", "1.2.3.4", 80, tlsHello); v != Passthrough || reason != "port" {
		t.Errorf("port 80: got (%v,%q), want (%v,%q)", v, reason, Passthrough, "port")
	}
}

func TestDecision_MultiCIDR(t *testing.T) {
	d := NewDecision(DecisionOpts{
		Scope:         ScopeExternal,
		InternalCIDRs: []string{"10.0.0.0/8", "192.168.0.0/16"},
	})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	// IP matching the SECOND cidr exercises the loop past the first entry.
	if v, reason := d.Classify("tool.svc", "192.168.1.5", 443, tlsHello); v != Passthrough || reason != "in-cluster" {
		t.Errorf("got (%v,%q), want (%v,%q)", v, reason, Passthrough, "in-cluster")
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
