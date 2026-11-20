package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Evolve is the plugin's view of an altk-evolve service. The concrete
// implementation (mcpEvolve) calls the stock evolve MCP server's tools; tests
// inject a fake.
type Evolve interface {
	// Guidelines returns the guidelines relevant to a task from a namespace.
	Guidelines(ctx context.Context, namespace, task, sessionID string) ([]Guideline, error)
	// Learn sends a trajectory batch to evolve, which generates guidelines from
	// it into namespace.
	Learn(ctx context.Context, namespace string, t Trajectory) error
}

// Guideline is one served guideline plus the provenance agentop shows for it.
type Guideline struct {
	ID       string
	Content  string
	Revision *int
	// Source is the trajectory the guideline was learned from (its source task
	// id), or several, comma-separated, for a guideline consolidated from more
	// than one.
	Source string
}

// Trajectory is evolve's processing input (altk_evolve.processing.Trajectory).
type Trajectory struct {
	Messages        []map[string]any `json:"messages"`
	ContextMessages []map[string]any `json:"context_messages,omitempty"`
	Batch           *Batch           `json:"batch,omitempty"`
	TraceID         string           `json:"trace_id,omitempty"`
	Model           string           `json:"model,omitempty"`
	Metadata        map[string]any   `json:"metadata,omitempty"`
}

// Batch is evolve's idempotency key for one bounded contribution to a
// conversation: re-sending the same batch is skipped by evolve.
type Batch struct {
	Source         string `json:"source"`
	ConversationID string `json:"conversation_id"`
	BatchID        string `json:"batch_id"`
	Scope          string `json:"scope,omitempty"`
}

// ErrUnavailable wraps every failure to get an answer from evolve: transport
// errors, timeouts, tool errors and undecodable results. The plugin fails open
// on all of them.
var ErrUnavailable = errors.New("evolve unavailable")

// maxServedGuidelines is the most guidelines read from the served namespace
// when evolve can't return them with their attribution (list_entities caps a
// page at 200).
const maxServedGuidelines = 200

// connectTimeout bounds establishing the SSE session: the dial, the stream's
// response headers, the endpoint event and initialize. A caller waits for a
// connect only as long as its own context allows; the connect carries on, for
// the calls after it, until this bound.
const connectTimeout = 10 * time.Second

// sentinelHeader marks the plugin's own calls so that, should one ever loop
// back through the listener, OnRequest skips it instead of recursing.
const sentinelHeader = "X-Evolve-Cortex"

// profileDefinition is the processing profile the plugin owns on the evolve
// server: standard guideline generation, with the named guideline prompt, or
// with the server's own default prompt when prompt is empty.
// ensure_processing_profile creates it once and keeps operator edits made to it
// afterwards.
func profileDefinition(prompt string) map[string]any {
	config := map[string]any{"guidelines_mode": "standard"}
	if prompt != "" {
		config["guidelines_prompt"] = prompt
	}
	return map[string]any{
		"processors": []any{map[string]any{"id": "guidelines", "plugin": "evolve.guidelines", "config": config}},
	}
}

// mcpEvolve talks to `evolve-mcp --transport sse`. One SSE session is kept
// open and re-established after a failure. Connecting happens off the lock, one
// attempt at a time, so a slow or hung evolve costs each call at most its own
// timeout and never blocks calls on other sessions.
//
// Its http.Client never uses a proxy: Cortex is usually also the machine's
// HTTPS_PROXY, and routing evolve's calls through it would record them as
// agent traffic. The sentinel header is belt-and-suspenders on top of that.
type mcpEvolve struct {
	url     string
	profile string
	prompt  string // guidelines_prompt for the processing profile; "" = the server's default
	timeout time.Duration
	http    *http.Client

	mu     sync.Mutex
	client *conn
	// connecting is closed when the connect in progress ends; nil when none is.
	connecting chan struct{}
	connErr    error // why the last connect failed
	// gen counts Close calls, so a connect that finishes after one is discarded.
	gen     int
	ensured bool
	// noDetails records that this evolve predates include_details on
	// get_guidelines_with_attribution, so Guidelines reads the guidelines with
	// list_entities instead. Cleared with the session, in case evolve was
	// upgraded in the meantime.
	noDetails bool
	// noPrompt records that this evolve rejected guidelines_prompt (it predates
	// altk-evolve #479, or doesn't have the prompt), so the profile is created
	// with the server's default prompt. Cleared with the session, like noDetails.
	noPrompt bool
}

