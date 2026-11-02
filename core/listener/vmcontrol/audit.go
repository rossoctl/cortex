package vmcontrol

import (
	"encoding/json"
	"io"
	"net/url"
	"sync"
	"time"
)

// Decision values recorded in Event.Decision. This slice uses the three
// terminal decisions a VM listener's own wrapper emits; the full moca set
// (destination_denied, conn_cap, credential_*, …) arrives with #1482, which
// also fills the fields left empty here.
const (
	DecisionTunnel  = "tunnel"
	DecisionForward = "forward"
	DecisionAborted = "aborted"
)

// viaName is the fixed value of Event.Via.
const viaName = "moca-egress"

// Event is one audit record, in moca's shape (remote-worker/internal/egress
// audit.go): same fields, same JSON tags, so a reader built for moca-egress
// lines parses ours. Path carries no query string, fragment or userinfo (see
// RedactPath); Credential is a name, never a value.
type Event struct {
	Time       time.Time `json:"time"`
	Via        string    `json:"via"`
	VMID       string    `json:"vm_id"`
	SID        string    `json:"sid"`
	Sub        string    `json:"sub,omitempty"`
	Host       string    `json:"host"`
	DialAddr   string    `json:"dial_addr,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path,omitempty"`
	Status     int       `json:"status"`
	BytesUp    int64     `json:"bytes_up"`
	BytesDown  int64     `json:"bytes_down"`
	Credential string    `json:"credential,omitempty"`
	Decision   string    `json:"decision"`
	Dropped    uint64    `json:"dropped,omitempty"`
	Coalesced  uint64    `json:"coalesced,omitempty"`
}

// AuditSink receives audit events.
type AuditSink interface{ Emit(Event) }

// jsonSink writes one JSON line per event, synchronously. moca wraps this in
// an async sink because its stdout can stall journald; the seam that matters
// here is the Event shape, and the async layer is #1482's to add where the
// sink actually lands.
type jsonSink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSONSink returns a sink writing one JSON line per event to w. Via is
// forced to "moca-egress" and a zero Time is filled with the current UTC time.
// Write errors are ignored: auditing must not stall the proxy.
func NewJSONSink(w io.Writer) AuditSink {
	return &jsonSink{enc: json.NewEncoder(w)}
}

func (s *jsonSink) Emit(e Event) {
	e.Via = viaName
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(e)
}

// RedactPath returns the escaped path of u and nothing else: no query,
// fragment or userinfo. An empty path or nil u yields "/".
func RedactPath(u *url.URL) string {
	if u == nil {
		return "/"
	}
	if p := u.EscapedPath(); p != "" {
		return p
	}
	return "/"
}
