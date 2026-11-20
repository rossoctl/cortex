// Package toolpair pairs a session's tool calls with their results.
//
// A tool call is emitted on one response and its result arrives inside a LATER request's
// conversation, so neither event alone says how long the call took. Pairing them needs the
// tool-use id, a forward walk and per-agent state. This package is that walk, and it is shared
// because two consumers need the same answer from the same events:
//
//   - core/session/timeline draws the spans in agentop's Timeline pane.
//   - core/sessionexport projects them to content-free records for GET /v1/export.
//
// WHY A SHARED PACKAGE RATHER THAN A SECOND IMPLEMENTATION. Closing a call correctly has five
// cases beyond "its result arrived", and each one was a bug before it was a case: a call that
// ends the run by design and never gets a result, an agent that moved on without one, a run
// stopped by a <task-notification>, a result that lands before the asking response finished
// (clamped, not negative), and a result that only acknowledges a background job. Five
// regression tests in core/session/timeline pin them. A consumer reimplementing this would
// drift from those tests, silently, in the direction of plausible-looking latency figures.
//
// THE MESSAGE SCAN COMES WITH IT, AND THAT IS THE PRICE OF THE ABOVE. Two of the closing cases
// are only knowable from message text: a <task-notification> says a run ended, and a tool
// result's "Command running in background" says the call merely started a job. So this package
// scans the new messages of every request, which is also where the timeline's Returns and Jobs
// come from — hence Result carries those too, for the one consumer that wants them. The scan
// cannot be reduced to the task-notification match alone: it must skip the history each request
// re-sends (commonPrefix), or it is quadratic in turns — 0.26-0.47GB of text against 35-42MB
// new, on real sessions. core/session/timeline's TestBuildBudget enforces the consequence.
//
// NO ARGUMENT TEXT AND NO RESULT TEXT LEAVE THIS PACKAGE unless a caller asks. A Call carries
// the SIZE of a result, never its content, and carries a call's arguments only under
// Options.Arguments — which core/sessionexport does not set, so an export's Call values hold no
// argument bytes at all, rather than holding them beside a field nobody reads. The timeline sets
// it, because its preview is built from them.
package toolpair

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// MainAgent and UnseparatedAgent are the two synthetic agent keys. A request that names no
// subagent is the main agent's; one that declares the subagent role without an id joins the
// shared bucket, because pre-#1459 traffic cannot be told apart.
//
// Spelled here rather than imported from core/session/timeline, which is this package's
// consumer: the dependency runs the other way. The timeline re-exports its own copies.
const (
	MainAgent        = "main"
	UnseparatedAgent = "subagents"
)

// The two ways a background subagent's result reaches the agent that started it, and the two
// markers a background shell job leaves. Each names its subject by the id Claude Code sends as
// X-Claude-Code-Agent-Id, which the proxy records as SessionEvent.Subagent, so a return links
// to its row without any text matching.
var (
	agentMessageRe = regexp.MustCompile(`<agent-message from="([A-Za-z0-9_-]+)"`)
	taskIDRe       = regexp.MustCompile(`<task-notification>\s*<task-id>([A-Za-z0-9_-]+)</task-id>`)
	// backgroundRe is Claude Code's result for a shell command it started in the background;
	// the command's <task-notification> later names the same id.
	backgroundRe = regexp.MustCompile(`^Command running in background with ID: ([A-Za-z0-9_-]+)`)
	// stoppedRe is TaskStop's result. A stopped job gets no <task-notification>, so without it
	// the job stays open and covers every later wait of its agent.
	stoppedRe = regexp.MustCompile(`^\{"message":"Successfully stopped (?:task: |background command )([A-Za-z0-9_-]+)`)
)

// EndReason says how a tool call ended. Its ZERO VALUE MEANS STILL OPEN, which is a different
// fact from ReasonLost: a call the agent is still waiting on has no duration yet, while a lost
// one never will. core/session/timeline draws the first as a tick and the second as a
// bar, and its fixture test asserts one open call and eight lost ones on a real session.
//
// Only ReasonResult and ReasonBackground have an end. For the other three a consumer has a
// start and no duration — see Call.End.
type EndReason string

