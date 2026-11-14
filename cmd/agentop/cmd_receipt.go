package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/money"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// receiptWidth is the card's width in columns. Narrow enough that the card survives being pasted
// into a chat message read on a phone, which is the one place it is for: Slack's code block on a
// phone wraps at about 40 monospace columns, and a wrapped card is a broken one.
const receiptWidth = 36

// receiptNow is the clock the window's dates and the session count are read against. A var so
// tests can pin a day boundary.
var receiptNow = time.Now

// overflowSeriesLabel is the band the server folds series into past usage.MaxSeriesInResponse.
// usage keeps its own constant unexported; this is the same spelling, matched on the wire.
const overflowSeriesLabel = "(other)"

// runReceipt prints a summary of a window of usage that is safe to share.
//
// It exists because the obvious ways to show someone what Cortex found leak things the sender
// did not mean to post: a screenshot of `agentop observe` carries the TITLE column, which the
// proxy derives from the session's own prompts, and `agentop cost --by project` names every
// repository. THE CARD CARRIES AGGREGATES ONLY — no session title, project, path, host, prompt
// or tool name ever reaches it — so a user can paste it without reading it first. That property
// is the command's whole reason to exist, and TestRunReceipt_CarriesNoTitleOrProject holds it.
//
// Same shape as runCost: its own FlagSet writing to stderr, an exit code returned rather than
// os.Exit, so main owns process exit and a test can call this directly.
func runReceipt(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentop receipt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false,
		"emit the card's figures as JSON, with usage.Counts' own field names under totals")
	window := fs.String("window", usage.Window7d,
		"window to summarise: 7d, today, month, or a duration such as 6h")
	endpoint := fs.String("endpoint", "",
		"session API URL of the proxy (default: the Cortex installed on this machine)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `agentop receipt — a summary of your agents' usage that is safe to share

Usage:
  agentop receipt                  the last seven days
  agentop receipt --window month   this month, from the 1st
  agentop receipt --window today   today, from local midnight
  agentop receipt --json           the same figures as JSON, for a script

The card is meant to be pasted into a chat inside a code block. Every line on it is an
aggregate: it never shows a session title, a project or repository name, a path, a host,
a prompt or a tool name, so it can be posted as it is.

Spend follows the same rules as "agentop cost": a figure nothing could price shows "-",
never "$0.00", and anything that qualifies a figure is printed as a note under the card,
so the notes travel with it.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentop receipt: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	target := *endpoint
	if target == "" {
		target = localSessionEndpoint()
	}
	if target == "" {
		if why := localConfigProblem(); why != "" {
			fmt.Fprintf(stderr, "agentop receipt: no --endpoint given, and %s\n", why)
			fmt.Fprintln(stderr, "  fix the file; `agentop service status` says whether Cortex is still serving")
			return 1
		}
		fmt.Fprintln(stderr, "agentop receipt: no --endpoint given and no local Cortex is configured")
		fmt.Fprintln(stderr, "  is Cortex installed and running? `agentop service status`")
		return 1
	}

	// ONE BUDGET FOR ALL THREE REQUESTS, run side by side, so the command waits as long as
	// `agentop cost` does and no longer. costFetchTimeout's comment says why that figure, and why
	// it must not grow: a CLI that appears to hang is its own kind of wrong answer.
	ctx, cancel := context.WithTimeout(context.Background(), costFetchTimeout)
	defer cancel()
	client := apiclient.New(target)

	type usageResult struct {
		snap *usage.Snapshot
		err  error
	}
	type sessionsResult struct {
		list apiclient.SessionList
		err  error
	}
	byModelCh := make(chan usageResult, 1)
	byAgentCh := make(chan usageResult, 1)
	sessionsCh := make(chan sessionsResult, 1)
	go func() {
		snap, err := client.GetUsageWindow(ctx, *window, 0, "", usage.GroupModel)
		byModelCh <- usageResult{snap, err}
	}()
	go func() {
		snap, err := client.GetUsageWindow(ctx, *window, 0, "", usage.GroupAgent)
		byAgentCh <- usageResult{snap, err}
	}()
	go func() {
		list, err := client.ListSessionsArchived(ctx)
		sessionsCh <- sessionsResult{list, err}
	}()
	byModel, byAgent, sessions := <-byModelCh, <-byAgentCh, <-sessionsCh

	// THE TWO USAGE READS ARE FATAL, the session list is not. The usage reads carry every figure
	// on the card; the session list carries one line, and a card without it is still true.
	for _, r := range []usageResult{byModel, byAgent} {
		if r.err != nil {
			reportReceiptUsageError(r.err, *window, stderr)
			return 1
		}
	}
	var list *apiclient.SessionList
	if sessions.err == nil {
		list = &sessions.list
	} else {
		fmt.Fprintf(stderr, "agentop receipt: sessions unavailable, so the card omits that line: %v\n", sessions.err)
	}

	r := buildReceipt(byModel.snap, byAgent.snap, list, *window, receiptNow())
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintf(stderr, "agentop receipt: %v\n", err)
			return 1
		}
		return 0
	}
	writeReceiptCard(r, stdout)
	return 0
}

