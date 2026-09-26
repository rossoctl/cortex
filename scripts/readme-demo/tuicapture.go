package main

// tuicapture drives the real abctl TUI headlessly and captures its screens.
//
// Nothing here draws a terminal. The screens in the demo are rendered by the
// same Bubble Tea model, the same session store, the same usage aggregator and
// the same cost ledger that run in production — fed synthetic events instead of
// a live proxy. That is what keeps the demo honest: a column that changes name,
// a figure that changes format, or a pane that gains a row shows up here on the
// next regeneration rather than silently making the asset a lie.
//
// No PTY and no teatest. tea.Cmd is func() tea.Msg and tea.BatchMsg is []Cmd,
// both exported, so a small synchronous runtime is the whole requirement.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/cmd/abctl/tui"
)

// Turn is one request/response exchange with a model.
type Turn struct {
	Messages   int `yaml:"messages"`
	Tools      int `yaml:"tools"`
	Input      int `yaml:"input"`
	CacheRead  int `yaml:"cache_read"`
	CacheWrite int `yaml:"cache_write"`
	Output     int `yaml:"output"`
	// Reasoning is the share of Output the model spent thinking, so the `$`
	// breakdown's reasoning child renders a real figure rather than the
	// not-known cell. A SUBSET of Output, never added to it.
	Reasoning int     `yaml:"reasoning"`
	PromptUSD float64 `yaml:"prompt_usd"`
	OutputUSD float64 `yaml:"output_usd"`
	// ToolCall, when set, adds an outbound MCP tool call after the model
	// response, so the events timeline shows an agent doing something and not
	// only talking to a model.
	ToolCall string `yaml:"tool_call"`
	ToolHost string `yaml:"tool_host"`
}

// FixtureSession is one Claude Code session's synthetic history.
type FixtureSession struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	Model string `yaml:"model"`
	Turns []Turn `yaml:"turns"`
}

// Fixture is every session the demo shows. Authored, never harvested.
type Fixture struct {
	Sessions []FixtureSession `yaml:"sessions"`
}

// Capturer owns a live-but-synthetic cortex stack plus a TUI model attached to it.
type Capturer struct {
	store  *session.Store
	usage  *usage.Aggregator
	ledger *ledger.Writer
	ts     *httptest.Server
	cancel context.CancelFunc

	m      tea.Model
	cmds   []tea.Cmd
	cols   int
	rows   int
	perCmd time.Duration
	// inflight holds commands that outlived their deadline. Their goroutines
	// still carry the model's timer chain, so the channels are kept and read
	// later rather than discarded — see settle.
	inflight []chan tea.Msg
}

// tierShare splits a turn's prompt cost across the three prompt tiers, weighted
// by what each tier actually bills, so the `$` breakdown explains the COST
// column instead of contradicting it.
func tierShare(t Turn) *event.TierCost {
	// A cache read bills at ~0.1x input and a write at ~1.25x. Weighting by
	// these rather than by raw token counts is what makes the breakdown reflect
	// spend rather than volume — the whole point of the pane.
	wIn := float64(t.Input) * 1.0
	wRd := float64(t.CacheRead) * 0.1
	wWr := float64(t.CacheWrite) * 1.25
	total := wIn + wRd + wWr
	if total == 0 {
		return &event.TierCost{Output: t.OutputUSD}
	}
	return &event.TierCost{
		Input:      t.PromptUSD * wIn / total,
		CacheRead:  t.PromptUSD * wRd / total,
		CacheWrite: t.PromptUSD * wWr / total,
		Output:     t.OutputUSD,
	}
}

// costPlugins is the record a litellm-budget-track plugin publishes on a
// response event. The COST column, the spend band and the `$` tier breakdown all
// read it, so a fixture without one shows dashes everywhere money belongs.
func costPlugins(t Turn) map[string]json.RawMessage {
	ev := event.Event{
		CostUSD:   t.PromptUSD + t.OutputUSD,
		Source:    event.SourceUsageFallback,
		PromptUSD: t.PromptUSD,
		OutputUSD: t.OutputUSD,
		Tiers:     tierShare(t),
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		panic(err) // a struct we own; a marshal failure is a programming error
	}
	return map[string]json.RawMessage{event.Key: raw}
}