const (
	// ReasonResult is the ordinary case: a later request carried the call's result.
	ReasonResult EndReason = "result"
	// ReasonBackground is a result that only ACKNOWLEDGED a job that keeps running. The call's
	// end is the acknowledgement, so its duration is real but it is not the command's: that
	// time is in a Job and is not counted here. A consumer totalling tool time undercounts by
	// however long such commands ran, which is why this is a reason of its own rather than
	// folded into ReasonResult.
	ReasonBackground EndReason = "background"
	// ReasonEndedRun is a call that ends its run by design and therefore never gets a result: a
	// workflow agent's StructuredOutput, a subagent's SubagentHandback, a Bob subtask's
	// end_subtask. Every other call of the same response ends this way too.
	ReasonEndedRun EndReason = "ended_run"
	// ReasonStopped is a call still open when its agent's run was stopped.
	ReasonStopped EndReason = "stopped"
	// ReasonAbandoned is a call whose agent asked again without ever carrying its result.
	ReasonAbandoned EndReason = "abandoned"
	// ReasonLost is open with no reason established. Reserved: Calls does not currently return
	// it, because every no-result case it can see has a more specific reason. A consumer should
	// treat it as "no result, cause unknown" rather than assuming it never appears.
	ReasonLost EndReason = "lost"
)

// HasEnd says whether r is a reason that carries a duration. The five no-result reasons leave
// Call.End nil, and a consumer that renders a missing end as a zero-length call reports it as
// an instant one — which drags a p50 toward zero.
func (r EndReason) HasEnd() bool { return r == ReasonResult || r == ReasonBackground }

// Call is one tool call the model asked for, with when it started, how it ended, and the keys
// that place it among its siblings and against the events either side.
type Call struct {
	// Agent is which caller under one session id made the call: MainAgent, UnseparatedAgent, or
	// a subagent's own id. An opaque key, not a label — every map in this package is keyed by
	// it, and a result only closes a call of the SAME agent.
	Agent string

	// ToolUseID is the model's id for the call, and the key its result is matched by.
	//
	// INTERNAL TO A PAIRING. core/sessionexport must not export it: it joins a record back to a
	// conversation, which is the one thing a content-free export must not offer. The timeline
	// does carry it, because agentop addresses a span by it.
	ToolUseID string

	// Name is the tool's name, verbatim as the model emitted it.
	Name string

	// Start is when the call began: the end of the response that asked for it, so EVERY call has
	// one, including a call that never finished. Derived as the asking request's At plus the
	// response's measured Duration — never from the response event's own timestamp, which is
	// when the proxy recorded it.
	Start time.Time

	// End is when the result arrived, and is nil for every reason that has no end. Clamped up to
	// Start when a result lands first: Claude Code runs a tool while the response that asked for
	// it is still streaming, so that request can be recorded before the response finished, and
	// the honest answer is a zero-length call rather than a negative one.
	End *time.Time

	EndReason EndReason

	// IsError is whether the result said it was an error. False when there was no result.
	IsError bool

	// ResultBytes is the size of the result's content. A SIZE, NEVER THE CONTENT: the effort a
	// call returned is a number an export may carry, and the text is not.
	ResultBytes int

	// CallSeq is the seq of the response that asked for this call; ResultSeq the seq of the
	// request that carried its result, and 0 when none did. Together they let a consumer rejoin
	// a call to the events either side of it WITHOUT the tool-use id.
	CallSeq   uint64
	ResultSeq uint64

	// CallIndex is the call's position among the calls its response asked for, from 0, in the
	// order the model emitted them; SiblingN is how many that response asked for in total.
	// SiblingN is 1 for a lone call, so SiblingN > 1 is the batch predicate.
	//
	// Both exist because a batch is otherwise unorderable and uncountable: its calls share a
	// Start by construction (see Calls), so timestamps cannot put them back in order.
	CallIndex int
	SiblingN  int

	// AskedAt is when the asking REQUEST started, and ModelDur is how long its response took.
	// Carried so a consumer gets the model time beside the tool time without a second lookup,
	// and because the stopped comparison needs the request's own start rather than the call's.
	AskedAt  time.Time
	ModelDur time.Duration

	// Arguments is the call's arguments as the model emitted them, and is EMPTY UNLESS THE
	// CALLER SET Options.Arguments. It is argument text — a Bash command line is effectively a
	// secret — so a consumer that must not hold it does not ask for it, and then cannot leak it
	// through a log line or a reflective marshal either.
	Arguments string
}

