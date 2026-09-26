package pricing

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Handler serves the pricing table for inspection on the diagnostic listener, beside
// /config and /reload/status.
//
//	GET /pricing/table          the raw table: every row and multiplier, unscaled
//	GET /pricing/table?host=H   what H actually charges, multiplier applied
//
// The host form is the one worth having. "Which rows exist" is answerable from the
// source; "what will this gateway charge me" requires host scoping, specificity,
// provenance precedence and the discount to be applied together, which is not
// something a reader can carry out by eye — and is exactly the gap that let a 0.76x
// gateway be reported at vendor list for months.
func (r *Registry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		if host := req.URL.Query().Get("host"); host != "" {
			body = r.EffectiveFor(host)
		} else {
			body = r.Describe()
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(body); err != nil {
			// Matching the reloader's status handler: a broken pipe on a diagnostic
			// endpoint is not worth an ERROR line.
			slog.Debug("pricing: table encode failed", "error", err)
		}
	}
}