func newMCPEvolve(url, profile, prompt string, timeout time.Duration) *mcpEvolve {
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	return &mcpEvolve{
		url:     url,
		profile: profile,
		prompt:  prompt,
		timeout: timeout,
		// No client timeout: the SSE stream is long-lived. Calls are bounded by ctx.
		// The dial and response-header timeouts bound only getting an answer started,
		// so a server that accepts the connection and never answers can't hold one.
		http: &http.Client{Transport: &http.Transport{
			Proxy: nil, DialContext: dialer.DialContext, ResponseHeaderTimeout: connectTimeout,
			MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second,
		}},
	}
}

// conn is one SSE session and the number of calls using it. A session that
// failed is retired: later calls get a new one, and it is closed once its last
// call returns. Closing it while a call still waits for its reply would cancel
// that call, and could panic: mcp-go's reader sends a reply on the call's
// channel after releasing its lock, and Close closes the channels still waiting.
type conn struct {
	c       *mcpclient.Client
	calls   int
	retired bool
	closed  bool
}

// session returns the open SSE session, counted as in use until release,
// starting a connect when there is none and none is in progress. The caller
// waits for the connect only until its ctx ends; the connect itself is bounded
// by connectTimeout.
func (e *mcpEvolve) session(ctx context.Context) (*conn, error) {
	e.mu.Lock()
	if e.client != nil {
		cn := e.client
		cn.calls++
		e.mu.Unlock()
		return cn, nil
	}
	wait := e.connecting
	if wait == nil {
		wait = make(chan struct{})
		e.connecting = wait
		go e.connect(wait, e.gen)
	}
	e.mu.Unlock()
	select {
	case <-wait:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.client != nil:
		e.client.calls++
		return e.client, nil
	case e.connErr != nil:
		return nil, e.connErr
	}
	return nil, errors.New("connection closed")
}

// connect establishes a session and publishes it, unless Close ran meanwhile.
func (e *mcpEvolve) connect(done chan struct{}, gen int) {
	c, err := e.dial()
	e.mu.Lock()
	e.connecting = nil
	e.connErr = err
	if err == nil {
		if e.gen == gen {
			e.client = &conn{c: c}
		} else {
			_ = c.Close()
		}
	}
	e.mu.Unlock()
	close(done)
}

func (e *mcpEvolve) dial() (*mcpclient.Client, error) {
	c, err := mcpclient.NewSSEMCPClient(e.url,
		transport.WithHTTPClient(e.http),
		transport.WithHeaders(map[string]string{sentinelHeader: "1"}))
	if err != nil {
		return nil, err
	}
	// The stream outlives the connect, so its context has no deadline. The timer
	// cancels it only if the session isn't up in time: mcp-go's own 30 s wait for
	// the endpoint event starts only once the stream's headers have arrived.
	stream, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(connectTimeout, cancel)
	fail := func(err error) (*mcpclient.Client, error) {
		timer.Stop()
		cancel()
		_ = c.Close()
		return nil, err
	}
	if err := c.Start(stream); err != nil {
		return fail(err)
	}
	initCtx, cancelInit := context.WithTimeout(context.Background(), connectTimeout)
	defer cancelInit()
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "cortex-evolve-plugin", Version: "1"}
	if _, err := c.Initialize(initCtx, init); err != nil {
		return fail(err)
	}
	if !timer.Stop() {
		return fail(fmt.Errorf("not connected within %s", connectTimeout))
	}
	return c, nil
}

// release ends a call on cn. A call whose session failed retires it, and what
// the plugin learned about this evolve (ensured, noDetails, noPrompt) is
// forgotten with it, in case evolve was upgraded or replaced. A retired session
// is closed once no call is using it.
func (e *mcpEvolve) release(cn *conn, failed bool) {
	e.mu.Lock()
	cn.calls--
	if failed && e.client == cn {
		e.retire()
	}
	closeNow := cn.retired && cn.calls == 0 && !cn.closed
	cn.closed = cn.closed || closeNow
	e.mu.Unlock()
	if closeNow {
		_ = cn.c.Close()
	}
}