// Job is a background shell command: from the tool result that acknowledged it to the
// <task-notification> naming that id, or to the TaskStop result that stopped it. Its starting
// call ends at the acknowledgement, so without this the agent looks idle while the job runs.
type Job struct {
	Agent     string
	ID        string
	ToolUseID string // the call whose result started the job
	Start     time.Time
	End       *time.Time
	Seq       uint64
}

// Return is a subagent's result reaching another agent's context.
type Return struct {
	Agent string
	Into  string
	At    time.Time
	Seq   uint64
}

// Options asks for the outputs a caller actually wants. The message scan runs either way —
// two closing cases depend on it — so these skip the bookkeeping and the allocations, not the
// work. core/sessionexport passes the zero value.
type Options struct {
	// Returns fills Result.Returns.
	Returns bool
	// Arguments fills Call.Arguments. See that field: off means an export's Call values hold no
	// argument bytes at all.
	Arguments bool
	// AgentResults fills Result.AgentResults.
	AgentResults bool
}

// Result is what one walk produced.
type Result struct {
	// Calls is every tool call, in the order its asking response appeared, then by CallIndex.
	Calls []Call
	// Turns is every accepted request beside its response, in request order. See Turn.
	Turns []Turn
	// Jobs is every background shell command the scan saw acknowledged.
	Jobs []Job
	// Returns is every subagent result delivered into another agent's context. Empty unless
	// Options.Returns.
	Returns []Return
	// AgentResults maps an Agent-or-Task call's tool-use id to its result text, for a caller
	// that links a spawned subagent to the call that started it. Empty unless
	// Options.AgentResults. CONTENT: only the timeline asks for this.
	AgentResults map[string]string
	// ScannedBytes is how much message text the scan read, for the budget test that guards the
	// incremental skip.
	ScannedBytes int
}

// Turn is one accepted inference request beside its response, as the pairing resolved them.
//
// RETURNED SO A CONSUMER DOES NOT WALK AGAIN. The timeline needs the same request/response
// pairs for its own turns and its agent tree, and pairing is not free: it decides each
// request's agent, drops the agent's own housekeeping calls, and resolves a reused request id.
// Doing that twice over one session measured ~30% of a whole timeline build.
type Turn struct {
	Req, Resp *model.SessionEvent
	Agent     string
}

// call is one inference request and, once seen, its response.
type call struct {
	req, resp *model.SessionEvent
	agent     string
}