// reportReceiptUsageError turns a failed usage read into the next command to run. The same three
// causes runCost distinguishes, with the same fixes.
func reportReceiptUsageError(err error, window string, stderr io.Writer) {
	fmt.Fprintf(stderr, "agentop receipt: %v\n", err)
	switch {
	case errors.Is(err, apiclient.ErrNotFound):
		fmt.Fprintln(stderr, "  this proxy has no usage aggregation — is session tracking enabled?")
		fmt.Fprintln(stderr, "  is Cortex running? `agentop service status`")
	case errors.Is(err, apiclient.ErrBadRequest):
		fmt.Fprintf(stderr, "  this proxy does not accept --window %q\n", window)
		fmt.Fprintln(stderr, "  it may predate the today/month/7d windows; try --window 6h, or a duration it does hold")
	default:
		fmt.Fprintln(stderr, "  is Cortex running? `agentop service status`")
	}
}

// receipt is what the card shows, and the --json shape.
//
// Totals is usage.Counts verbatim, for runCost's reason: a script reading this should meet the
// same words for the same quantities as /v1/usage and `agentop cost --json`. Everything else is
// derived from it or from the two breakdowns, and each derived field is a pointer or omitempty so
// that a figure the window cannot support is absent rather than zero.
type receipt struct {
	// Window is what the server SERVED. Requested is present only when that differs, which is
	// what happens when a proxy without a cost ledger answers "7d" with the hours it holds.
	Window    string    `json:"window"`
	Requested string    `json:"requested,omitempty"`
	From      time.Time `json:"from,omitzero"`
	To        time.Time `json:"to"`

	Totals     usage.Counts `json:"totals"`
	Priced     bool         `json:"priced"`
	Currencies []string     `json:"currencies,omitempty"`

	// Agents are the recognised coding agents seen in the window, busiest first. OtherClients
	// counts the labels that are not one — a raw User-Agent such as curl, the "unknown" bucket for
	// traffic that sent none, and the server's overflow band — so the card can say "+2 other"
	// without naming a program the user may not want to post.
	Agents       []string `json:"agents,omitempty"`
	OtherClients int      `json:"otherClients,omitempty"`

	// Models counts the distinct models named in the window. ModelsAtLeast says the server folded
	// some into its overflow band, so the count is a floor.
	Models        int           `json:"models,omitempty"`
	ModelsAtLeast bool          `json:"modelsAtLeast,omitempty"`
	TopModel      *receiptModel `json:"topModel,omitempty"`

	// Sessions counts the sessions with any activity in the window. Absent when the session list
	// could not be read, and never a zero standing in for "unknown".
	Sessions *int `json:"sessions,omitempty"`

	// CacheHitPercent is the share of prompt tokens served from the provider's cache, and
	// ReadPerWrite is prompt tokens per output token. Each is absent unless the window's
	// producers reported the kinds it is computed from; see tokenKindReported.
	CacheHitPercent *float64 `json:"cacheHitPercent,omitempty"`
	ReadPerWrite    *float64 `json:"readPerWrite,omitempty"`

	Persona *receiptPersona `json:"persona,omitempty"`

	// Notes are the qualifications on the figures above, in the order the card prints them.
	Notes []string `json:"notes,omitempty"`
}

type receiptModel struct {
	Name string `json:"name"`
	// TokenPercent is this model's share of the window's tokens.
	TokenPercent float64 `json:"tokenPercent"`
}

// receiptPersona is the card's one playful line. Each names the figure that earned it, so it is a
// claim a reader can check against the card rather than a horoscope.
type receiptPersona struct {
	Name string `json:"name"`
	Line string `json:"line"`
}

