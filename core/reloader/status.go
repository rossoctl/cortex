package reloader

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Status is the JSON body of /reload/status. Snapshotted inside an
// atomic.Pointer — readers get a consistent view even if an in-flight
// reload mutates the fields a moment later.
type Status struct {
	// LastAttempt is the timestamp of the most recent reload attempt,
	// regardless of success. Zero until the first event.
	LastAttempt time.Time `json:"last_attempt"`

	// LastSuccess is the timestamp of the most recent successful swap.
	// Zero until the first success. Initialized to process-start time
	// by New so operators see a non-zero value before the first reload.
	LastSuccess time.Time `json:"last_success"`

	// LastError is the error string from the most recent failed reload,
	// or empty after a success. Phrased for operators reading a JSON
	// response, not a developer stack trace.
	LastError string `json:"last_error"`

	// ReloadsOK counts successful swaps since process start.
	ReloadsOK int64 `json:"reloads_ok"`

	// ReloadsFailed counts failed reload attempts since process start
	// (validation, build, Start, or unreloadable-field rejection).
	ReloadsFailed int64 `json:"reloads_failed"`

	// ActiveConfigSHA256 is the sha256 hex digest of the currently-
	// active config file bytes. Lets operators cross-check which YAML
	// revision is running.
	ActiveConfigSHA256 string `json:"active_config_sha256"`
}

// Handler returns an http.HandlerFunc that renders Status as JSON on
// GET /reload/status. Reads are lock-free via atomic.Pointer so the
// handler adds no contention with the reload path.
func (r *Reloader) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(r.Status()); err != nil {
			slog.Debug("reloader: status encode failed", "error", err)
		}
	}
}
