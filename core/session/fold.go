package session

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
)

// applyTitle is the title rule: FIRST-WINS, EXCEPT THAT A /rename ALWAYS OVERRIDES. It returns
// the rank and title a session holds after a candidate of candRank and candText.
//
// Shared by appendLocked and SummaryFold, so that a resident session and an archived one can
// never be titled by two rules.
//
// THE SECOND DISJUNCT IS THE WHOLE RULE, not a tie-break detail: without it a re-rename is
// silently ignored, because the equal rank never beats the one already held. Widening it to a
// plain `<=` instead is the opposite failure — the title then shifts on every turn, since each
// new prose message equals the rank of the last.
//
// THE BLANK SCREEN IS REDUNDANT AND NO TEST CAN SHOW IT, which is worth saying rather than
// leaving a surviving mutant for the next reader to re-derive. titleCandidate returns "" only
// ever paired with rankNone (every blank-folding shape — empty, whitespace, reminder-only, an
// empty <user_query> — comes back as rankNone), and a fresh fold initialises to rankNone, so
// `rankNone < rankNone` is false and the rank test alone rejects it. Kept as a local statement
// of what the fold requires: a future titleCandidate returning a blank at a real rank fails
// here instead of storing one.
func applyTitle(rank int, title string, candRank int, candText string) (int, string) {
	if candText != "" && (candRank < rank || (candRank == rank && candRank == rankRename)) {
		// sanitizeTitle IS SAFE UNDER THE STORE'S LOCK, where appendLocked calls this, because it
		// is bounded rather than O(candidate): the secret rules see titleRawBytes of it and the
		// fold stops at titleWindow emitted runes, so a 190KB candidate costs what a 1.3KB one
		// does. (Before the secret rules it stopped at 80 runes: 328ns against 916µs unbounded.)
		//
		// A CALL-COUNTING ARGUMENT IS NOT ENOUGH HERE, which is why the cap and not the count is
		// what this rests on. "At most once per rank improvement" bounds the fold at three times
		// per session under first-wins — except for /rename, which the second disjunct lets win
		// repeatedly and which the client controls. Flooding /rename with a 190KB argument paid
		// ~938µs of write-lock hold per append, unbounded in repetitions; measured on that flood
		// with a concurrent reader, mean ListSessions latency is 205µs uncapped against 16.4µs
		// capped.
		//
		// Hoisting it above the lock would still be wrong, just cheaply so: it would fold every
		// event's candidate including the ones about to be discarded on rank.
		if t := sanitizeTitle(candText); t != "" {
			return candRank, t
		}
	}
	return rank, title
}

// applyAgent records the first label a client sent into a session, one entry per client. The
// default and pending buckets name no agent: they collect traffic for no one session. Shared by
// appendLocked and SummaryFold for applyTitle's reason.
//
// RECOGNISED OR NOT — #1214. An agent the proxy does not recognise is keyed by its own label,
// because an AffinityName is the one thing it has not got; agentOfLocked then reports that label
// only when no recognised agent in the session can answer, so a session that already named an
// agent keeps naming it. Before this, such a session carried no agents entry at all and served
// no `agent` field, so nothing in /v1/sessions said which program it belonged to and no
// `agent=` query could reach it.
//
// A CLIENT THAT CAN NAME NOTHING STILL RECORDS NOTHING, which is what keeps `agent` omitempty
// honest: event.AgentLabel answers model.UnknownClientLabel ("unknown") for an event that
// carried no User-Agent at all, and that is a RESERVED BUCKET rather than an agent name — see
// EventClient.Label. Putting it in this slice would render untagged traffic as a program that
// spent money, on exactly the field a client reads to attribute a session.
//
// THE LABEL IS CLIENT-ASSERTED AND SPOOFABLE, and it reaches this slice already bounded:
// event.AgentLabel folds the version away, sanitizes every control, bidi and zero-width rune
// and caps at event.MaxLabelLen (96) bytes on a rune boundary — the same
// treatment the usage ring's byAgent keys and the ledger's rows get, which is what makes a
// session's `agent` and its `group=agent` row the same string. Nothing new is invented here, and
// nothing here may be read as a trusted identity; see EventClient's godoc.
func applyAgent(agents []sessionAgent, sessionID, agentName string, c *model.EventClient) []sessionAgent {
	if sessionID == DefaultSessionID || strings.HasPrefix(sessionID, PendingPrefix) {
		return agents
	}
	// THE DEDUP RUNS BEFORE THE LABEL IS COMPUTED, on the one path where it can. This is
	// the per-event append path under the store's write lock, and event.AgentLabel is
	// a regexp match, a whole-string rune scan and a cap.
	// In the steady state for every recognised agent the slice already holds its entry, so
	// computing the label first paid all of that on every event only to discard it at
	// agentLabelIn: measured 385ns/op and 2 allocs against 8.3ns/op and 0 this way.
	//
	// The unrecognised path cannot avoid it — the label IS its key, so there is nothing to
	// dedup on until it exists. That path is bounded by construction at one entry per
	// distinct unrecognised client per session, and it still dedups below.
	known := agentName != ""
	if known {
		if agentLabelIn(agents, agentName) != "" {
			return agents
		}
		return append(agents, sessionAgent{name: agentName, label: event.AgentLabel(c), known: true})
	}
	// Keyed by the label, since there is no canonical name. "unknown" means the event
	// carried no User-Agent — nothing to attribute, so nothing recorded.
	label := event.AgentLabel(c)
	if label == "" || label == model.UnknownClientLabel {
		return agents
	}
	if agentLabelIn(agents, label) != "" {
		return agents
	}
	return append(agents, sessionAgent{name: label, label: label, known: false})
}

