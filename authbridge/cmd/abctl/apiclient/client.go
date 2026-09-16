// Package apiclient talks to AuthBridge's session events HTTP API at
// :9094 by default. It owns the wire protocol so the TUI only deals in
// domain types.
package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Client is a handle to a session API endpoint. Safe for concurrent use.
//
// Two http.Clients share a single Transport: `http` for short REST calls,
// `httpStream` for SSE. NEITHER carries an http.Client.Timeout — the CALLER's
// context deadline is the bound, and getJSON supplies restDefaultTimeout only
// when the caller passed no deadline at all. Sharing the Transport keeps the
// idle-connection pool warm across reconnects so a long session doesn't leak
// Transports.
//
// The fixed timeout this used to set was 10s, which SILENTLY PRE-EMPTED any
// caller that budgeted more: `abctl cost` allows costFetchTimeout (15s) because
// a symbolic window reads day files off disk, and it could never reach it —
// http.Client.Timeout and the request context are both hard stops and the
// shorter one always wins. The comment explaining the 15s therefore described
// behaviour that could not happen. A per-call default that DEFERS to a deadline
// the caller set keeps the protection for callers with no deadline (the TUI
// passes its root context to GetPipeline / GetPluginCatalog / ListSessions /
// GetSession) without overriding one that does.
type Client struct {
	endpoint   string
	http       *http.Client
	httpStream *http.Client
}

// restDefaultTimeout bounds a REST call whose caller supplied NO deadline, so a
// dead endpoint cannot hang a caller forever. 10s, the value the http.Client
// carried before, because that is the bound those callers have always had — the
// TUI's pipeline, catalog and session fetches pass the app's root context, and
// this change is not the place to lengthen their failure time.
//
// A FLOOR, never a ceiling: a caller with its own deadline keeps it, shorter or
// longer. Callers that set one (every Cost/Usage/spend poll at 5s, `abctl cost` at
// 15s) are unaffected by this value in either direction.
//
// A var rather than a const so a test can shorten it and assert the behaviour in
// milliseconds. Nothing in production writes it.
var restDefaultTimeout = 10 * time.Second

// New returns a Client pointed at endpoint (e.g. "http://localhost:9094").
// Trailing slash is tolerated.
func New(endpoint string) *Client {
	// Clone the default transport rather than reuse it so tests / multiple
	// Clients don't share connection pools.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{
		endpoint: trimSlash(endpoint),
		// No Timeout on either: see the type doc. An http.Client.Timeout applies to
		// every call this Client will ever make, so it cannot be reconciled with
		// per-call budgets that legitimately differ by 3x.
		http: &http.Client{
			Transport: transport,
		},
		httpStream: &http.Client{
			Transport: transport,
		},
	}
}

// Endpoint returns the server's base URL. Used by the TUI to display context.
func (c *Client) Endpoint() string { return c.endpoint }

// ListSessions fetches /v1/sessions.
func (c *Client) ListSessions(ctx context.Context) ([]session.SessionSummary, error) {
	var body struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}
	if err := c.getJSON(ctx, "/v1/sessions", &body); err != nil {
		return nil, err
	}
	return body.Sessions, nil
}

