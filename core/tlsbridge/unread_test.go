package tlsbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type reportingTrust struct{ trusted bool }

func (reportingTrust) OSTrustOnly(string) bool { return false }
func (r reportingTrust) OSTrustState() (bool, time.Time, bool) {
	return r.trusted, time.Unix(1700000000, 0), true
}

func TestUnreadReport_ListsProgramsAndHosts(t *testing.T) {
	e := &Engine{Skip: NewSkipSet(), Programs: NewProgramSkipSet(), Unread: NewUnreadLog(), Trust: reportingTrust{}}
	py := Program{Exe: "/usr/bin/python3"}.Key()
	// The program set counts a rejection only once the window before it has ended, so
	// stopping an entry takes rejections spread across windows.
	e.Programs.base, e.Programs.ttl = time.Millisecond, 2*time.Millisecond
	refuseAcrossWindows(e.Programs, py, programStopAfter)
	e.Unread.Note(py, UnreadProgramRefused, "example.org")
	helm := Program{Exe: "/opt/homebrew/bin/helm", Agent: "/bin/claude"}.Key()
	e.Unread.Note(helm, UnreadOSTrustOnly, "llm-d-incubation.github.io")
	e.Unread.Note(helm, UnreadOSTrustOnly, "llm-d-incubation.github.io")
	e.Skip.Fail("api.example.com")

	r := e.UnreadReport()
	if r.OSTrustsCA == nil || *r.OSTrustsCA || r.OSTrustCheckedAt == nil {
		t.Errorf("OS trust = %v at %v, want false with a time", r.OSTrustsCA, r.OSTrustCheckedAt)
	}
	byExe := map[string]UnreadProgram{}
	for _, p := range r.Programs {
		byExe[p.Program] = p
	}
	if p := byExe["/usr/bin/python3"]; p.Reason != UnreadProgramRefused || !p.Stopped || p.Until != nil ||
		p.Failures != programStopAfter || p.Connections != 1 || p.LastHost != "example.org" || p.PID != 0 {
		t.Errorf("python3 = %+v", p)
	}
	if p := byExe["/opt/homebrew/bin/helm"]; p.Reason != UnreadOSTrustOnly || p.Agent != "/bin/claude" ||
		p.Connections != 2 || p.LastSeen == nil {
		t.Errorf("helm = %+v", p)
	}
	if len(r.Hosts) != 1 || r.Hosts[0].Host != "api.example.com" || r.Hosts[0].Until.IsZero() {
		t.Errorf("hosts = %+v, want api.example.com with a window", r.Hosts)
	}
}

// A refusal from a process older than the CA is that process's alone (Program.ProcessKey),
// and the report names the process, so a reader can tell "restart this one" from "this
// program refuses". A program-wide entry names none.
func TestUnreadReport_NamesTheProcessBehindAProcessEntry(t *testing.T) {
	e := &Engine{Skip: NewSkipSet(), Programs: NewProgramSkipSet(), Unread: NewUnreadLog()}
	stale := Program{Exe: "/bin/claude", PID: 4242, Start: 1}
	e.Programs.Fail(stale.ProcessKey())
	e.Unread.Note(stale.ProcessKey(), UnreadProgramRefused, "api.anthropic.com")
	e.Unread.Note(stale.ProcessKey(), UnreadProgramRefused, "api.anthropic.com")
	e.Programs.Fail(Program{Exe: "/bin/claude"}.Key())
	e.Unread.Note(Program{Exe: "/bin/claude"}.Key(), UnreadProgramRefused, "x.example")

	r := e.UnreadReport()
	if len(r.Programs) != 2 {
		t.Fatalf("programs = %+v, want the program's entry and the process's", r.Programs)
	}
	// Sorted program-wide first, so the order is stable.
	prog, proc := r.Programs[0], r.Programs[1]
	if prog.Program != "/bin/claude" || prog.PID != 0 || prog.Connections != 1 || prog.LastHost != "x.example" {
		t.Errorf("program entry = %+v", prog)
	}
	if proc.Program != "/bin/claude" || proc.PID != 4242 || proc.Connections != 2 || proc.LastHost != "api.anthropic.com" {
		t.Errorf("process entry = %+v", proc)
	}
	b, err := json.Marshal(r.Programs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), `"pid":`) != 1 || !strings.Contains(string(b), `"pid":4242`) {
		t.Errorf("programs = %s; only the process entry may carry a pid", b)
	}
}

// A prediction stands only while the OS still refuses the CA: once the CA is trusted, a
// Go program is bridged again, and listing it as unread would be false.
func TestUnreadReport_DropsPredictionsOnceTheOSTrustsTheCA(t *testing.T) {
	e := &Engine{Skip: NewSkipSet(), Programs: NewProgramSkipSet(), Unread: NewUnreadLog(), Trust: reportingTrust{trusted: true}}
	e.Unread.Note(Program{Exe: "/opt/homebrew/bin/helm"}.Key(), UnreadOSTrustOnly, "x.example")
	if r := e.UnreadReport(); len(r.Programs) != 0 {
		t.Errorf("programs = %+v, want none: the OS trusts the CA now", r.Programs)
	}
}