// maxSessionProjects caps SessionSummary.Projects.
const maxSessionProjects = 8

// applyProject puts label at the front of projects, the distinct project labels of a session's
// events, most recent first and at most maxSessionProjects. "" and the default and pending
// buckets change nothing: those buckets collect traffic for no one session. Shared by
// appendLocked and SummaryFold for applyTitle's reason.
func applyProject(projects []string, sessionID, label string) []string {
	if label == "" || sessionID == DefaultSessionID || strings.HasPrefix(sessionID, PendingPrefix) {
		return projects
	}
	// The steady state: one string compare under the store's lock.
	if len(projects) > 0 && projects[0] == label {
		return projects
	}
	if i := slices.Index(projects, label); i >= 0 {
		projects = slices.Delete(projects, i, i+1)
	}
	projects = slices.Insert(projects, 0, label)
	if len(projects) > maxSessionProjects {
		projects = projects[:maxSessionProjects]
	}
	return projects
}

// applyParent is SessionSummary.ParentSession after one more event: the first non-empty parent.
// The default and pending buckets keep none, for applyProject's reason. Shared by appendLocked,
// the rekey and SummaryFold.
func applyParent(cur, sessionID, parent string) string {
	if cur != "" || parent == "" || sessionID == DefaultSessionID || strings.HasPrefix(sessionID, PendingPrefix) {
		return cur
	}
	return parent
}

// agentLabelIn is the first label the agent named name sent, or "".
func agentLabelIn(agents []sessionAgent, name string) string {
	for _, a := range agents {
		if a.name == name {
			return a.label
		}
	}
	return ""
}

// firstAgentLabel is the first label in earlier then in agents whose entry's known matches, or
// "". Two slices in one call because the history before this entry is searched first wherever a
// session's agent is picked, and the recognised and unrecognised passes must each run over both
// in that order — searching one slice at a time would prefer this entry's recognised agent to the
// history's, which is the wrong way round for a first-wins field.
func firstAgentLabel(earlier, agents []sessionAgent, known bool) string {
	for _, as := range [2][]sessionAgent{earlier, agents} {
		for _, a := range as {
			if a.known == known {
				return a.label
			}
		}
	}
	return ""
}

// inferenceHostOf is the host e names for SessionSummary.InferenceHost, or "" when e does not
// move it: only a request that carried an inference parse does. Shared by appendLocked and
// SummaryFold for applyTitle's reason.
//
// "" FOR EVERYTHING ELSE, AND THE CALLERS KEEP WHAT THEY HAD. A session's events are not all
// turns: a bridged CONNECT's tunnel-open lands between an inference request and its response, and
// MCP calls and responses interleave too. Clearing the field on any of them would blank a
// session's server on most of its rows — the intern table's lesson (CLAUDE.md, the session store gotcha).
func inferenceHostOf(e *model.SessionEvent) string {
	if e.Phase != model.SessionRequest || e.Inference == nil {
		return ""
	}
	return e.Host
}