// WriteTitles writes the session-title metadata abctl reads from
// $HOME/.cortex/session-metadata.json.
//
// The caller must have pointed HOME at a scratch directory first. The generator
// must never read the operator's own Claude Code history, and a temp HOME is how
// that stays true while still exercising the real title code path rather than a
// stub of it.
func WriteTitles(home string, f Fixture) error {
	dir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	meta := make(map[string]tui.SessionMetadata, len(f.Sessions))
	for _, s := range f.Sessions {
		meta[s.ID] = tui.SessionMetadata{Title: s.Title, AgentType: "Claude Code"}
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "session-metadata.json"), blob, 0o644)
}

// NewCapturer builds the synthetic stack, replays the fixture into it, and
// attaches a TUI model sized to cols x rows.
func NewCapturer(f Fixture, cols, rows int, ledgerDir string) (*Capturer, error) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)

	ledger, err := ledger.New(ledgerDir)
	if err != nil {
		return nil, fmt.Errorf("cost ledger: %w", err)
	}
	c := &Capturer{
		store:  session.New(0, 0, 100),
		usage:  usage.New(),
		ledger: ledger,
		cols:   cols,
		rows:   rows,
		perCmd: 400 * time.Millisecond,
	}

	for _, e := range c.build(f) {
		c.record(e.session, e.event)
	}
	if err := c.ledger.Flush(); err != nil {
		return nil, fmt.Errorf("ledger flush: %w", err)
	}

	srv := sessionapi.New(":0", c.store,
		sessionapi.WithHeartbeatInterval(time.Hour),
		sessionapi.WithUsage(c.usage),
		sessionapi.WithCostLedger(c.ledger),
	)
	c.ts = httptest.NewServer(srv.Server().Handler)

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.m = tui.New(ctx, apiclient.New(c.ts.URL))
	c.pump(c.m.Init())
	c.resize()

	c.warm(len(f.Sessions))
	c.resize()
	return c, nil
}

// warm fills the model's per-session event caches by drilling into each session
// and backing out again.
//
// CONTEXT(1M) and the per-session token totals fold the model's own event cache,
// which a session only fills when its events arrive — over the stream while the
// viewer watches, or in the snapshot a drill-in fetches. The generator uses the
// drill-in, because the alternative is to record events AFTER the server is up so
// they arrive live, and that version had the spend band disagreeing with the COST
// column: each of the band's four spans polls on its own cadence (the hour every
// 20s, the wider spans slower), so a capture taken any time short of the slowest
// interval showed LAST 1H exceeding TODAY. Recording everything before the server
// exists means every span's first fetch is already complete and all four agree.
func (c *Capturer) warm(sessions int) {
	c.Press("g")
	for i := 0; i < sessions; i++ {
		c.Press("enter", "esc")
		c.Press("down")
	}
	c.Press("g")
}

type pendingEvent struct {
	session string
	event   pipeline.SessionEvent
}

