package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// fakeUsageServer answers GET /v1/usage with body and 404s everything else, so a
// test states exactly what the proxy reported and nothing else can satisfy the
// command by accident.
//
// A server rather than an injected decoder: the command's job includes reaching the
// endpoint and turning a failure into a recovery hint, and a stubbed transport would
// test everything except that.
func fakeUsageServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
}

func TestRunCost_PrintsAHumanSummary(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"tokens":218100000,"costMicros":4170000,"pricedRequests":306,`+
		`"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"$4.17", "today", "318"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunCost_DisclosesTheCoverageGap(t *testing.T) {
	// 306 of 318 priced. Presenting the dollar total without the gap presents a
	// subtotal as the whole spend — the failure the coverage counters exist for.
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":306,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if !strings.Contains(out.String(), "12") {
		t.Errorf("output does not disclose the 12 unpriced requests:\n%s", out.String())
	}
}

func TestRunCost_FullyPricedCarriesNoWarning(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if strings.Contains(strings.ToLower(out.String()), "unpriced") {
		t.Errorf("fully priced output still warns:\n%s", out.String())
	}
}

// A WINDOW REACHING PAST RETENTION SAYS SO, which is the coverage statement this CLI was silent
// about while the TUI band marked the same figure partial.
//
// --window month against a ledger keeping less than a month is the ordinary case for it: the total
// is a subtotal, and printed alone it reads as the month's spend. The line is a COVERAGE claim, so
// it must not say anything was lost — nothing records the ledger's inception or what prune removed,
// so a fresh install reaching past its horizon may have lost nothing at all.
func TestRunCost_DisclosesAWindowPastRetention(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"month","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},`+
		`"priced":true,"daysOutsideRetention":21}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL, "--window", "month"}, &out, &errOut)
	got := out.String()

	if !strings.Contains(got, "21") {
		t.Errorf("output does not disclose the 21 days the ledger cannot reach:\n%s", got)
	}
	if !strings.Contains(got, "retention") {
		t.Errorf("output names no cause for the shortfall:\n%s", got)
	}
	// AND IT DOES NOT CLAIM A LOSS. "Pruned" or "missing" asserts spend existed on those days,
	// which is the false disclosure this whole feature was narrowed away from.
	for _, forbidden := range []string{"pruned", "missing", "lost"} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Errorf("output says %q about days outside retention, which claims a loss nothing "+
				"here can know about:\n%s", forbidden, got)
		}
	}
	// AND IT IS HEDGED, which is the other half of the same rule. "any spend on them is outside
	// the total" was the wording, and it is false whenever prune has floored its window at the
	// newest day file: those days are reported outside AND summed. See cost/ledger's
	// TestSnapshot_ADayReportedOutsideRetentionCanStillBeInTheTotal, which reproduces it.
	for _, overclaim := range []string{"is outside the total", "are outside the total"} {
		if strings.Contains(strings.ToLower(got), overclaim) {
			t.Errorf("output says %q, which is certain about a total it cannot inspect:\n%s",
				overclaim, got)
		}
	}
}

// AND A WINDOW INSIDE THE HORIZON IS SILENT, so the line above is a signal rather than furniture.
func TestRunCost_AWindowInsideRetentionSaysNothingAboutIt(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	// THE EXIT CODE AND A POSITIVE ANCHOR, because absence alone is not evidence: a command
	// that failed before printing anything satisfies "does not mention retention" too.
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("runCost = %d, want 0\nstdout: %s\nstderr: %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "$4.17") {
		t.Fatalf("output does not carry the total, so it proves nothing about retention:\n%s",
			out.String())
	}

	if strings.Contains(out.String(), "retention") {
		t.Errorf("a window the ledger covers still mentions retention:\n%s", out.String())
	}
}

// An inexact total must SAY it is inexact. A truncated stream's figure is a floor,
// and printing it beside an exact-looking "$4.17" claims a precision the data does
// not have — the claim usage.Counts.IncompleteRequests exists to withdraw.
func TestRunCost_DisclosesAnInexactTotal(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318,`+
		`"incompleteRequests":4},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if !strings.Contains(got, "inexact") {
		t.Errorf("output does not say the total is inexact:\n%s", got)
	}
	// THE PHRASE, not the digit "4" — which "$4.17" satisfies whatever incompleteRequests holds,
	// so the assertion could not fail on its own fixture.
	if !strings.Contains(got, "4 of 318 priced requests carry an inexact figure") {
		t.Errorf("output does not name how many figures are inexact, or how many they are out "+
			"of:\n%s", got)
	}
	// Disclosed, not deducted: the dollar total still stands.
	if !strings.Contains(got, "$4.17") {
		t.Errorf("output withheld the figure instead of qualifying it:\n%s", got)
	}
}

// And an exact total must not carry the caveat, for the reason a fully priced one
// carries no coverage warning.
func TestRunCost_ExactTotalCarriesNoInexactWarning(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if strings.Contains(out.String(), "inexact") {
		t.Errorf("an exact total still warns:\n%s", out.String())
	}
}

func TestRunCost_NothingPricedSaysUnavailableNotZero(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,`+
		`"priceableRequests":10},"priced":false}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 — nothing priced is not an error", code)
	}
	got := out.String()
	if !strings.Contains(got, "unavailable") {
		t.Errorf("output does not say cost is unavailable:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("output renders $0.00 for an unknown cost:\n%s", got)
	}
}

// No inference traffic at all is a finding, not an absence. Saying so beats a
// coverage line reading "0 of 0", which looks like a bug.
func TestRunCost_NoPriceableTrafficSaysSo(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":4},"priced":false}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if !strings.Contains(got, "no priceable traffic") {
		t.Errorf("output does not say there was nothing to price:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("output renders $0.00 for an unknown cost:\n%s", got)
	}
}

func TestRunCost_JSONUsesTheCountsFieldNames(t *testing.T) {
	// The schema rule: one vocabulary from parser to aggregate to ledger to CLI.
	// An unattended workload parses this, so the names are the contract.
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"inputTokens":100,"cacheReadTokens":2000,"cacheWriteTokens":50,`+
		`"outputTokens":30,"costMicros":250000,"pricedRequests":2,`+
		`"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	for _, field := range []string{"inputTokens", "cacheReadTokens", "cacheWriteTokens", "outputTokens", "costMicros"} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("--json output missing %q:\n%s", field, out.String())
		}
	}
	// The window the SERVER served is part of the JSON too, so a script learns which
	// span its number covers without asking again.
	if decoded["window"] != "today" {
		t.Errorf("window = %v, want \"today\"", decoded["window"])
	}
}

// costJSON.Window claims to be what the SERVER served, so "a script learns it got six hours
// rather than a day without having to ask a second question". Nothing tested that claim:
// TestRunCost_JSONUsesTheCountsFieldNames asks for "today" against a fixture that also
// answers "today", so it cannot tell reporting the answer from echoing the request —
// hardcoding costJSON{Window: "today"} passed the entire suite. The human path had the
// equivalent check (TestRunCost_NoLedgerServesAShorterWindowAndSaysSo); the machine path did
// not, and the machine path is the one nobody eyeballs.
//
// A proxy with no durable cost ledger — Kubernetes by design — is where this happens.
func TestRunCost_JSONReportsTheWindowServedNotTheOneRequested(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"6h0m0s","totals":{"requests":5,`+
		`"costMicros":100000,"pricedRequests":5,"priceableRequests":5},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--window", "today", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if decoded["window"] != "6h0m0s" {
		t.Errorf("window = %v, want \"6h0m0s\": the span the server served, not the \"today\" that was asked for", decoded["window"])
	}
	// And it must not be the request under any spelling — "today" appearing anywhere in the
	// window field would mean the request leaked into the answer.
	if w, _ := decoded["window"].(string); strings.Contains(w, "today") {
		t.Errorf("window = %q echoes the requested window; a script would report a six-hour figure as a day's spend", w)
	}
}

// costJSON described itself as the totals verbatim and omitted pricedBy and unpricedBy, so
// the one reader that cannot eyeball anything was the one reader that could not tell a
// MODELLED total from a BILLED one — the distinction usage_render.go's comment says
// matters, and which the human summary, the strip and the Cost pane all label.
func TestRunCost_JSONCarriesProvenanceAndTheNamedGaps(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,`+
		`"costMicros":1240000,"pricedRequests":7,"priceableRequests":10},"priced":true,`+
		`"pricedBy":{"authoritative":4,"bundled":3},`+
		`"unpricedBy":{"api.openai.com gpt-5":3}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		PricedBy   map[string]int64 `json:"pricedBy"`
		UnpricedBy map[string]int64 `json:"unpricedBy"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	// A total that is part modelled is not the same figure as one a gateway billed, and
	// $1.24 says nothing about which this is.
	if decoded.PricedBy["bundled"] != 3 || decoded.PricedBy["authoritative"] != 4 {
		t.Errorf("pricedBy = %v, want the provenance split the server reported:\n%s",
			decoded.PricedBy, out.String())
	}
	// "3 requests unpriced" is not actionable; the endpoint and model name the pricing
	// entry that would close the gap.
	if decoded.UnpricedBy["api.openai.com gpt-5"] != 3 {
		t.Errorf("unpricedBy = %v, want the named gap:\n%s", decoded.UnpricedBy, out.String())
	}
}

// All three maps are omitempty, so a response carrying none prints exactly what it printed
// before — a null or an empty object would make a script that checks for presence read
// "there were no gaps", which is a claim the ledger path in particular cannot make. For
// incompleteBy the misreading would be worse: absence there is not even a claim that the
// figures ARE exact, only that this window does not record which way they are not.
func TestRunCost_JSONOmitsTheMapsWhenTheServerSentNone(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"costMicros":250000,"pricedRequests":2,"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, absent := range []string{"pricedBy", "unpricedBy", "incompleteBy"} {
		if strings.Contains(out.String(), absent) {
			t.Errorf("--json emitted %q for a response that carried none:\n%s", absent, out.String())
		}
	}
}

func TestRunCost_UnreachableProxyExitsNonZeroAndSaysWhatToRun(t *testing.T) {
	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", "http://127.0.0.1:1"}, &out, &errOut)

	if code == 0 {
		t.Fatal("exit = 0 for an unreachable proxy")
	}
	// A user whose proxy is down needs the next command, not a bare dial error.
	if !strings.Contains(errOut.String(), "agentop service status") {
		t.Errorf("stderr does not name the recovery command:\n%s", errOut.String())
	}
}