// retire stops handing out the current session. Called with e.mu held.
func (e *mcpEvolve) retire() {
	e.client.retired = true
	e.client = nil
	e.ensured = false
	e.noDetails = false
	e.noPrompt = false
}

// broken reports whether a call's error means its session failed: a transport
// error (the stream is gone, or evolve refused the request) other than the
// caller's own deadline or cancellation. A tool error or a JSON-RPC error comes
// back over a working session, and a call that timed out says nothing about the
// calls still waiting on the same session.
func broken(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)
}

// call invokes a tool and returns its text result.
func (e *mcpEvolve) call(ctx context.Context, tool string, args map[string]any) (string, error) {
	cn, err := e.session(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: connect: %v", ErrUnavailable, err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := cn.c.CallTool(ctx, req)
	e.release(cn, err != nil && broken(err))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrUnavailable, tool, err)
	}
	text := resultText(res)
	if res.IsError {
		return "", fmt.Errorf("%w: %s: %s", ErrUnavailable, tool, preview(text, 240))
	}
	return text, nil
}

func resultText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if t, ok := c.(mcp.TextContent); ok {
			return t.Text
		}
	}
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			return string(b)
		}
	}
	return ""
}

type attribution struct {
	EntityIDs       []string        `json:"entity_ids"`
	EntityRevisions map[string]*int `json:"entity_revisions"`
	// Guidelines is present when evolve honoured include_details: each selected
	// guideline's content and sources, in the order of EntityIDs.
	Guidelines *[]attributed `json:"guidelines"`
	Error      string        `json:"error"`
}

type attributed struct {
	ID            string   `json:"id"`
	Content       any      `json:"content"`
	SourceTaskIDs []string `json:"source_task_ids"`
}

// detailsArg is get_guidelines_with_attribution's option to return each
// guideline's content and sources (altk-evolve #485). An evolve that predates it
// rejects it as an unexpected argument.
const detailsArg = "include_details"

func (e *mcpEvolve) Guidelines(ctx context.Context, namespace, task, sessionID string) ([]Guideline, error) {
	args := map[string]any{"task": task, "namespace_id": namespace, "session_id": sessionID}
	e.mu.Lock()
	details := !e.noDetails
	e.mu.Unlock()
	if details {
		args[detailsArg] = true
	}
	text, err := e.call(ctx, "get_guidelines_with_attribution", args)
	if err != nil && details && strings.Contains(err.Error(), detailsArg) {
		e.mu.Lock()
		e.noDetails = true
		e.mu.Unlock()
		delete(args, detailsArg)
		text, err = e.call(ctx, "get_guidelines_with_attribution", args)
	}
	if err != nil {
		return nil, err
	}
	var a attribution
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		return nil, fmt.Errorf("%w: decode get_guidelines_with_attribution: %v", ErrUnavailable, err)
	}
	if a.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, a.Error)
	}
	if len(a.EntityIDs) == 0 {
		return nil, nil
	}
	// The attribution's prose is evolve's own prompt wording; the plugin owns the
	// block's format, so it takes each guideline's content and renders the block
	// itself.
	if a.Guidelines != nil {
		out := make([]Guideline, 0, len(*a.Guidelines))
		for _, g := range *a.Guidelines {
			out = append(out, Guideline{
				ID: g.ID, Content: contentText(g.Content), Revision: a.EntityRevisions[g.ID],
				Source: strings.Join(g.SourceTaskIDs, ","),
			})
		}
		return out, nil
	}
	// An evolve without include_details returns only the ids, so the guidelines
	// are read separately. list_entities rather than get_entity: get_entity
	// refuses a guideline whose agent_id differs from the caller's, and a caller
	// cannot name one agent that owns both unowned and agent-owned guidelines.
	byID, err := e.guidelineIndex(ctx, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]Guideline, 0, len(a.EntityIDs))
	for _, id := range a.EntityIDs {
		g, ok := byID[id]
		if !ok {
			continue // removed between the two calls, or beyond maxServedGuidelines
		}
		g.Revision = a.EntityRevisions[id]
		out = append(out, g)
	}
	return out, nil
}