// hostEvidence is what one event tells the session's InferenceHosts table. hostEvidenceOf
// takes it from the event alone, in prepareAppend, above the store's lock: TurnOf scans the
// event's messages.
type hostEvidence struct {
	ok       bool   // the event is evidence: an outbound inference request that is no aside turn
	agent    string // model.AgentName(e.Client.Label()), as the router's serverOf compares it
	unrouted bool   // CapturePath takes the request and it has no RequestedHost: the session ran unrouted
	server   string // model.RoutedServer of the request: "" when the router did not route it
}

// hostEvidenceOf is the inference-router's serverOf without its config: the half of the rule
// that does not need the router's servers. The router applies its servers when it reads the
// table (model.InferenceHosts.LastServer), because a hot reload can change them.
func hostEvidenceOf(e *model.SessionEvent) hostEvidence {
	if !model.IsInferenceRequest(e) || TurnOf(e.Inference) == TurnAside {
		return hostEvidence{}
	}
	_, captured := CapturePath(e.HTTPMethod, e.HTTPPath)
	return hostEvidence{ok: true, agent: model.AgentName(e.Client.Label()), unrouted: captured && e.RequestedHost == "",
		server: model.RoutedServer(e.Invocations)}
}

// applyHost folds the evidence of the event with this host and seq into h, and returns h. It
// changes h's elements in place. Every fold of an InferenceHosts table calls it, for
// applyTitle's reason: a table in memory and a table in the archive must use one rule.
func applyHost(h model.InferenceHosts, ev hostEvidence, host string, seq uint64) model.InferenceHosts {
	if !ev.ok {
		return h
	}
	for i := range h {
		if h[i].Agent == ev.agent && h[i].Host == host && h[i].Server == ev.server {
			h[i].Last = max(h[i].Last, seq)
			if ev.unrouted {
				h[i].LastUnrouted = max(h[i].LastUnrouted, seq)
			}
			return h
		}
	}
	s := model.InferenceHostSeen{Agent: ev.agent, Host: host, Server: ev.server, Last: seq}
	if ev.unrouted {
		s.LastUnrouted = seq
	}
	return append(h, s)
}

// mergeHost folds the entry x into h by the highest seq of each field, and returns h. It changes
// h's elements in place. Two tables of one session merge this way: each seq is one event, in one
// table only, so the merge keeps each seq in one entry, as LastServer requires.
func mergeHost(h model.InferenceHosts, x model.InferenceHostSeen) model.InferenceHosts {
	for i := range h {
		if h[i].Agent == x.Agent && h[i].Host == x.Host && h[i].Server == x.Server {
			h[i].Last, h[i].LastUnrouted = max(h[i].Last, x.Last), max(h[i].LastUnrouted, x.LastUnrouted)
			return h
		}
	}
	return append(h, x)
}

// hostRuleVersion is the version of the InferenceHosts rule (hostEvidenceOf and applyHost) in a
// persisted table. A table of another version decodes as unknown. Bump it when the rule changes:
// TestHostRule_Golden fails until you do.
const hostRuleVersion = 2

