// Package facts classifies which LLM provider a request went to, and gives the cache facts
// (accountant.EntryFacts) that provider's prompt cache has for a model. It is Cortex's own table,
// the host part of accountant/docs/cortex-wiring.md W6 (cortex-wiring.md's cached-prefix.md
// companion document is cited on each row). It ports the shape of the context-guru proxy's
// entryFacts (proxy/accountant_wire.go), minus the ping facts, which Task 2 applies in
// core/accounting.
package facts

import (
	"strings"
	"time"

	"github.com/rossoctl/accountant"
)

// Provider is the LLM provider a request went to, as far as Cortex's cache-facts table
// distinguishes them. It is Cortex's own enum: the proxy uses bifrost's bschemas.ModelProvider,
// which core/accounting does not import (global-constraints.md: no context-guru dependency, and
// bschemas is a context-guru dependency, not a Cortex one).
type Provider int

const (
	ProviderUnknown   Provider = iota
	ProviderAnthropic          // Claude Messages API: explicit cache_control breakpoints, exact TTL
	ProviderOpenAI             // Chat Completions / Responses API: no breakpoints, provider-managed cache
)

// Classify says which provider a request went to, from its destination host (after any redirect
// — that is the server that billed, design §5.4) and its path. The path decides first, because a
// gateway that fronts more than one provider under its own hostname (LiteLLM, OpenRouter, Azure)
// still speaks one of the two wire dialects on the wire, and the dialect says which provider's
// cache rules apply (cached-prefix.md: the two providers' rules are keyed to their own wire
// shape, not to who operates the endpoint). The host is the fallback, for an endpoint whose path
// this package does not recognize.
func Classify(host, path string) Provider {
	switch {
	case strings.HasSuffix(path, "/v1/messages"):
		return ProviderAnthropic
	case strings.HasSuffix(path, "/v1/chat/completions"), strings.HasSuffix(path, "/v1/responses"):
		return ProviderOpenAI
	}
	host = strings.ToLower(host)
	switch {
	case strings.Contains(host, "anthropic"):
		return ProviderAnthropic
	case strings.Contains(host, "openai"):
		return ProviderOpenAI
	}
	return ProviderUnknown
}

// anthropicCacheTTL5m and anthropicCacheTTL1h are Anthropic's two breakpoint lifetimes
// (cached-prefix.md "Anthropic": "The lifetime is exact. It is 5 minutes, or 1 hour with
// ttl: \"1h\"").
const (
	anthropicCacheTTL5m = 5 * time.Minute
	anthropicCacheTTL1h = time.Hour
)

// minCacheableHaiku and minCacheableDefault are Anthropic's minimum cacheable prefix, in the
// provider's own tokens (cached-prefix.md "Anthropic": "A minimum length applies. It is 512,
// 1,024, 2,048 or 4,096 tokens, depending on the model."). The two-value split (haiku-class models
// need the larger floor, every other Claude model this table knows needs the smaller one) is
// ported from context-guru's internal/modelinfo/cachefloor.go (MinCacheableHaiku,
// MinCacheableSmall), which measured both floors against the live gateway; see that file for the
// measurements. A model this table cannot classify gets the larger, safer floor, for the same
// reason context-guru's Family comment gives: the failure worth avoiding is a cache write that is
// never read, not a cache offered a little later than it could have been.
const (
	minCacheableHaiku   = 4096
	minCacheableDefault = 1024
)

// For returns the accountant.EntryFacts for provider p and model, given the breakpoints Cortex
// read from the forwarded body (wire.Parse). It does not know about keep-alive pings; Task 2's
// core/accounting applies those on top, as entryFacts' PingStore lookup does in the proxy.
func For(p Provider, model string, breakpoints []accountant.Breakpoint) accountant.EntryFacts {
	switch p {
	case ProviderAnthropic:
		return anthropicFacts(model, breakpoints)
	case ProviderOpenAI:
		return openAIFacts(model)
	default:
		// Unknown provider: no cache rule applies, so the entry's lifetime cannot be proven exact
		// or even a floor. Default to the shorter, safer tier (cached-prefix.md names 5m as
		// Anthropic's default breakpoint lifetime, and no provider's default is longer).
		return accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeUnknown}
	}
}

