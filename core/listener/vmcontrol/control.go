// Package vmcontrol carries the per-VM listening socket from MOCA's vmpool to
// the Cortex forward proxy (#1477).
//
// SBX microVMs have no network: each VM's egress lands on its own host Unix
// socket, bound by vmpool (the only process that knows the jail layout) and
// handed to the proxy at run time over a Unix control socket as SCM_RIGHTS.
// The proxy accepts guest connections on that fd without ever touching the
// jail path, and vmpool never sees a credential. The descriptor is the
// identity: which listener accepted a connection says which VM is calling,
// and nothing inside the request can move it.
//
// Wire format: a 4-byte big-endian length followed by a marshalled
// ControlRequest; the server answers each request with exactly one framed
// ControlReply, in order. A Register's fd rides on the sendmsg that carries
// the frame's first byte.
//
// The transport is Unix-only (SCM_RIGHTS, peer credentials); on other
// platforms the package holds only the shared types so that importers still
// compile, and the control server itself does not build.
//
// Ported from rossoctl/moca remote-worker/internal/egress/control @ c13d556
// (v0.9.0), which is internal to that repo and cannot be imported here.
package vmcontrol

import (
	"fmt"
	"net"
)

// maxFrame bounds a frame body. Control messages are tiny; a larger length
// means a confused or hostile peer, and the stream cannot be resynchronised
// after it.
const maxFrame = 64 << 10

// Refusal codes carried in ControlReply.Code.
const (
	CodeBadRequest   = "bad_request"
	CodeDuplicateVM  = "duplicate_vm"
	CodeHandlerError = "handler_error"
)

// Handler is the proxy's side of the channel.
type Handler interface {
	// Register binds vmID's listener. The handler owns ln only when it returns
	// nil; on error the server closes it.
	Register(vmID, sessionID string, grant []byte, ln net.Listener) error
	// Unregister drops vmID's binding. An unknown vmID is not an error.
	Unregister(vmID string) error
}

// RefusalError is a ControlReply{ok:false}. A Handler may return one to choose
// the code; any other handler error is reported as CodeHandlerError.
type RefusalError struct{ Code, Message string }

func (e *RefusalError) Error() string {
	return fmt.Sprintf("vm control refused: %s: %s", e.Code, e.Message)
}