// SummaryFold is ListSessions' per-session figures folded one event at a time and never shed:
// the session archive's running summary for a session the store may no longer hold.
//
// It uses the store's own rules — moneyOf through prepareAppend, sumTokens' response-only
// token rule, applyTitle, applyAgent, and entry.context's CTX fold — so a fold over an
// untrimmed session's events reports what ListSessions does. TestSummaryFold_AgreesWithListSessions
// holds that.
//
// ONE DIFFERENCE IS INHERENT: Agent is the first recognised agent's label (else the first
// unrecognised client's), while the store prefers the label of the agent that CLAIMED the
// session, which only the store knows. A resident session's row therefore always comes from the
// store, which adds the archive's fold of the history before its entry (PriorKeeper).
//
// The zero value is not ready — rankRename is 0, so a zero titleRank would claim the session
// had been renamed. Use NewSummaryFold. Not safe for concurrent use.
type SummaryFold struct {
	events    int
	tokens    int
	cost      event.CostSum
	avoided   figureSum
	units     map[string]int
	titleRank int
	title     string
	agents    []sessionAgent
	// projects is SessionSummary.Projects, by the store's rule (applyProject).
	projects []string
	// parentSession is SessionSummary.ParentSession (applyParent).
	parentSession string
	// context is the CTX figure folded so far, by the store's own rule (entry.context), and
	// restoredContext the figure a fold resumed from session.json started from. TWO FIELDS BECAUSE
	// model.PromptContextFold HAS NO RESTORE PATH: what persists is its published figure, and
	// since the published order is the fold's order, merging the restored figure with what was
	// folded since gives what one uninterrupted fold would have.
	context         model.PromptContextFold
	restoredContext *model.PromptContext
	// inferenceHost is SessionSummary.InferenceHost: the latest inference request's host.
	// inferenceServer is SessionSummary.InferenceServer and inferenceKeyOf is
	// SessionSummary.InferenceKeyOf. Both move with inferenceHost.
	inferenceHost   string
	inferenceServer string
	inferenceKeyOf  string
	// procs, owner and lastRequestAt are the entry's fields of the same names, by the same
	// rules (applyProcs, applyOwner).
	procs         map[string]ProcSpan
	owner         string
	lastRequestAt time.Time
	// hosts is the session's InferenceHosts table, by the store's rule (applyHost). hostsUnknown
	// is true when the fold resumed from history it has no table for.
	hosts        model.InferenceHosts
	hostsUnknown bool
	// imported is the first imported event's Source, and live is true after any live event
	// (Source ""). Summary marks the session as imported only while live is false.
	imported string
	live     bool
	// noRecord is true once any event asked not to be recorded (SessionEvent.NoRecord, #1923),
	// or once MarkNoRecord was called. Sticky: a later event without the flag does not clear it.
	noRecord bool
}

// MarkNoRecord marks the session as not recorded without folding an event, for the archive's
// content-free marker of an opted-out session (#1923).
func (f *SummaryFold) MarkNoRecord() { f.noRecord = true }

// NoRecord reports whether the session asked not to be recorded; see SummaryFold.noRecord.
func (f *SummaryFold) NoRecord() bool { return f.noRecord }

// InferenceHosts is a copy of the fold's table, and false when the fold holds history it has no
// table for.
func (f *SummaryFold) InferenceHosts() (model.InferenceHosts, bool) {
	return slices.Clone(f.hosts), !f.hostsUnknown
}

// ForgetInferenceHosts marks the fold's InferenceHosts table unknown, for a fold that holds
// events of more than one session entry: the archive moves such a fold to a new id when it
// cannot tell the entry's events from the history's. Unknown is sticky, as after a decode.
func (f *SummaryFold) ForgetInferenceHosts() { f.hosts, f.hostsUnknown = nil, true }

// NewSummaryFold returns an empty fold.
func NewSummaryFold() *SummaryFold { return &SummaryFold{titleRank: rankNone} }

// Clone returns a copy of f that later Adds to either do not reach.
func (f *SummaryFold) Clone() *SummaryFold {
	c := *f
	c.units = maps.Clone(f.units)
	c.agents = slices.Clone(f.agents)
	c.projects = slices.Clone(f.projects)
	c.procs = maps.Clone(f.procs)
	c.hosts = slices.Clone(f.hosts)
	return &c
}

// Add folds one event recorded under sessionID. It does the work appendLocked hoists above the
// store's lock — a plugin-map decode and a scan of message content — so call it from a
// goroutine of your own, never from Record.
func (f *SummaryFold) Add(sessionID string, e *model.SessionEvent) {
	p := prepareAppend(e)
	f.events++
	if e.Phase == model.SessionResponse && e.Inference != nil {
		f.tokens += e.Inference.TotalTokens
	}
	f.cost.Add(p.money.cost)
	f.avoided.Add(p.money.avoided)
	if p.money.priced || !p.money.avoided.IsZero() {
		if f.units == nil {
			f.units = make(map[string]int, 1)
		}
		f.units[p.money.unit]++
	}
	f.titleRank, f.title = applyTitle(f.titleRank, f.title, p.titleRank, p.titleText)
	f.agents = applyAgent(f.agents, sessionID, p.agentName, e.Client)
	f.projects = applyProject(f.projects, sessionID, e.RollupProject())
	f.parentSession = applyParent(f.parentSession, sessionID, e.ParentSession)
	f.context.Add(e)
	if h := inferenceHostOf(e); h != "" {
		f.inferenceHost, f.inferenceServer = h, model.RoutedServer(e.Invocations)
		f.inferenceKeyOf = model.RoutedKeyOf(e.Invocations)
	}
	f.procs = applyProcs(f.procs, sessionID, e)
	f.owner = applyOwner(f.owner, sessionID, p.agentName, e)
	if e.Phase == model.SessionRequest && e.At.After(f.lastRequestAt) {
		f.lastRequestAt = e.At
	}
	f.hosts = applyHost(f.hosts, p.host, e.Host, e.Seq)
	if e.Source == "" {
		f.live = true
	} else if f.imported == "" {
		f.imported = e.Source
	}
	if e.NoRecord {
		f.noRecord = true
	}
}