// GetSession fetches /v1/sessions/{id}. Returns an error whose Unwrap chain
// includes ErrNotFound if the server returned 404.
func (c *Client) GetSession(ctx context.Context, id string) (*pipeline.SessionView, error) {
	var view pipeline.SessionView
	path := "/v1/sessions/" + url.PathEscape(id)
	if err := c.getJSON(ctx, path, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// ErrNotFound is returned when the server responds 404.
var ErrNotFound = fmt.Errorf("apiclient: not found")

// ErrBadRequest is returned when the server responds 400 — it understood the request
// and refused it.
//
// Distinguished from every other non-200 because it is the one that is the CALLER's
// fault and the one a caller can act on: an unsupported window, a resolution the
// storage cannot divide, session= alongside a symbolic window. Without it "unexpected
// status 400" was indistinguishable from a dial failure, and `abctl cost` told a user
// their proxy was down when the real answer was "that proxy does not know that
// window". The server's own message is carried through, since every message this
// endpoint returns is a fixed string authored server-side.
var ErrBadRequest = fmt.Errorf("apiclient: bad request")

// PipelineView is the decoded shape of GET /v1/pipeline.
type PipelineView struct {
	Inbound  []PipelinePlugin `json:"inbound"`
	Outbound []PipelinePlugin `json:"outbound"`
}

// PipelinePlugin describes one plugin's position, direction, and
// capabilities. Mirrors the server's pipelinePluginView exactly.
type PipelinePlugin struct {
	Name        string          `json:"name"`
	Direction   string          `json:"direction"`
	Position    int             `json:"position"`
	ReadsBody   bool            `json:"readsBody"`
	Requires    []string        `json:"requires,omitempty"`
	RequiresAny []string        `json:"requiresAny,omitempty"`
	Description string          `json:"description,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Metrics     []PluginMetric  `json:"metrics,omitempty"`
}

// PluginMetric mirrors authlib/pipeline.Metric on the wire. Kept as a local
// type rather than importing the server struct, matching PluginFieldEntry:
// the client owns its decode shape, and a decode test guards the tags
// against drift.
type PluginMetric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
	Note  string  `json:"note,omitempty"`
}

// GetPipeline fetches /v1/pipeline.
func (c *Client) GetPipeline(ctx context.Context) (*PipelineView, error) {
	var view PipelineView
	if err := c.getJSON(ctx, "/v1/pipeline", &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// PluginCatalog is the decoded shape of GET /v1/plugins.
type PluginCatalog struct {
	Plugins []PluginCatalogEntry `json:"plugins"`
}

// PluginCatalogEntry mirrors the server's sessionapi.CatalogEntry.
// Describes a registered plugin's static type-level metadata; the
// catalog includes plugins not currently in the active pipeline.
type PluginCatalogEntry struct {
	Name        string             `json:"name"`
	Direction   string             `json:"direction,omitempty"`
	ReadsBody   bool               `json:"readsBody,omitempty"`
	Requires    []string           `json:"requires,omitempty"`
	RequiresAny []string           `json:"requiresAny,omitempty"`
	Description string             `json:"description,omitempty"`
	Fields      []PluginFieldEntry `json:"fields,omitempty"`
}

// PluginFieldEntry mirrors sessionapi.FieldSchemaEntry — per-field
// schema metadata for a plugin's config. Used by abctl edit's
// templates renderer; nil for plugins without configs.
type PluginFieldEntry struct {
	Name        string             `json:"name"`
	Type        string             `json:"type"`
	Required    bool               `json:"required,omitempty"`
	Description string             `json:"description,omitempty"`
	Default     string             `json:"default,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Fields      []PluginFieldEntry `json:"fields,omitempty"`
}

// GetPluginCatalog fetches /v1/plugins. Returns ErrNotFound when the
// server is too old to serve the endpoint (no WithCatalog option) so
// callers can degrade gracefully.
func (c *Client) GetPluginCatalog(ctx context.Context) (*PluginCatalog, error) {
	var cat PluginCatalog
	if err := c.getJSON(ctx, "/v1/plugins", &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	// The caller's deadline governs; this only supplies one where there is none.
	// Checked rather than applied unconditionally, because context.WithTimeout
	// SHORTENS but never lengthens: applying it to `abctl cost`'s 15s budget would
	// reinstate the pre-emption this replaced.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, restDefaultTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.endpoint+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if resp.StatusCode == http.StatusBadRequest {
		// The server's own words, bounded. Every message /v1/* returns for a 400 is a
		// fixed string authored server-side and interpolates no query input — that is a
		// stated requirement of writeUsageError — so forwarding it cannot reflect the
		// caller's own bytes back at them. Bounded anyway, because this client cannot
		// verify what it is talking to.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if detail := badRequestDetail(msg); detail != "" {
			return fmt.Errorf("%s: %w: %s", path, ErrBadRequest, detail)
		}
		return fmt.Errorf("%s: %w", path, ErrBadRequest)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: unexpected status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", path, err)
	}
	return nil
}

// badRequestDetail pulls the "error" field out of a 400 body.
//
// Returns "" for anything it cannot read as the documented shape, so a proxy that
// answered 400 with HTML or with nothing produces a bare ErrBadRequest rather than a
// line of markup in a terminal.
func badRequestDetail(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Error)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// GetUsage fetches time-bucketed usage aggregates from GET /v1/usage.
//
// sessionID empty means all sessions combined. resolution is the width of the
// buckets returned — the server folds, so a 1h window at 5m arrives as 12
// buckets rather than 60. Read the returned Snapshot.BucketSeconds rather than
// assuming the requested resolution was honored.
//
// Returns ErrNotFound when the proxy has no usage aggregator wired (older
// binary, or session tracking disabled), which callers should render as
// "unavailable" rather than as an empty chart.
func (c *Client) GetUsage(ctx context.Context, window, resolution time.Duration, sessionID string, group usage.Group) (*usage.Snapshot, error) {
	// Expressed in terms of GetUsageWindow so there is ONE request-building path.
	// Two would drift on query encoding or on the ErrNotFound behaviour the godoc
	// above promises, and the drift would show up as a chart that works on one code
	// path and 404s on the other.
	return c.GetUsageWindow(ctx, window.String(), resolution, sessionID, group)
}

// GetUsageWindow fetches a snapshot for a symbolic window the server names —
// "today", "7d" — which a time.Duration cannot express.
//
// A sibling of GetUsage rather than a widened signature: GetUsage has several
// callers and its duration parameters are the right shape for the chart windows,
// which really are fixed lengths. "Today" is not a length, it is a boundary.
//
// Read Snapshot.Window rather than assuming this one was served. A symbolic window
// requested where the proxy has no durable cost ledger — Kubernetes, by design —
// is answered from the in-memory ring's maximum span instead, and the response
// names the window it actually served. Labelling that figure "today" would report
// six hours as a day.
//
// resolution of 0 omits the parameter, leaving the server's default. The ledger
// serves a symbolic window as a single bucket and does not read the resolution at
// all, so there is no meaningful value for a caller to invent — and "0s" would be
// rejected as finer than the storage bucket.
func (c *Client) GetUsageWindow(ctx context.Context, window string, resolution time.Duration, sessionID string, group usage.Group) (*usage.Snapshot, error) {
	q := url.Values{}
	q.Set("window", window)
	if resolution > 0 {
		q.Set("resolution", resolution.String())
	}
	if sessionID != "" {
		q.Set("session", sessionID)
	}
	if group != "" && group != usage.GroupNone {
		q.Set("group", string(group))
	}
	var snap usage.Snapshot
	if err := c.getJSON(ctx, "/v1/usage?"+q.Encode(), &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}