// A 400 is the proxy ANSWERING: it understood the request and refused it. Telling the
// user to go and check whether Cortex is running sends them to the one place that has
// nothing wrong with it. The real causes are an older proxy that predates today/7d and
// a --window this one does not accept.
func TestRunCost_RefusedWindowBlamesTheWindowNotTheProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(`{"error":"bad window (want a duration such as 10m, 1h or 6h)"}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", srv.URL, "--window", "today"}, &out, &errOut)

	if code == 0 {
		t.Fatal("exit = 0 for a refused window")
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, "does not accept --window") {
		t.Errorf("stderr does not blame the window:\n%s", stderr)
	}
	if strings.Contains(stderr, "agentop service status") {
		t.Errorf("stderr sends the user to check a proxy that just answered:\n%s", stderr)
	}
	// The server's own words reach the user: every 400 message this endpoint returns is
	// a fixed string authored server-side, so it is the most specific thing available.
	if !strings.Contains(stderr, "bad window") {
		t.Errorf("stderr drops the server's own explanation:\n%s", stderr)
	}
}

func TestRunCost_NoLedgerServesAShorterWindowAndSaysSo(t *testing.T) {
	// Kubernetes, or a local install with the ledger disabled. The server answers
	// with the window it actually served; the CLI must print THAT, not "today".
	srv := fakeUsageServer(t, `{"window":"6h0m0s","totals":{"requests":5,`+
		`"costMicros":100000,"pricedRequests":5,"priceableRequests":5},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if strings.Contains(got, "today") {
		t.Errorf("output claims \"today\" when the server served 6h:\n%s", got)
	}
	if !strings.Contains(got, "6h") {
		t.Errorf("output does not name the window actually served:\n%s", got)
	}
}

// The split line is the shape of the bill for a long-running agent: cache reads are
// roughly a tenth of uncached input and cache writes a quarter more, so one scalar
// cannot explain a total.
func TestRunCost_PrintsTheTokenSplitThatWasReported(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"tokens":2180,"inputTokens":100,"cacheReadTokens":2000,"outputTokens":80,`+
		`"presentKinds":11,"costMicros":250000,"pricedRequests":2,`+
		`"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	for _, want := range []string{"input 100", "cache-read 2k", "output 80"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// presentKinds 0b1011 leaves cache-write unset, and its value is 0. Printing
	// "cache-write 0" would assert this traffic wrote no cache when the truth is that
	// nothing reported the counter.
	if strings.Contains(got, "cache-write") {
		t.Errorf("output prints a kind nothing reported:\n%s", got)
	}
}

// A real charge below half a cent must not print as $0.00 — the one string this
// command is forbidden to print for an unknown cost, so it must not be reachable for
// a known small one either.
func TestRunCost_SubCentChargeIsNotRenderedAsZero(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":1,`+
		`"costMicros":1200,"pricedRequests":1,"priceableRequests":1},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if strings.Contains(got, "$0.00") {
		t.Errorf("a sub-cent charge rendered as $0.00:\n%s", got)
	}
	if !strings.Contains(got, "<$0.01") {
		t.Errorf("output does not floor a sub-cent charge:\n%s", got)
	}
}

func TestRunCost_HelpListsTheFlags(t *testing.T) {
	var out, errOut strings.Builder
	if code := runCost([]string{"--help"}, &out, &errOut); code != 0 {
		t.Errorf("exit = %d for --help, want 0", code)
	}
	combined := out.String() + errOut.String()
	for _, want := range []string{"--json", "--window", "--endpoint"} {
		if !strings.Contains(combined, want) {
			t.Errorf("help does not mention %q:\n%s", want, combined)
		}
	}
	// The ring-versus-ledger difference has to be somewhere a user comparing two
	// figures on screen will actually look. It was documented only at the top of a Go
	// source file, which is the one place they will not.
	if !strings.Contains(combined, "can disagree") {
		t.Errorf("help does not warn that a duration window and today/7d can differ:\n%s", combined)
	}
}

// --window is forwarded, not ignored. Without this the default made every test pass
// while `--window 7d` quietly reported today.
func TestRunCost_ForwardsTheRequestedWindow(t *testing.T) {
	var gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWindow = r.URL.Query().Get("window")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"window":"7d","totals":{"requests":1},"priced":false}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL, "--window", "7d"}, &out, &errOut)

	if gotWindow != "7d" {
		t.Errorf("server saw window=%q, want \"7d\"", gotWindow)
	}
}

// The default window is today: the question this command exists to answer.
func TestRunCost_DefaultsToToday(t *testing.T) {
	var gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWindow = r.URL.Query().Get("window")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"window":"today","totals":{"requests":1},"priced":false}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if gotWindow != "today" {
		t.Errorf("server saw window=%q, want \"today\"", gotWindow)
	}
}

// TestRunCost_ANegativeTotalIsNotPrintedAsARefund.
//
// The CLI is a money surface too, and it had the hole the TUI's Cost pane closed in two
// places: costUSD is faithful about the sign, so a negative total printed "$-5.00" in the
// headline. The session API refuses to publish one, so this can only be a broken producer
// — and inheriting a guarantee silently is how it stops holding.
//
// "cost unavailable" plus a line naming the real cause. The headline on its own points a
// reader at pricing coverage, which is the ordinary reason for that string and the wrong
// place to look here.
func TestRunCost_ANegativeTotalIsNotPrintedAsARefund(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":-5000000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if strings.Contains(got, "$-") {
		t.Errorf("output prints a negative total as an amount:\n%s", got)
	}
	if !strings.Contains(got, "cost unavailable") {
		t.Errorf("output neither showed a figure nor declined one:\n%s", got)
	}
	if !strings.Contains(got, "negative") {
		t.Errorf("output does not name the reason there is no figure:\n%s", got)
	}
}

// TestRunCost_APositiveTotalStillPrints is the mirror. Without it the guard above could be
// satisfied by never printing a figure at all.
func TestRunCost_APositiveTotalStillPrints(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)
	got := out.String()
	if !strings.Contains(got, "$4.17") {
		t.Errorf("output lost a legitimate figure:\n%s", got)
	}
	if strings.Contains(got, "negative") {
		t.Errorf("output carries a caveat with nothing to act on:\n%s", got)
	}
}

// TestRunCost_DisclosesADamagedLedgerRead.
//
// usage.Snapshot.Degraded says the answer is MISSING ROWS. The server populates it and logs
// a warning; nothing in cmd/agentop read it, so a damaged read printed a total byte-identical
// to a clean one — a short figure under priced:true with no caveat anywhere in it, which is
// the failure the field's own doc says it exists to prevent.
//
// The default path: --window defaults to today, and today is one of the two windows the
// durable cost ledger serves, which is the only place the field can be populated.
func TestRunCost_DisclosesADamagedLedgerRead(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true,`+
		`"degraded":{"skippedLines":3,"truncatedDays":1}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	// The figure stays: it is short, not wrong, and withholding it would report a day of
	// known spend as unavailable.
	if !strings.Contains(got, "$4.17") {
		t.Errorf("output withheld a figure that is short rather than unknown:\n%s", got)
	}
	// Both counters, named. "Incomplete" alone gives an operator nothing to act on.
	for _, want := range []string{"SHORT", "3", "1 day file"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q — the damage is not disclosed:\n%s", want, got)
		}
	}
}

// TestRunCost_ADamagedReadIsNotSpelledAsAnInexactOne.
//
// usage.Snapshot.Degraded's doc is explicit that this is a DIFFERENT claim from
// Totals.IncompleteRequests and that the two must not be merged or shown with one marker:
// that counter says a figure the answer CARRIES is inexact, this says rows are missing from
// the sum. Both live in this fixture, and each has to be recognisable on its own.
func TestRunCost_ADamagedReadIsNotSpelledAsAnInexactOne(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":300,"priceableRequests":318,`+
		`"incompleteRequests":7},"priced":true,"degraded":{"skippedLines":3}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)
	got := out.String()

	// Three caveat lines, three claims, one each. Merged into one line — or one dropped —
	// this count is wrong.
	if n := strings.Count(got, "\n  ! "); n != 3 {
		t.Errorf("got %d caveat lines, want 3 (damaged, inexact, coverage):\n%s", n, got)
	}
	// The inexactness line still says what it always said, in its own words.
	if !strings.Contains(got, "inexact figure") {
		t.Errorf("the inexactness caveat lost its own wording:\n%s", got)
	}
	// And the damage line does not borrow them.
	dmg := ""
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "SHORT") {
			dmg = l
		}
	}
	if dmg == "" {
		t.Fatalf("no damage line at all:\n%s", got)
	}
	if strings.Contains(dmg, "inexact") {
		t.Errorf("the damage line is worded as an inexactness caveat: %q", dmg)
	}
	// The damage line leads: it is the only one of the three saying the SUM is incomplete.
	if i, j := strings.Index(got, "SHORT"), strings.Index(got, "inexact figure"); i > j {
		t.Errorf("the damage line follows the inexactness one (%d > %d):\n%s", i, j, got)
	}
}

// TestRunCost_ACleanReadCarriesNoDamageLine is the mirror, and it is the half that keeps the
// disclosure worth reading: a permanent warning with nothing to act on is what teaches an
// operator to ignore the one signal that matters.
func TestRunCost_ACleanReadCarriesNoDamageLine(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)
	if got := out.String(); strings.Contains(got, "SHORT") {
		t.Errorf("a clean read carries a damage caveat:\n%s", got)
	}
}

// TestRunCost_JSONCarriesTheDamageDisclosure.
//
// The machine path is the one this matters most on: a human can read the server's log line
// if they know to look, a script cannot. It was also the reader with no other way to tell a
// short total from a complete one — priced:true and a plausible figure look identical.
//
// Verbatim field names, because the schema rule is one vocabulary from parser to aggregate
// to ledger to CLI.
func TestRunCost_JSONCarriesTheDamageDisclosure(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true,`+
		`"degraded":{"skippedLines":3,"truncatedDays":1}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		Degraded *struct {
			SkippedLines  int64 `json:"skippedLines"`
			TruncatedDays int64 `json:"truncatedDays"`
		} `json:"degraded"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if decoded.Degraded == nil {
		t.Fatalf("--json dropped the damage disclosure entirely:\n%s", out.String())
	}
	if decoded.Degraded.SkippedLines != 3 || decoded.Degraded.TruncatedDays != 1 {
		t.Errorf("degraded = %+v, want skippedLines 3 and truncatedDays 1:\n%s",
			*decoded.Degraded, out.String())
	}
}

// TestRunCost_JSONOmitsTheDamageDisclosureWhenTheReadWasClean.
//
// The pointer's whole point: absence means the read was clean, so a clean answer has to be
// byte-identical to what this printed before the field existed. Zeros in an always-present
// object would read as "checked, fine" — the same false reassurance as $0.00 over unpriced
// traffic.
func TestRunCost_JSONOmitsTheDamageDisclosureWhenTheReadWasClean(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"costMicros":250000,"pricedRequests":2,"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out.String(), "degraded") {
		t.Errorf("--json emitted \"degraded\" for a clean read:\n%s", out.String())
	}
}

// TestCostDegradedText_NamesWhatEachKindOfDamageLost.
//
// Each branch on its own, because the three sentences make different claims and the
// unbounded one is the point: a skipped line is one request, an abandoned file is a day.
//
// The zero-counter case is a disclosure too — presence is the claim, not the counters, since
// the field is a pointer so that a clean read serialises nothing.
func TestCostDegradedText_NamesWhatEachKindOfDamageLost(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   usage.Degraded
		want []string
		not  []string
	}{
		{"lines only", usage.Degraded{SkippedLines: 3},
			[]string{"SHORT", "3 unreadable lines"}, []string{"day file", "unbounded"}},
		{"one line", usage.Degraded{SkippedLines: 1},
			[]string{"1 unreadable line;"}, []string{"lines"}},
		{"days only", usage.Degraded{TruncatedDays: 2},
			[]string{"SHORT", "2 day files", "unbounded"}, []string{"unreadable line"}},
		{"both", usage.Degraded{SkippedLines: 3, TruncatedDays: 1},
			[]string{"3 unreadable lines", "1 day file"}, nil},
		{"neither", usage.Degraded{},
			[]string{"SHORT", "without saying how much"}, []string{"unreadable line", "day file"}},

		// THE TWO COUNTERS THAT WENT UNRENDERED. Each fell through to the "without saying how
		// much" branch while the response said precisely how much — the defect UnreadableDays
		// was added to the struct to end, arriving again at the rendering step.
		{"unreadable days only", usage.Degraded{UnreadableDays: 2},
			[]string{"SHORT", "could not read 2 day files at all", "unbounded"},
			// It must NOT claim the read merely did not say, and must not invent other damage.
			[]string{"without saying how much", "unreadable line", "part-way"}},
		{"one unreadable day", usage.Degraded{UnreadableDays: 1},
			[]string{"could not read 1 day file at all"}, []string{"day files"}},
		{"dropped rows only", usage.Degraded{DroppedRowsTotal: 7},
			// And it says the count is the PROXY's running total, not this window's loss — a
			// reader who subtracted it from this window would be wrong.
			[]string{"SHORT", "dropped 7 rows before they reached disk", "running total for this proxy"},
			[]string{"without saying how much", "unbounded"}},
		{"one dropped row", usage.Degraded{DroppedRowsTotal: 1},
			[]string{"dropped 1 row before"}, []string{"1 rows"}},

		// COMBINED, because the clause list is what replaced a switch that could only describe
		// the pairs someone thought of.
		{"an unreadable day beside skipped lines", usage.Degraded{UnreadableDays: 1, SkippedLines: 4},
			[]string{"could not read 1 day file at all", "skipped 4 unreadable lines", "unbounded"},
			[]string{"without saying how much"}},
		{"all four", usage.Degraded{
			UnreadableDays: 1, TruncatedDays: 2, SkippedLines: 3, DroppedRowsTotal: 4},
			[]string{
				"could not read 1 day file at all",
				"abandoned 2 day files part-way",
				"skipped 3 unreadable lines",
				"dropped 4 rows before they reached disk",
				"unbounded",
				"running total for this proxy",
			},
			[]string{"without saying how much"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := costDegradedText(&tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q missing %q", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("%q contains %q, which does not apply", got, n)
				}
			}
		})
	}
}

