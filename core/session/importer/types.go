// Package importer writes sessions from an agent's own transcripts on disk into the
// session archive and the cost ledger, as sessions with the imported mark.
package importer

import (
	"iter"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// Source reads the sessions of one agent from its store on disk.
type Source interface {
	// Agent is the agent name, for example "claude-code".
	Agent() string
	// PricingHost is the host for settle.Input.Host, for example "api.anthropic.com".
	PricingHost() string
	// Sessions yields the sessions newest first (by file mtime). A zero since gives all
	// sessions. A non-nil error stops the import of this agent only, and Run reports it.
	// Before it parses a session, it calls skip with the session id and a time that is not
	// before the parsed Updated. When skip gives true, it does not parse or yield the session.
	// It calls progress (never nil) with the sessions done and the sessions found, before each
	// session and once after the last.
	Sessions(since time.Time, skip func(id string, updated time.Time) bool, progress func(done, total int)) iter.Seq2[Session, error]
}

// Session is one agent session that a Source read.
type Session struct {
	ID       string               // the Cortex session id
	Updated  time.Time            // At of the newest event
	SizeHint int64                // bytes of the files read, for the archive size check
	Events   []model.SessionEvent // request/response pairs in order; Seq unset
	Skipped  int                  // lines not recognized
	// Unreadable is true when the session's main transcript could not be read. Run counts the
	// session in AgentReport.Unreadable, and does not write it.
	Unreadable bool
}

// Request is the body of POST /v1/import.
type Request struct {
	Agent  string    `json:"agent,omitempty"` // "" = every registered source
	Since  time.Time `json:"since,omitempty"`
	DryRun bool      `json:"dryRun,omitempty"`
	// Progress asks for Progress lines before the Report. See POST /v1/import.
	Progress bool `json:"progress,omitempty"`
}

// Progress is one line of POST /v1/import with Request.Progress: Done of the Total sessions
// that the source of Agent found.
type Progress struct {
	Agent string `json:"agent"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// AgentReport is the result of the import for one agent.
type AgentReport struct {
	Agent          string `json:"agent"`
	Imported       int    `json:"imported"`
	Replaced       int    `json:"replaced"`
	SkippedLive    int    `json:"skippedLive"`
	SkippedCurrent int    `json:"skippedCurrent"`
	SkippedLines   int    `json:"skippedLines"`
	SkippedExpired int    `json:"skippedExpired,omitempty"` // older than the archive's retention
	Unreadable     int    `json:"unreadable,omitempty"`     // sessions whose transcript could not be read
	NotImported    int    `json:"notImported,omitempty"`    // left after an archive-full stop
	Stopped        string `json:"stopped,omitempty"`        // "archive full" or ""
	Error          string `json:"error,omitempty"`
}

// Report is the response body of POST /v1/import.
type Report struct {
	DryRun bool          `json:"dryRun,omitempty"`
	Agents []AgentReport `json:"agents"`
}
