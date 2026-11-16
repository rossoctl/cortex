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
	Sessions(since time.Time) iter.Seq2[Session, error]
}

// Session is one agent session that a Source read.
type Session struct {
	ID       string               // the Cortex session id
	Updated  time.Time            // At of the latest request event
	SizeHint int64                // bytes of the files read, for the archive size check
	Events   []model.SessionEvent // request/response pairs in order; Seq unset
	Skipped  int                  // lines not recognized
}

// Request is the body of POST /v1/import.
type Request struct {
	Agent  string    `json:"agent,omitempty"` // "" = every registered source
	Since  time.Time `json:"since,omitempty"`
	DryRun bool      `json:"dryRun,omitempty"`
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
	NotImported    int    `json:"notImported,omitempty"`    // left after an archive-full stop
	Stopped        string `json:"stopped,omitempty"`        // "archive full" or ""
	Error          string `json:"error,omitempty"`
}

// Report is the response body of POST /v1/import.
type Report struct {
	DryRun bool          `json:"dryRun,omitempty"`
	Agents []AgentReport `json:"agents"`
}