// TestRunCost_JSONCarriesWhichWayTheTotalIsInexact.
//
// usage.Snapshot.IncompleteBy says WHICH WAY a figure is inexact; Totals.IncompleteRequests
// says only HOW MANY. Those are different claims about money — "at least $12.40" is a bound
// that will be exceeded and usually a transient failure worth chasing, "roughly $12.40" is a
// standing property of a gateway — and the field reached no client in cmd/agentop at all, so a
// script could read the count and had to render the two identically.
func TestRunCost_JSONCarriesWhichWayTheTotalIsInexact(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,`+
		`"costMicros":1240000,"pricedRequests":10,"priceableRequests":10,`+
		`"incompleteRequests":4},"priced":true,`+
		`"incompleteBy":{"output-uncounted":3,"split-unreported":1}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		IncompleteBy map[string]int64 `json:"incompleteBy"`
		Totals       struct {
			IncompleteRequests int64 `json:"incompleteRequests"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if decoded.IncompleteBy["output-uncounted"] != 3 || decoded.IncompleteBy["split-unreported"] != 1 {
		t.Errorf("incompleteBy = %v, want the reason split the server reported; without it a "+
			"script cannot tell \"at least $1.24\" from \"roughly $1.24\":\n%s",
			decoded.IncompleteBy, out.String())
	}
	// VERBATIM keys, not a friendlier spelling of them: the whole point of the shared schema
	// is that the CLI, /v1/usage and pricing.ReasonOutputUncounted say the same words.
	if strings.Contains(out.String(), "outputUncounted") || strings.Contains(out.String(), "lowerBound") {
		t.Errorf("--json re-keyed the reasons into a vocabulary of its own:\n%s", out.String())
	}
	// The count still stands beside the split. It is the field both window kinds populate,
	// and dropping it in favour of the map would lose exactness on a ledger window entirely.
	if decoded.Totals.IncompleteRequests != 4 {
		t.Errorf("totals.incompleteRequests = %d, want 4 alongside the split",
			decoded.Totals.IncompleteRequests)
	}
}

// TestRunCost_HumanSummarySaysWhichWayTheTotalIsInexact.
//
// The count line says the total is not exact; these lines say in which direction, and that is
// the difference between a figure a reader should treat as a floor and one they should treat
// as fuzzy in both directions. Both grammatical numbers are exercised — three of one reason,
// one of the other — because a caveat about money that reads as a typo is a caveat an
// operator learns to discount.
func TestRunCost_HumanSummarySaysWhichWayTheTotalIsInexact(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,`+
		`"costMicros":1240000,"pricedRequests":10,"priceableRequests":10,`+
		`"incompleteRequests":4},"priced":true,`+
		`"incompleteBy":{"output-uncounted":3,"split-unreported":1}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	// The floor: named, counted, and with the direction stated.
	if !strings.Contains(got, "3 are LOWER BOUNDS") {
		t.Errorf("the summary does not say three figures are lower bounds:\n%s", got)
	}
	if !strings.Contains(got, "real total is higher") {
		t.Errorf("the summary states a floor without saying which way it is wrong:\n%s", got)
	}
	// The approximation: singular, and explicitly NOT given a direction.
	if !strings.Contains(got, "1 is an APPROXIMATION") {
		t.Errorf("the summary does not name the approximate figure, or names it in the plural:\n%s", got)
	}
	if !strings.Contains(got, "no known direction") {
		t.Errorf("the summary presents an approximation as if it had a direction:\n%s", got)
	}
	// The count line survives above them: the split explains it, it does not replace it.
	if !strings.Contains(got, "4 of 10 priced requests carry an inexact figure") {
		t.Errorf("the split displaced the count it qualifies:\n%s", got)
	}
	// Order: the count, then the reasons under it.
	if strings.Index(got, "inexact figure") > strings.Index(got, "LOWER BOUNDS") {
		t.Errorf("the reasons print above the count they belong to:\n%s", got)
	}
}

// TestRunCost_AnUnknownInexactnessReasonIsPrintedNotDropped.
//
// IncompleteBy's counts sum to Totals.IncompleteRequests by contract, so a key this build
// does not recognise cannot be quietly skipped: a reader subtracting what was printed from
// the count would conclude the remainder were EXACT figures, which is the reading the whole
// disclosure exists to prevent. A newer proxy naming a third reason is the case — an older
// agentop against a newer sidecar is the normal deployment, not an exotic one.
func TestRunCost_AnUnknownInexactnessReasonIsPrintedNotDropped(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,`+
		`"costMicros":1240000,"pricedRequests":10,"priceableRequests":10,`+
		`"incompleteRequests":2},"priced":true,`+
		`"incompleteBy":{"rate-card-stale":2}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "rate-card-stale") {
		t.Errorf("an unrecognised reason was dropped, so 2 of the inexact figures now read as "+
			"exact:\n%s", got)
	}
	if !strings.Contains(got, "2 inexact under") {
		t.Errorf("the unrecognised reason lost its count:\n%s", got)
	}
}

// TestRunCost_AbsentReasonsPrintNothingAndClaimNoDirection.
//
// The DEFAULT path for this command: "today" is ledger-backed, and a persisted per-minute row
// carries IncompleteRequests without the reason, because the reason is no part of that row's
// key. usage.Snapshot.IncompleteBy's doc is explicit that absence is not a claim of
// exactness — so the count line must still print, and nothing may invent a direction for it.
func TestRunCost_AbsentReasonsPrintNothingAndClaimNoDirection(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,`+
		`"costMicros":1240000,"pricedRequests":10,"priceableRequests":10,`+
		`"incompleteRequests":4},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "4 of 10 priced requests carry an inexact figure") {
		t.Errorf("absence of the reasons suppressed the inexactness caveat itself, which reads "+
			"as an exact total:\n%s", got)
	}
	for _, banned := range []string{"LOWER BOUND", "APPROXIMATION", "no known direction",
		"real total is higher", "inexact under"} {
		if strings.Contains(got, banned) {
			t.Errorf("the summary says %q for a window that does not record which way its "+
				"figures are inexact:\n%s", banned, got)
		}
	}
}

// TestCostIncompleteReasonLines_OrdersTheFloorFirstAndSaysNothingForNone is the unit-level
// pin on the two rules the rendering has to keep: a fixed order (the floor first, because it
// is the stronger claim and the one with a direction) and NOTHING at all for an empty map.
//
// Directly on the helper because the order of two lines and the emptiness of a slice are
// awkward to assert through a whole command's output, and because a map's iteration order is
// randomised — a table here fails on the first run that shuffles, where a substring check on
// the rendered page might not.
func TestCostIncompleteReasonLines_OrdersTheFloorFirstAndSaysNothingForNone(t *testing.T) {
	if got := costIncompleteReasonLines(nil); got != nil {
		t.Errorf("costIncompleteReasonLines(nil) = %v, want nothing: absence is not a claim "+
			"about direction in either direction", got)
	}
	if got := costIncompleteReasonLines(map[string]int64{}); got != nil {
		t.Errorf("costIncompleteReasonLines(empty) = %v, want nothing", got)
	}
	got := costIncompleteReasonLines(map[string]int64{
		"split-unreported": 2,
		"output-uncounted": 5,
		"unlabelled":       1,
		"zz-unknown":       3,
		"aa-unknown":       4,
	})
	if len(got) != 5 {
		t.Fatalf("got %d lines, want 5 (three known reasons and two unknown): %v", len(got), got)
	}
	wantPrefixes := []string{
		"5 are LOWER BOUNDS",
		"2 are APPROXIMATIONS",
		"1 carries a caveat",
		"4 inexact under \"aa-unknown\"",
		"3 inexact under \"zz-unknown\"",
	}
	for i, want := range wantPrefixes {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("line %d = %q, want it to start %q — the floor leads, then the "+
				"approximation, then the unnamed caveat, then unknown keys in a stable order",
				i, got[i], want)
		}
	}
}

// TestRunCost_AsksForAnAxisThatCannotCarryAResidual pins the PREMISE behind this command
// showing no residual band, so the absence stays a decision rather than becoming an
// oversight.
//
// usage.Snapshot.UngroupedCostMicros is the part of the total no SERIES entry carries, and
// both producers compute it only where usage.Group.Reconcilable is true. This command asks
// for group=none, which is not reconcilable, so the field can never arrive — and would have
// nothing to disclose if it did, since the headline is Totals.CostMicros and no series is
// summed here.
//
// The value of this test is what it says WHEN IT FAILS. Anyone giving this command a real
// axis — a by-model table, say — starts receiving a residual, and a table summing to less
// than the headline above it with nothing to explain the gap is the defect the field exists
// to end.
func TestRunCost_AsksForAnAxisThatCannotCarryAResidual(t *testing.T) {
	var gotGroup string
	// HITS, because "" is the answer to two different questions. gotGroup is "" when the
	// command asked for no group AND when the handler never ran at all, and
	// usage.ParseGroup("") returns GroupNone with no error — which is non-reconcilable, so the
	// assertion below passed either way. This test is cited from two places in cmd_cost.go as
	// the pin that fails if this command ever takes an axis, so a green run has to mean a
	// request was actually made and inspected.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotGroup = r.URL.Query().Get("group")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"window":"today","totals":{"requests":1},"priced":false}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	// And the exit code, discarded before: a command that failed before it reached the server
	// leaves hits at zero, but one that reached it and then failed would still have exercised
	// nothing this test is about.
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if hits != 1 {
		t.Fatalf("the server saw %d requests, want exactly 1: with none, gotGroup is \"\" and "+
			"every assertion below passes without this command having asked for anything", hits)
	}

	// Absent on the wire is how apiclient spells GroupNone, and usage.ParseGroup reads "" as
	// exactly that — so the parse is the check rather than a string comparison that would
	// pass for a group nobody validated. Sound only because hits is 1 above.
	group, err := usage.ParseGroup(gotGroup)
	if err != nil {
		t.Fatalf("server saw group=%q, which the API does not accept: %v", gotGroup, err)
	}
	if group.Reconcilable() {
		t.Errorf("this command now asks for group=%q, whose series CAN be reconciled against the "+
			"total — so the answer may carry usage.Snapshot.UngroupedCostMicros, the spend no "+
			"series row accounts for. Nothing in this command reads it. Either go back to a "+
			"group that offers no reconciliation, or disclose the residual wherever the "+
			"breakdown is printed (tui.costUngroupedRow is the pane's form of it) and carry it "+
			"in costJSON for the scripted reader", group)
	}
}

// TestRunCost_TheHeadlineIsThePublishedTotalNotASeriesSum.
//
// The rule usage.Snapshot.UngroupedCostMicros' own doc states for every client: never
// present the sum of a series as the window's total. This surface prints one figure, and it
// must be the one the server published — a total that already includes the spend no series
// entry carries.
//
// The fixture is a reconcilable answer whose series is SHORT of its total by a quarter of a
// dollar: 4.00 in the one series entry, 0.25 ungrouped, 4.25 in Totals. That is the shape a
// gateway-priced /v1/embeddings response produces, and it is served here on a duration
// window because a ring-backed answer is where a client can ask for a group at all. A
// command that re-derived its headline by adding the breakdown up would print $4.00 and be
// short by real money.
func TestRunCost_TheHeadlineIsThePublishedTotalNotASeriesSum(t *testing.T) {
	const body = `{"window":"1h","group":"model","priced":true,` +
		`"totals":{"requests":5,"costMicros":4250000,"pricedRequests":5,"priceableRequests":5},` +
		`"buckets":[{"at":"2026-01-01T00:00:00Z","requests":5,"costMicros":4250000,` +
		`"series":{"claude-opus-5":{"requests":4,"costMicros":4000000,"pricedRequests":4,"priceableRequests":4}}}],` +
		`"ungroupedCostMicros":250000}`
	// The identity the field restores, asserted on the fixture rather than assumed: a test
	// that only matched the headline would look the same against numbers that never
	// reconciled.
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}
	var series int64
	for _, b := range snap.Buckets {
		for _, c := range b.Series {
			series += c.CostMicros
		}
	}
	if snap.UngroupedCostMicros == nil {
		t.Fatal("fixture premise is wrong: no residual was published")
	}
	if series+*snap.UngroupedCostMicros != snap.Totals.CostMicros {
		t.Fatalf("fixture does not reconcile: series %d + ungrouped %d != totals %d",
			series, *snap.UngroupedCostMicros, snap.Totals.CostMicros)
	}

	srv := fakeUsageServer(t, body)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--window", "1h"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "$4.25") {
		t.Errorf("headline is not the published total (want $4.25):\n%s", got)
	}
	if strings.Contains(got, "$4.00") {
		t.Errorf("the series sum is presented as the window total:\n%s", got)
	}
}

// TestRunCost_DisclosesARefusedTokenReportAndClearsTheDollars.
//
// The defect this closes: usage.Counts.RefusedTokenRequests was aggregated by the server and
// read by nothing, so a window that threw away token reports printed a token count and a split
// byte-identical to a complete one. The counter exists because capping cost while leaving tokens
// unbounded is not a position that survives being stated — and a bound whose refusals are
// invisible is the same thing again one step later.
//
// The ASYMMETRY is the part that has to be in the words. A refused token report removes nothing
// from CostMicros: cost is settled by a different producer and bounded twice over. So a line that
// let a reader doubt the dollar figure would send them after the one number in the answer that is
// right, and this asserts the line says so.
func TestRunCost_DisclosesARefusedTokenReportAndClearsTheDollars(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318,`+
		`"tokens":120000,"inputTokens":20000,"presentKinds":1,`+
		`"refusedTokenRequests":3},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"3 token reports refused",
		"SHORT",
		"dollar total is unaffected",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q — the refusal is not disclosed as a token shortfall:\n%s",
				want, got)
		}
	}
	// The token figures still print: they are short, not unknown, and withholding them would
	// report measured traffic as unmeasured.
	if !strings.Contains(got, "120k tokens") || !strings.Contains(got, "input 20k") {
		t.Errorf("the token figures were withheld over a refusal:\n%s", got)
	}
	// It sits with the figures it qualifies, above the dollar caveats. A caveat printed beside a
	// figure it is not about is a misattribution, not a warning.
	if split, refusal := strings.Index(got, "input 20k"), strings.Index(got, "refused"); split > refusal {
		t.Errorf("the refusal line is printed above the split it qualifies:\n%s", got)
	}
}

// TestRunCost_NoRefusedReportsCarryNoLine is the mirror, and the half that keeps the disclosure
// worth reading. A permanent "0 token reports refused" is the "checked, fine" claim from a
// producer that never checked.
func TestRunCost_NoRefusedReportsCarryNoLine(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318,`+
		`"tokens":120000,"inputTokens":20000,"presentKinds":1},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)
	if got := out.String(); strings.Contains(got, "refused") {
		t.Errorf("a window with no refusals carries a refusal line:\n%s", got)
	}
}

// TestRunCost_DisclosesAClampedAggregateAheadOfADamagedRead.
//
// usage.Counts.Saturated says an addition into these totals reached the int64 ceiling and was
// CLAMPED rather than allowed to wrap, so the requests, the tokens and the cost on the headline
// are all floors. Its own doc argues the clamp is only acceptable BECAUSE the flag travels with
// it, and that it is deliberately NOT logged — "the disclosure travels on the same response as
// the number it qualifies" — so a client that drops it is what turns the clamp back into a lie.
//
// ORDER as well as presence. The clamp leads even the damaged read: a damaged read is short in
// the dollars, a clamp is short in every column of the aggregate. And the two keep separate
// words, because one sends an operator to a day file and the other to whatever produced 9.2e18
// micros of traffic.
func TestRunCost_DisclosesAClampedAggregateAheadOfADamagedRead(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318,`+
		`"saturated":true},"priced":true,"degraded":{"skippedLines":3}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"every figure above is a FLOOR",
		"rather than allowed to wrap",
		"requests, tokens and cost are all larger",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q — the clamp is not disclosed:\n%s", want, got)
		}
	}
	// The figure still prints: it is a floor, not an unknown.
	if !strings.Contains(got, "$4.17") {
		t.Errorf("a clamped figure was withheld rather than qualified:\n%s", got)
	}
	if clamp, damaged := strings.Index(got, "is a FLOOR"), strings.Index(got, "total is SHORT"); damaged < 0 {
		t.Errorf("premise is wrong: no damage line in:\n%s", got)
	} else if clamp > damaged {
		t.Errorf("the damaged-read line outranks the clamp:\n%s", got)
	}
}

// TestRunCost_ACleanAggregateCarriesNoClampLine is the mirror: false must mean "the arithmetic
// held", so a correct deployment prints nothing for it.
func TestRunCost_ACleanAggregateCarriesNoClampLine(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)
	if got := out.String(); strings.Contains(got, "FLOOR") {
		t.Errorf("a clean aggregate carries a clamp line:\n%s", got)
	}
}

// TestRunCost_JSONCarriesTheClampAndTheRefusalUnderCountsOwnNames.
//
// The machine path, and the reason Totals is usage.Counts embedded rather than re-keyed: a
// disclosure added to Counts reaches a script the day the server sends it, with no line in
// costJSON at all. This asserts that property rather than assuming it — the whole point of the
// verbatim rule is that it holds without anyone remembering to extend a struct.
//
// A script is the reader that needs both most. It cannot see a rendered caveat, and neither
// field has a server log line it could read instead.
func TestRunCost_JSONCarriesTheClampAndTheRefusalUnderCountsOwnNames(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318,`+
		`"tokens":120000,"refusedTokenRequests":3,"saturated":true},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		Totals struct {
			RefusedTokenRequests int64 `json:"refusedTokenRequests"`
			Saturated            bool  `json:"saturated"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if decoded.Totals.RefusedTokenRequests != 3 {
		t.Errorf("totals.refusedTokenRequests = %d, want 3:\n%s",
			decoded.Totals.RefusedTokenRequests, out.String())
	}
	if !decoded.Totals.Saturated {
		t.Errorf("totals.saturated is false; a clamped aggregate reaches no script:\n%s", out.String())
	}
}

// TestRunCost_JSONOmitsTheClampAndTheRefusalWhenThereAreNone.
//
// omitempty on both, so a healthy answer is byte-identical to what this printed before the
// fields existed. A zero would have to carry two meanings — "checked, none" and "not checked" —
// which is the reading the whole absent-not-zero convention exists to refuse.
func TestRunCost_JSONOmitsTheClampAndTheRefusalWhenThereAreNone(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"costMicros":250000,"pricedRequests":2,"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, unwanted := range []string{"refusedTokenRequests", "saturated"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("--json emitted %q for an answer with nothing to disclose:\n%s",
				unwanted, out.String())
		}
	}
}