// buildReceipt derives the card from the two usage reads and the session list.
//
// THE TOTALS COME FROM byModel ALONE. The two reads are separate requests and, on a window served
// from the in-memory ring, can disagree by whatever arrived between them; taking every figure from
// one read keeps the card internally consistent, and byAgent contributes only its labels.
func buildReceipt(byModel, byAgent *usage.Snapshot, sessions *apiclient.SessionList, asked string, now time.Time) *receipt {
	r := &receipt{
		Window:     byModel.Window,
		To:         now,
		Totals:     byModel.Totals,
		Priced:     byModel.Priced,
		Currencies: byModel.Currencies,
	}
	if costWindowLabel(byModel.Window) != costWindowLabel(asked) {
		r.Requested = asked
	}
	// THE WINDOW'S START, computed here from the label the server served. Exact for a local install,
	// where agentop and the proxy share a clock and a zone; against a remote --endpoint in another
	// zone "today" starts at a different instant there, which moves the session count by at most the
	// sessions in the hours between. The usage figures are unaffected — the server computed those.
	if spec, err := usage.ParseWindowSpec(byModel.Window, now); err == nil {
		if spec.Symbolic() {
			r.From = spec.From
		} else {
			r.From = now.Add(-spec.Dur)
		}
	}

	t := r.Totals
	if prompt, ok := promptTokens(t); ok && prompt > 0 {
		if tokenKindReported(t, usage.KindCacheRead, t.CacheReadTokens) {
			v := 100 * float64(t.CacheReadTokens) / float64(prompt)
			r.CacheHitPercent = &v
		}
		if tokenKindReported(t, usage.KindOutput, t.OutputTokens) && t.OutputTokens > 0 {
			v := float64(prompt) / float64(t.OutputTokens)
			r.ReadPerWrite = &v
		}
	}

	// A BREAKDOWN THE SERVER DID NOT SERVE IS NOT READ. A downgraded answer carries the ungrouped
	// series, and reading its labels as models or agents would name things that are not.
	if byModel.Group == usage.GroupModel {
		r.Models, r.ModelsAtLeast, r.TopModel = modelsOf(usage.FoldSeriesAcrossWindow(byModel.Buckets), t.Tokens)
	}
	if byAgent.Group == usage.GroupAgent {
		r.Agents, r.OtherClients = agentsOf(usage.FoldSeriesAcrossWindow(byAgent.Buckets))
	}
	if sessions != nil && !r.From.IsZero() {
		n := 0
		for _, s := range sessions.Sessions {
			// UpdatedAt is the session's last activity, so "last activity at or after the window's
			// start" is exactly "any activity inside the window". CreatedAt cannot answer it: a
			// session id can span weeks of unrelated work.
			if !s.UpdatedAt.Before(r.From) {
				n++
			}
		}
		r.Sessions = &n
	}
	r.Persona = pickPersona(r)
	r.Notes = receiptNotes(r, byModel)
	return r
}

// tokenKindReported reports whether a token kind is a measurement in this window.
//
// tokenSplit's rule: a zero with its PresentKinds bit unset means "nothing here reports this kind",
// not "none were used". A cache-hit percentage over a provider that exposes no cache counter would
// print 0% for a cache that may be doing all the work.
func tokenKindReported(t usage.Counts, bit uint8, v int64) bool {
	return t.PresentKinds&bit != 0 || v != 0
}

// promptTokens is what the agent sent: uncached input plus cache reads plus cache writes, the three
// prompt-side kinds. ok is false when the window reported none of them.
func promptTokens(t usage.Counts) (int64, bool) {
	var sum int64
	ok := false
	for _, k := range []struct {
		bit uint8
		v   int64
	}{
		{usage.KindInput, t.InputTokens},
		{usage.KindCacheRead, t.CacheReadTokens},
		{usage.KindCacheWrite, t.CacheWriteTokens},
	} {
		if tokenKindReported(t, k.bit, k.v) {
			ok = true
			sum += k.v
		}
	}
	return sum, ok
}

