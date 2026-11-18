package accounting

import (
	"net/http"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/pipeline"
)

// billingContractCase is one row of billingContractCases.
type billingContractCase struct {
	name string
	h    http.Header
	want accountant.BillingMode
}

// billingContractCases mirrors accountant's own TestBillingFrom table
// (internal/engine/billing_test.go on the accountant branch feat/billing-from, commit db69f7e) —
// same header cases, same expected modes — so a rule change in BillingFrom that this repo's
// RequestStart has not caught up with fails here instead of surfacing as a silent mode mismatch
// (design §5.7: "a contract test ... expects the same mode as the accountant test table").
func billingContractCases() []billingContractCase {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.c2lnbmF0dXJl"
	return []billingContractCase{
		{"x-api-key present (Anthropic API key) -> api",
			http.Header{"X-Api-Key": []string{"sk-ant-api03-abcdef"}}, accountant.BillingAPI},
		{"x-api-key present with any value -> api",
			http.Header{"X-Api-Key": []string{"anything-at-all"}}, accountant.BillingAPI},
		{"Authorization Bearer Anthropic API key -> api",
			http.Header{"Authorization": []string{"Bearer sk-ant-api03-abcdef"}}, accountant.BillingAPI},
		{"Authorization Bearer Anthropic OAuth token (subscription login) -> subscription",
			http.Header{"Authorization": []string{"Bearer sk-ant-oat01-abcdef"}}, accountant.BillingSubscription},
		{"Authorization Bearer OpenAI project key -> api",
			http.Header{"Authorization": []string{"Bearer sk-proj-abcdef"}}, accountant.BillingAPI},
		{"Authorization Bearer OpenAI key -> api",
			http.Header{"Authorization": []string{"Bearer sk-abcdef123456"}}, accountant.BillingAPI},
		{"Authorization Bearer JWT (ChatGPT login) -> subscription",
			http.Header{"Authorization": []string{"Bearer " + jwt}}, accountant.BillingSubscription},
		{"Authorization Basic -> unknown",
			http.Header{"Authorization": []string{"Basic dXNlcjpwYXNz"}}, accountant.BillingUnknown},
		{"no credential -> unknown",
			http.Header{}, accountant.BillingUnknown},
		{"header names are case-insensitive (x-api-key)",
			http.Header{"X-API-KEY": []string{"sk-ant-api03-abcdef"}}, accountant.BillingAPI},
		{"header names are case-insensitive (authorization)",
			http.Header{"authorization": []string{"Bearer sk-ant-oat01-abcdef"}}, accountant.BillingSubscription},
		{"Authorization with two spaces between scheme and token -> api",
			http.Header{"Authorization": []string{"Bearer  sk-ant-api03-abcdef"}}, accountant.BillingAPI},
		{"Authorization with a tab between scheme and token -> subscription",
			http.Header{"Authorization": []string{"Bearer\tsk-ant-oat01-abcdef"}}, accountant.BillingSubscription},
		{"Authorization with leading and trailing whitespace -> api",
			http.Header{"Authorization": []string{"  Bearer sk-ant-api03-abcdef  "}}, accountant.BillingAPI},
		{`Authorization scheme is lowercase "bearer" -> subscription`,
			http.Header{"Authorization": []string{"bearer sk-ant-oat01-abcdef"}}, accountant.BillingSubscription},
		{"empty x-api-key value is treated as absent, falls through to Authorization",
			http.Header{
				"X-Api-Key":     []string{""},
				"Authorization": []string{"Bearer sk-ant-oat01-abcdef"},
			}, accountant.BillingSubscription},
		{"x-api-key wins over a present Authorization header",
			http.Header{
				"X-Api-Key":     []string{"sk-ant-api03-abcdef"},
				"Authorization": []string{"Bearer sk-ant-oat01-abcdef"},
			}, accountant.BillingAPI},
	}
}

// TestBillingContract feeds billingContractCases through Host.RequestStart — the code path a
// real request takes, not accountant.BillingFrom directly — and checks the stored mode matches
// what accountant's own table expects.
func TestBillingContract(t *testing.T) {
	h := &Host{}
	for _, c := range billingContractCases() {
		t.Run(c.name, func(t *testing.T) {
			pctx := &pipeline.Context{Headers: c.h}
			h.RequestStart(pctx)
			got := pipeline.GetState[accountant.Billing](pctx, billingKey)
			if got == nil {
				t.Fatal("RequestStart stored no billing mode")
			}
			if got.Mode != c.want {
				t.Errorf("mode = %q, want %q", got.Mode, c.want)
			}
		})
	}
}
