package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// receiptProxy answers what agentop receipt asks of a proxy: /v1/usage by group, and the session
// list. It records every group the command asked for, so a test can hold the command to the
// aggregates it promises to stay within.
type receiptProxy struct {
	usage    map[string]string // group= value -> /v1/usage body
	sessions string            // /v1/sessions body; empty means 500
	mu       sync.Mutex
	groups   []string
}

func (p *receiptProxy) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			group := r.URL.Query().Get("group")
			p.mu.Lock()
			p.groups = append(p.groups, group)
			p.mu.Unlock()
			body, ok := p.usage[group]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		case "/v1/sessions":
			if p.sessions == "" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(p.sessions))
		default:
			http.NotFound(w, r)
		}
	}))
}

// pinReceiptClock fixes the card's "now" for one test.
func pinReceiptClock(t *testing.T, now time.Time) {
	t.Helper()
	prev := receiptNow
	receiptNow = func() time.Time { return now }
	t.Cleanup(func() { receiptNow = prev })
}

var receiptTestNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// A week of Claude Code: 1,284 calls, all four token kinds reported (presentKinds 15 =
// input|cache-read|cache-write|output), fully priced.
const receiptTotals = `"totals":{"requests":1284,"tokens":44799200,"inputTokens":186400,` +
	`"cacheReadTokens":41900000,"cacheWriteTokens":2400000,"outputTokens":312800,"presentKinds":15,` +
	`"costMicros":26820000,"pricedRequests":1284,"priceableRequests":1284},"priced":true`

const receiptByModel = `{"window":"7d","group":"model","bucketSeconds":604800,"buckets":[` +
	`{"at":"2026-11-05T12:00:00Z","series":{` +
	`"claude-sonnet-4-5":{"requests":1100,"tokens":39500000},` +
	`"claude-haiku-4-5":{"requests":184,"tokens":5299200}}}],` + receiptTotals + `}`

// The agent axis carries one recognised agent and one raw User-Agent. The raw one is spelled to
// be caught if it ever reaches the card.
const receiptByAgent = `{"window":"7d","group":"agent","bucketSeconds":604800,"buckets":[` +
	`{"at":"2026-11-05T12:00:00Z","series":{` +
	`"claude-code":{"requests":1280},"secret-internal-tool":{"requests":4}}}],` + receiptTotals + `}`

// One session active in the window and one last seen before it. The titles are the content the
// card must never carry.
const receiptSessions = `{"sessions":[` +
	`{"id":"s1","createdAt":"2026-10-01T09:00:00Z","updatedAt":"2026-11-11T10:00:00Z","eventCount":900,` +
	`"active":true,"title":"SECRET-TITLE fix the payroll export","agent":"claude-code"},` +
	`{"id":"s2","createdAt":"2026-10-01T09:00:00Z","updatedAt":"2026-10-20T10:00:00Z","eventCount":40,` +
	`"active":false,"title":"SECRET-TITLE-TWO","agent":"claude-code"}]}`

func receiptFixture() *receiptProxy {
	return &receiptProxy{
		usage:    map[string]string{"model": receiptByModel, "agent": receiptByAgent},
		sessions: receiptSessions,
	}
}

func runReceiptAgainst(t *testing.T, p *receiptProxy, args ...string) (code int, out, errOut string) {
	t.Helper()
	srv := p.serve(t)
	defer srv.Close()
	var o, e strings.Builder
	code = runReceipt(append([]string{"--endpoint", srv.URL}, args...), &o, &e)
	return code, o.String(), e.String()
}

func TestRunReceipt_PrintsTheCard(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	code, out, errOut := runReceiptAgainst(t, receiptFixture())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut)
	}
	for _, want := range []string{
		"CORTEX RECEIPT",
		"Nov 5 - Nov 12, 2026",
		"claude-code +1 other",
		"1,284",
		"94.2%",   // 41.9M of 44.49M prompt tokens were cache reads
		"142 : 1", // 44.49M prompt tokens for 312.8k output
		"88% of tokens",
		"claude-sonnet-4-5",
		"$26.82",
		"CACHE HOARDER",
		"94% of the prompt came from cache",
	} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("card missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Sessions                           1") {
		t.Errorf("card should count the one session active in the window:\n%s", out)
	}
	// A clean, fully priced window qualifies nothing, so the card carries no notes.
	if strings.Contains(out, "\n* ") {
		t.Errorf("a clean window printed a note:\n%s", out)
	}
}