// modelsOf counts the models in a by-model series and picks the one that carried the most tokens.
//
// BY TOKENS, NOT BY COST, unlike usage.SortSeriesLabels: the card's line is "which model did the
// work", and a model nothing could price would never win on cost however much it carried.
func modelsOf(series map[string]usage.Counts, totalTokens int64) (count int, atLeast bool, top *receiptModel) {
	var best string
	var bestTokens int64
	for label, c := range series {
		switch label {
		case "", "unknown":
			continue
		case overflowSeriesLabel:
			atLeast = true
			continue
		}
		count++
		if c.Tokens > bestTokens || (c.Tokens == bestTokens && c.Tokens > 0 && label < best) {
			best, bestTokens = label, c.Tokens
		}
	}
	if best != "" && totalTokens > 0 {
		top = &receiptModel{Name: best, TokenPercent: 100 * float64(bestTokens) / float64(totalTokens)}
	}
	return count, atLeast, top
}

// agentsOf splits a by-agent series into the recognised coding agents, busiest first, and a count
// of everything else.
//
// Folded with AgentName and tested with IsKnownAgent, as agentop's AGENTS pane does: a raw
// User-Agent ("curl") is a label too, and nothing in the string marks it as unrecognised.
func agentsOf(series map[string]usage.Counts) (agents []string, other int) {
	requests := map[string]int64{}
	for label, c := range series {
		name := pipeline.AgentName(label)
		if !pipeline.IsKnownAgent(name) {
			other++
			continue
		}
		requests[name] += c.Requests
	}
	for name := range requests {
		agents = append(agents, name)
	}
	sort.Slice(agents, func(i, j int) bool {
		if requests[agents[i]] != requests[agents[j]] {
			return requests[agents[i]] > requests[agents[j]]
		}
		return agents[i] < agents[j]
	})
	return agents, other
}

// pickPersona chooses the card's persona: the first rule that matches wins.
//
// RAREST FIRST, so the persona says something about this user rather than about the agent they
// run. Nearly every Claude Code window clears 90% cache hits, so Cache Hoarder sits near the end,
// and the rules a user has to do something to earn — turning on tool pruning, running a second
// agent — come first.
//
// NO RULE REWARDS VOLUME OR SPEND. A persona for the biggest bill would pay people to burn tokens.
func pickPersona(r *receipt) *receiptPersona {
	t := r.Totals
	if t.Requests <= 0 {
		return nil
	}
	unit, labelled := money.WindowUnit(r.Currencies)
	// A saving of at least a twentieth of what the window would have cost without it. Only where the
	// spend itself is printable: a saving next to a withheld total has nothing to be a share of.
	if t.AvoidedMicros > 0 && labelled && r.Priced && t.CostMicros >= 0 &&
		t.AvoidedMicros*20 >= t.CostMicros+t.AvoidedMicros {
		return &receiptPersona{"PRUNER", fmt.Sprintf("Tool pruning saved ~%s (estimate)",
			costIn(float64(t.AvoidedMicros)/1e6, unit))}
	}
	if len(r.Agents) >= 2 {
		return &receiptPersona{"AGENT JUGGLER", fmt.Sprintf("%d agents, one proxy", len(r.Agents))}
	}
	if r.Models >= 3 {
		more := ""
		if r.ModelsAtLeast {
			more = "+"
		}
		return &receiptPersona{"POLYGLOT", fmt.Sprintf("%d%s models in one window", r.Models, more)}
	}
	if r.ReadPerWrite != nil && *r.ReadPerWrite >= 300 {
		return &receiptPersona{"BOOKWORM", fmt.Sprintf("Read %s tokens for every one it wrote",
			commas(int(math.Round(*r.ReadPerWrite))))}
	}
	if r.CacheHitPercent != nil && *r.CacheHitPercent >= 90 {
		// Rounded DOWN, so 94.9% never reads as 95%: the persona must not overstate the figure on
		// the line above it.
		return &receiptPersona{"CACHE HOARDER", fmt.Sprintf("%.0f%% of the prompt came from cache",
			math.Floor(*r.CacheHitPercent))}
	}
	return &receiptPersona{"EXPLORER", fmt.Sprintf("Saw all %s model calls", commas(int(t.Requests)))}
}