// Calls walks events and returns every tool call paired with its result.
//
// events must be one session's, in seq order. The walk buffers nothing beyond its own maps: it
// is one forward pass, then one closing pass over the calls it collected.
func Calls(events []model.SessionEvent, opts Options) Result {
	var res Result
	calls := pairCalls(events)
	if len(calls) == 0 {
		return res
	}
	// PREALLOCATED, because a Call is ~192 bytes and a real session has thousands: letting the
	// slice double copies megabytes for nothing, and this walk is on the timeline's 50ms budget
	// (core/session/timeline's TestBuildBudget).
	n := 0
	for _, c := range calls {
		if c.resp != nil && c.resp.Inference != nil {
			n += len(c.resp.Inference.ToolCalls)
		}
	}
	if n > 0 {
		res.Calls = make([]Call, 0, n)
	}

	// prev is the conversation the agent sent last time, for the incremental scan.
	prev := map[string][]model.InferenceMessage{}
	jobs := map[string]int{} // background job id → index in res.Jobs
	// ends is agent → tool-use id → the first result that closed it. One map rather than the
	// timeline's two parallel ones, because an export needs the carrying request's SEQ as well
	// as its time and parallel maps are how those drift apart.
	ends := map[string]map[string]toolEnd{}
	// bgAck marks a tool-use id whose result only acknowledged a background job.
	bgAck := map[string]bool{}
	agentTools := map[string]bool{}
	seenReturn := map[string]bool{}
	stopped := map[string]time.Time{} // agent → its latest <task-notification>: its run ended
	// A run that ends with SubagentHandback returns twice: its <agent-message> at once, and a
	// <task-notification> that Claude Code sends later, often with the parent's next request.
	handedBack, delivered := map[string]bool{}, map[string]bool{}
	lastAsk := map[string]time.Time{} // agent → its latest request's At
	if opts.AgentResults {
		res.AgentResults = map[string]string{}
	}

	for _, c := range calls {
		inf := c.req.Inference
		delete(handedBack, c.agent)
		delete(delivered, c.agent)

		// Tool ends: the first request in this agent that carries a tool's result.
		for i := range inf.ToolResults {
			r := &inf.ToolResults[i]
			if ends[c.agent] == nil {
				ends[c.agent] = map[string]toolEnd{}
			}
			if _, seen := ends[c.agent][r.ToolUseID]; seen {
				continue
			}
			ends[c.agent][r.ToolUseID] = toolEnd{at: c.req.At, seq: c.req.Seq, isError: r.IsError, bytes: len(r.Content)}
			if opts.AgentResults && agentTools[r.ToolUseID] {
				res.AgentResults[r.ToolUseID] = r.Content
			}
			if m := backgroundRe.FindStringSubmatch(r.Content); m != nil {
				bgAck[r.ToolUseID] = true
				if _, dup := jobs[m[1]]; !dup {
					jobs[m[1]] = len(res.Jobs)
					res.Jobs = append(res.Jobs, Job{Agent: c.agent, ID: m[1], ToolUseID: r.ToolUseID, Start: c.req.At, Seq: c.req.Seq})
				}
			}
			if m := stoppedRe.FindStringSubmatch(r.Content); m != nil {
				if j, ok := jobs[m[1]]; ok && res.Jobs[j].End == nil {
					at := c.req.At
					res.Jobs[j].End = &at
				}
			}
		}

		// Returns and run-ended markers: scan only the messages this agent has not sent before.
		// A conversation re-sends its whole history every turn, so scanning all of it is
		// quadratic in turns.
		//
		// Not for pre-#1459 subagent traffic: every subagent shares that one bucket, so each
		// request's history differs from the last one's and every scan would be a full one.
		msgs := inf.Messages
		k := CommonPrefix(prev[c.agent], msgs)
		if c.agent == UnseparatedAgent {
			k = len(msgs)
		}
		for mi, m := range msgs[k:] {
			if m.Role == "assistant" {
				continue // an agent quoting the markers is not a delivery
			}
			res.ScannedBytes += len(m.Content)
			for _, re := range []*regexp.Regexp{agentMessageRe, taskIDRe} {
				for _, idx := range re.FindAllStringSubmatchIndex(m.Content, -1) {
					from := m.Content[idx[2]:idx[3]]
					tail := m.Content[idx[0]:min(len(m.Content), idx[1]+200)]
					// Keyed by the message's position in the history as well as its text: a
					// fork re-sends its parent's deliveries at the same positions, while a
					// second, identically worded hand-back lands at a new one.
					key := fmt.Sprintf("%s\x00%d\x00%s", from, k+mi, tail)
					if seenReturn[key] {
						continue // a fork inherits its parent's history, deliveries included
					}
					seenReturn[key] = true
					// The notification after a delivered hand-back is not a return: it came a
					// median 22s and up to hours later, and riding on the user's next prompt it
					// labelled the user's time as waiting on the subagent.
					if re == agentMessageRe && handedBack[from] {
						delivered[from] = true
					}
					if opts.Returns && (re == agentMessageRe || !delivered[from]) {
						res.Returns = append(res.Returns, Return{Agent: from, Into: c.agent, At: c.req.At, Seq: c.req.Seq})
					}
					// Only a notification says the run ended: an <agent-message> can arrive mid-run.
					if re == taskIDRe && c.req.At.After(stopped[from]) {
						stopped[from] = c.req.At
					}
					// A background job ends at the first notification naming it.
					if j, ok := jobs[from]; ok && res.Jobs[j].End == nil {
						at := c.req.At
						res.Jobs[j].End = &at
					}
				}
			}
		}
		prev[c.agent] = msgs
		if lastAsk[c.agent].Before(c.req.At) {
			lastAsk[c.agent] = c.req.At
		}

		if c.resp == nil || c.resp.Inference == nil {
			continue
		}
		end := ResponseEnd(c.req, c.resp)
		tcs := c.resp.Inference.ToolCalls
		for i, tc := range tcs {
			k := Call{
				Agent:     c.agent,
				ToolUseID: tc.ID,
				Name:      tc.Name,
				Start:     end,
				CallSeq:   c.resp.Seq,
				CallIndex: i,
				SiblingN:  len(tcs),
				AskedAt:   c.req.At,
				ModelDur:  c.resp.Duration,
			}
			if opts.Arguments {
				k.Arguments = tc.Arguments
			}
			res.Calls = append(res.Calls, k)
			if tc.Name == "SubagentHandback" {
				handedBack[c.agent] = true
			}
			if tc.Name == "Agent" || tc.Name == "Task" {
				agentTools[tc.ID] = true
			}
		}
	}

	res.Turns = make([]Turn, len(calls))
	for i, c := range calls {
		res.Turns[i] = Turn{Req: c.req, Resp: c.resp, Agent: c.agent}
	}
	closeCalls(res.Calls, ends, bgAck, stopped, lastAsk)
	return res
}

