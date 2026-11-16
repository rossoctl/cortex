package sessionapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/session/importer"
)

// importBodyCap is the largest POST /v1/import body.
const importBodyCap = 4 << 10

// Import turns on POST /v1/import. The route exists only when the archive is on, the API is
// loopback-only and Sources is not empty.
type Import struct {
	// Rates prices the imported turns. nil leaves them unpriced.
	Rates pricing.Resolver
	// Sources are the agents that the route can import, one per agent name.
	Sources []importer.Source
}

// WithImport turns on POST /v1/import. See Import.
func WithImport(i Import) Option {
	return func(s *Server) { s.importCfg = i }
}

// handleImport runs the import of the selected sources. The caller gives no path, events or
// files: each source reads only its agent's own store.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importer.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, importBodyCap)).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad JSON body"}`, http.StatusBadRequest)
		return
	}
	srcs := s.importCfg.Sources
	if req.Agent != "" {
		i := slices.IndexFunc(srcs, func(src importer.Source) bool { return src.Agent() == req.Agent })
		if i < 0 {
			names := make([]string, len(srcs))
			for j, src := range srcs {
				names[j] = src.Agent()
			}
			body, _ := json.Marshal(map[string]string{
				"error": "unknown agent " + req.Agent + "; supported: " + strings.Join(names, ", ")})
			http.Error(w, string(body), http.StatusBadRequest)
			return
		}
		srcs = srcs[i : i+1]
	}
	if !s.importMu.TryLock() {
		http.Error(w, `{"error":"an import is already running"}`, http.StatusConflict)
		return
	}
	defer s.importMu.Unlock()

	// A large history can take longer than the client's header timeout, so send the header now.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	rep := importer.Report{DryRun: req.DryRun, Agents: make([]importer.AgentReport, 0, len(srcs))}
	for _, src := range srcs {
		rep.Agents = append(rep.Agents, importer.Run(r.Context(), src, importer.Options{
			Archive: s.archive, Store: s.store, Ledger: s.ledger, Rates: s.importCfg.Rates,
			Since: req.Since, DryRun: req.DryRun,
		}))
	}
	if err := json.NewEncoder(w).Encode(rep); err != nil {
		slog.Debug("session API: import report write failed", "error", err)
	}
}
