package tlsbridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/internal/wirefixture"
)

// agentop decodes testdata/unread.json too (doctor_unread.go), so a renamed key on
// either side fails a test. The engine holds one entry of each kind, so that every
// optional key is set on some program.
func TestUnreadHandler_CoversTheWireFixture(t *testing.T) {
	e := &Engine{Skip: NewSkipSet(), Programs: NewProgramSkipSet(), Unread: NewUnreadLog(), Trust: reportingTrust{}}
	// A stopped program entry. Short windows make the refusals fast.
	base, ttl := e.Programs.base, e.Programs.ttl
	e.Programs.base, e.Programs.ttl = time.Millisecond, 2*time.Millisecond
	py := Program{Exe: "/usr/bin/python3"}.Key()
	refuseAcrossWindows(e.Programs, py, programStopAfter)
	e.Unread.Note(py, UnreadProgramRefused, "example.org")
	// A process entry in its window: pid and until. The usual window, so that it is
	// still open when the report is made.
	e.Programs.base, e.Programs.ttl = base, ttl
	e.Programs.Fail(Program{Exe: "/bin/claude", PID: 4242, Start: 1}.ProcessKey())
	// A prediction from a program an agent started: agent.
	e.Unread.Note(Program{Exe: "/opt/homebrew/bin/helm", Agent: "/bin/claude"}.Key(), UnreadOSTrustOnly, "x.example")
	e.Skip.Fail("api.example.com")

	w := httptest.NewRecorder()
	e.UnreadHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tls-bridge/unread", nil))
	wirefixture.Covers(t, "testdata/unread.json", w.Body.Bytes())
}
