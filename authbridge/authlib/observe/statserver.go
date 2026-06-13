package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/redact"
)

type StatServer struct {
	server *http.Server
}

// StatsProvider returns a fresh *auth.Stats per /stats request. The
// host typically implements this by calling auth.MergeStats over the
// per-plugin stats collected from each pipeline — see
// plugins.CollectStats. Called per HTTP request, so implementations
// should be cheap (a few map copies).
type StatsProvider func() *auth.Stats

// ConfigProvider returns the currently-active *config.Config per
// /config request. Used so the endpoint reflects a hot-reload swap
// performed by authlib/reloader; for setups without hot-reload the
// host just wraps a captured pointer (`func() *Config { return cfg }`).
type ConfigProvider func() *config.Config

// Option configures a StatServer at construction time.
type Option func(*statServerOpts)

type statServerOpts struct {
	reloadStatus http.Handler
}

// WithReloadStatus registers a /reload/status handler (typically the
// Handler returned by an authlib/reloader.Reloader). Omit when hot-
// reload isn't wired up — the endpoint simply won't exist.
func WithReloadStatus(h http.Handler) Option {
	return func(o *statServerOpts) { o.reloadStatus = h }
}

// NewStatServer builds the stat HTTP server. configProvider is
// invoked per /config request and statsProvider per /stats request,
// so both reflect current state rather than a snapshot captured at
// construction.
func NewStatServer(addr string, configProvider ConfigProvider, statsProvider StatsProvider, opts ...Option) *StatServer {
	var o statServerOpts
	for _, opt := range opts {
		opt(&o)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/config", handleConfigFactory(configProvider))
	mux.HandleFunc("/stats", handleStatsFactory(statsProvider))
	if o.reloadStatus != nil {
		mux.Handle("/reload/status", o.reloadStatus)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `
<!DOCTYPE html>
<html>
  <body>
    <ul>
    <li><a href="/config">Kagenti AuthBridge configuration</a></li>
    <li><a href="/stats">Kagenti AuthBridge statistics</a></li>
    <li><a href="/reload/status">Config reload status</a></li>
    </ul>
  </body>
</html>`)
	})

	return &StatServer{
		server: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

func handleConfigFactory(provider ConfigProvider) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw, err := json.Marshal(provider())
		if err != nil {
			slog.Default().Info("Failed to marshal configuration", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"marshal failed"}` + "\n"))
			return
		}
		redacted := redact.JSON(raw)
		redacted = append(redacted, '\n')
		if _, err := w.Write(redacted); err != nil {
			slog.Default().Info("Failed to send configuration", "err", err)
		}
	}
}

func handleStatsFactory(provider StatsProvider) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Provider returns a freshly-merged *auth.Stats. Nil means
		// "no source plugins" — render an empty object rather than
		// failing, so the endpoint shape is stable even on pipelines
		// that register no stats sources.
		stats := provider()
		if stats == nil {
			stats = auth.NewStats()
		}
		err := json.NewEncoder(w).Encode(stats)
		if err != nil {
			slog.Default().Info("Failed to send stats", "err", err)
		}
	}
}

func (s *StatServer) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *StatServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
