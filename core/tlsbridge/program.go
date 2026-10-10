package tlsbridge

import (
	"strconv"
	"strings"
)

// Program names the client program behind a bridged connection, for the per-program
// skip set (NewProgramSkipSet). Exe is the client process's executable, absolute and
// symlink-resolved. Agent is the executable of the nearest process in its ancestry
// that has named a session, and empty when none has.
//
// The pair is the identity, not Exe alone. Whether a program trusts the bridge CA
// depends on what it was told, and an agent's children inherit the CA variables the
// agent's settings export while the same interpreter run by cron does not — so
// python3 under Claude Code and python3 on its own can disagree, and one memory for
// both would flip between them the way a host-keyed one flips between clients.
type Program struct {
	Exe   string
	Agent string
	// PID and Start name the client process itself, Start in Unix nanoseconds as
	// session.Proc has it, for ProcessKey. Key ignores them. Zero when unknown.
	PID   int32
	Start int64
}

// Key is p as a SkipSet key: the program, whichever process runs it. NUL separates the
// halves because no path contains one, so a Key holds exactly one NUL.
func (p Program) Key() string { return p.Exe + "\x00" + p.Agent }

// ProcessKey is p's process as a SkipSet key: Key, then a second NUL and
// "<pid>@<start>", both decimal. It holds exactly two NULs, so it never equals any Key,
// and splitting a key on NUL tells the two apart: two fields name a program, three a
// process.
//
// It is for a process that started before the bridge CA. CA files are read once at
// startup, so such a process cannot have been told about the CA, and its refusal is
// evidence about that process, not the program. Recorded under Key, a stale Claude
// Code's refusals would stop the key every freshly started Claude Code also has until
// it names a session, and no new instance would be read until Cortex restarted.
func (p Program) ProcessKey() string {
	return p.Key() + "\x00" + strconv.FormatInt(int64(p.PID), 10) + "@" + strconv.FormatInt(p.Start, 10)
}

// ProgramFromKey is the inverse of Key and ProcessKey, for the unread report, which has
// only the program memory's keys to name its entries by. Two fields give a program and
// leave PID and Start zero; three give a process, read back from "<pid>@<start>".
func ProgramFromKey(k string) Program {
	f := strings.SplitN(k, "\x00", 3)
	p := Program{Exe: f[0]}
	if len(f) > 1 {
		p.Agent = f[1]
	}
	if len(f) == 3 {
		pid, start, _ := strings.Cut(f[2], "@")
		if n, err := strconv.ParseInt(pid, 10, 32); err == nil {
			p.PID = int32(n)
		}
		if n, err := strconv.ParseInt(start, 10, 64); err == nil {
			p.Start = n
		}
	}
	return p
}
