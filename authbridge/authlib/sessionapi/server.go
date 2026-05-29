// Package sessionapi exposes AuthBridge's in-memory session store over HTTP:
// JSON snapshots plus an SSE stream of live events. Intended for local
// operators debugging the plugin pipeline via kubectl port-forward and for
// the abctl TUI.
//
// Trust model: no authentication. Bind only on in-cluster addresses, never
// behind an ingress. The payload may contain user messages, LLM completions,
// and tool results verbatim.
package sessionapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

// defaultHeartbeatInterval is how often the SSE stream sends a keep-alive
// comment so clients can detect a dead connection. Tuneable for tests via
// WithHeartbeatInterval.
const defaultHeartbeatInterval = 30 * time.Second

// Server wraps an http.Server bound to a session store.
//
// inbound / outbound are holders (not raw pipelines) so a pipeline
// hot-swap under the running server is reflected in the next
// GET /v1/pipeline response without restarting.
type Server struct {
	server    *http.Server
	store     *session.Store
	inbound   *pipeline.Holder
	outbound  *pipeline.Holder
	heartbeat time.Duration
	// catalog returns the registered-plugin metadata for /v1/plugins.
	// nil disables the endpoint (returns 404). The binary wires this to
	// plugins.Catalog; tests inject a stub provider.
	catalog CatalogProvider
}

// CatalogEntry is the wire shape for one plugin in /v1/plugins. Mirrors
// pipelinePluginView's metadata fields so abctl can use the same
// rendering paths for the active pipeline and the catalog browser.
//
// Uses readsBody (the modern field name) instead of pipelinePluginView's
// legacy bodyAccess: this is a new wire shape introduced in the same PR
// that documents bodyAccess as deprecated, so there's no compat cost to
// emit the right name from day one.
type CatalogEntry struct {
	Name        string   `json:"name"`
	Direction   string   `json:"direction,omitempty"`
	ReadsBody   bool     `json:"readsBody,omitempty"`
	Writes      []string `json:"writes,omitempty"`
	Reads       []string `json:"reads,omitempty"`
	Requires    []string `json:"requires,omitempty"`
	RequiresAny []string `json:"requiresAny,omitempty"`
	After       []string `json:"after,omitempty"`
	Claims      []string `json:"claims,omitempty"`
	Description string   `json:"description,omitempty"`
}

// CatalogProvider is the function the binary supplies to expose
// registered-plugin metadata to /v1/plugins. Decoupled so the
// sessionapi package doesn't import authlib/plugins.
type CatalogProvider func() []CatalogEntry

// Option configures a Server at construction time.
type Option func(*Server)

// WithHeartbeatInterval overrides the SSE heartbeat cadence. Primarily for
// tests — production deployments should use the default.
func WithHeartbeatInterval(d time.Duration) Option {
	return func(s *Server) { s.heartbeat = d }
}

// WithPipelines attaches the inbound and outbound pipeline holders so
// the server can expose their current composition at GET /v1/pipeline.
// Either may be nil when a mode doesn't configure that direction.
func WithPipelines(inbound, outbound *pipeline.Holder) Option {
	return func(s *Server) {
		s.inbound = inbound
		s.outbound = outbound
	}
}

// WithCatalog attaches a CatalogProvider so the server exposes the
// registered-plugin catalog at GET /v1/plugins. Without this option the
// endpoint returns 404 — useful in tests, harmless in production
// because plugins.Catalog is always available to the binary.
func WithCatalog(c CatalogProvider) Option {
	return func(s *Server) { s.catalog = c }
}

// New constructs an HTTP server serving the session API at addr. store must
// be non-nil; callers should only instantiate when session tracking is on.
func New(addr string, store *session.Store, opts ...Option) *Server {
	s := &Server{
		store:     store,
		heartbeat: defaultHeartbeatInterval,
	}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions", s.handleList)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGet)
	mux.HandleFunc("GET /v1/events", s.handleStream)
	mux.HandleFunc("GET /v1/pipeline", s.handlePipeline)
	mux.HandleFunc("GET /v1/plugins", s.handlePluginCatalog)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	s.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Server returns the underlying *http.Server so callers can register it for
// graceful shutdown alongside the binary's other HTTP listeners.
func (s *Server) Server() *http.Server { return s.server }

// ListenAndServe blocks until the server returns. Returns http.ErrServerClosed
// on graceful shutdown.
func (s *Server) ListenAndServe() error { return s.server.ListenAndServe() }

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.server.Shutdown(ctx) }