// toolEnd is the first result that closed a call: when it arrived, which request carried it,
// and the two facts a consumer may keep about it. NOT ITS CONTENT — the size is the effort
// proxy an export may carry, and the text never leaves this package.
type toolEnd struct {
	at      time.Time
	seq     uint64
	isError bool
	bytes   int
}

// endsRun are the tool calls that end their run by design, so neither they nor any sibling of
// the same response ever gets a result.
var endsRun = map[string]bool{"SubagentHandback": true, "StructuredOutput": true, "end_subtask": true}

// closeCalls decides each call's end and reason, in place.
//
// PRECEDENCE, AND ONE DECISION THE TIMELINE LEFT UNSPECIFIED. A result wins over every
// no-result reason. Among the no-result reasons, ended_run wins (it is a property of the call
// itself), then stopped, then abandoned. The timeline tests all three in one disjunction
// (`endedRun || lastTurn || stopped`), so a call that satisfies both stopped and abandoned had
// no defined answer before this package named one: stopped is the more specific fact — a run
// was ended by something — where abandoned only says the agent asked again. The choice changes
// no timeline behaviour, because both map to the same Lost bool.
func closeCalls(
	calls []Call,
	ends map[string]map[string]toolEnd,
	bgAck map[string]bool,
	stopped map[string]time.Time,
	lastAsk map[string]time.Time,
) {
	// A response whose calls end the run gives its OTHER calls no result either, and no
	// notification arrives to close them. Keyed by the response's seq, which a response's calls
	// share.
	endedRun := map[uint64]bool{}
	for i := range calls {
		k := &calls[i]
		if _, ok := ends[k.Agent][k.ToolUseID]; !ok && endsRun[k.Name] {
			endedRun[k.CallSeq] = true
		}
	}
	for i := range calls {
		k := &calls[i]
		if e, ok := ends[k.Agent][k.ToolUseID]; ok {
			at := e.at
			if at.Before(k.Start) {
				at = k.Start
			}
			k.End = &at
			k.IsError, k.ResultBytes, k.ResultSeq = e.isError, e.bytes, e.seq
			if bgAck[k.ToolUseID] {
				k.EndReason = ReasonBackground
			} else {
				k.EndReason = ReasonResult
			}
			continue
		}
		switch {
		case endedRun[k.CallSeq]:
			k.EndReason = ReasonEndedRun
		case stopped[k.Agent].After(k.AskedAt):
			// The run ended while the call was open. Compared against the asking REQUEST's
			// start, not the call's: a run can end while the asking response still streams, and
			// a resumed run's request starts after the earlier notification.
			k.EndReason = ReasonStopped
		case lastAsk[k.Agent].After(k.Start):
			k.EndReason = ReasonAbandoned
		}
		// Anything else is still open: the zero EndReason, with no End.
	}
}

