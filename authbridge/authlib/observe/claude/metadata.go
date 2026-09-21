// Package claude owns the Claude Code session-metadata contract: the shape abctl's
// harvester writes to ~/.cortex/session-metadata.json, the path it lives at, and the
// harvest that produces it.
//
// It exists because that file is a contract between two things that cannot import each
// other: the harvester, reached from abctl's package main, and the viewer in
// cmd/abctl/tui. Before this, both the shape and the harvest lived in package main, so
// nothing else could reach them and `abctl observe` could not name a session without
// shelling out to another subcommand.
//
// Anything that changes the file's name or shape belongs here, so that a change breaks
// compilation in every reader rather than only the behaviour in one. Same reasoning as
// authlib/clientstate, which owns ~/.cortex/claude-code-state.json.
//
// Under authlib/observe because it serves OBSERVATION rather than authorization: nothing
// here makes an access decision, and no part of the auth path reads it. A session title
// only ever answers "which session is this?" for someone reading a pane. That is the
// dividing line for this subtree — authlib's other packages exist to decide whether a
// request proceeds; these exist to describe traffic that already happened.
//
// One package per agent is the intended shape. SessionMetadata carries AgentType and
// AgentConfigDir precisely so a second harvester can merge into the same file, and that
// one would sit beside this as authlib/observe/<agent>. When it lands, the shape and the
// path are the parts worth hoisting; the transcript layout is not.
package claude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionMetadata is what a coding agent knows about one of its own sessions that
// Cortex does not.
//
// Cortex buckets traffic by session id and nothing more: `/v1/sessions` returns a
// UUID, and a UUID does not tell an operator which session it is. The agent that
// produced the traffic already knows — Claude Code writes a per-session transcript
// carrying an AI-generated title and the directory it ran in — so this is the shape
// that carries those facts back, keyed by the same id Cortex uses.
//
// Every field is optional. A harvester that cannot determine one must leave it empty
// rather than invent a value, and a consumer must render an entry that has only some
// of them: the fields come from an agent's own on-disk layout, which is not a
// contract anyone here controls.
//
// This is written as JSON to ~/.cortex/session-metadata.json. Key names stay camelCase
// to match the sortColumn / sortDesc keys in abctl's own settings file rather than
// introducing a second convention.
type SessionMetadata struct {
	// Title is a human-readable name for the session. From Claude Code this is the
	// model-generated title when the transcript carries one, and otherwise the
	// working directory it ran in — so it may well be a path rather than a phrase,
	// and a consumer that assumes prose will be wrong most of the time (measured: 2
	// of 109 local sessions had a real title).
	Title string `json:"title,omitempty"`
	// AgentType names the agent that produced the session, e.g. "Claude Code". A
	// plain display string, not an enum: the set of agents Cortex fronts is open,
	// and a consumer showing this has no decision to make on it.
	AgentType string `json:"agentType,omitempty"`
	// AgentConfigDir is the directory the metadata was harvested from — the parent
	// of the agent's own per-session layout. Recorded per entry rather than once per
	// file so a future harvester can merge two agents, or two config dirs of one
	// agent, into the same map without the entries becoming ambiguous.
	AgentConfigDir string `json:"agentConfigDir,omitempty"`
	// LogFile is the transcript this entry was read from, as a path. The one field
	// that lets a reader go and check: a title that looks wrong is answerable by
	// opening the file it came from.
	//
	LogFile string `json:"logFile,omitempty"`
	// LogModTime is the transcript's modification time as of the harvest that wrote
	// this entry, and it is what makes an incremental harvest possible: a transcript
	// whose mtime has not moved past this cannot have grown a new title, so it does
	// not need re-reading. See ReadSessions.
	//
	// Recorded rather than derived. The obvious cheaper design — re-Stat LogFile and
	// compare — cannot work: it compares the transcript's mtime against ITSELF, which
	// is never "after", so every transcript would be skipped forever and a renamed
	// session would keep its old title permanently. A test caught exactly that.
	//
	// Optional, like every other field, and its zero value is the safe answer: an
	// entry written before this field existed has no timestamp, cannot vouch for its
	// transcript, and is therefore re-parsed rather than trusted.
	//
	// NOT omitempty, unlike every field above, because it could not work: encoding/json
	// treats only empty scalars, maps and slices as empty, so a zero time.Time is a struct
	// and gets written out as "0001-01-01T00:00:00Z" regardless. Tagging it omitempty would
	// have read as a promise this file does not keep — every entry carries a timestamp,
	// which is why the rule that matters is stated on the READING side: harvestedAt treats
	// the zero value as "cannot tell" and re-parses.
	LogModTime time.Time `json:"logModTime"`
}

// SessionMetadataRel is the harvested metadata file, relative to the user's home.
//
// One constant so the harvester that writes the file and the viewer that reads it name
// the same path. Two constants for one path is the kind of drift that only shows up as
// an empty column nobody can explain.
//
// Follows yankDirRel and cmd_claudecode.go's cortexCfgRel / stateRel, so there is one
// ~/.cortex tree rather than a new dotfile per feature.
const SessionMetadataRel = ".cortex/session-metadata.json"

// SessionMetadataPath returns ~/.cortex/session-metadata.json.
func SessionMetadataPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	if home == "" {
		// A separate branch, not folded into the one above: %w on a nil error renders as
		// "%!w(<nil>)", which would make this path unreadable to whoever hits it. Same
		// shape as yankDir and userConfigPath.
		return "", errors.New("cannot determine your home directory: it is empty")
	}
	return filepath.Join(home, SessionMetadataRel), nil
}