// --- handlers -------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// pipelinePluginView is the wire shape for one plugin in /v1/pipeline.
//
// The capability fields below (Reads/Writes/Requires/RequiresAny/After/
// Claims/Description) are static type-level metadata: same for every
// instance produced by a given factory. abctl uses them to render the
// plugin-detail pane and to compute the "deps satisfied" indicator on
// the Pipeline pane without needing a separate /v1/plugins call.
type pipelinePluginView struct {
	Name        string          `json:"name"`
	Direction   string          `json:"direction"`
	Position    int             `json:"position"` // 1-based order within its direction
	BodyAccess  bool            `json:"bodyAccess"`
	Writes      []string        `json:"writes,omitempty"`
	Reads       []string        `json:"reads,omitempty"`
	Requires    []string        `json:"requires,omitempty"`
	RequiresAny []string        `json:"requiresAny,omitempty"`
	After       []string        `json:"after,omitempty"`
	Claims      []string        `json:"claims,omitempty"`
	Description string          `json:"description,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
}

// handlePipeline returns the composition of the inbound and outbound
// pipelines. Empty arrays when a pipeline is unconfigured (mode-dependent).
func (s *Server) handlePipeline(w http.ResponseWriter, _ *http.Request) {
	body := struct {
		Inbound  []pipelinePluginView `json:"inbound"`
		Outbound []pipelinePluginView `json:"outbound"`
	}{
		Inbound:  describePipeline(s.inbound, "inbound"),
		Outbound: describePipeline(s.outbound, "outbound"),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("sessionapi: pipeline encode failed", "error", err)
	}
}

// handlePluginCatalog returns every registered plugin's metadata —
// not just the ones in the active pipeline. abctl renders this in
// the catalog browser pane so operators can see what's available
// before adding one to the pipeline.
//
// Auth: none, consistent with the rest of /v1/* (the package-level
// trust model gates this server to in-cluster networking only). The
// catalog reveals plugin metadata — names, dependency declarations,
// descriptions — never user content or secrets, so this is fine for
// the current posture. Revisit if sessionapi ever gates auth.
func (s *Server) handlePluginCatalog(w http.ResponseWriter, _ *http.Request) {
	if s.catalog == nil {
		http.NotFound(w, nil)
		return
	}
	body := struct {
		Plugins []CatalogEntry `json:"plugins"`
	}{Plugins: s.catalog()}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("sessionapi: catalog encode failed", "error", err)
	}
}

// describePipeline turns a *pipeline.Holder into its wire form, or an
// empty slice when nil. Loads through the Holder so a hot-swap that
// landed between requests is reflected immediately.
func describePipeline(h *pipeline.Holder, direction string) []pipelinePluginView {
	if h == nil {
		return []pipelinePluginView{}
	}
	plugins := h.Plugins()
	out := make([]pipelinePluginView, len(plugins))
	for i, pl := range plugins {
		caps := pl.Capabilities().Normalize()
		view := pipelinePluginView{
			Name:      pl.Name(),
			Direction: direction,
			Position:  i + 1,
			// Normalize folds BodyAccess (deprecated) into ReadsBody;
			// emit ReadsBody as the wire's BodyAccess field for backward
			// compatibility with abctl < the catalog PR.
			BodyAccess:  caps.ReadsBody,
			Writes:      caps.Writes,
			Reads:       caps.Reads,
			Requires:    caps.Requires,
			RequiresAny: caps.RequiresAny,
			After:       caps.After,
			Claims:      caps.Claims,
			Description: caps.Description,
		}
		// Surface raw config when the plugin was wrapped by the registry.
		// Non-Configurable plugins don't satisfy RawConfigProvider; Config
		// stays nil and json.Marshal omits it via omitempty.
		//
		// Trust note: bytes are emitted verbatim. The framework convention
		// is that secrets live behind *_file paths, never inline (mirrors
		// the policy at :9093/config). This endpoint is in-cluster only;
		// a regex-based redaction layer would be illusory given how many
		// secret-like field names exist in practice. Enforce no-inline-
		// secrets at config-review time instead.
		if rc, ok := pl.(pipeline.RawConfigProvider); ok {
			view.Config = rc.RawConfig()
		}
		out[i] = view
	}
	return out
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}{Sessions: s.store.ListSessions()}); err != nil {
		slog.Debug("sessionapi: list encode failed", "error", err)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	view := s.store.View(id)
	if view == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(view); err != nil {
		slog.Debug("sessionapi: get encode failed", "error", err, "sessionID", id)
	}
}

// handleStream delivers new session events as an SSE stream. Supports
// ?session=<id> to filter to one session. A heartbeat comment is emitted
// at the configured interval so clients can detect dead connections.
//
// Lifecycle: subscribes to the store, flushes each event to the client, and
// exits when the client disconnects (via r.Context().Done()). The subscriber
// is always cancelled on exit to free the buffered channel.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	filter := strings.TrimSpace(r.URL.Query().Get("session"))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering if any

	// Subscribe BEFORE the ": ok" comment so any Append that happens between
	// the client reading ": ok" and returning to scan the stream is captured.
	// Flushing first and subscribing after opened a race where tests (and
	// real clients that react quickly on ": ok") could Append events before
	// the subscriber was registered, losing them.
	sub, cancel := s.store.Subscribe()
	defer cancel()

	// Initial comment lets the client know the stream is live before any events.
	fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()

	var id uint64
	var lastDrops uint64

	for {
		select {
		case <-r.Context().Done():
			return

		case event, ok := <-sub.Events():
			if !ok {
				// Store closed or subscription cancelled externally.
				return
			}
			if filter != "" && event.SessionID != filter {
				continue
			}

			payload, err := json.Marshal(event)
			if err != nil {
				slog.Debug("sessionapi: marshal event failed", "error", err)
				continue
			}
			id++
			fmt.Fprintf(w, "event: session-event\nid: %d\ndata: %s\n\n", id, payload)
			flusher.Flush()

		case <-heartbeat.C:
			// Surface accumulated drops so the operator can notice a slow client.
			if drops := sub.Drops(); drops > lastDrops {
				slog.Warn("sessionapi: sse consumer lagged",
					"drops", drops, "newDrops", drops-lastDrops)
				lastDrops = drops
			}
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
