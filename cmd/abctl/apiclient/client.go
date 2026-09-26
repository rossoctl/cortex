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

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Client is a handle to a session API endpoint. Safe for concurrent use.
//
// Two http.Clients share a single Transport: `http` for short REST calls,
// `httpStream` for SSE. NEITHER carries an http.Client.Timeout — the CALLER's
// context deadline is the bound, and getJSON supplies restDefaultTimeout only when
// the caller passed no deadline at all. Sharing the Transport keeps the
// idle-connection pool warm across reconnects so a long session doesn't leak
// Transports.
//
// NO CLIENT-WIDE TIMEOUT, because one applies to every call this Client will ever make and
// so cannot be reconciled with per-call budgets that legitimately differ by 3x. A per-call
// default that DEFERS to a deadline the caller set keeps the protection for callers with none
// (the TUI passes its root context to GetPipeline / GetPluginCatalog / ListSessions /
// GetSession) without overriding one that does. The transport's HeaderTimeout is the liveness
// backstop underneath both, sized so it cannot pre-empt either.
type Client struct {
	endpoint   string
	http       *http.Client
	httpStream *http.Client
}

// restDefaultTimeout bounds a REST call whose caller supplied NO deadline, so a dead
// endpoint cannot hang a caller forever. 10s, the value the http.Client carried
// before, because that is the bound those callers have always had — this change is not
// the place to lengthen their failure time.
//
// A FLOOR, never a ceiling: a caller with its own deadline keeps it, shorter or
// longer.
//
// A var rather than a const so a test can shorten it and assert the behaviour in
// milliseconds. Nothing in production writes it.
var restDefaultTimeout = 10 * time.Second

// HeaderTimeout is the longest this client waits for a peer to BEGIN answering, on every
// call, buffered or streamed. See New for why the bound belongs to the transport.
//
// A LIVENESS BACKSTOP, NEVER A BUDGET, and the difference is the whole reason it is two
// minutes rather than the 10s the deleted http.Client.Timeout used. "Waiting for headers" is
// not the same as "reaching the server": net/http starts this clock at the request and stops
// it at the first response header, so everything the SERVER does in between is inside the
// window. /v1/usage does all of its work there — handleUsage computes the snapshot and only
// then writes headers, and for a symbolic window that means walking up to eight day files off
// a path an operator configured and possibly a slow mount.
//
// So a 10s value reinstated exactly the pre-emption removing http.Client.Timeout was meant to
// end: `abctl cost` budgets 15s BECAUSE the ledger scan is slow, and a scan over 10s died at
// 10s with "timeout awaiting response headers". Measured, not reasoned about: with the bound
// at 200ms and a caller budget of 2s, a server that spent 500ms scanning failed at 202ms.
//
// Two minutes is above every budget any caller in this module sets and still bounds a wedged
// peer, which is all this is for. EXPORTED so that relationship can be asserted rather than
// remembered — see TestCallerBudgets_FitUnderTheHeaderBackstop in package main. A caller
// adding a longer budget than this has to raise it.
const HeaderTimeout = 2 * time.Minute

// responseHeaderTimeout is HeaderTimeout, as a var so a test can shorten it to milliseconds.
// Nothing in production writes it.
var responseHeaderTimeout = HeaderTimeout

// streamDefaultTimeout bounds a STREAMED call whose caller supplied no deadline — the whole
// call, body read included, which is exactly what HeaderTimeout does not do.
//
// IT CLOSES A REGRESSION THIS CLIENT INTRODUCED. Removing http.Client.Timeout removed the only
// bound the streaming path had on the body, and the header bound that replaced it stops at the
// first response header by definition. So a proxy that sent headers and then stalled mid-body
// hung the caller FOREVER, where the deleted 10s Timeout had bounded it — and the TUI is
// exactly that caller: tui/paging.go fetches each older page with the app's root context,
// which carries no deadline. The pre-header stall regressed from 10s to two minutes; this one
// had regressed from 10s to unbounded, which is the worse of the two.
//
// A GENEROUS 60s, not the old 10s, because 10s is what emptied the timeline: a full-body
// snapshot was ~209KB an event and 17 seconds of transfer. The page is ~1KB an event and
// SnapshotEventLimit events, so ~2MB — 60s is a floor of 34KB/s, which no working link is
// under and no bounded page needs.
//
// HERE AND NOT AT THE CALL SITE. One call site is one fix; the property that failed is "a
// deadline-less streaming caller is unbounded", and a per-caller budget leaves the next caller
// to rediscover it. This is the same rule getJSON already applies with restDefaultTimeout —
// supply a default only where the caller set none, never override one that exists — so a
// caller with its own budget is unaffected in either direction.
//
// SSE IS NOT AFFECTED, which is what makes a default here safe at all: Stream goes through
// c.httpStream.Do directly and never reaches getBody, so no long-lived stream inherits this.
// A var so a test can shorten it.
var streamDefaultTimeout = 60 * time.Second