// promptContext is the fold's CTX figure, nil when nothing can be said.
func (f *SummaryFold) promptContext() *model.PromptContext {
	return model.MergePromptContext(f.restoredContext, f.context.Publish())
}

// Summary sets EventCount, Title, Agent, TotalTokens, CostMicros, AvoidedMicros, Currencies,
// Saturated, PromptContext and InferenceHost on a SessionSummary for sessionID, and nothing else:
// the timestamps and the live-only fields (Active, Adopted) are the caller's.
func (f *SummaryFold) Summary(sessionID string) SessionSummary {
	sum := SessionSummary{
		ID:              sessionID,
		EventCount:      f.events,
		Title:           f.title,
		TotalTokens:     f.tokens,
		CostMicros:      f.cost.Micros,
		Currencies:      currenciesOf(f.units),
		Saturated:       f.cost.Saturated || f.avoided.Saturated,
		PromptContext:   f.promptContext(),
		InferenceHost:   f.inferenceHost,
		InferenceServer: model.LenientString(f.inferenceServer),
		InferenceKeyOf:  model.LenientString(f.inferenceKeyOf),
		Procs:           procsList(f.procs),
		procSpans:       maps.Clone(f.procs),
		LastRequestAt:   f.lastRequestAt,
		Agent:           pickAgent(sessionID, f.owner, nil, f.agents),
		Projects:        slices.Clone(f.projects),
		NoRecord:        f.noRecord,
		ParentSession:   model.LenientString(f.parentSession),
	}
	f.avoided.setOn(&sum)
	if !f.live {
		sum.Source = f.imported
	}
	return sum
}

// foldJSON is SummaryFold's state as the session archive persists it in session.json, so a
// fold resumes after a restart instead of being rebuilt by decoding every segment.
//
// TitleRank IS A POINTER because rankRename is 0: a document without it must decode to
// rankNone, not to a rename that would lock the title against every later candidate.
type foldJSON struct {
	EventCount int         `json:"eventCount"`
	Title      string      `json:"title,omitempty"`
	TitleRank  *int        `json:"titleRank,omitempty"`
	Agents     []foldAgent `json:"agents,omitempty"`
	// Projects is absent from a session.json written before it existed, which decodes to none.
	Projects []string `json:"projects,omitempty"`
	// ParentSession is SummaryFold.parentSession. A session.json written before it existed decodes to "".
	ParentSession    model.LenientString `json:"parentSession,omitempty"`
	TotalTokens      int                 `json:"totalTokens,omitempty"`
	CostMicros       int64               `json:"costMicros,omitempty"`
	CostSaturated    bool                `json:"costSaturated,omitempty"`
	AvoidedMicros    int64               `json:"avoidedMicros,omitempty"`
	AvoidedSaturated bool                `json:"avoidedSaturated,omitempty"`
	// The other event.Figures fields. Absent from a session.json written before they existed,
	// which decodes to 0: that history's savings were counted only in AvoidedMicros.
	PenaltyMicros              int64                `json:"penaltyMicros,omitempty"`
	AvoidedUnits               int64                `json:"avoidedUnits,omitempty"`
	PenaltyUnits               int64                `json:"penaltyUnits,omitempty"`
	AvoidedAPIEquivalentMicros int64                `json:"avoidedApiEquivalentMicros,omitempty"`
	AvoidedUnknownMicros       int64                `json:"avoidedUnknownMicros,omitempty"`
	PluginSpendMicros          int64                `json:"pluginSpendMicros,omitempty"`
	PluginSpendUnits           int64                `json:"pluginSpendUnits,omitempty"`
	PluginSpendUnknownMicros   int64                `json:"pluginSpendUnknownMicros,omitempty"`
	InteractionMicros          int64                `json:"interactionMicros,omitempty"`
	Units                      map[string]int       `json:"units,omitempty"`
	PromptContext              *model.PromptContext `json:"promptContext,omitempty"`
	// InferenceHost is absent from a session.json written before it existed, which decodes to "".
	// THERE "" MEANS UNKNOWN, NOT "NO INFERENCE TRAFFIC": that history may hold plenty, folded by
	// a Cortex that did not record where it went. The session's next inference request sets it.
	InferenceHost string `json:"inferenceHost,omitempty"`
	// InferenceServer moves with InferenceHost. A session.json written before it existed decodes to "".
	InferenceServer model.LenientString `json:"inferenceServer,omitempty"`
	// InferenceKeyOf moves with InferenceHost. A session.json written before it existed decodes to "".
	InferenceKeyOf model.LenientString `json:"inferenceKeyOf,omitempty"`
	// Procs, Owner and LastRequestAt are absent from a session.json written before they
	// existed: that history names no process, links nothing, and its agent is picked by label.
	Procs         map[string]ProcSpan `json:"procs,omitempty"`
	Owner         string              `json:"owner,omitempty"`
	LastRequestAt time.Time           `json:"lastRequestAt,omitzero"`
	// InferenceHosts is written only when the fold knows its table. Absent (a session.json from
	// before it existed, or an unknown table), or of another hostRuleVersion, it decodes as unknown.
	InferenceHosts *hostsDoc `json:"inferenceHosts,omitempty"`
	// Imported and Live are SummaryFold's fields of the same names. A session.json written
	// before they existed holds no imported events, so it decodes as not imported.
	Imported string `json:"imported,omitempty"`
	Live     bool   `json:"live,omitempty"`
	// NoRecord is SummaryFold's field of the same name. Absent from every recorded session's
	// session.json, which decodes to recorded.
	NoRecord bool `json:"noRecord,omitempty"`
}