// build turns the fixture into the events a real proxy would have produced.
//
// Every event is returned at once and recorded before the API server starts. The
// timestamps still span yesterday to forty seconds ago — an event's At is
// independent of when it is recorded — so the spend band's spans differ and the
// UPDATED column still reads as a live session, without any of it arriving late
// enough to make two panes disagree.
func (c *Capturer) build(f Fixture) []pendingEvent {
	var out []pendingEvent
	now := time.Now()
	for si, s := range f.Sessions {
		for ti, t := range s.Turns {
			at := turnTime(now, si, ti, len(s.Turns))
			reqID := fmt.Sprintf("%s-%03d", s.ID, ti)

			var turn []pendingEvent
			turn = append(turn, pendingEvent{s.ID, pipeline.SessionEvent{
				At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
				RequestID: reqID, Host: "api.anthropic.com",
				HTTPMethod: "POST", HTTPPath: "/v1/messages",
				Inference: &pipeline.InferenceExtension{
					Model:        s.Model,
					AgentRole:    pipeline.AgentRoleMain,
					IsAction:     true,
					Messages:     conversation(s.ID, t.Messages),
					Tools:        tools(t.Tools),
					MessageCount: t.Messages,
					ToolCount:    t.Tools,
				},
				// Without invocations the events table renders ACTION and PLUGIN
				// as dashes, which reads as "cortex did nothing to this request".
				Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
					{Plugin: "inference-parser", Action: pipeline.ActionObserve,
						Phase: pipeline.InvocationPhaseRequest},
				}},
			}})
			// The response carries the token split, the cost record, AND the
			// tool count and main-agent role: sessions_context.go folds the
			// CONTEXT(1M) gauge from responses only, skipping any response with
			// no tool manifest (a one-shot title call) or marked subagent. Omit
			// either and the gauge stays a dash.
			turn = append(turn, pendingEvent{s.ID, pipeline.SessionEvent{
				At: at.Add(2900 * time.Millisecond), Direction: pipeline.Outbound,
				Phase: pipeline.SessionResponse, RequestID: reqID,
				Host: "api.anthropic.com", StatusCode: 200, Duration: 2900 * time.Millisecond,
				Inference: &pipeline.InferenceExtension{
					Model:     s.Model,
					AgentRole: pipeline.AgentRoleMain,
					// The manifest and the conversation must be present as
					// ARRAYS, not just as counts: summarizeEvent recomputes both
					// counts from len() and then nils the arrays, so counts alone
					// arrive as zero and the CONTEXT(1M) fold skips the response.
					Tools:            tools(t.Tools),
					Messages:         conversation(s.ID, t.Messages),
					ToolCount:        t.Tools,
					MessageCount:     t.Messages,
					InputTokens:      t.Input,
					CacheReadTokens:  t.CacheRead,
					CacheWriteTokens: t.CacheWrite,
					OutputTokens:     t.Output,
					ReasoningTokens:  t.Reasoning,
					// Reasoning is NOT added: it is already inside Output.
					TotalTokens: t.Input + t.CacheRead + t.CacheWrite + t.Output,
				},
				Plugins: costPlugins(t),
				Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
					{Plugin: "inference-parser", Action: pipeline.ActionObserve,
						Phase: pipeline.InvocationPhaseResponse},
					{Plugin: "litellm-budget-track", Action: pipeline.ActionObserve,
						Phase: pipeline.InvocationPhaseResponse},
				}},
			}})
			if t.ToolCall != "" {
				turn = append(turn, pendingEvent{s.ID, pipeline.SessionEvent{
					At: at.Add(3300 * time.Millisecond), Direction: pipeline.Outbound,
					Phase: pipeline.SessionRequest, RequestID: reqID + "-tool",
					Host: t.ToolHost, HTTPMethod: "POST", HTTPPath: "/mcp",
					MCP: &pipeline.MCPExtension{Method: "tools/call", IsAction: true},
					Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
						{Plugin: "mcp-parser", Action: pipeline.ActionObserve,
							Phase: pipeline.InvocationPhaseRequest},
						{Plugin: "token-exchange", Action: pipeline.ActionModify,
							Phase: pipeline.InvocationPhaseRequest},
					}},
				}})
				turn = append(turn, pendingEvent{s.ID, pipeline.SessionEvent{
					At: at.Add(3600 * time.Millisecond), Direction: pipeline.Outbound,
					Phase: pipeline.SessionResponse, RequestID: reqID + "-tool",
					Host: t.ToolHost, StatusCode: 200, Duration: 300 * time.Millisecond,
					MCP: &pipeline.MCPExtension{Method: "tools/call"},
				}})
			}
			out = append(out, turn...)
		}
	}
	return out
}

