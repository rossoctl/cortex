package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/usage"
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
	// newest day file: those days are reported outside AND summed. See sessionapi's
	// TestLedgerSnapshot_ADayReportedOutsideRetentionCanStillBeInTheTotal, which reproduces it.
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
	if !strings.Contains(errOut.String(), "abctl service status") {
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
	if strings.Contains(stderr, "abctl service status") {
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
// a warning; nothing in cmd/abctl read it, so a damaged read printed a total byte-identical
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
// standing property of a gateway — and the field reached no client in cmd/abctl at all, so a
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
// abctl against a newer sidecar is the normal deployment, not an exotic one.
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