// A refusal whose window has ended is no longer being passed through.
func TestUnreadReport_DropsAnExpiredRefusal(t *testing.T) {
	e := &Engine{Skip: NewSkipSet(), Programs: NewProgramSkipSet(), Unread: NewUnreadLog()}
	e.Programs.base, e.Programs.ttl = time.Millisecond, time.Millisecond
	k := Program{Exe: "/usr/bin/curl"}.Key()
	e.Programs.Fail(k)
	e.Unread.Note(k, UnreadProgramRefused, "x.example")
	time.Sleep(5 * time.Millisecond)
	if r := e.UnreadReport(); len(r.Programs) != 0 {
		t.Errorf("programs = %+v, want none: the window ended", r.Programs)
	}
}

// Off darwin nothing asks the OS, and the report must say "not checked" rather than
// "not trusted".
func TestUnreadReport_OmitsOSTrustWhereNothingChecks(t *testing.T) {
	e := &Engine{Skip: NewSkipSet()}
	b, err := json.Marshal(e.UnreadReport())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"osTrustsCA", "osTrustCheckedAt"} {
		if _, ok := m[k]; ok {
			t.Errorf("report = %s; %s must be absent when nothing checked", b, k)
		}
	}
	if string(m["programs"]) != "[]" || string(m["hosts"]) != "[]" {
		t.Errorf("report = %s; empty lists must encode as [], not null", b)
	}
}

func TestUnreadHandler(t *testing.T) {
	h := (&Engine{Skip: NewSkipSet()}).UnreadHandler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tls-bridge/unread", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("GET = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/tls-bridge/unread", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", w.Code)
	}
}

func TestUnreadLog_IsBounded(t *testing.T) {
	u := NewUnreadLog()
	first := Program{Exe: "/bin/first"}.Key()
	u.Note(first, UnreadOSTrustOnly, "h")
	time.Sleep(2 * time.Millisecond) // so first is strictly the least recently seen
	var last string
	for i := 0; i < unreadMax+10; i++ {
		last = Program{Exe: "/bin/p" + string(rune('a'+i%26)) + time.Duration(i).String()}.Key()
		u.Note(last, UnreadOSTrustOnly, "h")
	}
	if len(u.m) > unreadMax {
		t.Errorf("the log holds %d programs; its bound is %d", len(u.m), unreadMax)
	}
	if _, ok := u.m[first]; ok {
		t.Error("the least recently seen program was kept past the bound")
	}
	if _, ok := u.m[last]; !ok {
		t.Error("the program noted last was forgotten")
	}
}

// A process's own entry is cleared only by that process completing a handshake, which it
// cannot do once it has exited — and restarting a stale agent is exactly what the
// rejection advises. So the report lists the entry only while the process runs, and nil
// ProcessAlive assumes it does. A program-wide entry names no process and is never asked
// about.
func TestUnreadReport_ListsAProcessEntryOnlyWhileTheProcessRuns(t *testing.T) {
	stale := Program{Exe: "/bin/claude", PID: 4242, Start: 1700000000123456789}
	e := &Engine{Skip: NewSkipSet(), Programs: NewProgramSkipSet(), Unread: NewUnreadLog()}
	e.Programs.base, e.Programs.ttl = time.Millisecond, 2*time.Millisecond
	refuseAcrossWindows(e.Programs, stale.ProcessKey(), programStopAfter)
	e.Programs.base, e.Programs.ttl = time.Hour, time.Hour
	e.Programs.Fail(Program{Exe: "/usr/bin/python3"}.Key())

	listed := func() (process, program bool) {
		for _, p := range e.UnreadReport().Programs {
			switch {
			case p.Program == stale.Exe && p.PID == stale.PID && p.Stopped:
				process = true
			case p.Program == "/usr/bin/python3" && p.PID == 0:
				program = true
			}
		}
		return process, program
	}
	if process, program := listed(); !process || !program {
		t.Fatalf("nil ProcessAlive: process listed = %v, program listed = %v; want both", process, program)
	}

	var askedPID int32
	var askedStart int64
	e.ProcessAlive = func(pid int32, start int64) bool { askedPID, askedStart = pid, start; return true }
	if process, _ := listed(); !process {
		t.Error("a stopped process entry was omitted while its process runs")
	}
	if askedPID != stale.PID || askedStart != stale.Start {
		t.Errorf("ProcessAlive asked about %d@%d, want %d@%d", askedPID, askedStart, stale.PID, stale.Start)
	}

	e.ProcessAlive = func(int32, int64) bool { return false }
	if process, program := listed(); process || !program {
		t.Errorf("process exited: process listed = %v, program listed = %v; want only the program", process, program)
	}
	if !e.Programs.Contains(stale.ProcessKey()) {
		t.Error("the report removed the entry from the program memory; it must only read it")
	}
}
