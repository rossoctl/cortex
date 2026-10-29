package attribution

import (
	"net/netip"

	"github.com/rossoctl/cortex/core/peerproc"
)

// MaxChain is how many processes a client's chain holds — the client and its parents —
// enough to reach the agent from a tool its shell ran.
const MaxChain = 16

// maxHints is how many processes known to have named a session a lookup looks at first. On
// Linux each costs a scan of that process's open files.
const maxHints = 8

// Lookup is the process chain holding the client end of a connection — the client first,
// then its parents — or nil when it cannot be named, and then the request is attributed as
// if process attribution were off. It is the one kernel question attribution asks, and it
// takes addresses, not a request, so a relay that sees only the connection can ask it.
//
// Must be asked while the client is connected: a closed connection names no process.
func (a *Attributor) Lookup(r peerproc.Resolver, client, server netip.AddrPort) ([]Proc, error) {
	owner, err := r.ConnOwner(client, server, a.ProcessHints(maxHints)...)
	if err != nil {
		return nil, err
	}
	procs, err := r.Ancestry(owner.PID, MaxChain)
	if err != nil || len(procs) == 0 {
		procs = []peerproc.Proc{owner}
	}
	chain := make([]Proc, len(procs))
	for i, p := range procs {
		chain[i] = Proc{PID: p.PID, Start: p.Start.UnixNano(), Exe: p.Exe}
	}
	return chain, nil
}