// THE PROPERTY THE COMMAND EXISTS FOR: what it prints can be posted without reading it. Session
// titles come from the user's prompts and a raw User-Agent can name an internal program, so both
// are planted in the fixture and neither may survive into the card or the JSON.
func TestRunReceipt_CarriesNoTitleOrProject(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	for _, args := range [][]string{nil, {"--json"}} {
		p := receiptFixture()
		code, out, errOut := runReceiptAgainst(t, p, args...)
		if code != 0 {
			t.Fatalf("%v: exit = %d; stderr = %s", args, code, errOut)
		}
		for _, leak := range []string{"SECRET", "payroll", "secret-internal-tool"} {
			if strings.Contains(out, leak) {
				t.Errorf("%v: output carries %q:\n%s", args, leak, out)
			}
		}
		// AND IT NEVER ASKS FOR AN AXIS THAT NAMES THINGS. project names repositories, session and
		// host name what the user worked on; the card is built from model and agent alone.
		for _, g := range p.groups {
			if g != "model" && g != "agent" {
				t.Errorf("%v: asked the proxy for group=%q", args, g)
			}
		}
	}
}

// The card is for a phone-width chat, so no line may be wider than it — including a long model id
// and every note at once.
func TestRunReceipt_EveryLineFitsTheCard(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	p := receiptFixture()
	p.usage["model"] = `{"window":"7d","group":"model","daysOutsideRetention":2,` +
		`"degraded":{"skippedLines":3},"currencies":["USD"],"buckets":[{"at":"2026-11-05T12:00:00Z","series":{` +
		`"rits/zai-org/glm-5-3-instruct-with-a-very-long-deployment-suffix":{"requests":5,"tokens":900}}}],` +
		`"totals":{"requests":10,"tokens":1000,"inputTokens":600,"cacheReadTokens":300,"outputTokens":100,` +
		`"presentKinds":11,"costMicros":1000,"avoidedMicros":500,"pricedRequests":6,"priceableRequests":9,` +
		`"incompleteRequests":2,"refusedTokenRequests":1,"saturated":true},"priced":true}`
	code, out, errOut := runReceiptAgainst(t, p)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, errOut)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > receiptWidth {
			t.Errorf("line is %d columns, card is %d: %q", n, receiptWidth, line)
		}
	}
	// The END of the model id survives, since that is the part that names the model.
	if !strings.Contains(out, "deployment-suffix") || !strings.Contains(out, "...") {
		t.Errorf("long model id not kept by its tail:\n%s", out)
	}
	for _, want := range []string{"floor", "ledger read was incomplete", "not exact",
		"could not be priced", "retention", "refused", "estimate"} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("card omits the note about %q:\n%s", want, out)
		}
	}
}

// An unpriced window is not a free one. The card says "-" and why, never "$0.00".
func TestRunReceipt_UnpricedIsNeverZeroDollars(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	p := receiptFixture()
	body := `{"window":"7d","group":"%s","buckets":[],"totals":{"requests":10,"tokens":5000,` +
		`"priceableRequests":10},"priced":false}`
	p.usage["model"] = strings.Replace(body, "%s", "model", 1)
	p.usage["agent"] = strings.Replace(body, "%s", "agent", 1)
	_, out, _ := runReceiptAgainst(t, p)
	if strings.Contains(out, "$0") {
		t.Errorf("unpriced window printed a dollar figure:\n%s", out)
	}
	if !strings.Contains(out, "SPEND                              -") {
		t.Errorf("unpriced window should print SPEND as -:\n%s", out)
	}
	if !strings.Contains(flat(out), "none of the 10 requests could be priced") {
		t.Errorf("unpriced window does not say why:\n%s", out)
	}
	// "Covers the rest" describes a figure, and this card shows none.
	if strings.Contains(flat(out), "covers the rest") {
		t.Errorf("note describes a spend figure the card does not show:\n%s", out)
	}
	if strings.Contains(out, "------------------------------------\n------------------------------------") {
		t.Errorf("an empty section left two rules together:\n%s", out)
	}
}