// TestRunCost_JSONCarriesTheBreakdownOvershoot.
//
// usage.Snapshot.SeriesOvershootMicros says the answer CONTRADICTS ITSELF: its breakdown summed
// to more than its own total, which a reconcilable group's series cannot do — so a value means
// the producer is wrong about its own arithmetic.
//
// ON THIS STRUCT THOUGH UngroupedCostMicros IS NOT, and the difference is what the ABSENCE means
// rather than how likely the presence is. A missing residual is ambiguous between "the breakdown
// accounts for every dollar" and "no breakdown was asked for", and this command asks for
// group=none — so the field would be a promise nothing keeps. A missing overshoot has one reading
// on every axis including none: nothing overshot. So absence is TRUE here rather than merely
// unpopulated, and a script that treats it as "this answer is not self-contradictory" is right
// today and stays right after an axis change.
//
// Served here by a producer that sends it anyway, which is the case worth covering: this side of
// the wire does not get to assume the other side obeys its own contract, and a broken or hostile
// aggregator is exactly when a defect report earns its place. The human summary prints nothing
// for it and writeCostSummary says why — it prints no breakdown, so the caveat would qualify
// nothing on screen.
func TestRunCost_JSONCarriesTheBreakdownOvershoot(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"1h","group":"model","totals":{"requests":2,`+
		`"costMicros":4000000,"pricedRequests":2,"priceableRequests":2},"priced":true,`+
		`"seriesOvershootMicros":250000}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		SeriesOvershootMicros *int64 `json:"seriesOvershootMicros"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if decoded.SeriesOvershootMicros == nil {
		t.Fatalf("--json dropped the overshoot entirely, so a script cannot tell a "+
			"self-contradictory answer from a sound one:\n%s", out.String())
	}
	if *decoded.SeriesOvershootMicros != 250_000 {
		t.Errorf("seriesOvershootMicros = %d, want 250000:\n%s",
			*decoded.SeriesOvershootMicros, out.String())
	}
	// VERBATIM, and not renamed on the way through: the schema rule is one vocabulary from
	// aggregate to CLI, so the key here has to be the key on the wire.
	if !strings.Contains(out.String(), `"seriesOvershootMicros"`) {
		t.Errorf("--json spells the overshoot under some other key:\n%s", out.String())
	}
}

// TestRunCost_JSONOmitsTheOvershootWhenNothingOvershot.
//
// A POINTER with omitempty, so a healthy answer serialises nothing and absence keeps meaning
// "nothing overshot" rather than becoming a zero that means both that and "not checked". This is
// also what keeps the field free: every correct answer is byte-identical to what this printed
// before it existed.
func TestRunCost_JSONOmitsTheOvershootWhenNothingOvershot(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"costMicros":250000,"pricedRequests":2,"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out.String(), "seriesOvershoot") {
		t.Errorf("--json emitted the overshoot for an answer that did not overshoot:\n%s",
			out.String())
	}
}

// A saving must be REPORTED, and reported with all three of its caveats. The figure is an
// estimate, it is gross of the prompt-cache re-warm, and it is not deducted from the total
// above — a reader who takes it as money in the bank has been misled by this line.
func TestRunCost_ReportsWhatPruningSavedWithItsCaveats(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"tokens":218100000,"costMicros":4170000,"avoidedMicros":982000,`+
		`"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	// "$0.98", not "$0.982": costUSD trims to the same precision as the headline, so the
	// saved figure is spelled exactly like the spend figure beside it.
	for _, want := range []string{"~$0.98", "saved", "estimate", "gross", "not deducted"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q — the figure and every one of its caveats must appear:\n%s",
				want, got)
		}
	}
	// The saving must not have moved the spend headline in either direction.
	if !strings.Contains(got, "$4.17") {
		t.Errorf("the spend total is no longer $4.17 — a counterfactual reached it:\n%s", got)
	}
}

// The line is silent when there is nothing to report, on this command's standing rule: a
// deployment not running tool-prune has nothing to act on, and a permanent "~$0.0000 saved"
// teaches an operator to stop reading these lines.
func TestRunCost_SaysNothingAboutSavingsWhenThereAreNone(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if strings.Contains(out.String(), "saved") {
		t.Errorf("output mentions savings with none recorded:\n%s", out.String())
	}
}

// A saving on a window where NOTHING could be priced must still print. The prompt was pruned
// whether or not the response could be priced — usage.Counts.AvoidedMicros does not travel
// with the cost figure — so "cost unavailable" and a real saving are both true at once, and
// gating the saved line on Priced would lose exactly the case that is most worth seeing.
func TestRunCost_ReportsASavingEvenWhenNothingWasPriced(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":12,`+
		`"avoidedMicros":41000,"priceableRequests":12},"priced":false}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if !strings.Contains(got, "cost unavailable") {
		t.Errorf("want the unpriced headline:\n%s", got)
	}
	if !strings.Contains(got, "saved") {
		t.Errorf("the saving is missing from an unpriced window, which is where it matters "+
			"most:\n%s", got)
	}
}

// The JSON carries the APPORTIONED split, and omits it when there is no mix.
//
// The raw modelled micros already reach a script for free: costJSON.Totals is usage.Counts
// embedded verbatim, which is the property the struct's own comment exists to protect. What
// is added here is the ANSWER rather than the ingredients — a script that apportioned the
// mix itself would be the second implementation of arithmetic the spec puts in one place,
// and the two would drift.
func TestRunCost_JSONCarriesTheApportionedTiers(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"1h","priced":true,"totals":{"requests":35,`+
		`"costMicros":4546200,"pricedRequests":35,"priceableRequests":35,`+
		`"inputCostMicros":3000,"cacheWriteCostMicros":7500,`+
		`"cacheReadCostMicros":30000,"outputCostMicros":45000}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		Tiers *struct {
			Input      int64 `json:"input"`
			CacheWrite int64 `json:"cacheWrite"`
			CacheRead  int64 `json:"cacheRead"`
			Output     int64 `json:"output"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if decoded.Tiers == nil {
		t.Fatalf("no tiers for a window with a modelled mix:\n%s", out.String())
	}
	// Apportioned, not the raw mix: the four must sum to the authoritative total, which is
	// two orders of magnitude above the 85,500-micro mix they were derived from.
	sum := decoded.Tiers.Input + decoded.Tiers.CacheWrite + decoded.Tiers.CacheRead + decoded.Tiers.Output
	if sum != 4_546_200 {
		t.Errorf("tiers sum to %d, want the authoritative 4546200 — these look like the raw "+
			"mix rather than the apportioned split:\n%s", sum, out.String())
	}
	if decoded.Tiers.Output <= decoded.Tiers.CacheRead {
		t.Errorf("output %d is not above cache-read %d, so the ranking was lost",
			decoded.Tiers.Output, decoded.Tiers.CacheRead)
	}
}

// No mix means the key is ABSENT, not four zeros: a script summing zeros would report the
// traffic as free, the same lie the human surface refuses with an em-dash.
func TestRunCost_JSONOmitsTiersWithNoMix(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"1h","priced":true,"totals":{"requests":35,`+
		`"costMicros":4546200,"pricedRequests":35,"priceableRequests":35}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if strings.Contains(out.String(), `"tiers"`) {
		t.Errorf("tiers emitted for a window with no modelled mix:\n%s", out.String())
	}
}

// AND --json CARRIES THE SAME COVERAGE FIGURE THE HUMAN SUMMARY PRINTS.
//
// costJSON re-keys the snapshot field by field, so a disclosure added to the server reaches a
// script only when someone adds a line here — and this one was missed: `--window month` against a
// shorter retention_days printed the "!" line for a reader and returned a total short by weeks,
// with no trace of it, to the consumer with nobody watching. That is the disagreement this
// command's own comment calls worse than either answer, on the surface where nothing notices.
func TestRunCost_JSONCarriesTheRetentionCoverage(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"month","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},`+
		`"priced":true,"daysOutsideRetention":21}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--window", "month", "--json"},
		&out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var got struct {
		DaysOutsideRetention int64 `json:"daysOutsideRetention"`
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if got.DaysOutsideRetention != 21 {
		t.Errorf("daysOutsideRetention = %d, want 21 — a script cannot see that the month is "+
			"short by three weeks:\n%s", got.DaysOutsideRetention, out.String())
	}
}

// And it is absent when the window fits, so a consumer can treat presence as the signal.
func TestRunCost_JSONOmitsTheCoverageWhenThereIsNone(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	// Same reason as the text form above: the exit code first, then a key that MUST be there,
	// so "the key is absent" is a statement about this JSON and not about an empty buffer.
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("runCost = %d, want 0\nstdout: %s\nstderr: %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "\"costMicros\"") {
		t.Fatalf("output is not the cost JSON, so the missing key proves nothing:\n%s", out.String())
	}
	if strings.Contains(out.String(), "daysOutsideRetention") {
		t.Errorf("a window the ledger covers still carries the key:\n%s", out.String())
	}
}

// --agent reports one agent's figures, and asks for the axis that can carry them.
//
// The group is asserted as well as the output because the two are one decision: without
// group=agent the response has no per-agent series at all, and the command would have to
// invent the number it prints.
func TestRunCost_AgentReportsThatAgentOnly(t *testing.T) {
	var gotGroup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			http.NotFound(w, r)
			return
		}
		gotGroup = r.URL.Query().Get("group")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"window":"today","group":"agent","priced":true,
			"totals":{"requests":1057,"tokens":298000000,"costMicros":146361600,
			          "pricedRequests":1048,"priceableRequests":1055},
			"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
			   "claude-code/2.1.270":{"requests":1049,"tokens":297961318,"costMicros":146361600,
			                          "pricedRequests":1048,"priceableRequests":1048},
			   "bob-shell/2.0.5":{"requests":8,"tokens":38682,"priceableRequests":7}}}]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", srv.URL, "--agent", "bob-shell/2.0.5"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if gotGroup != "agent" {
		t.Errorf("asked for group=%q, want %q — without it there is no per-agent series to read", gotGroup, "agent")
	}
	got := out.String()
	if !strings.Contains(got, "bob-shell/2.0.5") {
		t.Errorf("output does not name the agent it reports:\n%s", got)
	}
	// The OTHER agent's figures must not appear. This is the whole point of the flag, and the
	// failure it guards is the one the billing-unit work exists to prevent: Bob's 8 requests
	// reading as claude-code's 1,049, or the two totals summed.
	//
	// "1049", NOT "1,049": this surface's counts go through plainCount, which is %d and inserts
	// no separator, so the comma-formatted spelling could never appear and that operand could
	// never fire. The TUI's formatCount is the one that commas; asserting its output shape
	// against this writer's is how an operand ends up unable to fail.
	if strings.Contains(got, "1049") || strings.Contains(got, "146.36") {
		t.Errorf("output leaked the other agent's figures:\n%s", got)
	}
	// "8 requests", NOT a bare "8", which the 38.7k token cell satisfies on its own — a mutant
	// zeroing the scoped Requests left the bare form passing.
	if !strings.Contains(got, "8 requests") {
		t.Errorf("output missing this agent's request count:\n%s", got)
	}
	// AND THE UNPRICED AGENT READS "cost unavailable", never "$0.00" — the rule the AGENTS pane
	// keeps with "—", asserted here because scopeToAgent's own comment says the two surfaces
	// cannot disagree. Bob is priceable-but-unpriced in the fixture above (priceableRequests, no
	// pricedRequests), which is the case that read as free while the window's Priced bit
	// travelled along with the copy.
	if strings.Contains(got, "$0.00") {
		t.Errorf("an agent nothing priced was reported as free rather than unavailable:\n%s", got)
	}
	if !strings.Contains(got, "cost unavailable") {
		t.Errorf("output does not say cost is unavailable for an agent nothing priced:\n%s", got)
	}
}

// The scoped JSON narrows every statement about where the totals came from, and names its axis.
//
// A SCRIPT IS THE READER THAT CANNOT EYEBALL THE MISMATCH. scopeToAgent replaces Totals, so a
// whole-window `priced` or a whole-window by-model map left riding along beside one agent's
// figures is a claim about other agents' traffic attached to this agent's numbers — and unlike
// the human path there is no prose next to it to hedge. The maps are dropped rather than
// narrowed because a bucket's series is keyed by agent and carries no per-model split, so no
// honest per-agent value exists to put there.
//
// `agent` and `ungroupedCostMicros` are the two fields this axis adds, and both are asserted
// here rather than only in the human summary: the text twin was already guarded and the JSON
// half was not, which is how a field that is the whole point of the flag shipped with no test.
func TestRunCost_JSONScopedToAnAgentNarrowsProvenanceAndNamesTheAgent(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"totals":{"requests":1057,"tokens":298000000,"costMicros":146361600,
		          "pricedRequests":1048,"priceableRequests":1055,"incompleteRequests":4},
		"pricedBy":{"authoritative":1048},
		"unpricedBy":{"api.openai.com gpt-5":7},
		"incompleteBy":{"truncated_stream":4},
		"ungroupedCostMicros":750000,
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":1049,"tokens":297961318,"costMicros":146361600,
		                          "pricedRequests":1048,"priceableRequests":1048},
		   "bob-shell/2.0.5":{"requests":8,"tokens":38682,"priceableRequests":7}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--agent", "bob-shell/2.0.5", "--json"},
		&out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		Agent               string           `json:"agent"`
		Priced              bool             `json:"priced"`
		PricedBy            map[string]int64 `json:"pricedBy"`
		UnpricedBy          map[string]int64 `json:"unpricedBy"`
		IncompleteBy        map[string]int64 `json:"incompleteBy"`
		UngroupedCostMicros *int64           `json:"ungroupedCostMicros"`
		Totals              struct {
			Requests       int64 `json:"requests"`
			CostMicros     int64 `json:"costMicros"`
			PricedRequests int64 `json:"pricedRequests"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	// The axis, named. Without it a stored document cannot say which agent it describes, and two
	// runs against different agents are byte-comparable in every other respect.
	if decoded.Agent != "bob-shell/2.0.5" {
		t.Errorf("agent = %q, want the agent the figures describe:\n%s", decoded.Agent, out.String())
	}
	// FALSE, because nothing priced this agent — even though the window was priced. This is the
	// machine-path twin of "cost unavailable" and the reason the field cannot simply be copied.
	if decoded.Priced {
		t.Errorf("priced = true for an agent with no priced requests, so a script reads $0.00 as free:\n%s",
			out.String())
	}
	if decoded.Totals.PricedRequests != 0 || decoded.Totals.Requests != 8 {
		t.Errorf("totals = %+v, want this agent's 8 requests and no priced ones:\n%s",
			decoded.Totals, out.String())
	}
	// Absent, not narrowed and not inherited. Checked on the raw bytes too, because a nil map and
	// an absent key decode identically and only one of them is what omitempty promises.
	if decoded.PricedBy != nil || decoded.UnpricedBy != nil || decoded.IncompleteBy != nil {
		t.Errorf("by-model maps survived the scoping: pricedBy=%v unpricedBy=%v incompleteBy=%v\n%s",
			decoded.PricedBy, decoded.UnpricedBy, decoded.IncompleteBy, out.String())
	}
	for _, absent := range []string{"pricedBy", "unpricedBy", "incompleteBy"} {
		if strings.Contains(out.String(), absent) {
			t.Errorf("--json emitted %q beside one agent's totals, where it describes the whole window:\n%s",
				absent, out.String())
		}
	}
	// And the residual IS carried here, because group=agent is reconcilable. Present and equal to
	// the server's figure — not folded into this agent's cost, which is zero.
	if decoded.UngroupedCostMicros == nil || *decoded.UngroupedCostMicros != 750000 {
		t.Errorf("ungroupedCostMicros = %v, want the 750000 no agent carries:\n%s",
			decoded.UngroupedCostMicros, out.String())
	}
	if decoded.Totals.CostMicros != 0 {
		t.Errorf("costMicros = %d, want the residual stated beside this agent's total and never folded in:\n%s",
			decoded.Totals.CostMicros, out.String())
	}
}

