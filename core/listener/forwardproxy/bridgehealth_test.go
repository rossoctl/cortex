package forwardproxy

import (
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/tlsbridge"
)

// TestNoteBridgeAttempt_WarnsOnlyWhenNothingIsEverDecrypted covers the failure
// that looks like a plugin bug: the bridge is on, the client does not trust its
// CA, so every HTTPS request opens an opaque tunnel and every body-reading
// plugin correctly does nothing. Nothing errors — the only symptom is silence.
//
// Driven through noteBridgeAttempt, not noteTunnel: the trigger is a CONNECT the
// bridge actually tried to decrypt. See TestNoteTunnel_PassthroughNeverWarns.
func TestNoteBridgeAttempt_WarnsOnlyWhenNothingIsEverDecrypted(t *testing.T) {
	tests := []struct {
		name      string
		bridge    *tlsbridge.Engine
		tunnels   int
		bridged   uint64
		wantWarns int
	}{
		{
			name:      "bridge disabled: never warn, tunnels are the expected behaviour",
			bridge:    nil,
			tunnels:   50,
			wantWarns: 0,
		},
		{
			name:      "below threshold: a few attempts are normal (startup races)",
			bridge:    &tlsbridge.Engine{},
			tunnels:   tunnelWarnThreshold - 1,
			wantWarns: 0,
		},
		{
			name:      "attempts but something was decrypted: bridge is working",
			bridge:    &tlsbridge.Engine{},
			tunnels:   50,
			bridged:   1,
			wantWarns: 0,
		},
		{
			name:      "many attempts, nothing decrypted: warn",
			bridge:    &tlsbridge.Engine{},
			tunnels:   tunnelWarnThreshold,
			wantWarns: 1,
		},
		{
			name:      "and only once, however much traffic follows",
			bridge:    &tlsbridge.Engine{},
			tunnels:   200,
			wantWarns: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{TLSBridge: tc.bridge}
			s.bridgedRequests.Store(tc.bridged)
			var warns int
			// bridgeWarnOnce is the mechanism under test; count how many times
			// the guarded block would run by observing the sync.Once directly.
			for i := 0; i < tc.tunnels; i++ {
				before := s.warnFired()
				s.noteBridgeAttempt()
				if !before && s.warnFired() {
					warns++
				}
			}
			if warns != tc.wantWarns {
				t.Errorf("warned %d times, want %d", warns, tc.wantWarns)
			}
		})
	}
}

// TestCaFileHint_NamesTheAbsolutePath: a relative path in the fix hint is only
// correct for someone standing in the directory --demo was launched from, which
// is precisely how the trust anchor gets mismatched in the first place.
func TestCaFileHint_NamesTheAbsolutePath(t *testing.T) {
	s := &Server{TLSBridge: &tlsbridge.Engine{CAFile: "/abs/cortex-ca/ca.crt"}}
	if got := s.caFileHint(); got != "/abs/cortex-ca/ca.crt" {
		t.Errorf("caFileHint() = %q", got)
	}
	// Degrade to a placeholder rather than an empty string, so the log line
	// still reads as an instruction.
	bare := &Server{TLSBridge: &tlsbridge.Engine{}}
	if got := bare.caFileHint(); !strings.Contains(got, "ca.crt") {
		t.Errorf("caFileHint() = %q, want something naming ca.crt", got)
	}
}

// TestNoteTunnel_ConcurrentIsRaceFree: tunnels open on many goroutines.
func TestNoteTunnel_ConcurrentIsRaceFree(t *testing.T) {
	s := &Server{TLSBridge: &tlsbridge.Engine{}}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				s.noteTunnel()
			}
		}()
	}
	wg.Wait()
	if got := s.tunnelsOpened.Load(); got != 16*64 {
		t.Errorf("tunnelsOpened = %d, want %d", got, 16*64)
	}
}

// TestNoteTunnel_PassthroughNeverWarns is the regression this split exists for.
// A CONNECT to a host in TLSBridge.Skip, or one classification chose to pass
// through, is intentional — it is not evidence of a broken CA. Counting those
// let a correctly-configured proxy cry wolf, and because the warning is
// once-only, the false positive then MASKED the real failure if it came later.
func TestNoteTunnel_PassthroughNeverWarns(t *testing.T) {
	s := &Server{TLSBridge: &tlsbridge.Engine{}}
	for i := 0; i < tunnelWarnThreshold*20; i++ {
		s.noteTunnel()
	}
	if s.warnFired() {
		t.Error("warned about intentional passthrough tunnels")
	}
	// The warning must still be available afterwards for a genuine failure —
	// i.e. the sync.Once was not burned by the passthrough traffic above.
	for i := 0; i < tunnelWarnThreshold; i++ {
		s.noteBridgeAttempt()
	}
	if !s.warnFired() {
		t.Error("real bridge failure did not warn after passthrough traffic")
	}
}

// TestNoteBridgeHandshakeFailure_WarnsImmediately: a refused forged certificate
// is proof, so it must not wait for a threshold. It especially must not, because
// the refusal adds the host to Skip — later requests never reach
// noteBridgeAttempt, so the threshold alone would never be crossed.
func TestNoteBridgeHandshakeFailure_WarnsImmediately(t *testing.T) {
	s := &Server{TLSBridge: &tlsbridge.Engine{}}
	s.noteBridgeAttempt() // one attempt, well below threshold
	if s.warnFired() {
		t.Fatal("warned on a single attempt")
	}
	s.noteBridgeHandshakeFailure()
	if !s.warnFired() {
		t.Error("a rejected bridge certificate did not warn")
	}
}