// turnTime places a turn on the clock so three different surfaces each have
// something to show.
//
// The newest turn of every session is seconds old, because that is what makes a
// session look live. One turn sits yesterday, so the spend band's 7 DAYS and THIS
// MONTH exceed TODAY instead of all four spans repeating one figure. The remainder
// land inside the last eight minutes, which is what puts more than a single bar
// inside the usage chart's default ten-minute window.
//
// Everything inside that window is anchored to a WHOLE MINUTE, counted back from
// the truncated current minute. The usage chart buckets by absolute minute, so an
// offset that is not a multiple of a minute lands in one bucket or the next
// depending on where in the current minute the generator happens to run — which
// moved a 97k bar one column sideways between a laptop and CI and is the kind of
// difference no amount of digit masking can reconcile.
func turnTime(now time.Time, si, ti, n int) time.Time {
	minute := now.Truncate(time.Minute)
	switch {
	case ti == n-1:
		// Seconds old, but still pinned to a minute boundary: this event only has
		// to be inside the newest bucket, and being exactly on its edge is what
		// keeps it there however long the capture takes.
		return minute
	case ti == 0 && n >= 3:
		return minute.Add(-26 * time.Hour).Add(time.Duration(si) * 13 * time.Minute)
	default:
		return minute.Add(-time.Duration(8-ti-si) * time.Minute)
	}
}

// record publishes one event to every consumer a real proxy would feed.
func (c *Capturer) record(id string, e pipeline.SessionEvent) {
	c.store.Append(id, e)
	c.usage.Record(id, &e)
	c.ledger.Record(id, &e)
}

// settle gives timer-driven work a chance to land.
//
// A command that has not returned yet is NOT discarded: its goroutine is still
// running and holds the only handle on the model's timer chain. Dropping it —
// which an earlier version of this pump did — kills the 2s sessions refresh
// permanently, and the table then shows whichever snapshot existed when the
// model started, with CONTEXT(1M) stuck on a dash. Keeping the channel and
// reading it later is the entire fix.
func (c *Capturer) settle(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c.pump()
		if c.drainInflight() {
			continue
		}
		if len(c.inflight) == 0 && len(c.cmds) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	c.pump()
}

// drainInflight absorbs any in-flight command that has since completed,
// reporting whether anything landed.
func (c *Capturer) drainInflight() bool {
	progressed := false
	for i := 0; i < len(c.inflight); {
		select {
		case msg := <-c.inflight[i]:
			c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
			c.handle(msg)
			progressed = true
		default:
			i++
		}
	}
	return progressed
}

// handle feeds one message to the model, expanding batches and queueing whatever
// the update asks for next.
func (c *Capturer) handle(msg tea.Msg) {
	if msg == nil {
		return
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		c.cmds = append(c.cmds, batch...)
		return
	}
	var next tea.Cmd
	c.m, next = c.m.Update(msg)
	if next != nil {
		c.cmds = append(c.cmds, next)
	}
}

// pump runs queued commands to quiescence. A command that misses its deadline is
// parked in c.inflight rather than dropped; see settle.
func (c *Capturer) pump(extra ...tea.Cmd) {
	c.cmds = append(c.cmds, extra...)
	for i := 0; len(c.cmds) > 0 && i < 4000; i++ {
		cmd := c.cmds[0]
		c.cmds = c.cmds[1:]
		if cmd == nil {
			continue
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		select {
		case msg := <-done:
			c.handle(msg)
		case <-time.After(c.perCmd):
			c.inflight = append(c.inflight, done)
		}
	}
}

func (c *Capturer) resize() {
	var next tea.Cmd
	c.m, next = c.m.Update(tea.WindowSizeMsg{Width: c.cols, Height: c.rows})
	c.pump(next)
}

// Press sends keys, settling the model after each so the pane a key opens has
// its data before the next key or the capture.
func (c *Capturer) Press(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		var next tea.Cmd
		c.m, next = c.m.Update(msg)
		c.pump(next)
		c.settle(1500 * time.Millisecond)
	}
}