// anthropicFacts is Anthropic's row of the table. The TTL is 1h as soon as any breakpoint in the
// forwarded body asked for it: a position's own lifetime comes from the Breakpoint the caller
// passed for it (wire.Parse already resolves a position with both marks to "1h", W6.3), and this
// field is EntryFacts' one scalar fact about the entry this request wrote, so the longer of the
// two lifetimes applies to it, matching W6.3's "1h wins" rule at the entry level.
//
// This is a Cortex simplification of the proxy's rule: the keeper's entryFacts instead asks
// whether EVERY breakpoint in the body requests 1h (apply.AllBreakpointsAskExtendedTTL), because a
// 1h head over a 5m tail only keeps the TAIL alive for five minutes. Task 1 has no keeper yet, and
// the "any" rule is what docs/.../task-1-brief.md specifies ("A breakpoint with TTL \"1h\" sets TTL
// to 1h"); see docs/accounting-differences.json.
func anthropicFacts(model string, breakpoints []accountant.Breakpoint) accountant.EntryFacts {
	ttl := anthropicCacheTTL5m
	for _, bp := range breakpoints {
		if bp.TTL == "1h" {
			ttl = anthropicCacheTTL1h
			break
		}
	}
	return accountant.EntryFacts{
		TTL:          ttl,
		Lifetime:     accountant.LifetimeExact, // cached-prefix.md "Anthropic": "The lifetime is exact."
		MinCacheable: minCacheable(model),
		Provider: accountant.Provider{
			// Anthropic reports the cached-token count exactly, unlike OpenAI's rounding
			// (cached-prefix.md "OpenAI": "Granularity... From GPT-5.6 the cached count is exact
			// at the eligible boundary. On GPT-5.5 and earlier it is rounded down to a multiple
			// of 128."; Anthropic's own guide states no such rounding).
			ReadGranularity: 1,
			// cached-prefix.md "Anthropic": "The lifetime is exact. It is 5 minutes, or 1 hour
			// with ttl: \"1h\"." — no other lifetime string is valid on this wire.
			TTLs: []string{"5m", "1h"},
		},
	}
}

// minCacheable looks up a Claude model's minimum cacheable prefix by its tier word, the same
// substring match context-guru's internal/modelinfo.Family uses (it also matches a gateway
// prefix such as "aws/claude-haiku-4-5", because the match is a substring of the full id).
func minCacheable(model string) int {
	if strings.Contains(strings.ToLower(model), "haiku") {
		return minCacheableHaiku
	}
	return minCacheableDefault
}

// openAIFacts is OpenAI's row of the table. Cortex does not yet distinguish GPT-5.6+'s documented
// 30-minute minimum lifetime from an older model's undocumented retention (context-guru's
// modelinfo.OpenAI30MinuteCache gate does, by model name; porting that table is left for when
// Cortex's facts need it to price an OpenAI cache rescue). Every OpenAI model therefore gets
// LifetimeUnknown here: a floor that cannot be proven is safer than claiming one that might not
// hold (cached-prefix.md "OpenAI": "Lifetime is a minimum, not an expiry... with typical
// lifetimes of 5 to 10 minutes and about 30 minutes, and no documented maximum."). This is a
// Cortex simplification versus the proxy's model-aware rule; see docs/accounting-differences.json.
// model is not read yet; it stays a parameter for the per-model table this will grow into (see
// the comment above).
func openAIFacts(model string) accountant.EntryFacts {
	return accountant.EntryFacts{
		TTL:      5 * time.Minute,
		Lifetime: accountant.LifetimeUnknown,
		Provider: accountant.Provider{
			// cached-prefix.md "OpenAI": "On GPT-5.5 and earlier it is rounded down to a multiple
			// of 128." Cortex does not yet split GPT-5.6+'s exact granularity out (see above), so
			// every OpenAI model gets the rounded figure, the safer of the two.
			ReadGranularity: 128,
			// OpenAI's wire carries no cache_control breakpoints (prompt caching is automatic),
			// so no TTL string is valid here.
			TTLs: []string{},
		},
	}
}