// Without --agent neither field appears, and that absence is load-bearing.
//
// The default axis is group=none, which is not reconcilable, so a residual cannot arrive and an
// `agent` key would name an axis the command did not take. A script reads the absence of
// ungroupedCostMicros as "the breakdown reconciles"; emitting it on a path that asked for no
// breakdown quietly retracts that. This is the pair to the test above and the JSON half of the
// rule TestRunCost_WithoutAgentNoResidualLineIsPrinted pins for the human summary.
func TestRunCost_JSONWithoutAgentCarriesNeitherTheAxisNorTheResidual(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","priced":true,
		"totals":{"requests":10,"costMicros":1240000,"pricedRequests":10,"priceableRequests":10},
		"ungroupedCostMicros":750000}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	for _, absent := range []string{"agent", "ungroupedCostMicros"} {
		if strings.Contains(out.String(), absent) {
			t.Errorf("--json emitted %q on the default axis, which took no agent and can carry no residual:\n%s",
				absent, out.String())
		}
	}
}

// An unknown --agent fails and names the agents that do exist.
//
// A bare "not found" would leave the reader guessing at a string they cannot see — and the
// strings here are User-Agents, so they are neither short nor guessable. The label to type is
// exactly what the error has in hand, so withholding it would be a choice.
func TestRunCost_UnknownAgentNamesTheOnesThatExist(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"totals":{"requests":8},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":1},
		   "bob-shell/2.0.5":{"requests":7}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", srv.URL, "--agent", "bob"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for an agent that was not seen; stdout = %s", out.String())
	}
	msg := errOut.String()
	for _, want := range []string{"bob", "claude-code/2.1.270", "bob-shell/2.0.5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

// --agent discloses cost that belongs to no agent, when there is any.
//
// THIS IS THE OBLIGATION group=agent BRINGS. runCost's default axis is GroupNone, which is
// non-reconcilable, so no residual can ever arrive — and cmd_cost.go says in as many words
// that a future change of axis inherits the disclosure. group=agent IS reconcilable, so
// usage.Snapshot.UngroupedCostMicros can be non-zero: cost the totals include and no agent
// carries. Without a word about it, a reader adding up --agent for every agent and comparing
// that against plain `agentop cost` finds a shortfall with nothing to explain it.
// THE RESIDUAL MUST DIFFER FROM THIS AGENT'S OWN COST, and the first version of this test did
// not arrange that: it set both to 1_500_000 micros, so "1.50" appeared in the headline whether
// or not the disclosure printed. Deleting the disclosure left this test GREEN — a dead guard
// reading as coverage, caught by mutating the line it was supposed to protect. The residual is
// $0.75 here against the agent's $1.50 so the assertion can only be satisfied by the line it is
// about, and the sentence is asserted beside the figure because a figure can coincide where a
// sentence cannot.
func TestRunCost_AgentDisclosesCostNoAgentCarries(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"totals":{"requests":10,"costMicros":4250000,"pricedRequests":10,"priceableRequests":10},
		"ungroupedCostMicros":750000,
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":6,"costMicros":2000000,"pricedRequests":6,"priceableRequests":6},
		   "bob-shell/2.0.5":{"requests":4,"costMicros":1500000,"pricedRequests":4,"priceableRequests":4}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--agent", "bob-shell/2.0.5"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "0.75") {
		t.Errorf("output does not disclose the $0.75 that no agent carries:\n%s", got)
	}
	if !strings.Contains(got, "no agent") {
		t.Errorf("output carries the figure but not what it means:\n%s", got)
	}
	// This agent's own $1.50 must still be the headline: the residual is stated BESIDE the
	// figure, never folded into it.
	if !strings.Contains(got, "1.50") {
		t.Errorf("output lost this agent's own cost:\n%s", got)
	}
}

// Without --agent no residual line is printed, even if the server sends the field.
//
// The disclosure is gated on the FLAG, not merely on the field being present, and this is the
// half a mutation would otherwise reach unchallenged: dropping the `agent != ""` term leaves
// every other test here green, because they all pass the flag. A server sending the field on a
// group=none answer would then print a line whose own explanation — that per-agent figures do
// not sum to the total — is nonsense on a document containing no per-agent figures.
func TestRunCost_WithoutAgentNoResidualLineIsPrinted(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","priced":true,
		"totals":{"requests":10,"costMicros":4250000,"pricedRequests":10,"priceableRequests":10},
		"ungroupedCostMicros":750000}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if got := out.String(); strings.Contains(got, "no agent") {
		t.Errorf("unscoped run printed a per-agent residual line:\n%s", got)
	}
}

// With no --agent the axis stays GroupNone, so the default path carries no residual.
//
// TestRunCost_AsksForAnAxisThatCannotCarryAResidual already asserts the group, and it drives
// the command WITHOUT the flag — so it keeps passing and is the reason --agent had to be
// opt-in rather than a change of default. This states the pairing explicitly, because the two
// tests only mean something together: one says the default is safe, the other says the flag
// takes on the duty.
func TestRunCost_WithoutAgentTheDefaultAxisIsUnchanged(t *testing.T) {
	var gotGroup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGroup = r.URL.Query().Get("group")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"window":"today","totals":{"requests":1},"priced":false}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if gotGroup != "" && gotGroup != string(usage.GroupNone) {
		t.Errorf("default asked for group=%q, want none — a reconcilable axis owes a residual disclosure", gotGroup)
	}
}

// --by prints a row per label, ordered by cost, with the axis it asked for on the wire.
func TestRunCost_ByAgentPrintsARowPerAgent(t *testing.T) {
	var gotGroup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGroup = r.URL.Query().Get("group")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"window":"today","group":"agent","priced":true,
			"totals":{"requests":1057,"costMicros":146361600,"pricedRequests":1048,"priceableRequests":1055},
			"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
			   "claude-code/2.1.270":{"requests":1049,"tokens":297961318,"costMicros":146361600,
			                          "pricedRequests":1048,"priceableRequests":1048},
			   "bob-shell/2.0.5":{"requests":8,"tokens":38682,"priceableRequests":7}}}]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if gotGroup != "agent" {
		t.Errorf("asked for group=%q, want agent", gotGroup)
	}
	got := out.String()
	for _, want := range []string{"claude-code/2.1.270", "bob-shell/2.0.5"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing row %q:\n%s", want, got)
		}
	}
	// Costliest first, which is usage.SortSeriesLabels' rule. Asserted by POSITION, because
	// both labels being present says nothing about the order a reader scans.
	if i, j := strings.Index(got, "claude-code/2.1.270"), strings.Index(got, "bob-shell/2.0.5"); i > j {
		t.Errorf("rows are not ordered by cost descending:\n%s", got)
	}
}

// An unpriced row renders "—", and a row priced at a rate of zero renders "$0.00".
//
// THE WHOLE POINT OF THE COLUMN, and it takes three rows to state. bob-shell here sent 8
// requests and nothing could price them — no rates ship for it — and "$0.00" would assert that
// its traffic was free. (Not "because it bills in credits": a credits endpoint with rates
// configured prices normally. An absent RATE is what leaves a row unpriced.) Distinguishing "no rate
// configured" from "cost was zero" is the rule this codebase keeps everywhere a figure may be
// unknown.
//
// freerate IS THAT DISTINCTION: pricedRequests > 0 with costMicros == 0. Keyed on
// PricedRequests it renders a figure; keyed on CostMicros it renders a dash. It is the only row
// whose cell differs between the two readings, so without it writeCostBreakdown's stated rule
// holds by accident and `if c.CostMicros > 0` passes. tui/agents_pane_test.go's
// TestAgentCostCell_UnpricedIsADashAndAZeroRateIsAFigure says the same thing for the pane; this
// is the CLI table's half of it.
//
// ASSERTED PER ROW, not over the whole table: "an em dash appears" and "$0.00 does not appear"
// are both satisfied with the two cells on each other's rows.
func TestRunCost_ByRendersUnpricedAsADashNotZero(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"totals":{"requests":1059,"costMicros":146361600,"pricedRequests":1051,"priceableRequests":1058},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":1049,"costMicros":146361600,"pricedRequests":1049,"priceableRequests":1049},
		   "freerate/1.0":{"requests":2,"pricedRequests":2,"priceableRequests":2},
		   "bob-shell/2.0.5":{"requests":8,"priceableRequests":7}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	for _, tc := range []struct {
		label, want, reject, why string
	}{
		{"claude-code/2.1.270", "$146.36", "—", "priced, with a cost"},
		{"freerate/1.0", "$0.00", "—", "priced at a rate of zero: a real figure, not an unknown"},
		{"bob-shell/2.0.5", "—", "$0.00", "nothing could price it: unknown, not free"},
	} {
		row := costTableRow(t, got, tc.label)
		if !strings.Contains(row, tc.want) {
			t.Errorf("%s (%s): row does not carry %q:\n%s", tc.label, tc.why, tc.want, row)
		}
		if strings.Contains(row, tc.reject) {
			t.Errorf("%s (%s): row carries %q:\n%s", tc.label, tc.why, tc.reject, row)
		}
	}
}

// costTableRow returns the one --by table row mentioning label.
//
// Fails on 0 or 2+ matches rather than taking the first: a cell assertion scoped to "the output"
// says nothing about which row carried it, and a label that also appears in the summary above
// the table would silently widen the scope back out.
func costTableRow(t *testing.T, out, label string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, label) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 line mentioning %q, got %d:\n%s", label, len(found), out)
	}
	return found[0]
}

// A grouping the window cannot serve is reported, not silently swallowed.
//
// The server DOWNGRADES rather than refusing — ledger-backed windows serve only the axes a Row
// has a field for, and core/sessionapi reports the grouping IN EFFECT instead of a 400, which is
// a considered decision and not one to undo from here. So the duty on this side is to notice:
// compare what was asked for against snap.Group and say so, naming the axes that do work.
// Without that the command prints an ungrouped total under a heading claiming a breakdown.
func TestRunCost_ByDisclosesAGroupingTheServerDowngraded(t *testing.T) {
	// Asked for host; the response says group=none, which is what a ledger window does.
	srv := fakeUsageServer(t, `{"window":"today","group":"none","priced":true,
		"totals":{"requests":10,"costMicros":5000000,"pricedRequests":10,"priceableRequests":10}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", srv.URL, "--by", "host"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	combined := out.String() + errOut.String()
	if !strings.Contains(combined, "host") {
		t.Errorf("output does not name the grouping that was refused:\n%s", combined)
	}
	// And it must name at least one axis that DOES work, or the reader is told no and given
	// nowhere to go.
	if !strings.Contains(combined, "agent") {
		t.Errorf("output does not name an axis that works:\n%s", combined)
	}
}

// An axis the server SERVED but that came back empty says so, in wording a downgrade does not use.
//
// TWO ANSWERS A READER MUST NOT CONFUSE: "this window cannot break down by agent" (a downgrade —
// snap.Group differs from what was asked) and "it can, and there was no traffic" (the axis was
// served; the buckets were empty). The response here ECHOES group=agent, so reportDowngrade
// returns false and this arm is the one that answers.
//
// Without the arm the command prints the AGENT/REQUESTS/TOKENS/COST heading with nothing under
// it — the ungrouped-total-under-a-breakdown-heading shape reportDowngrade's own godoc says this
// surface must not produce. So the heading's absence is asserted too, not just the note's
// presence: a note printed above a bare heading would still be that shape.
func TestRunCost_ByServedButEmptySaysSoAndIsNotADowngrade(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"totals":{"requests":0}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "no agent breakdown for this window") {
		t.Errorf("a served-but-empty axis printed no note saying so:\n%s", got)
	}
	// reportDowngrade's wording. Its appearance here would tell the reader the axis was refused
	// when the server in fact served it.
	if strings.Contains(got, "the server answered with") {
		t.Errorf("served-but-empty was reported as a downgrade:\n%s", got)
	}
	// "REQUESTS" uppercase is the breakdown heading and nothing else in this command prints it.
	if strings.Contains(got, "REQUESTS") {
		t.Errorf("printed a breakdown heading with no rows under it:\n%s", got)
	}
}

// --by and --agent are contradictory and refused.
//
// One asks for every label as a table, the other for a single label's figures. Silently letting
// one win would print an answer to a question the operator did not ask — and which one won
// would be an implementation detail.
func TestRunCost_ByAndAgentTogetherAreRefused(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":1},"priced":false}`)
	defer srv.Close()

	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", srv.URL, "--by", "agent", "--agent", "bob-shell/2.0.5"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for two contradictory flags; stdout = %s", out.String())
	}
	if msg := errOut.String(); !strings.Contains(msg, "--by") || !strings.Contains(msg, "--agent") {
		t.Errorf("error does not name both flags:\n%s", msg)
	}
}