// New returns a Client pointed at endpoint (e.g. "http://localhost:9094").
// Trailing slash is tolerated.
func New(endpoint string) *Client {
	// Clone the default transport rather than reuse it so tests / multiple
	// Clients don't share connection pools.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Bounds the wait for a peer to START answering, without bounding the transfer — the one
	// shape a Timeout on the http.Client could not express, and the reason it is safe to
	// remove that Timeout rather than merely convenient.
	//
	// GetSessionPage streams through getBody and returns an unread body, so it has no
	// buffered-call deadline to inherit; without this, a proxy that accepted the connection
	// and then stalled BEFORE ANY HEADER would hang the TUI until the operator quit it. And it
	// fixes what the deleted Timeout broke: that one covered the body read too, which is why a
	// large snapshot failed at 10s and came up empty. A header bound cannot do that however
	// big the response is.
	//
	// WHICH IS ALSO ITS LIMIT, and stating only the pre-header stall here is how the other half
	// went missing: this bound ends at the first response header, so a peer that sends headers
	// and then stalls MID-BODY is not covered by it at all. The deleted Timeout did cover that.
	// streamDefaultTimeout is what covers it now, and it is not on the transport because it
	// must not apply to a caller that set its own budget.
	//
	// IT IS NOT A "REACH THE SERVER" BOUND, which is how it was first described and is the
	// mistake worth stating here: the server's own work happens inside this window, because
	// nothing obliges a handler to write headers before it computes — and /v1/usage does not.
	// That is why HeaderTimeout is a generous backstop and not a budget; see its doc.
	//
	// Safe on the shared transport, SSE included: a server sends event-stream headers
	// immediately and this does not bound the gaps between events afterwards.
	transport.ResponseHeaderTimeout = responseHeaderTimeout
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

// SummaryView is the projection every timeline fetch asks for. Must match
// sessionapi's recognised `view` value.
const SummaryView = "summary"

// SnapshotEventLimit is how many events a snapshot asks for.
//
// Sent explicitly rather than relying on the server's default, so what this client is
// willing to hold is visible here and tunable without a server change.
//
// RAISED FROM 500 TO THE SERVER'S OWN CEILING, because the reason for the smaller
// number is gone. 500 was picked when a snapshot carried message bodies: a day-old
// session was 5000 events and a gigabyte of JSON, 17s of transfer against a 10s
// client-wide timeout, and the whole request failed with an empty timeline to show for
// it. That timeout is GONE — the bound is on response HEADERS now and does not cap the
// body read, so the 17s transfer would succeed today; see New. The limit is about what
// abctl HOLDS IN MEMORY, which was always the better reason for it. With
// view=summary an event is ~1KB rather than ~209KB, so 2000 events is about 2MB —
// less than half of what 500 full events cost, fetched in a fraction of the time.
//
// The cost of the old number was not just latency. A 1000-event session showed its
// newest 500 and the operator had to know to press [o] for the rest, so "sort
// oldest first" silently meant "oldest of the newest 500". Reaching the server's
// maxEventLimit means a normal session arrives whole and that trap is gone.
//
// A ceiling still exists, and this is it: 2000 is what the server clamps to, so
// asking for more would be a request the server quietly shrinks. Sessions longer
// than that still page, which is what [o] and the "N older" footer note are for.
const SnapshotEventLimit = 2000

// GetSession fetches the most recent SnapshotEventLimit events of a session. Returns an
// error whose Unwrap chain includes ErrNotFound if the server returned 404.
//
// The returned view's TotalEvents is non-zero when older events exist that this
// response does not carry.
func (c *Client) GetSession(ctx context.Context, id string) (*pipeline.SessionView, error) {
	return c.GetSessionTail(ctx, id, SnapshotEventLimit)
}

// GetSessionTail fetches the most recent limit events of a session. The tail is the newest
// page, so this is GetSessionPage with no cursor.
func (c *Client) GetSessionTail(ctx context.Context, id string, limit int) (*pipeline.SessionView, error) {
	return c.GetSessionPage(ctx, id, 0, limit)
}

// ErrNotFound is returned when the server responds 404.
var ErrNotFound = fmt.Errorf("apiclient: not found")

// ErrBadRequest is returned when the server responds 400 — it understood the request
// and refused it.
//
// Distinguished from every other non-200 because it is the one that is the CALLER's
// fault and the one a caller can act on: an unsupported window, a resolution the
// storage cannot divide, session= alongside a symbolic window. Without it "unexpected
// status 400" was indistinguishable from a dial failure, and a CLI would tell a user
// their proxy was down when the real answer was "that proxy does not know that
// window". The server's own message is carried through — see getBody for why that is
// safe.
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

// PluginMetric mirrors core/pipeline.Metric on the wire. Kept as a local
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

// getBody issues the GET and hands back the body for the caller to read and close.
//
// Split out of getJSON so a caller that must NOT buffer the whole response — the session
// snapshot, which reaches hundreds of megabytes — can stream the body while still sharing
// this one place that knows how the API reports 404 and other statuses. See snapshot.go.
//
// A DEADLINE-LESS CALLER GETS streamDefaultTimeout, and it has to outlive this function: the
// body is returned unread, so a deferred cancel would abort the caller's first Read. It
// travels with the body instead and fires on Close — see bodyWithCancel. Every FAILURE path
// releases it here, through one deferred check on the named error rather than a call before
// each return, because that is the version that cannot be forgotten when a status branch is
// added.
func (c *Client) getBody(ctx context.Context, path string) (body io.ReadCloser, err error) {
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, streamDefaultTimeout)
	}
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	req, rerr := http.NewRequestWithContext(ctx, "GET", c.endpoint+path, nil)
	if rerr != nil {
		return nil, rerr
	}
	resp, derr := c.http.Do(req)
	if derr != nil {
		return nil, derr
	}
	if resp.StatusCode == http.StatusNotFound {
		// Drained before closing so the connection returns to the pool rather than
		// being torn down — abctl polls this API.
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining a discarded body
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if resp.StatusCode == http.StatusBadRequest {
		// The server's own words, bounded. Every message /v1/* returns for a 400 is a
		// fixed string authored server-side and interpolates no query input — that is a
		// stated requirement of writeUsageError, because the endpoint is unauthenticated —
		// so forwarding it cannot reflect the caller's own bytes back at them. Bounded
		// anyway, because this client cannot verify what it is talking to.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		if detail := badRequestDetail(msg); detail != "" {
			return nil, fmt.Errorf("%s: %w: %s", path, ErrBadRequest, detail)
		}
		return nil, fmt.Errorf("%s: %w", path, ErrBadRequest)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: unexpected status %d", path, resp.StatusCode)
	}
	return bodyWithCancel{ReadCloser: resp.Body, cancel: cancel}, nil
}