// receiptNotes lists what qualifies the card's figures, most serious first.
//
// THE SAME DISCLOSURES writeCostSummary makes, compressed to fit the card, and kept ON the card
// rather than printed beside it, because the card is what gets posted: a caveat left in the
// terminal is a caveat nobody who reads the post sees. Each is gated exactly as its counterpart in
// writeCostSummary is, so the two surfaces disagree about wording only.
func receiptNotes(r *receipt, snap *usage.Snapshot) []string {
	t := r.Totals
	var notes []string
	add := func(format string, a ...any) { notes = append(notes, fmt.Sprintf(format, a...)) }
	if r.Requested != "" {
		add("asked for %s; this proxy served %s", costWindowLabel(r.Requested), costWindowLabel(r.Window))
	}
	if t.Saturated {
		add("every figure is a floor: a total reached its limit and was clamped")
	}
	if snap.Degraded != nil {
		add("spend is short: the cost ledger read was incomplete (see agentop cost)")
	}
	if r.Priced && t.CostMicros < 0 {
		add("the proxy reported a negative total, which cannot be spend; none is shown")
	}
	if _, labelled := money.WindowUnit(r.Currencies); !labelled {
		add("spend is in %s, which cannot be added; none is shown", strings.Join(r.Currencies, " and "))
	}
	if t.IncompleteRequests > 0 {
		add("%s of %s priced requests carry an inexact figure; spend is not exact",
			commas(int(t.IncompleteRequests)), commas(int(t.PricedRequests)))
	}
	// "COVERS THE REST" ONLY WHEN THERE IS A REST. With nothing priced the card shows no figure at
	// all, and a note describing what that figure covers would be about a number that is not there.
	if gap := t.PriceableRequests - t.PricedRequests; gap > 0 {
		if t.PricedRequests <= 0 {
			add("none of the %s requests could be priced", commas(int(t.PriceableRequests)))
		} else {
			add("%s of %s requests could not be priced; spend covers the rest",
				commas(int(gap)), commas(int(t.PriceableRequests)))
		}
	}
	if snap.DaysOutsideRetention > 0 {
		add("the window reaches %s day%s past the cost ledger's retention; spend may not cover them",
			commas(int(snap.DaysOutsideRetention)), plainPlural(snap.DaysOutsideRetention))
	}
	if t.RefusedTokenRequests > 0 {
		add("%s token report%s refused as implausible; token figures are short",
			commas(int(t.RefusedTokenRequests)), plainPlural(t.RefusedTokenRequests))
	}
	if _, labelled := money.WindowUnit(r.Currencies); t.AvoidedMicros > 0 && labelled {
		add("the saving is an estimate, gross of cache re-warm, and not deducted from spend")
	}
	return notes
}

