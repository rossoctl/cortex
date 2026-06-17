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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reason := d.Classify(tc.host, tc.ip, tc.port, tc.first)
			if v != tc.expect || (tc.expect == Passthrough && reason != tc.reason) {
				t.Errorf("got (%v,%q), want (%v,%q)", v, reason, tc.expect, tc.reason)
			}
		})
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