// --by discloses cost that no label in the table carries.
//
// THIS is the case the residual exists for, and the one cmd_cost.go's own comment predicted: a
// table that sums to less than the headline above it, with nothing to explain the difference.
// The residual is $0.75 against a $4.25 total, deliberately not equal to any row, so only the
// disclosure can satisfy the assertion.
func TestRunCost_ByDisclosesCostNoLabelCarries(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"totals":{"requests":10,"costMicros":4250000,"pricedRequests":10,"priceableRequests":10},
		"ungroupedCostMicros":750000,
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":6,"costMicros":2000000,"pricedRequests":6,"priceableRequests":6},
		   "bob-shell/2.0.5":{"requests":4,"costMicros":1500000,"pricedRequests":4,"priceableRequests":4}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "0.75") {
		t.Errorf("table does not disclose the $0.75 no row carries:\n%s", got)
	}
	if !strings.Contains(got, "no ") {
		t.Errorf("figure is present but unexplained:\n%s", got)
	}
}

// An unknown --by names the axes that are accepted — and "none" is unknown.
//
// "none" IS THE SECOND CASE BECAUSE ParseGroup ACCEPTS IT: it returns GroupNone with a nil
// error, so the err check alone lets it through and `g == usage.GroupNone` is the only thing
// rejecting it. Without that arm --by none asks for a breakdown by nothing and prints a heading
// over an ungrouped total.
//
// ASSERTS THE WORDING because the message is the half that names the accepted axes. The endpoint
// is unreachable on purpose — the axis is parsed before any request, so a typo costs no
// connection.
func TestRunCost_UnknownByNamesTheAcceptedAxes(t *testing.T) {
	for _, by := range []string{"banana", "none"} {
		t.Run(by, func(t *testing.T) {
			var out, errOut strings.Builder
			code := runCost([]string{"--endpoint", "http://127.0.0.1:1", "--by", by}, &out, &errOut)
			if code == 0 {
				t.Fatal("exit = 0, want non-zero for an axis that does not exist")
			}
			msg := errOut.String()
			if !strings.Contains(msg, "is not an axis") {
				t.Errorf("--by %q was not refused as an axis; an unreachable endpoint exits non-zero too:\n%s", by, msg)
			}
			if !strings.Contains(msg, by) {
				t.Errorf("error does not echo the rejected value:\n%s", msg)
			}
			for _, want := range []string{"agent", "model", "endpoint"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error does not name the accepted axis %q:\n%s", want, msg)
				}
			}
			// currency IS ONE OF THEM, pinned in both constants.
			//
			// Nothing asserted this, so either list could quietly lose the axis the cross-unit
			// refusal sends readers to. costByAxes' own godoc says it exists "in one place so the
			// flag's help and its error message cannot list different sets" — a property no test
			// held it to. ledgerServedAxes is the same word on the downgrade path, where a reader
			// who was just refused a combined total is told which axes a ledger window can serve.
			if !strings.Contains(msg, "currency") {
				t.Errorf("the accepted axes omit currency, the axis the cross-unit refusal points "+
					"at:\n%s", msg)
			}
			for name, list := range map[string]string{
				"costByAxes":       costByAxes,
				"ledgerServedAxes": ledgerServedAxes,
			} {
				if !strings.Contains(list, "project") {
					t.Errorf("%s = %q, which does not name project", name, list)
				}
				if !strings.Contains(list, "currency") {
					t.Errorf("%s = %q, which does not name currency; the refusal would point at "+
						"an axis this command then rejects", name, list)
				}
			}
		})
	}
}

// longProjectLabel is 60 runes, longer than the 34-rune default LABEL column.
const longProjectLabel = "~/works/go/src/github.com/example/a-rather-long-repository-name"

// costTable runs `agentop cost --by <by>` against reply and returns the table lines:
// the header (which begins with the upper-case axis) and every line after it.
func costTable(t *testing.T, by, reply string) []string {
	t.Helper()
	srv := fakeUsageServer(t, reply)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", by}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	lines := strings.Split(out.String(), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "  "+strings.ToUpper(by)+" ") {
			var table []string
			for _, r := range lines[i:] {
				if r == "" {
					break
				}
				table = append(table, r)
			}
			return table
		}
	}
	t.Fatalf("no %s header in:\n%s", strings.ToUpper(by), out.String())
	return nil
}

const projectReply = `{"window":"1h","group":"project","priced":true,
	"totals":{"requests":6,"costMicros":6000,"pricedRequests":6,"priceableRequests":6},
	"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
	   "/repo/a":{"requests":3,"tokens":300,"costMicros":3000,"pricedRequests":3,"priceableRequests":3},
	   "unknown":{"requests":1,"tokens":100,"costMicros":1000,"pricedRequests":1,"priceableRequests":1},
	   "` + longProjectLabel + `":{"requests":2,"tokens":200,"costMicros":2000,"pricedRequests":2,"priceableRequests":2}}}]}`

// --by project prints a row per project, a long label whole.
func TestRunCost_ByProjectPrintsARowPerProject(t *testing.T) {
	table := costTable(t, "project", projectReply)
	if len(table) != 4 {
		t.Fatalf("want a header and 3 rows, got %d lines:\n%s", len(table), strings.Join(table, "\n"))
	}
	for _, want := range []string{"/repo/a", "unknown", longProjectLabel} {
		found := false
		for _, row := range table[1:] {
			if strings.HasPrefix(row, "  "+want+" ") {
				found = true
			}
		}
		if !found {
			t.Errorf("no row for %q:\n%s", want, strings.Join(table, "\n"))
		}
	}
}

// A label longer than the default column widens it for every row and the header.
// Only short labels keep the column at 34.
func TestRunCost_ByAlignsALongLabel(t *testing.T) {
	table := costTable(t, "project", projectReply)
	end := strings.Index(table[0], "REQUESTS") + len("REQUESTS")
	if want := 2 + len(longProjectLabel) + 1 + 10; end != want {
		t.Errorf("REQUESTS ends at %d, want %d:\n%s", end, want, strings.Join(table, "\n"))
	}
	for _, row := range table[1:] {
		f := strings.Fields(row)
		if len(row) < end || !strings.HasSuffix(row[:end], " "+f[1]) {
			t.Errorf("requests cell does not end under REQUESTS (col %d):\n%s", end, strings.Join(table, "\n"))
		}
	}

	short := costTable(t, "agent", `{"window":"1h","group":"agent","priced":true,
		"totals":{"requests":1,"costMicros":1000,"pricedRequests":1,"priceableRequests":1},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "bob":{"requests":1,"tokens":10,"costMicros":1000,"pricedRequests":1,"priceableRequests":1}}}]}`)
	if want := "  " + fmt.Sprintf("%-34s", "AGENT") + " "; !strings.HasPrefix(short[0], want) {
		t.Errorf("short labels: header = %q, want prefix %q", short[0], want)
	}
}

// --by --json carries the axis, the folded series and the residual.
//
// THE HALF WITH NO READER UNTIL NOW. The human table is well covered, but costJSON is the shape
// the struct's own comments argue hardest for — "a script is the reader that needs it most" — and
// --by's three JSON-only producers (seriesForBreakdown, costJSON.By, and ungroupedForBreakdown's
// by term) were reachable with nothing pointed at them. Each is asserted here by VALUE, not by
// presence: a `by` key holding "" and a `series` key holding null both satisfy "the key is there"
// while telling a script nothing.
//
// Decoded into map[string]any rather than costJSON, because unmarshalling into the struct that
// produced it would agree with any renaming the struct made. The keys a script reads are the
// assertion.
func TestRunCost_ByJSONCarriesTheAxisTheSeriesAndTheResidual(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"totals":{"requests":1057,"costMicros":146361600,"pricedRequests":1048,"priceableRequests":1055},
		"ungroupedCostMicros":750000,
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":1049,"tokens":297961318,"costMicros":146361600,
		                          "pricedRequests":1048,"priceableRequests":1048}}},
		          {"at":"2026-09-27T11:00:00Z","series":{
		   "bob-shell/2.0.5":{"requests":8,"tokens":38682,"priceableRequests":7}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "agent", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}

	// The axis itself. Without it a consumer sees labels with no dimension, and
	// "claude-code/2.1.270" and "api.anthropic.com" are both just strings.
	if by, _ := got["by"].(string); by != "agent" {
		t.Errorf("by = %q, want %q", by, "agent")
	}

	// The series, keyed on that axis and FOLDED ACROSS BUCKETS — the two labels arrive in
	// different buckets above, so a producer handing over snap.Buckets verbatim, or one taking
	// only the last bucket, fails here rather than looking plausible.
	series, ok := got["series"].(map[string]any)
	if !ok {
		t.Fatalf("series is absent or not an object: %#v", got["series"])
	}
	// pricedRequests IS THE FIELD THAT SEPARATES THE TWO READINGS, not costMicros. costMicros is
	// omitempty (usage.go:165), so "nothing priced this label" and "priced at a rate of zero" both
	// decode to 0 from it and an assertion on it cannot tell them apart — zeroing PricedRequests
	// across the fold passed this whole package before this line existed. The machine-path twin of
	// the TUI's four-row agentCostCell table.
	for label, want := range map[string]struct{ cost, priced float64 }{
		"claude-code/2.1.270": {cost: 146361600, priced: 1048},
		// Unpriced: requests but nothing priced them, so both the money fields and pricedRequests
		// are omitempty-absent. pricedRequests is what makes this row distinguishable from a
		// priced-at-zero one, which carries pricedRequests > 0 with the same absent cost.
		"bob-shell/2.0.5": {cost: 0, priced: 0},
	} {
		entry, ok := series[label].(map[string]any)
		if !ok {
			t.Errorf("series is missing label %q: %#v", label, series)
			continue
		}
		cost, _ := entry["costMicros"].(float64)
		if cost != want.cost {
			t.Errorf("series[%q].costMicros = %v, want %v", label, cost, want.cost)
		}
		priced, _ := entry["pricedRequests"].(float64)
		if priced != want.priced {
			t.Errorf("series[%q].pricedRequests = %v, want %v — this is the field that tells "+
				"\"nothing priced it\" from \"priced at a rate of zero\"", label, priced, want.priced)
		}
	}

	// The residual, which --by now owes for the same reason --agent does: without it a script
	// summing the series against the total finds a shortfall with nothing to explain it.
	if res, _ := got["ungroupedCostMicros"].(float64); res != 750000 {
		t.Errorf("ungroupedCostMicros = %v, want 750000", res)
	}
}

// Without --by, --json serialises none of the three breakdown keys.
//
// THE OTHER HALF OF THE CONTRACT, and the reason the positive test above is not enough: costJSON
// spends forty lines arguing that the ABSENCE of these keys means "no breakdown was asked for",
// so a producer that emitted `"by":""` or `"series":null` on the default path would break a
// promise while still passing any present-and-correct assertion. Same fixture, flag removed.
func TestRunCost_WithoutByJSONCarriesNoBreakdownKeys(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"none","priced":true,
		"totals":{"requests":1057,"costMicros":146361600,"pricedRequests":1048,"priceableRequests":1055},
		"ungroupedCostMicros":750000,
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":1049,"costMicros":146361600,
		                          "pricedRequests":1048,"priceableRequests":1048}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	// ungroupedCostMicros is in the fixture and still must not be serialised: the field's own
	// comment says the default path asks for group=none, which cannot produce a residual, so a
	// value arriving there means the producer changed and dropping it keeps the promise.
	for _, absent := range []string{"by", "series", "ungroupedCostMicros"} {
		if _, present := got[absent]; present {
			t.Errorf("default path serialised %q = %#v; its absence is what means "+
				"\"no breakdown was asked for\"", absent, got[absent])
		}
	}
}

// --agent does not carry the WINDOW's provenance beside ONE AGENT's figure.
//
// A MISATTRIBUTION, NOT A MISSING FEATURE. pricedBy, unpricedBy and incompleteBy describe the
// whole window — only the ring populates them, keyed by reason rather than by agent, so a
// snapshot holds nothing to re-derive one agent's share from. Copied through unchanged they sit
// beside a costMicros that is one agent's, and the human path is worse than the machine one:
// writeCostSummary prints "N of M priced requests carry an inexact figure" from the AGENT's
// totals and then indents the WINDOW's reasons under it, which can account for more requests
// than the line above them.
//
// This is writeCostSummary's own rule applied to itself — "a caveat printed beside a figure it is
// not about is not a warning but a misattribution". The fixture is a ring-shaped window (a
// duration, which is the kind that populates the maps at all) and the agent asked for owns 4 of
// the window's 10 requests, so a leaked map is arithmetically visible and not just present.
func TestRunCost_AgentDropsTheWindowsProvenance(t *testing.T) {
	body := `{"window":"1h","group":"agent","priced":true,
		"totals":{"requests":10,"costMicros":1240000,"pricedRequests":7,"priceableRequests":10},
		"pricedBy":{"authoritative":4,"bundled":3},
		"unpricedBy":{"api.openai.com gpt-5":3},
		"incompleteBy":{"floor":2},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":4,"costMicros":500000,"pricedRequests":4,"priceableRequests":4},
		   "bob-shell/2.0.5":{"requests":6,"priceableRequests":6}}}]}`

	t.Run("json carries no window-wide map", func(t *testing.T) {
		srv := fakeUsageServer(t, body)
		defer srv.Close()
		var out, errOut strings.Builder
		if code := runCost([]string{"--endpoint", srv.URL, "--agent", "claude-code/2.1.270", "--json"},
			&out, &errOut); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
			t.Fatalf("output is not JSON: %v\n%s", err, out.String())
		}
		// The agent's own figure must still be there — otherwise this test would pass against a
		// scopeToAgent that returned an empty snapshot.
		if agent, _ := got["agent"].(string); agent != "claude-code/2.1.270" {
			t.Fatalf("agent = %q, want the one asked for", agent)
		}
		totals, ok := got["totals"].(map[string]any)
		if !ok {
			t.Fatalf("totals is absent or not an object: %#v", got["totals"])
		}
		if cost, _ := totals["costMicros"].(float64); cost != 500000 {
			t.Fatalf("totals.costMicros = %v, want this agent's 500000 (not the window's 1240000)", cost)
		}
		for _, leaked := range []string{"pricedBy", "unpricedBy", "incompleteBy"} {
			if v, present := got[leaked]; present {
				t.Errorf("--agent shipped the window's %q = %#v beside one agent's costMicros",
					leaked, v)
			}
		}
	})

	// NO SUBTEST HERE FOR THE Priced NARROWING, deliberately. main already landed both halves —
	// TestRunCost_AgentReportsThatAgentOnly asserts "cost unavailable" and never "$0.00" on the
	// human path, and TestRunCost_JSONScopedToAnAgentNarrowsProvenanceAndNamesTheAgent asserts
	// priced=false on the machine path, each for this same priceable-but-unpriced shape. Mutant
	// M28 (scopeToAgent stops narrowing Priced) dies on those. A second pair here would be two
	// names for one rule, which is the duplication this branch dropped its own agentCostCell
	// copies to avoid.

	t.Run("human summary carries no window-wide reason", func(t *testing.T) {
		srv := fakeUsageServer(t, body)
		defer srv.Close()
		var out, errOut strings.Builder
		if code := runCost([]string{"--endpoint", srv.URL, "--agent", "claude-code/2.1.270"},
			&out, &errOut); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
		}
		got := out.String()
		// The named gap and the inexactness reason are the two the window's maps would supply.
		for _, leaked := range []string{"api.openai.com gpt-5", "floor"} {
			if strings.Contains(got, leaked) {
				t.Errorf("--agent printed the window's caveat %q beside one agent's figure:\n%s",
					leaked, got)
			}
		}
	})
}