// pairCalls walks events once and returns each accepted inference request beside its response.
//
// FIRST RESPONSE WINS. A client that reuses a request id loses the earlier request's response
// rather than overwriting the later one.
func pairCalls(events []model.SessionEvent) []call {
	var calls []call
	byReq := map[string]int{}
	for i := range events {
		e := &events[i]
		if e.Direction != model.Outbound {
			continue
		}
		switch e.Phase {
		case model.SessionRequest:
			if e.Inference == nil {
				continue
			}
			id, ok := agentOf(e)
			// A side call carries the agent's whole context to ask for a status line or an
			// away recap, so it is not the agent working. An imported transcript has no such
			// calls to tell apart.
			if !ok || (!e.Imported() && isSideCall(e.Inference)) {
				continue
			}
			byReq[e.RequestID] = len(calls)
			calls = append(calls, call{req: e, agent: id})
		case model.SessionResponse, model.SessionDenied:
			if j, ok := byReq[e.RequestID]; ok && calls[j].resp == nil {
				calls[j].resp = e
			}
		}
	}
	return calls
}

// CommonPrefix is how many leading messages cur shares with prev. The store interns repeated
// message content, so equal strings usually share a backing array and compare in O(1).
func CommonPrefix(prev, cur []model.InferenceMessage) int {
	k := 0
	for k < len(prev) && k < len(cur) && prev[k].Role == cur[k].Role && prev[k].Content == cur[k].Content {
		k++
	}
	return k
}

// ResponseEnd is when a response finished: its request's start plus the measured duration,
// falling back to the response event's own timestamp when nothing measured it.
func ResponseEnd(req, resp *model.SessionEvent) time.Time {
	if resp.Duration > 0 {
		return req.At.Add(resp.Duration)
	}
	return resp.At
}

// agentOf is which agent a request belongs to, and whether it counts as agent traffic at all.
func agentOf(e *model.SessionEvent) (string, bool) {
	if e.Subagent != "" {
		return e.Subagent, true
	}
	if e.Inference != nil {
		switch e.Inference.AgentRole {
		case model.AgentRoleSubagent:
			return UnseparatedAgent, true
		case model.AgentRoleMain:
			return MainAgent, true
		}
	}
	if e.Client != nil && model.IsKnownAgent(e.Client.Name) {
		return MainAgent, true
	}
	return "", false
}

// progressPrompt is the instruction Claude Code appends when it asks a model for a subagent's
// 3-5 word status line. Those calls carry the subagent's whole context and tool list, so only
// their last message tells them apart from the subagent's real turns; Claude Code can send
// system messages after it (a <total_tokens> count). They are a third of all subagent calls on
// real sessions.
const progressPrompt = "Describe your most recent action in 3-5 words"

// recapPrompt asks for the summary Claude Code shows when the user comes back to an idle
// session. The same shape as a progress label: whole context and tool list, told apart only by
// its last message. Counted as a turn it was the agent working while the user was away, and two
// recaps of an unchanged conversation read as a retry: 74 hours of it on one session.
const recapPrompt = "The user stepped away and is coming back"

// isSideCall drops a call that is not an agent's work. The no-tools rule is Claude Code only
// (AgentRole set): it holds because Claude always sends a manifest on real turns, while another
// client's tool-less calls (Bob's security classifier) are real turns with real cost. The prompt
// literals run for every client: they match Claude's own progress and recap prompt text, which
// no other client sends, and a recap storm reads as turns on any session that carries one —
// including pre-marker Claude Code traffic, attributed by client name alone.
//
// SKIPS TRAILING SYSTEM MESSAGES, NOT NON-USER ONES: Claude Code appends a system message after
// the prompt, and the message the literals must be tested against is the last non-system one.
func isSideCall(inf *model.InferenceExtension) bool {
	if inf.AgentRole != "" && len(inf.Tools) == 0 && inf.ToolCount == 0 {
		return true
	}
	for i := len(inf.Messages) - 1; i >= 0; i-- {
		if m := inf.Messages[i]; m.Role != "system" {
			return strings.Contains(m.Content, progressPrompt) || strings.Contains(m.Content, recapPrompt)
		}
	}
	return false
}