// bodyWithCancel releases a request's deadline when the body is closed.
//
// The deadline getBody may add covers the body read, so it cannot be released when getBody
// returns — that is the whole point of it — and Go has nowhere else to hang the lifetime. The
// caller already closes the body (snapshot.go defers it), so Close is the honest hook.
//
// CLOSE FIRST, THEN CANCEL. Cancelling a live request tears the connection down; closing a
// fully-read body returns it to the pool, and abctl polls this API. Reversing the two costs a
// connection per page.
//
// A caller that never closes leaks nothing but the timer, until the deadline fires. That is
// bounded by construction, which is more than the unbounded read this replaced.
type bodyWithCancel struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b bodyWithCancel) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
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
	// CONTROL RUNES STRIPPED before anything else. This string is printed to a terminal and, in
	// the TUI, into a flash line — so an escape sequence in it can reposition the cursor, recolour
	// the rest of the session or hide what follows. The endpoint is UNAUTHENTICATED and this client
	// cannot verify what answered, which is the same reason the read is already bounded at 512
	// bytes: that bound stops a flood, this one stops a payload that fits inside it.
	//
	// pipeline.IsControlRune is the predicate the ledger and the aggregate already sanitise their
	// labels with, so a byte refused on one surface is not accepted on another.
	var clean strings.Builder
	for _, r := range payload.Error {
		if pipeline.IsControlRune(r) {
			continue
		}
		clean.WriteRune(r)
	}
	return strings.TrimSpace(clean.String())
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	// The caller's deadline governs; this only supplies one where there is none.
	// Checked rather than applied unconditionally, because context.WithTimeout
	// SHORTENS but never lengthens: applying it to a 15s budget would reinstate the
	// pre-emption removing http.Client.Timeout was meant to end. See restDefaultTimeout.
	//
	// HERE AND NOT IN getBody, which is the shared status-handling path and would look
	// like the tidier home. getBody RETURNS an unread body — snapshot.go streams a
	// session that reaches hundreds of megabytes, 17 seconds of transfer — so a
	// deadline applied there would cancel mid-read and resurrect the empty-timeline
	// failure that SnapshotEventLimit exists to bound. This function buffers and decodes
	// before returning, so its deadline covers exactly the work it waits on.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, restDefaultTimeout)
		defer cancel()
	}
	body, err := c.getBody(ctx, path)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // read-only body
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", path, err)
	}
	return nil
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
// "today", "7d", "month" — which a time.Duration cannot express.
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