// A window spanning two units does NOT get a single total.
//
// THIS IS THE WHOLE POINT OF BILLING UNITS. Summing credits into dollars produces a number that is
// neither, and it looks exactly like a correct one — larger, never obviously wrong. Measured on one
// real day before this existed: $146.3616 of Claude Code spend would have silently absorbed 0.0774
// credits of Bob spend, and one scalar just looks slightly bigger where two subtotals side by side
// would look obviously wrong.
//
// So the headline is WITHHELD rather than qualified. A caveat under a wrong figure still leaves the
// wrong figure on screen, and this is the one disclosure on this surface where the number itself
// cannot be salvaged.
func TestRunCost_RefusesACombinedTotalAcrossUnits(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","priced":true,
		"currencies":["USD","credits"],
		"totals":{"requests":1057,"costMicros":146439200,"pricedRequests":1055,"priceableRequests":1055}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	// The combined figure must not appear in ANY of its spellings.
	for _, forbidden := range []string{"$146.4392", "$146.44", "146.4392"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("a cross-unit total was printed as %q; it is neither dollars nor credits:\n%s",
				forbidden, got)
		}
	}
	// And it must name BOTH units, or the reader cannot tell what the window actually holds.
	for _, want := range []string{"USD", "credits"} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not name the unit %q:\n%s", want, got)
		}
	}
	// Tokens and requests are still reported: they are unit-free, and they are the one comparison
	// that stays legal across gateways. Withholding them would be over-refusal.
	// plainCount, so no thousands separator on this surface — asserted as the figure it actually
	// prints rather than as the one a reader might expect.
	if !strings.Contains(got, "1057 requests") {
		t.Errorf("requests were withheld; they carry no unit and stay comparable:\n%s", got)
	}
}

// One unit — every deployment today — prints exactly as it did before.
//
// The refusal must be a signal, not furniture. A single-currency window, and a producer that does
// not report currencies at all (the in-memory ring), both have to keep the headline: turning an
// absent field into a refusal would break every existing caller on traffic that is perfectly
// summable.
func TestRunCost_OneUnitOrNoneStillPrintsTheTotal(t *testing.T) {
	for _, tc := range []struct{ name, currencies string }{
		{"the field is absent, as the ring leaves it", ""},
		{"exactly one unit", `"currencies":["USD"],`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeUsageServer(t, `{"window":"today","priced":true,`+tc.currencies+
				`"totals":{"requests":318,"costMicros":4170000,"pricedRequests":318,"priceableRequests":318}}`)
			defer srv.Close()

			var out, errOut strings.Builder
			if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
			}
			got := out.String()
			if !strings.Contains(got, "$4.17") {
				t.Errorf("a summable window lost its total:\n%s", got)
			}
			// THE STRINGS PRODUCTION ACTUALLY PRINTS. This read "two units" and "not a figure",
			// and neither appears anywhere in this command — the headline is "%d units", with a
			// digit, and the caveat is "which cannot be added". So the assertion guarding the one
			// direction that breaks existing users could not fail under any input. Both strings
			// below are lifted from writeCostSummary's own format strings.
			if strings.Contains(got, " units") || strings.Contains(got, "cannot be added") {
				t.Errorf("a summable window was refused:\n%s", got)
			}
		})
	}
}

// --by currency labels each cell in the unit its OWN ROW names.
//
// THE SURFACE THE REFUSAL POINTS AT. writeCostSummary withholds a mixed window's headline and says
// "use --by currency for a figure per unit"; every cell in that table was formatted by costUSD,
// which hard-codes "$", so the credits row came back as "$0.08" — a withheld figure replaced by a
// wrong one, on the strength of this command's own advice.
//
// THE currency AXIS IS THE ONE THAT CAN PRINT FIGURES FOR A MIXED WINDOW, because the row label IS
// the unit. That is exactly why the refusal names it.
func TestRunCost_ByCurrencyLabelsEachRowInItsOwnUnit(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"currency","priced":true,
		"currencies":["USD","credits"],
		"totals":{"requests":1057,"costMicros":146439000,"pricedRequests":1057,"priceableRequests":1057},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "USD":{"requests":1000,"costMicros":146361600,"pricedRequests":1000,"priceableRequests":1000},
		   "credits":{"requests":57,"costMicros":77400,"pricedRequests":57,"priceableRequests":57}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "currency"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	// Scoped to the row, so a "$" anywhere on it fails. The defect was a "$" in the COST cell,
	// several columns from the label that names the unit.
	creditsLine := tableRow(t, got, "credits")
	if strings.Contains(creditsLine, "$") {
		t.Errorf("the credits row claims dollars:\n%s", creditsLine)
	}
	if !strings.Contains(creditsLine, "0.08 credits") {
		t.Errorf("the credits row lost its figure or its unit:\n%s", creditsLine)
	}
	// And the USD row keeps "$" character-for-character, which is what keeps this from costing
	// every existing reader the label they had.
	if usdLine := tableRow(t, got, "USD"); !strings.Contains(usdLine, "$146.36") {
		t.Errorf("the USD row lost its dollar figure:\n%s", usdLine)
	}
	// Every row here is a real unit, so the table withholds nothing and has nothing to explain.
	// Pins the gate to the cells actually withheld rather than to the window being mixed, which is
	// true of every window this axis exists for.
	for _, ln := range strings.Split(breakdownSection(t, got, "currency"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "!") {
			t.Errorf("a caveat under a table that withheld nothing:\n%s", got)
		}
	}
}

// On any OTHER axis a mixed window withholds each cell, because a row may itself span units.
//
// One agent calling two gateways is a row whose costMicros is the cross-unit sum the headline just
// refused, and a folded series cannot separate it. So the cell gets the headline's posture rather
// than a label — withheld, with the units named once under the table.
func TestRunCost_ByAgentOnAMixedWindowWithholdsTheCostCells(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"currencies":["USD","credits"],
		"totals":{"requests":1057,"costMicros":146439000,"pricedRequests":1057,"priceableRequests":1057},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":1000,"costMicros":146361600,"pricedRequests":1000,"priceableRequests":1000},
		   "bob-shell/2.0.5":{"requests":57,"costMicros":77400,"pricedRequests":57,"priceableRequests":57}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	if strings.Contains(got, "$146.36") || strings.Contains(got, "$0.08") {
		t.Errorf("a per-agent cell was labelled in dollars on a mixed window:\n%s", got)
	}
	if !strings.Contains(got, mixedCostCell) {
		t.Errorf("no withheld cell; a figure that may span units was printed anyway:\n%s", got)
	}
	// Said once, under the table, naming the units and the axis that resolves them. A column of
	// "(mixed)" with nothing explaining it reads as a defect in the tool.
	//
	// SCOPED TO THE TABLE'S OWN OUTPUT, not asserted against the whole document, and that is the
	// point rather than a tidiness preference. Two rounds of this test picked a STRING meant to be
	// unique to writeCostBreakdown and both were wrong: "cannot be added" and then "use --by
	// currency for a figure per unit" are each printed by writeCostSummary too, which runs first — so
	// the assertion passed on the summary's caveat while the breakdown's was suppressed. Choosing a
	// third string would be the same bet a third time. breakdownSection removes the possibility
	// instead: nothing the summary prints is inside what it returns, so a shared phrase cannot
	// satisfy these assertions however the wording drifts.
	section := breakdownSection(t, got, "agent")
	if !strings.Contains(section, "these rows hold") {
		t.Errorf("the withheld column is unexplained by the table's own caveat:\n%s", got)
	}
	if !strings.Contains(section, "use --by currency for a figure per unit") {
		t.Errorf("the withheld column does not point at the axis that resolves it:\n%s", got)
	}
}

// breakdownSection is the part of `agentop cost` output that writeCostBreakdown produced.
//
// EXISTS BECAUSE THE TWO SURFACES SHARE VOCABULARY. writeCostSummary prints a caveat naming the
// same units and pointing at the same flag, immediately above this table, and it runs first — so a
// Contains over the whole document cannot tell which surface satisfied it. That is not hypothetical:
// it is how one mutant went from killed to surviving between rounds, and then how its replacement
// assertion was dead on arrival.
//
// CUT AT THE TABLE HEADER, which writeCostBreakdown emits as the uppercased axis name beside
// REQUESTS / TOKENS / COST, and everything from there on belongs to the table — its rows, its
// caveat and its residual note. Located by content rather than by a line offset, so it survives any
// edit to the summary above it.
//
// FAILS LOUDLY when the header is absent, for the DIAGNOSTIC and not for the outcome. An earlier
// draft of this comment claimed the Fatalf is what stops an empty section passing vacuously; it is
// not, and the claim was the same kind of unchecked assertion this helper was written to remove.
// Returning "" would already fail both Contains assertions below — so the test fails either way,
// and this only replaces "missing: these rows hold" with the reason the section was empty. Its
// mutant is therefore equivalent by construction, and is recorded as such rather than chased with
// a fixture that reaches it.
func breakdownSection(t *testing.T, out, asked string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, strings.ToUpper(asked)) && strings.Contains(ln, "REQUESTS") {
			section := strings.Join(lines[i:], "\n")
			// The scoping is the property, so it is asserted rather than assumed: the summary's own
			// caveat must be OUTSIDE what this returns, or the helper gives the false confidence it
			// was written to replace.
			if strings.Contains(section, "no combined figure is shown") {
				t.Fatalf("breakdownSection captured writeCostSummary's caveat, so anything asserted "+
					"inside it may be the summary's:\n%s", section)
			}
			return section
		}
	}
	t.Fatalf("no %s breakdown table in the output, so the table's own caveat cannot be asserted:\n%s",
		asked, out)
	return ""
}

// A window in ONE non-USD unit prints its total in that unit, not behind a "$".
//
// The other half of the same defect, and the one no finding named: a credits-only deployment has
// exactly one currency, so it is not "mixed", so it took the ordinary headline path and printed
// costUSD's "$" over credits — with nothing anywhere on the surface to contradict it. A single unit
// is the case with no refusal to fall back on, which makes the label the only thing carrying the
// fact.
func TestRunCost_ASingleNonUSDUnitIsLabelledNotDollared(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","priced":true,"currencies":["credits"],
		"totals":{"requests":57,"tokens":38682,"costMicros":77400,"pricedRequests":57,
		"priceableRequests":57,"avoidedMicros":12000}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	if strings.Contains(got, "$") {
		t.Errorf("a credits-only window printed a dollar glyph:\n%s", got)
	}
	if !strings.Contains(got, "0.08 credits") {
		t.Errorf("the headline lost its unit:\n%s", got)
	}
	// The saving is money too, and it took costUSD as well.
	if !strings.Contains(got, "0.01 credits") {
		t.Errorf("the saving is not labelled in the window's unit:\n%s", got)
	}
	// And it is NOT refused: one unit is summable, which is the direction that would break every
	// deployment that exists today.
	if strings.Contains(got, "cannot be added") {
		t.Errorf("a single-unit window was refused:\n%s", got)
	}
}

// --json carries the units, so the reader nobody eyeballs can make the same refusal.
//
// The human path withheld and the machine path emitted the sum with no field naming the conflict.
// Present for a single unit too, which is how a credits deployment learns what its
// own total is denominated in.
func TestRunCost_JSONCarriesTheCurrencies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		currencies string
		want       []string
	}{
		{"mixed, the case the human path refuses", `"currencies":["USD","credits"],`, []string{"USD", "credits"}},
		{"a single unit answers \"in what\"", `"currencies":["credits"],`, []string{"credits"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeUsageServer(t, `{"window":"today","priced":true,`+tc.currencies+
				`"totals":{"requests":1057,"costMicros":146439000,"pricedRequests":1057,"priceableRequests":1057}}`)
			defer srv.Close()

			var out, errOut strings.Builder
			if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
			}
			// Decoded rather than string-matched, so this pins the FIELD a script reads and not
			// merely the presence of the words somewhere in the document.
			var doc struct {
				Currencies []string `json:"currencies"`
			}
			if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
				t.Fatalf("the document does not decode: %v\n%s", err, out.String())
			}
			if len(doc.Currencies) != len(tc.want) {
				t.Fatalf("currencies = %v, want %v", doc.Currencies, tc.want)
			}
			for i := range tc.want {
				if doc.Currencies[i] != tc.want[i] {
					t.Errorf("currencies = %v, want %v", doc.Currencies, tc.want)
				}
			}
		})
	}
}

func TestRunCost_JSONOmitsCurrenciesWhenTheProducerDoesNotReportThem(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","priced":true,
		"totals":{"requests":318,"costMicros":4170000,"pricedRequests":318,"priceableRequests":318}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if strings.Contains(out.String(), "currencies") {
		t.Errorf("a window named a currencies key:\n%s", out.String())
	}
}

// --agent on a mixed window says WHOSE mixture it is.
func TestRunCost_AgentOnAMixedWindowNamesTheWindowAsTheSource(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"currencies":["USD","credits"],
		"totals":{"requests":1057,"costMicros":146439000,"pricedRequests":1057,"priceableRequests":1057},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":1000,"costMicros":146361600,"pricedRequests":1000,"priceableRequests":1000}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", srv.URL, "--agent", "claude-code/2.1.270"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	if !strings.Contains(got, "window's mixture") {
		t.Errorf("the refusal does not say it is the window's mixture and not this agent's:\n%s", got)
	}
	// Still withheld: the safe direction, since this agent's own traffic may be the mixed part.
	if strings.Contains(got, "$146.36") {
		t.Errorf("a per-agent figure was printed from a mixed window:\n%s", got)
	}
}