// localSessionPort is the port a real local cortex serves its session API on.
// The harness binds an ephemeral port, and abctl prints whatever endpoint it is
// attached to in its header — so the raw capture would show a five-digit port
// that changes every run. Rewriting it to the real one makes the asset both
// deterministic and more accurate: the ephemeral port is a fact about this
// generator, not about cortex.
const localSessionPort = "9094"

// clockPatterns match everything on a screen that moves with the wall clock: the
// UPDATED ages, the events table's TIME column, the ISO timestamps in the detail
// pane, and the usage chart's axis ticks.
//
// They are rewritten to fixed digits of the SAME LENGTH, which is what makes the
// asset byte-identical between runs. Length matters as much as value: the TUI pads
// to fixed column widths, so "9s ago" and "21s ago" consume different amounts of
// the surrounding padding and shift every cell after them.
var clockPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\d{2}:\d{2}:\d{2}(?:\.\d+)?`),
	regexp.MustCompile(`\b\d{2}:\d{2}\b`),
	regexp.MustCompile(`:\d{2}\b`),
}

// isoTimestamp matches the RFC3339 stamps the detail pane prints, and
// canonicalISO is the single value they are all rewritten to.
//
// Unlike the patterns above this one does NOT preserve length, because its length
// is exactly what varies: Go renders a UTC time's offset as "Z" and a local one as
// "-04:00", and trailing zeros in the fractional seconds are trimmed. That shifted
// the tspan's textLength by 42px between a laptop and CI. Trailing spaces are never
// emitted, so replacing the whole stamp with a fixed string of any length leaves
// the rest of the row untouched.
var isoTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[+-]\d{2}:\d{2}|Z)`)

const canonicalISO = "2026-09-22T17:42:03.424242-04:00"

// agePattern matches an UPDATED cell together with the padding that follows it,
// and canonicalAge is what it becomes.
//
// The trailing spaces are part of the match on purpose. An age's LENGTH varies —
// "9s ago", "45s ago" and "1m ago" are six, seven and six characters, and which
// one renders depends on how long the capture took — and the TUI pads the cell to
// a fixed column width, so the age and its padding share one styled run. Replacing
// both together and re-padding to the match's original length makes that run
// identical between runs while leaving every column after it exactly where it was.
var agePattern = regexp.MustCompile(`\d{1,3}[smhd] ago +`)

const canonicalAge = "42s ago"

func fixAge(match string) string {
	if len(match) < len(canonicalAge) {
		return fixClock(match)
	}
	return canonicalAge + strings.Repeat(" ", len(match)-len(canonicalAge))
}

// canonicalDigits is the repeating filler clock digits are replaced with.
const canonicalDigits = "42"

// fixClock rewrites one match's digits, preserving its length exactly.
func fixClock(match string) string {
	var b strings.Builder
	for i, r := range match {
		if r >= '0' && r <= '9' {
			b.WriteByte(canonicalDigits[i%len(canonicalDigits)])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Screen is the current settled view, ANSI included, with the harness's ephemeral
// endpoint and every clock-dependent figure rewritten so two runs of the generator
// produce the same bytes.
func (c *Capturer) Screen() string {
	view := c.m.View()
	if c.ts != nil {
		host := strings.TrimPrefix(c.ts.URL, "http://")
		if _, port, err := net.SplitHostPort(host); err == nil {
			view = strings.ReplaceAll(view, ":"+port, ":"+localSessionPort)
		}
	}
	view = isoTimestamp.ReplaceAllString(view, canonicalISO)
	view = agePattern.ReplaceAllStringFunc(view, fixAge)
	for _, re := range clockPatterns {
		view = re.ReplaceAllStringFunc(view, fixClock)
	}
	return view
}

func (c *Capturer) Close() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.ts != nil {
		c.ts.Close()
	}
	if c.ledger != nil {
		_ = c.ledger.Close()
	}
	if c.store != nil {
		c.store.Close()
	}
}