// hostsDoc is SummaryFold's InferenceHosts table in session.json. V is the hostRuleVersion that
// wrote it.
type hostsDoc struct {
	V     int                       `json:"v"`
	Hosts []model.InferenceHostSeen `json:"hosts,omitempty"`
}

// UnmarshalJSON decodes a table of a shape it cannot read as one of no version: unknown, so the
// session's other figures still load.
func (d *hostsDoc) UnmarshalJSON(b []byte) error {
	type plain hostsDoc
	if err := json.Unmarshal(b, (*plain)(d)); err != nil {
		*d = hostsDoc{}
	}
	return nil
}

// foldAgent is one sessionAgent in session.json.
//
// UNRECOGNISED IS THE ENCODED STATE, not recognised: a document written before #1214 holds only
// recognised agents, and absent must decode to what those were. `unknown: true` therefore marks
// the new case, so an old session.json resumes with its agents still preferred over any
// unrecognised client the session sees next — which is what the live store would do for the same
// history.
type foldAgent struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Unknown bool   `json:"unknown,omitempty"`
}

// MarshalJSON persists the fold's whole state, not just its Summary: resuming needs the title
// rank and each currency's count, which a summary does not carry.
func (f *SummaryFold) MarshalJSON() ([]byte, error) {
	j := foldJSON{
		EventCount:                 f.events,
		Title:                      f.title,
		TitleRank:                  &f.titleRank,
		TotalTokens:                f.tokens,
		CostMicros:                 f.cost.Micros,
		CostSaturated:              f.cost.Saturated,
		AvoidedMicros:              f.avoided.Figures.AvoidedMicros,
		AvoidedSaturated:           f.avoided.Saturated,
		PenaltyMicros:              f.avoided.Figures.PenaltyMicros,
		AvoidedUnits:               f.avoided.Figures.AvoidedUnits,
		PenaltyUnits:               f.avoided.Figures.PenaltyUnits,
		AvoidedAPIEquivalentMicros: f.avoided.Figures.AvoidedAPIEquivalentMicros,
		AvoidedUnknownMicros:       f.avoided.Figures.AvoidedUnknownMicros,
		PluginSpendMicros:          f.avoided.Figures.PluginSpendMicros,
		PluginSpendUnits:           f.avoided.Figures.PluginSpendUnits,
		PluginSpendUnknownMicros:   f.avoided.Figures.PluginSpendUnknownMicros,
		InteractionMicros:          f.avoided.Figures.InteractionMicros,
		Units:                      f.units,
		PromptContext:              f.promptContext(),
		InferenceHost:              f.inferenceHost,
		InferenceServer:            model.LenientString(f.inferenceServer),
		InferenceKeyOf:             model.LenientString(f.inferenceKeyOf),
		Procs:                      f.procs,
		Owner:                      f.owner,
		LastRequestAt:              f.lastRequestAt,
		Projects:                   f.projects,
		ParentSession:              model.LenientString(f.parentSession),
		Imported:                   f.imported,
		Live:                       f.live,
		NoRecord:                   f.noRecord,
	}
	if !f.hostsUnknown {
		j.InferenceHosts = &hostsDoc{V: hostRuleVersion, Hosts: f.hosts}
	}
	for _, a := range f.agents {
		j.Agents = append(j.Agents, foldAgent{Name: a.name, Label: a.label, Unknown: !a.known})
	}
	return json.Marshal(j)
}

