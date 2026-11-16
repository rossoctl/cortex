package main

// tuicapture drives the real agentop TUI headlessly and captures its screens.
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
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/tui"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
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

// Fixture is every session the demo shows.
type Fixture struct {
	Sessions []FixtureSession `yaml:"sessions"`
	Replay   *Replay          `yaml:"replay"`
}

// Replay is one recorded session, scrubbed to what the timeline reads, replayed into
// the store as the beats' At values advance. Its text is hashes and its labels are
// invented: see testdata/README.md for how it was made.
type Replay struct {
	File  string `yaml:"file"`
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
}

// Capturer owns a live-but-synthetic cortex stack plus a TUI model attached to it.
type Capturer struct {
	store  *session.Store
	usage  *usage.Aggregator
	ledger *ledger.Writer
	ts     *httptest.Server
	cancel context.CancelFunc
	// now is the one clock the whole stack reads — see demoAnchor.
	now func() time.Time

	m      tea.Model
	cmds   []tea.Cmd
	cols   int
	rows   int
	perCmd time.Duration
	// shift moves the clock forward to the current beat's At, in nanoseconds. Atomic,
	// because the API server's goroutines read the clock while a beat changes it.
	shift atomic.Int64
	// replay is the recorded session's events not yet recorded, oldest first, already
	// moved onto the demo clock; replayID is the session they file under.
	replay   []pipeline.SessionEvent
	replayID string
	// frozen stops the clock at demoAnchor plus shift. Set with a replay: the timeline
	// cuts its cells from "now", so a clock that moved during a settle moved every cell
	// by however long that settle took, and two runs differed. The model's timers run on
	// real time either way, so its polls still fire.
	frozen atomic.Bool
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

// WriteTitles writes the session-title metadata agentop reads from
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
	if r := f.Replay; r != nil {
		meta[r.ID] = tui.SessionMetadata{Title: r.Title, AgentType: "Claude Code"}
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "session-metadata.json"), blob, 0o644)
}

// demoAnchor is the instant the demo's clock reads when the capture starts.
//
// THE FIXTURE IS LAID OUT RELATIVE TO "NOW", and the spend band's spans are calendar
// spans. turnTime puts one turn per session 26 hours back so 7 DAYS and THIS MONTH exceed
// TODAY — which on the real clock stops being true on the 1st of every month, and early on
// the 2nd, when that turn lands in the previous month: THIS MONTH collapsed to TODAY's
// figure and the committed asset read as stale for a day and a half. The turns minutes
// old crossed into yesterday the same way in the first minutes of every day.
//
// So the ledger, the aggregator, the session API and the TUI all read one clock, shifted
// so that it starts here: mid-month and midday, as far from both boundaries as a calendar
// allows. Shifted, not frozen — it still advances in real time, because the capture's
// polls and the newest turn's "seconds old" depend on time actually passing. Local, not
// UTC: the ledger cuts its day in its clock's zone, and noon in that zone is what keeps
// the anchor clear of midnight in every timezone the generator runs in.
//
// FIVE SECONDS PAST THE MINUTE, not on it. The newest turn sits on the current minute
// boundary, so an anchor on the boundary put it under a second old for the first frames,
// and "just now" is the one age the masking does not rewrite.
var demoAnchor = time.Date(2026, time.September, 15, 12, 0, 5, 0, time.Local)