// Two billing units cannot be added, so the card withholds the figure and names the units, as
// agentop cost does.
func TestRunReceipt_MixedUnitsWithholdSpend(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	p := receiptFixture()
	p.usage["model"] = strings.Replace(receiptByModel, `"group":"model",`,
		`"group":"model","currencies":["USD","credits"],`, 1)
	_, out, _ := runReceiptAgainst(t, p)
	if strings.Contains(out, "$26.82") {
		t.Errorf("mixed-unit window printed a combined figure:\n%s", out)
	}
	if !strings.Contains(flat(out), "USD and credits") {
		t.Errorf("mixed-unit window does not name its units:\n%s", out)
	}
}

// A provider that exposes no cache counter has an unknown cache hit, not a 0% one.
func TestRunReceipt_NoCacheCounterMeansNoCacheHit(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	p := receiptFixture()
	// presentKinds 9 = input|output: no cache kind reported, and both cache figures zero.
	body := `{"window":"7d","group":"%s","buckets":[],"totals":{"requests":50,"tokens":120000,` +
		`"inputTokens":100000,"outputTokens":20000,"presentKinds":9,"costMicros":500000,` +
		`"pricedRequests":50,"priceableRequests":50},"priced":true}`
	p.usage["model"] = strings.Replace(body, "%s", "model", 1)
	p.usage["agent"] = strings.Replace(body, "%s", "agent", 1)
	_, out, _ := runReceiptAgainst(t, p)
	for _, absent := range []string{"Cache hit", "cache reads", "cache writes", "CACHE HOARDER"} {
		if strings.Contains(out, absent) {
			t.Errorf("card shows %q with no cache counter reported:\n%s", absent, out)
		}
	}
	if !strings.Contains(out, "5 : 1") {
		t.Errorf("read : write should still print from input and output:\n%s", out)
	}
}

// The session list carries one line. Losing it must not cost the card.
func TestRunReceipt_SessionListFailureOmitsOnlyThatLine(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	p := receiptFixture()
	p.sessions = ""
	code, out, errOut := runReceiptAgainst(t, p)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut)
	}
	if strings.Contains(out, "Sessions") {
		t.Errorf("card shows a session count it could not read:\n%s", out)
	}
	if !strings.Contains(errOut, "sessions unavailable") {
		t.Errorf("stderr does not say the line was omitted: %s", errOut)
	}
	if !strings.Contains(out, "$26.82") {
		t.Errorf("card lost its figures with the session list:\n%s", out)
	}
}

func TestRunReceipt_UsageFailureExitsWithAHint(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	p := receiptFixture()
	p.usage = map[string]string{} // every /v1/usage request 404s
	code, out, errOut := runReceiptAgainst(t, p)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out != "" {
		t.Errorf("printed a card without usage figures:\n%s", out)
	}
	if !strings.Contains(errOut, "usage aggregation") {
		t.Errorf("stderr gives no next step: %s", errOut)
	}
}

// A proxy with no cost ledger answers "7d" with the hours it holds. The card is labelled with what
// it got and says what it was asked for.
func TestRunReceipt_SaysWhenTheServedWindowDiffers(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	p := receiptFixture()
	p.usage["model"] = strings.Replace(receiptByModel, `"window":"7d"`, `"window":"6h0m0s"`, 1)
	_, out, _ := runReceiptAgainst(t, p)
	if !strings.Contains(flat(out), "asked for 7d; this proxy served 6h") {
		t.Errorf("card does not disclose the narrower window:\n%s", out)
	}
	if !strings.Contains(out, "Last 6h") {
		t.Errorf("card is not labelled with the served window:\n%s", out)
	}
}

// A breakdown the server downgraded carries the ungrouped series. Its labels are not models.
func TestRunReceipt_DowngradedBreakdownIsNotRead(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	p := receiptFixture()
	p.usage["model"] = strings.Replace(
		strings.Replace(receiptByModel, `"group":"model"`, `"group":"none"`, 1),
		`"claude-sonnet-4-5"`, `"not-a-model"`, 1)
	_, out, _ := runReceiptAgainst(t, p)
	for _, absent := range []string{"Top model", "not-a-model", "Models used"} {
		if strings.Contains(out, absent) {
			t.Errorf("card read %q from a downgraded breakdown:\n%s", absent, out)
		}
	}
}

