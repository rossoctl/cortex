// Package clientstate owns the on-disk record abctl writes when it points an agent
// at Cortex, and which authbridge-proxy reads back to check the agent is still
// pointed at the CA in force.
//
// It exists because that record is a contract between two separate main packages in
// two separate modules (cmd/abctl writes it, cmd/authbridge-proxy reads it) that
// cannot import each other. Before this, each carried its own copy of the relative
// path and its own inline struct, with nothing tying them together: renaming a JSON
// field or moving the file would leave the reader silently returning nothing, the
// warning quietly dead, and every test still green — because the tests held copies
// of the shape too.
//
// Anything that changes the file's name or shape belongs here, so that a change
// breaks compilation in both binaries rather than only the behaviour in one.
package clientstate

import (
	"encoding/json"
	"os"
)

// RelPath is where the record lives, relative to the cortex dir (~/.cortex).
const RelPath = "claude-code-state.json"

// CAEnvVar is the client environment variable naming the CA file an agent loads
// into its trust store. Node reads it once at process start, which is why a client
// pointed at a stale file cannot recover until it restarts.
const CAEnvVar = "NODE_EXTRA_CA_CERTS"

// State is the record abctl writes.
//
// Prior holds what abctl DISPLACED, captured before it overwrote the settings, and
// exists so `disable` can restore rather than delete. It is emphatically not what
// the client currently uses — every enable makes the two differ — so a reader asking
// "what CA does this client load?" must go through Settings. See CurrentCA.
type State struct {
	Settings string             `json:"settings"`
	Prior    map[string]*string `json:"prior"`
}

// Load reads the record at path. A missing file is (nil, nil): absent is a normal
// state, not an error, and callers should treat it as "nothing recorded".
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path) //nolint:gosec // caller-resolved path under the cortex dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// CurrentCA returns the CA file the recorded client is configured with RIGHT NOW, by
// reading the live value out of the settings file the record names. Returns "" when
// that cannot be established for any reason.
//
// Deliberately never consults State.Prior. Doing so answers a different question —
// what abctl replaced — and gets the important cases backwards: for Prior to hold
// another environment's CA, the same enable that recorded it must also have pointed
// the client at the current one, so a Prior-based comparison flags clients that are
// already correct. Worse, abctl freezes the record on first write, so such a
// comparison can never go quiet afterwards.
//
// Every failure answers "": this feeds diagnostics, and a settings file that is
// absent, truncated or hand-edited is not a reason to fail a proxy boot.
func (s *State) CurrentCA() string {
	if s == nil || s.Settings == "" {
		return ""
	}
	b, err := os.ReadFile(s.Settings) //nolint:gosec // path comes from abctl's own record
	if err != nil {
		return ""
	}
	var doc struct {
		Env map[string]any `json:"env"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	// A non-string value is not a path; mirrors abctl's own envStrings.
	if v, ok := doc.Env[CAEnvVar].(string); ok {
		return v
	}
	return ""
}