func contentText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	b, _ := json.Marshal(content)
	return string(b)
}

// guidelineIndex reads the guidelines in namespace, keyed by id, for an evolve
// that can't return them with their attribution. It reads at most
// maxServedGuidelines, so in a larger namespace (one learned into directly, say)
// some attributed guidelines are missed.
func (e *mcpEvolve) guidelineIndex(ctx context.Context, namespace string) (map[string]Guideline, error) {
	text, err := e.call(ctx, "list_entities", map[string]any{
		"namespace_id": namespace, "entity_types": []string{"guideline"},
		"include_content": true, "limit": maxServedGuidelines,
	})
	if err != nil {
		return nil, err
	}
	var page struct {
		Items []struct {
			ID       string         `json:"id"`
			Content  any            `json:"content"`
			Metadata map[string]any `json:"metadata"`
		} `json:"items"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		return nil, fmt.Errorf("%w: decode list_entities: %v", ErrUnavailable, err)
	}
	if page.Error != "" {
		return nil, fmt.Errorf("%w: list_entities: %s", ErrUnavailable, page.Error)
	}
	out := make(map[string]Guideline, len(page.Items))
	for _, item := range page.Items {
		g := Guideline{ID: item.ID, Content: contentText(item.Content)}
		g.Source, _ = item.Metadata["source_task_id"].(string)
		out[item.ID] = g
	}
	return out, nil
}

func (e *mcpEvolve) Learn(ctx context.Context, namespace string, t Trajectory) error {
	if err := e.ensure(ctx, namespace); err != nil {
		return err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal trajectory: %w", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	_, err = e.call(ctx, "process_trajectory", map[string]any{
		"trajectory": obj, "namespace_id": namespace, "processing_profile": e.profile,
	})
	return err
}

// ensure prepares the server for learning, once per session: the processing
// profile, and the learn namespace. process_trajectory requires the namespace to
// exist, and the MCP server has no tool that only creates one; every other tool
// that takes namespace_id creates it on first use, so a one-row listing does.
func (e *mcpEvolve) ensure(ctx context.Context, namespace string) error {
	e.mu.Lock()
	done := e.ensured
	e.mu.Unlock()
	if done {
		return nil
	}
	if err := e.ensureProfile(ctx); err != nil {
		return err
	}
	if _, err := e.call(ctx, "list_entities", map[string]any{"namespace_id": namespace, "limit": 1}); err != nil {
		return err
	}
	e.mu.Lock()
	e.ensured = true
	e.mu.Unlock()
	return nil
}

// ensureProfile creates or refreshes the plugin's processing profile. An evolve
// that can't take the configured guideline prompt (one that predates altk-evolve
// #479 rejects the key, and one without the prompt rejects the name) still
// learns, with its own default prompt: the plugin warns once and asks again
// without the prompt, as it fails open everywhere else.
func (e *mcpEvolve) ensureProfile(ctx context.Context) error {
	e.mu.Lock()
	prompt := e.prompt
	if e.noPrompt {
		prompt = ""
	}
	e.mu.Unlock()
	_, err := e.call(ctx, "ensure_processing_profile", map[string]any{
		"profile_id": e.profile, "definition": profileDefinition(prompt),
	})
	if err != nil && prompt != "" && strings.Contains(err.Error(), "guidelines_prompt") {
		slog.Warn("evolve: evolve can't take the configured guideline prompt; learning with its default prompt",
			"guidelines_prompt", prompt, "err", err)
		e.mu.Lock()
		e.noPrompt = true
		e.mu.Unlock()
		_, err = e.call(ctx, "ensure_processing_profile", map[string]any{
			"profile_id": e.profile, "definition": profileDefinition(""),
		})
	}
	return err
}

// Close retires the session, which closes once the calls still using it
// return; those are bounded by their own contexts. It doesn't wait for a connect
// in progress, which closes its own session when it finishes.
func (e *mcpEvolve) Close() {
	e.mu.Lock()
	e.gen++
	var cn *conn
	if e.client != nil {
		cn = e.client
		e.retire()
		if cn.calls > 0 || cn.closed {
			cn = nil
		} else {
			cn.closed = true
		}
	}
	e.mu.Unlock()
	if cn != nil {
		_ = cn.c.Close()
	}
}

func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