// NewCapturer builds the synthetic stack, replays the fixture into it, and
// attaches a TUI model sized to cols x rows.
func NewCapturer(f Fixture, cols, rows int, ledgerDir string, start time.Duration) (*Capturer, error) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)

	offset := time.Until(demoAnchor)
	c := &Capturer{}
	c.frozen.Store(f.Replay != nil)
	clock := func() time.Time {
		if c.frozen.Load() {
			return demoAnchor.Add(time.Duration(c.shift.Load()))
		}
		return time.Now().Add(offset + time.Duration(c.shift.Load()))
	}

	ledger, err := ledger.New(ledgerDir, ledger.WithClock(clock))
	if err != nil {
		return nil, fmt.Errorf("cost ledger: %w", err)
	}
	c.store = session.New(0, 0, 100, session.WithClock(clock))
	c.usage = usage.New(usage.WithClock(clock))
	c.ledger = ledger
	c.now = clock
	c.cols, c.rows = cols, rows
	c.perCmd = 400 * time.Millisecond

	for _, e := range c.build(f) {
		if c.frozen.Load() {
			// The store dates a session's update by its own clock: a stopped clock tied
			// every session, and the list order was left to chance.
			c.shift.Store(int64(e.event.At.Sub(demoAnchor)))
		}
		c.record(e.session, e.event)
	}
	c.shift.Store(0)
	if f.Replay != nil {
		if err := c.loadReplay(*f.Replay); err != nil {
			return nil, err
		}
		// The whole replay's cost goes in now, and only its events arrive later: the
		// spend band's spans poll on different cadences, so cost arriving mid-capture
		// showed LAST 1H above THIS MONTH, as warm's comment describes. Dated now, not
		// at its own time: a span's first fetch leaves out a cost dated after it.
		for i := range c.replay {
			e := c.replay[i]
			e.Client = demoClient
			e.At = c.now()
			c.usage.Record(c.replayID, &e)
			c.ledger.Record(c.replayID, &e)
		}
		// The store dates a session's update by its own clock, so the clock must be at
		// the first beat when its events go in, or every session ties and the list order
		// is left to chance.
		c.shift.Store(int64(start))
		c.feed(start)
	}
	if err := c.ledger.Flush(); err != nil {
		return nil, fmt.Errorf("ledger flush: %w", err)
	}

	srv := sessionapi.New(":0", c.store,
		sessionapi.WithHeartbeatInterval(time.Hour),
		sessionapi.WithUsage(c.usage),
		sessionapi.WithCostLedger(c.ledger),
		sessionapi.WithClock(c.now),
	)
	c.ts = httptest.NewServer(srv.Server().Handler)

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.m = tui.New(ctx, apiclient.New(c.ts.URL), tui.WithClock(c.now))
	c.pump(c.m.Init())
	c.resize()

	c.warm(len(f.Sessions))
	c.resize()
	return c, nil
}