// writeReceiptCard renders the receipt as a fixed-width card. Plain ASCII throughout, so it pastes
// the same into a terminal, a chat and an issue, with no font missing a box character.
func writeReceiptCard(r *receipt, w io.Writer) {
	rule := strings.Repeat("-", receiptWidth)
	var lines []string
	add := func(s ...string) { lines = append(lines, s...) }
	row := func(label, value string) { add(receiptRow(label, value)...) }

	add(receiptCenter("CORTEX RECEIPT"), receiptCenter(receiptDates(r)), rule)

	t := r.Totals
	if len(r.Agents) > 0 || r.OtherClients > 0 {
		agents := strings.Join(r.Agents, ", ")
		if r.OtherClients > 0 {
			if agents != "" {
				agents += " "
			}
			agents += fmt.Sprintf("+%d other", r.OtherClients)
		}
		row("Agents", agents)
	}
	row("Model calls", commas(int(t.Requests)))
	if r.Sessions != nil {
		row("Sessions", commas(*r.Sessions))
	}
	add(rule)

	if t.Requests <= 0 {
		add(receiptCenter("No model calls in this window"), rule)
	} else {
		// Each section closes with a rule only when it printed something: a window whose producers
		// reported no token kinds would otherwise print two rules with nothing between them.
		tokensFrom := len(lines)
		if prompt, ok := promptTokens(t); ok {
			row("Prompt tokens", compactTokens(prompt))
			if tokenKindReported(t, usage.KindCacheRead, t.CacheReadTokens) {
				row("  cache reads", compactTokens(t.CacheReadTokens))
			}
			if tokenKindReported(t, usage.KindCacheWrite, t.CacheWriteTokens) {
				row("  cache writes", compactTokens(t.CacheWriteTokens))
			}
			if tokenKindReported(t, usage.KindInput, t.InputTokens) {
				row("  uncached", compactTokens(t.InputTokens))
			}
		}
		if tokenKindReported(t, usage.KindOutput, t.OutputTokens) {
			row("Output tokens", compactTokens(t.OutputTokens))
		}
		if r.CacheHitPercent != nil {
			row("Cache hit", fmt.Sprintf("%.1f%%", *r.CacheHitPercent))
		}
		if r.ReadPerWrite != nil {
			row("Read : write", commas(int(math.Round(*r.ReadPerWrite)))+" : 1")
		}
		if len(lines) > tokensFrom {
			add(rule)
		}
		if r.TopModel != nil {
			row("Top model", fmt.Sprintf("%.0f%% of tokens", r.TopModel.TokenPercent))
			add("  " + receiptTail(r.TopModel.Name, receiptWidth-2))
		}
		if r.Models > 0 {
			n := commas(r.Models)
			if r.ModelsAtLeast {
				n += "+"
			}
			row("Models used", n)
		}
		if r.TopModel != nil || r.Models > 0 {
			add(rule)
		}
	}

	unit, labelled := money.WindowUnit(r.Currencies)
	spend := "-"
	if r.Priced && t.CostMicros >= 0 && labelled {
		spend = costIn(float64(t.CostMicros)/1e6, unit)
	}
	row("SPEND", spend)
	if t.AvoidedMicros > 0 && labelled {
		row("Saved (estimate)", "~"+costIn(float64(t.AvoidedMicros)/1e6, unit))
	}
	add(rule)

	if p := r.Persona; p != nil {
		add(receiptCenter("* " + p.Name + " *"))
		for _, l := range receiptWrap(p.Line, receiptWidth) {
			add(receiptCenter(l))
		}
		add(rule)
	}
	for _, n := range r.Notes {
		for i, l := range receiptWrap(n, receiptWidth-2) {
			if i == 0 {
				add("* " + l)
			} else {
				add("  " + l)
			}
		}
	}
	add(receiptCenter("agentop receipt"))

	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// receiptDates names the window the card covers, in the words a reader of the post needs: dates,
// not a flag value.
func receiptDates(r *receipt) string {
	day := func(t time.Time) string { return t.Format("Jan 2") }
	full := func(t time.Time) string { return t.Format("Jan 2, 2006") }
	switch {
	case r.From.IsZero():
		return costWindowLabel(r.Window)
	case r.Window == usage.WindowToday:
		return "Today, " + full(r.To)
	case r.Window == usage.WindowMonth:
		return r.To.Format("January 2006") + ", to date"
	case r.From.Year() != r.To.Year():
		return full(r.From) + " - " + full(r.To)
	case r.To.Sub(r.From) < 24*time.Hour && r.From.YearDay() == r.To.YearDay():
		return "Last " + costWindowLabel(r.Window) + ", " + full(r.To)
	default:
		return day(r.From) + " - " + full(r.To)
	}
}

// receiptRow lays out one label and value across the card: the value right-aligned on the label's
// line when both fit, else the label alone and the value wrapped beneath it, right-aligned. Nothing
// is cut, so the card can grow taller but never wider.
func receiptRow(label, value string) []string {
	gap := receiptWidth - utf8.RuneCountInString(label) - utf8.RuneCountInString(value)
	if gap >= 1 {
		return []string{label + strings.Repeat(" ", gap) + value}
	}
	out := []string{label}
	for _, l := range receiptWrap(value, receiptWidth-2) {
		out = append(out, strings.Repeat(" ", receiptWidth-utf8.RuneCountInString(l))+l)
	}
	return out
}

// receiptCenter centres s on the card. A line wider than the card is returned as it is.
func receiptCenter(s string) string {
	pad := (receiptWidth - utf8.RuneCountInString(s)) / 2
	if pad <= 0 {
		return s
	}
	return strings.Repeat(" ", pad) + s
}

// receiptWrap breaks s into lines of at most width runes at spaces. A single word longer than the
// width keeps a line of its own rather than being cut.
func receiptWrap(s string, width int) []string {
	var lines []string
	var cur string
	for _, word := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = word
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(word) <= width:
			cur += " " + word
		default:
			lines = append(lines, cur)
			cur = word
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// receiptTail fits a model name into width runes by keeping its END, behind "...".
//
// THE END, because that is where a model id names the model: "rits/zai-org/glm-5-3" is a gateway
// route and an organisation before it is a model. The same choice the usage project label makes
// for a repository path, for the same reason.
func receiptTail(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n <= width || width <= 3 {
		return s
	}
	runes := []rune(s)
	return "..." + string(runes[n-(width-3):])
}
