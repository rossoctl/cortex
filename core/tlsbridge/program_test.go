package tlsbridge

import "testing"

// The agent half is the point of the key: the same interpreter run by an agent
// (which exports the CA variables) and by cron (which does not) can disagree about
// trusting the CA, and must not share one memory.
func TestProgramKey_SeparatesTheSameExecutableUnderDifferentAgents(t *testing.T) {
	underAgent := Program{Exe: "/usr/bin/python3", Agent: "/bin/claude"}.Key()
	alone := Program{Exe: "/usr/bin/python3"}.Key()
	if underAgent == alone {
		t.Fatalf("both keys are %q; python3 under an agent and python3 alone must be remembered apart", alone)
	}
	if (Program{Exe: "/a", Agent: "/b"}).Key() == (Program{Exe: "/a/b"}).Key() {
		t.Fatal("the key is ambiguous: a separator that can appear in a path joins two programs into one")
	}
}

// A process older than the CA is remembered under its own key, so that key must never
// collide with a program's, and Key must stay the program-wide one whatever process runs
// it. The format is pinned because the program-memory report parses it back.
func TestProgramProcessKey_NamesOneProcessAndNeverAProgram(t *testing.T) {
	p := Program{Exe: "/bin/claude", PID: 4242, Start: 1700000000123456789}
	if p.Key() != (Program{Exe: "/bin/claude"}).Key() {
		t.Error("Key depends on the process; it must be the same for every process running the program")
	}
	if got, want := p.ProcessKey(), "/bin/claude\x00\x004242@1700000000123456789"; got != want {
		t.Errorf("ProcessKey = %q, want %q", got, want)
	}
	if p.ProcessKey() == p.Key() {
		t.Fatal("a process key equals its program's key, so one process's refusal would stop the program")
	}
	other := p
	other.Start++
	if other.ProcessKey() == p.ProcessKey() {
		t.Error("two processes under one pid, started at different times, share a process key")
	}
}

// The unread report names each program-memory entry by parsing its key back, so both
// key forms must survive the trip: a program's gives no process, and a process's gives
// the very process. The agent path holds an "@", which the process half also uses.
func TestProgramFromKey_ReadsBothKeyForms(t *testing.T) {
	p := Program{Exe: "/usr/bin/python3", Agent: "/Users/x/.nvm/node@20/bin/node", PID: 4242, Start: 1700000000123456789}
	if got, want := ProgramFromKey(p.Key()), (Program{Exe: p.Exe, Agent: p.Agent}); got != want {
		t.Errorf("ProgramFromKey(Key) = %+v, want %+v", got, want)
	}
	if got := ProgramFromKey(p.ProcessKey()); got != p {
		t.Errorf("ProgramFromKey(ProcessKey) = %+v, want %+v", got, p)
	}
	if got, want := ProgramFromKey(Program{Exe: "/bin/sh"}.Key()), (Program{Exe: "/bin/sh"}); got != want {
		t.Errorf("ProgramFromKey of a program under no agent = %+v, want %+v", got, want)
	}
}