// tableRow returns the breakdown row whose LABEL is exactly label.
//
// Matched on the row's first field rather than with Contains, and that is the point: a unit name
// also appears in the caveat above the table, so "the credits row carries no $" is unprovable by
// substring — it would read the caveat line and pass for the wrong reason. Scoped to one line
// because the claim is about one cell, and the USD row of the same table legitimately has a "$".
func tableRow(t *testing.T, out, label string) string {
	t.Helper()
	var hits []string
	for _, ln := range strings.Split(out, "\n") {
		if fields := strings.Fields(ln); len(fields) > 0 && fields[0] == label {
			hits = append(hits, ln)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly one table row labelled %q, got %d:\n%s", label, len(hits), out)
	}
	return hits[0]
}

// A series label that is not a reported unit does not become one in the --by currency table.
//
// The label is "(other)", the band a capped series folds into, in a window that reports only USD.
// Taking every row's label as its unit would render "0.08 (other)"; the window's own unit is the
// right answer, and is what this cell rendered before units existed.
//
// ASSERTED IN BOTH DIRECTIONS, because the guard has two ways to be wrong: swallowing a real unit
// (the credits row must keep its label) and trusting a fake one (the capped row must not get one).
func TestRunCost_ByCurrencyDoesNotTreatTheOverflowLabelAsAUnit(t *testing.T) {
	// A USD-only window whose series include a label that is not a unit.
	srv := fakeUsageServer(t, `{"window":"today","group":"currency","priced":true,
		"currencies":["USD"],
		"totals":{"requests":1057,"costMicros":146439000,"pricedRequests":1057,"priceableRequests":1057},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "USD":{"requests":1000,"costMicros":146361600,"pricedRequests":1000,"priceableRequests":1000},
		   "(other)":{"requests":57,"costMicros":77400,"pricedRequests":57,"priceableRequests":57}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "currency"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	other := tableRow(t, got, "(other)")
	if strings.Contains(other, "0.08 (other)") {
		t.Errorf("the capped row's figure is labelled with the overflow label, which normaliseUnit "+
			"could never have accepted as a unit:\n%s", other)
	}
	// It falls back to the WINDOW's unit, which for a single-currency deployment is the right
	// answer and is byte-identical to what this cell rendered before units existed.
	if !strings.Contains(other, "$0.08") {
		t.Errorf("the capped row lost the figure it had before billing units:\n%s", other)
	}
	if usd := tableRow(t, got, "USD"); !strings.Contains(usd, "$146.36") {
		t.Errorf("the real unit's row changed:\n%s", usd)
	}
}

// And in a MIXED window the capped row is withheld rather than given either unit.
//
// The other direction of the same fallthrough: there is no window unit to fall back to, and a
// capped row folds rows that each had a real unit and may well span two — so its figure is exactly
// the cross-unit sum the headline refused.
func TestRunCost_ByCurrencyWithholdsACappedRowOnAMixedWindow(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"currency","priced":true,
		"currencies":["USD","credits"],
		"totals":{"requests":1114,"costMicros":146516400,"pricedRequests":1114,"priceableRequests":1114},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "USD":{"requests":1000,"costMicros":146361600,"pricedRequests":1000,"priceableRequests":1000},
		   "credits":{"requests":57,"costMicros":77400,"pricedRequests":57,"priceableRequests":57},
		   "(other)":{"requests":57,"costMicros":77400,"pricedRequests":57,"priceableRequests":57}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "currency"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	if other := tableRow(t, got, "(other)"); !strings.Contains(other, mixedCostCell) {
		t.Errorf("a capped row in a mixed window was given a unit rather than withheld:\n%s", other)
	}
	// The real units keep their own figures — the guard must not swallow them.
	if credits := tableRow(t, got, "credits"); !strings.Contains(credits, "0.08 credits") {
		t.Errorf("the credits row lost its label:\n%s", credits)
	}
	if usd := tableRow(t, got, "USD"); !strings.Contains(usd, "$146.36") {
		t.Errorf("the USD row lost its figure:\n%s", usd)
	}
	// The withheld cell is explained under the table, and NOT with the other axes' advice: that
	// sentence ends "use --by currency for a figure per unit", which is the table the reader is
	// already looking at. Scoped by breakdownSection because the summary above prints that advice
	// too, legitimately — see TestRunCost_ByAgentOnAMixedWindowWithholdsTheCostCells.
	section := breakdownSection(t, got, "currency")
	if !strings.Contains(section, "(other) may hold more than one unit") {
		t.Errorf("the withheld cell is unexplained on the currency axis:\n%s", got)
	}
	if strings.Contains(section, "use --by currency") {
		t.Errorf("the currency table sends the reader to itself:\n%s", got)
	}
}

// A unit spelled two ways is still that unit's row, not a withheld one.
//
// WHY THE MEMBERSHIP TEST FOLDS. Snapshot.Currencies keeps the FIRST spelling it meets and
// canonicalises only USD, while ledger.labelFor answers with each folded row's own Currency — and
// Row.key() folds case only WITHIN a minute, so two minutes spelled "Credits" and "credits" reach
// the client as one entry in Currencies and a series label that may differ from it in case. An
// exact comparison there would read a real, operator-configured unit as unrecognised and withhold a
// figure it should label, which is the same over-refusal in miniature that this table exists to
// avoid.
//
// THE FIXTURE IS MIXED BECAUSE WITHHOLDING NEEDS ONE. In a single-unit window an unrecognised label
// falls back to the window's own unit and renders "0.08 Credits" — a spelling change, not a
// withheld figure — so the mixedCostCell assertion below could not fire on any implementation.
func TestRunCost_ByCurrencyMatchesAUnitSpelledADifferentWay(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"currency","priced":true,
		"currencies":["USD","Credits"],
		"totals":{"requests":1057,"costMicros":146439000,"pricedRequests":1057,"priceableRequests":1057},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "USD":{"requests":1000,"costMicros":146361600,"pricedRequests":1000,"priceableRequests":1000},
		   "credits":{"requests":57,"costMicros":77400,"pricedRequests":57,"priceableRequests":57}}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--by", "currency"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	row := tableRow(t, got, "credits")
	if !strings.Contains(row, "0.08 credits") {
		t.Errorf("a unit spelled differently from the reported set lost its label:\n%s", row)
	}
	if strings.Contains(row, mixedCostCell) {
		t.Errorf("a real configured unit was withheld as unrecognised:\n%s", row)
	}
}

// The widest unit pricing accepts is rendered in full, on both surfaces.
//
// %-14s ON THE HEADLINE WAS SIZED FOR costUSD, whose widest output is "$12345.67". costIn can now
// place a figure plus a space plus up to pricing's maxUnitLen (16) there, so the widest legal
// headline is ~25 columns. Go's %-14s pads and never truncates, so what happens is that the tail of
// that one line shifts right — measured, and asserted here rather than left to be rediscovered:
// widening the field by 11 columns would cost every USD deployment, which is all of them.
//
// THE TABLE'S COST CELL IS THE LAST COLUMN, so a long unit lengthens its own row and moves nothing.
// Both directions matter, so both are checked: nothing truncated, and the sibling USD row unmoved.
func TestRunCost_TheWidestLegalUnitIsNeverTruncated(t *testing.T) {
	// 16 bytes, all charset-legal. That is pricing's maxUnitLen today, but maxUnitLen is
	// UNEXPORTED and cmd/agentop cannot import it, so this literal does not track it and must not
	// claim to: raising the bound to 24 leaves this test green. The bound itself is pinned where it
	// is visible, by the accepted/refused boundary rows in pricing's own config_test.go; this
	// fixture only has to be wide enough to overflow a 14-column field, which 16 is.
	const widest = "Bobcoins12345678"

	t.Run("headline shifts but does not truncate", func(t *testing.T) {
		srv := fakeUsageServer(t, `{"window":"today","priced":true,"currencies":["`+widest+`"],
			"totals":{"requests":1057,"tokens":298000000,"costMicros":12345670000,
			"pricedRequests":1057,"priceableRequests":1057}}`)
		defer srv.Close()
		var out, errOut strings.Builder
		if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
		}
		got := out.String()
		if !strings.Contains(got, "12345.67 "+widest) {
			t.Errorf("the headline truncated the unit or the figure:\n%s", got)
		}
		// The unit-free counts survive the shift — they are what the line still has to carry.
		if !strings.Contains(got, "1057 requests") || !strings.Contains(got, "298M tokens") {
			t.Errorf("the shifted tail lost a figure:\n%s", got)
		}
	})

	t.Run("table row lengthens without moving its siblings", func(t *testing.T) {
		srv := fakeUsageServer(t, `{"window":"today","group":"currency","priced":true,
			"currencies":["`+widest+`","USD"],
			"totals":{"requests":1057,"costMicros":146439000,"pricedRequests":1057,"priceableRequests":1057},
			"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
			   "USD":{"requests":1000,"costMicros":146361600,"pricedRequests":1000,"priceableRequests":1000},
			   "`+widest+`":{"requests":57,"costMicros":12345670000,"pricedRequests":57,"priceableRequests":57}}}]}`)
		defer srv.Close()
		var out, errOut strings.Builder
		if code := runCost([]string{"--endpoint", srv.URL, "--by", "currency"}, &out, &errOut); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
		}
		got := out.String()
		if row := tableRow(t, got, widest); !strings.Contains(row, "12345.67 "+widest) {
			t.Errorf("the widest unit's cell was truncated:\n%s", row)
		}
		// The USD row is the invariant: the long row must not have shifted it. AGAINST THE HEADER,
		// not against a column number and not with HasSuffix.
		//
		// HasSuffix was the first attempt and it cannot fail for this: it pins where the figure
		// ENDS THE STRING, not where it sits. `%14s` right-aligns, so widening that field moves the
		// figure from column 73 to 79 and the row still ends with it — both mutants survived.
		// A literal 73 would work and would be a constant nothing derives.
		//
		// The header and the rows are printed from TWO SEPARATE format literals, three lines apart
		// in writeCostBreakdown, and that is the drift this guards: unequal length means the columns
		// have stopped lining up, and equal length survives a deliberate re-widening of BOTH.
		//
		// NOT a proof of alignment — a width-preserving permutation of the header's own fields would
		// pass. It catches every widening, which is the drift a format-string edit actually causes.
		usd := tableRow(t, got, "USD")
		header := tableRow(t, got, "CURRENCY")
		if len(usd) != len(header) {
			t.Errorf("the USD row is %d columns and its header is %d, so the table no longer lines "+
				"up:\n%q\n%q", len(usd), len(header), header, usd)
		}
		if !strings.Contains(usd, "$146.36") {
			t.Errorf("the USD row lost its figure:\n%q", usd)
		}
	})
}

// With seriesCurrencies the scoped list is the agent's own, so a single-unit agent in a mixed
// window gets its figure, and an agent whose own traffic is mixed is not told the mixture is the
// window's.
func TestRunCost_AgentWithSeriesCurrenciesIsJudgedOnItsOwnUnits(t *testing.T) {
	for _, tc := range []struct {
		units, want, notWant string
	}{
		{`["USD"]`, "$146.36", "cannot be added"},
		{`["USD","credits"]`, "cannot be added", "window's mixture"},
	} {
		srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
			"currencies":["USD","credits"],"seriesCurrencies":{"claude-code/2.1.270":`+tc.units+`},
			"totals":{"requests":1057,"costMicros":146439000,"pricedRequests":1057,"priceableRequests":1057},
			"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
			   "claude-code/2.1.270":{"requests":1000,"costMicros":146361600,"pricedRequests":1000,"priceableRequests":1000}}}]}`)
		var out, errOut strings.Builder
		if code := runCost([]string{"--endpoint", srv.URL, "--agent", "claude-code/2.1.270"}, &out, &errOut); code != 0 {
			t.Fatalf("units %s: exit = %d, stderr = %s", tc.units, code, errOut.String())
		}
		if got := out.String(); !strings.Contains(got, tc.want) || strings.Contains(got, tc.notWant) {
			t.Errorf("units %s: want %q and not %q in:\n%s", tc.units, tc.want, tc.notWant, got)
		}
		srv.Close()
	}
}

// An agent the server sent no units for had nothing priced; it gets "cost unavailable", not the
// window's mixture — which, by then, describes other agents' traffic.
func TestRunCost_AgentWithNoUnitsOfItsOwnIsNotRefusedAsMixed(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"10m","group":"agent","priced":true,
		"currencies":["Bobcoins","USD"],"seriesCurrencies":{"bob-shell/2.0.5":["Bobcoins"],"claude-code/2.1.284":["USD"]},
		"totals":{"requests":3,"costMicros":257800,"pricedRequests":2,"priceableRequests":2},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{"curl/8.0":{"requests":1}}}]}`)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--agent", "curl/8.0"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if got := out.String(); strings.Contains(got, "cannot be added") || strings.Contains(got, "units") {
		t.Errorf("an agent with nothing priced was refused as a unit mixture:\n%s", got)
	}
}

// The residual belongs to no agent, so it is labelled by the WINDOW's units, not the scoped
// agent's: in a mixed window it is the same un-addable sum the headline refuses.
func TestRunCost_AgentResidualKeepsTheWindowsUnits(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"currencies":["USD","credits"],"seriesCurrencies":{"claude-code/2.1.270":["USD"]},
		"ungroupedCostMicros":5000000,
		"totals":{"requests":20,"costMicros":9000000,"pricedRequests":20,"priceableRequests":20},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":14,"costMicros":4000000,"pricedRequests":14,"priceableRequests":14}}}]}`)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--agent", "claude-code/2.1.270"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if got := out.String(); strings.Contains(got, "$5.00") {
		t.Errorf("a whole-window residual in a mixed window was stated in the agent's dollars:\n%s", got)
	}
}

// Under --agent the JSON's currencies is the agent's own list, and a script labels every figure
// by it; the whole-window residual is only emitted where the window's unit is that same one.
func TestRunCost_AgentJSONOmitsAResidualInAnotherUnit(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","group":"agent","priced":true,
		"currencies":["USD","credits"],"seriesCurrencies":{"claude-code/2.1.270":["USD"]},
		"ungroupedCostMicros":5000000,
		"totals":{"requests":20,"costMicros":9000000,"pricedRequests":20,"priceableRequests":20},
		"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
		   "claude-code/2.1.270":{"requests":14,"costMicros":4000000,"pricedRequests":14,"priceableRequests":14}}}]}`)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--agent", "claude-code/2.1.270", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if got := out.String(); strings.Contains(got, "ungroupedCostMicros") {
		t.Errorf("a mixed window's residual was emitted beside the agent's currencies [USD]:\n%s", got)
	}
}