// warm fills the model's per-session event caches by drilling into each session
// and backing out again.
//
// CTX(1M) and the per-session token totals fold the model's own event cache,
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
	now := c.now()
	for si, s := range f.Sessions {
		for ti, t := range s.Turns {
			at := turnTime(now, si, ti, len(s.Turns))
			reqID := fmt.Sprintf("%s-%03d", s.ID, ti)

			msgs := conversation(s.ID, t.Messages)
			history, results := toolHistory(msgs)
			// The response asks for the tools whose results the next turn sends back:
			// that is what gives the events pane its tool rows.
			var asked []pipeline.InferenceToolCall
			finish := "end_turn"
			if ti+1 < len(s.Turns) {
				next := conversation(s.ID, s.Turns[ti+1].Messages)
				if len(next) > len(msgs) {
					asked, _ = toolHistory(next[len(msgs):])
				}
			}
			if len(asked) > 0 {
				finish = "tool_use"
			}
			var turn []pendingEvent
			turn = append(turn, pendingEvent{s.ID, pipeline.SessionEvent{
				At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
				RequestID: reqID, Host: "api.anthropic.com",
				HTTPMethod: "POST", HTTPPath: "/v1/messages",
				Inference: &pipeline.InferenceExtension{
					Model:            s.Model,
					AgentRole:        pipeline.AgentRoleMain,
					IsAction:         true,
					Messages:         msgs,
					HistoryToolCalls: history,
					ToolResults:      results,
					Tools:            tools(t.Tools),
					MessageCount:     t.Messages,
					ToolCount:        t.Tools,
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
			// CTX(1M) gauge from responses only, skipping any response with
			// no tool manifest (a one-shot title call) or marked subagent. Omit
			// either and the gauge stays a dash.
			turn = append(turn, pendingEvent{s.ID, pipeline.SessionEvent{
				At: at.Add(2900 * time.Millisecond), Direction: pipeline.Outbound,
				Phase: pipeline.SessionResponse, RequestID: reqID,
				Host: "api.anthropic.com", StatusCode: 200, Duration: 2900 * time.Millisecond,
				Inference: &pipeline.InferenceExtension{
					Model:        s.Model,
					AgentRole:    pipeline.AgentRoleMain,
					ToolCalls:    asked,
					FinishReason: finish,
					// The manifest and the conversation must be present as
					// ARRAYS, not just as counts: summarizeEvent recomputes both
					// counts from len() and then nils the arrays, so counts alone
					// arrive as zero and the CTX(1M) fold skips the response.
					Tools:            tools(t.Tools),
					Messages:         msgs,
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

// turnTime places a turn on the clock. The newest turn of the first session sits on
// the current minute, which makes it look live, and each later session's 5 seconds
// earlier, so the sessions list has one order and not a tie left to chance. Each
// earlier turn is 20 seconds before the next: the gap is what the events pane measures
// a tool row's DURATION from, so it has to be a plausible tool's run.
func turnTime(now time.Time, si, ti, n int) time.Time {
	return now.Truncate(time.Minute).Add(-time.Duration(si)*5*time.Second - time.Duration(n-1-ti)*20*time.Second)
}

// loadReplay reads the recorded session and moves it onto the demo clock: its first
// event lands on demoAnchor, so a beat's At is how far into the recording it is.
func (c *Capturer) loadReplay(r Replay) error {
	f, err := os.Open(r.File)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("replay %s: %w", r.File, err)
	}
	var v pipeline.SessionView
	if err := json.NewDecoder(zr).Decode(&v); err != nil {
		return fmt.Errorf("replay %s: %w", r.File, err)
	}
	if len(v.Events) == 0 {
		return fmt.Errorf("replay %s: no events", r.File)
	}
	d := demoAnchor.Sub(v.Events[0].At)
	for i := range v.Events {
		v.Events[i].At = v.Events[i].At.Add(d)
		v.Events[i].Seq = 0 // the store numbers what it is given
	}
	c.replay, c.replayID = v.Events, r.ID
	return nil
}

// advance moves the demo to at into the replay: the clock jumps, and every replayed
// event up to then is recorded while the TUI is attached, so it arrives over the live
// stream the way a proxy's events do. The settle is longer than a key's, because the
// timeline refetches on its own one-second tick after the stream marks it stale.
func (c *Capturer) advance(at time.Duration) {
	c.shift.Store(int64(at))
	c.feed(at)
	c.settle(2500 * time.Millisecond)
}

// feed records every replayed event at or before at into the replay.
func (c *Capturer) feed(at time.Duration) {
	cut := demoAnchor.Add(at)
	for len(c.replay) > 0 && !c.replay[0].At.After(cut) {
		e := c.replay[0]
		if e.Client == nil {
			e.Client = demoClient
		}
		c.store.Append(c.replayID, e) // its cost is already in: see NewCapturer
		c.replay = c.replay[1:]
	}
}

var demoClient = pipeline.ParseUserAgent("claude-cli/2.1.323 (external, cli)")

// record publishes one event to every consumer a real proxy would feed.
func (c *Capturer) record(id string, e pipeline.SessionEvent) {
	if e.Client == nil {
		// The replay's client on every event: one agent, so agentop lands on Sessions.
		e.Client = demoClient
	}
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
// model started, with CTX(1M) stuck on a dash. Keeping the channel and
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
// The harness binds an ephemeral port, and agentop prints whatever endpoint it is
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

// eventRate matches the footer's events/sec, which agentop measures over real time, so
// it read differently on every run; canonicalRate is the one figure it becomes.
var eventRate = regexp.MustCompile(`\d+\.\d events/sec`)

const canonicalRate = "12.4 events/sec"

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
	view = eventRate.ReplaceAllString(view, canonicalRate)
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