// UnmarshalJSON restores a fold MarshalJSON wrote; a missing titleRank is rankNone.
func (f *SummaryFold) UnmarshalJSON(b []byte) error {
	var j foldJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*f = SummaryFold{
		events: j.EventCount,
		tokens: j.TotalTokens,
		cost:   event.CostSum{Micros: j.CostMicros, Saturated: j.CostSaturated},
		avoided: figureSum{Saturated: j.AvoidedSaturated, Figures: event.Figures{
			AvoidedMicros:              j.AvoidedMicros,
			PenaltyMicros:              j.PenaltyMicros,
			AvoidedUnits:               j.AvoidedUnits,
			PenaltyUnits:               j.PenaltyUnits,
			AvoidedAPIEquivalentMicros: j.AvoidedAPIEquivalentMicros,
			AvoidedUnknownMicros:       j.AvoidedUnknownMicros,
			PluginSpendMicros:          j.PluginSpendMicros,
			PluginSpendUnits:           j.PluginSpendUnits,
			PluginSpendUnknownMicros:   j.PluginSpendUnknownMicros,
			InteractionMicros:          j.InteractionMicros,
		}},
		units:           j.Units,
		titleRank:       rankNone,
		title:           redactStoredTitle(j.Title), // written before the secret rules applied, maybe
		restoredContext: j.PromptContext,
		inferenceHost:   j.InferenceHost,
		inferenceServer: string(j.InferenceServer),
		inferenceKeyOf:  string(j.InferenceKeyOf),
		procs:           j.Procs,
		owner:           model.CanonicalAgent(j.Owner),
		lastRequestAt:   j.LastRequestAt,
		projects:        j.Projects,
		parentSession:   string(j.ParentSession),
		imported:        j.Imported,
		// A fold with events and no imported one holds only live events, also in a
		// session.json written before Live existed.
		live:         j.Live || (j.EventCount > 0 && j.Imported == ""),
		hostsUnknown: j.InferenceHosts == nil || j.InferenceHosts.V != hostRuleVersion,
		noRecord:     j.NoRecord,
	}
	if j.TitleRank != nil {
		f.titleRank = *j.TitleRank
	}
	// Agent names in their canonical spelling, for the reason given for f.agents below. Two
	// spellings of one agent become one entry.
	if !f.hostsUnknown {
		for _, x := range j.InferenceHosts.Hosts {
			x.Agent = model.CanonicalAgent(x.Agent)
			f.hosts = mergeHost(f.hosts, x)
		}
	}
	// THE PERSISTED NAMES ARE FOLDED TO THEIR CANONICAL SPELLING HERE, at the one place a
	// session.json's labels re-enter memory, because every reader of a restored fold reads
	// f.agents: Summary and withPriorLocked's agentOfLocked serve it as the session's agent,
	// Add's dedup keys on the name, and a re-Marshal persists what this leaves. A document
	// written before an agent-name rename holds the spelling of its day ("bob-shell",
	// "ibm-bob"), and restoring it verbatim would serve the retired spelling beside today's
	// row — the exact split the rename's fold exists to prevent. Idempotent for everything
	// else: applyAgent computed each label through AgentName when it was written, and
	// CanonicalAgent is the identity on every current name.
	for _, a := range j.Agents {
		f.agents = append(f.agents, sessionAgent{
			name:  model.CanonicalAgent(a.Name),
			label: model.AgentName(a.Label),
			known: !a.Unknown,
		})
	}
	return nil
}