func TestRunReceipt_JSONCarriesTheFigures(t *testing.T) {
	pinReceiptClock(t, receiptTestNow)
	_, out, errOut := runReceiptAgainst(t, receiptFixture(), "--json")
	var got struct {
		Window string `json:"window"`
		Totals struct {
			Requests   int64 `json:"requests"`
			CostMicros int64 `json:"costMicros"`
		} `json:"totals"`
		Agents   []string `json:"agents"`
		Sessions *int     `json:"sessions"`
		TopModel *struct {
			Name string `json:"name"`
		} `json:"topModel"`
		Persona *struct {
			Name string `json:"name"`
		} `json:"persona"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s\nstderr: %s", err, out, errOut)
	}
	if got.Window != "7d" || got.Totals.Requests != 1284 || got.Totals.CostMicros != 26820000 {
		t.Errorf("figures = %+v", got)
	}
	if len(got.Agents) != 1 || got.Agents[0] != "claude-code" {
		t.Errorf("agents = %v", got.Agents)
	}
	if got.Sessions == nil || *got.Sessions != 1 {
		t.Errorf("sessions = %v, want 1", got.Sessions)
	}
	if got.TopModel == nil || got.TopModel.Name != "claude-sonnet-4-5" {
		t.Errorf("topModel = %+v", got.TopModel)
	}
	if got.Persona == nil || got.Persona.Name != "CACHE HOARDER" {
		t.Errorf("persona = %+v", got.Persona)
	}
	if len(got.Notes) != 0 {
		t.Errorf("clean window carries notes: %v", got.Notes)
	}
}

// The persona rules run rarest first, and each line names the figure that earned it.
func TestPickPersona_RarestFirst(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	base := func() *receipt {
		r := &receipt{Priced: true}
		r.Totals.Requests = 100
		r.Totals.CostMicros = 10_000_000
		return r
	}
	cases := []struct {
		name   string
		mutate func(*receipt)
		want   string
		line   string
	}{
		{"pruning beats everything", func(r *receipt) {
			r.Totals.AvoidedMicros = 1_000_000 // 1 of 11 would-be dollars
			r.Agents = []string{"bob", "claude-code"}
			r.CacheHitPercent = f(99)
		}, "PRUNER", "Tool pruning saved ~$1.00 (estimate)"},
		{"a saving under a twentieth is not a persona", func(r *receipt) {
			r.Totals.AvoidedMicros = 100_000
		}, "EXPLORER", "Saw all 100 model calls"},
		{"two agents", func(r *receipt) {
			r.Agents = []string{"bob", "claude-code"}
			r.CacheHitPercent = f(99)
		}, "AGENT JUGGLER", "2 agents, one proxy"},
		{"three models, a floor", func(r *receipt) {
			r.Models, r.ModelsAtLeast = 3, true
		}, "POLYGLOT", "3+ models in one window"},
		{"reads far more than it writes", func(r *receipt) {
			r.ReadPerWrite = f(412.4)
			r.CacheHitPercent = f(95)
		}, "BOOKWORM", "Read 412 tokens for every one it wrote"},
		{"cache, rounded down so it never overstates", func(r *receipt) {
			r.CacheHitPercent = f(94.9)
		}, "CACHE HOARDER", "94% of the prompt came from cache"},
		{"a saving beside a withheld total is not a share of anything", func(r *receipt) {
			r.Totals.AvoidedMicros = 5_000_000
			r.Currencies = []string{"USD", "credits"}
		}, "EXPLORER", "Saw all 100 model calls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(r)
			p := pickPersona(r)
			if p == nil || p.Name != tc.want || p.Line != tc.line {
				t.Errorf("persona = %+v, want %s / %q", p, tc.want, tc.line)
			}
		})
	}
	if p := pickPersona(&receipt{}); p != nil {
		t.Errorf("an empty window earned a persona: %+v", p)
	}
}
