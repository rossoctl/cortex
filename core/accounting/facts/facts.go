// Package facts classifies which LLM provider a request went to, and gives the cache facts
// (accountant.EntryFacts) that provider's prompt cache has for a model. It is Cortex's own table,
// the host part of accountant/docs/cortex-wiring.md W6 (cortex-wiring.md's cached-prefix.md
// companion document is cited on each row). It matches the context-guru proxy's entryFacts
// (proxy/accountant_wire.go:229-246) exactly, minus the ping facts, which the caller in
// core/accounting applies on top.
package facts

import (
	"strings"
	"time"

	"github.com/rossoctl/accountant"
)

// Provider is the LLM provider a request went to, as far as Cortex's cache-facts table
// distinguishes them. It is Cortex's own enum: the proxy uses bifrost's bschemas.ModelProvider,
// which core/accounting does not import: Cortex does not depend on context-guru, and bschemas is
// a context-guru dependency, not a Cortex one.
type Provider int

const (
	ProviderUnknown   Provider = iota
	ProviderAnthropic          // Claude Messages API: explicit cache_control breakpoints, exact TTL
	ProviderOpenAI             // Chat Completions / Responses API: no breakpoints, provider-managed cache
)

// Classify says which provider a request went to, from its path alone — the same rule the proxy
// follows (explicitBreakpointProvider / modelinfo.HonoursCacheControl classify by the wire
// dialect, never by hostname): a gateway that fronts more than one provider under its own
// hostname (LiteLLM, OpenRouter, Azure) still speaks one of the two wire dialects, and the dialect
// is what decides which provider's cache rules apply.
//
// host plays no part in the answer. It is kept as a parameter for a future caller that may need
// it (for example, to log which destination a request with no recognizable path went to). An
// unrecognized path is ProviderUnknown whatever the host says, even a host whose name contains
// "anthropic" or "openai", because a host name does not say which wire dialect the request uses.
func Classify(host, path string) Provider {
	switch {
	case strings.HasSuffix(path, "/v1/messages"):
		return ProviderAnthropic
	case strings.HasSuffix(path, "/v1/chat/completions"), strings.HasSuffix(path, "/v1/responses"):
		return ProviderOpenAI
	}
	return ProviderUnknown
}

// anthropicCacheTTL5m and anthropicCacheTTL1h are Anthropic's two breakpoint lifetimes
// (cached-prefix.md "Anthropic": "The lifetime is exact. It is 5 minutes, or 1 hour with
// ttl: \"1h\""), the same constants modelinfo.AnthropicCacheTTL5m/1h name in the proxy.
const (
	anthropicCacheTTL5m = 5 * time.Minute
	anthropicCacheTTL1h = time.Hour
)

// For returns the accountant.EntryFacts for provider p and model, given allMarksAsk1h — whether
// EVERY raw cache_control mark in the forwarded body asked for the 1h tier. It does not know about
// keep-alive pings; the caller in core/accounting applies those on top, as entryFacts' PingStore
// lookup does in the proxy.
//
// The 1h tier needs every raw mark to ask 1h, which a merged accountant.Breakpoint cannot express:
// a position can merge a 5m mark and a 1h mark into one Breakpoint{TTL:"1h"}. So callers pass
// wire.AllMarksAsk1h(raw) over the raw forwarded body, and must not derive this bool from the
// Breakpoint slice wire.Parse returns.
func For(p Provider, model string, allMarksAsk1h bool) accountant.EntryFacts {
	switch p {
	case ProviderAnthropic:
		return anthropicFacts(allMarksAsk1h)
	case ProviderOpenAI:
		return openAIFacts(model)
	default:
		// Unknown provider: no cache rule applies, matching entryFacts' own starting value
		// before either of the proxy's two CacheLifetime branches overrides it.
		return accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeUnknown}
	}
}

// anthropicFacts is Anthropic's row of the table, matching entryFacts' CacheLifetimeExact branch
// exactly: TTL and Lifetime only. It does NOT set MinCacheable or Provider (ReadGranularity,
// TTLs) — the proxy's entryFacts never sets them either; those fields exist for a host that knows
// them, and the proxy, like Cortex here, leaves them at their zero values.
//
// allMarksAsk1h (wire.AllMarksAsk1h) is the proxy's apply.AllBreakpointsAskExtendedTTL question:
// the TTL is 1h only when EVERY cache_control mark in the body asked for it, not when at least
// one position's merged mark did — a 1h head over a 5m tail only keeps the tail alive for five
// minutes, so "any" would overstate the entry's lifetime.
func anthropicFacts(allMarksAsk1h bool) accountant.EntryFacts {
	ttl := anthropicCacheTTL5m
	if allMarksAsk1h {
		ttl = anthropicCacheTTL1h
	}
	return accountant.EntryFacts{TTL: ttl, Lifetime: accountant.LifetimeExact}
}

// openAIFacts is OpenAI's row of the table, matching entryFacts' CacheLifetimeMinimum /
// CacheLifetimeUnknown branches: modelinfo.OpenAI30MinuteCache gates on the model (GPT-5.6 and
// later get a documented 30-minute minimum retention; everything else has an undocumented,
// variable one and stays Unknown). Like anthropicFacts, this sets only TTL and Lifetime.
func openAIFacts(model string) accountant.EntryFacts {
	if gpt56OrLater(model) {
		// modelinfo.OpenAIMinimumCacheTTL / apply.CacheLifetimeMinimum.
		return accountant.EntryFacts{TTL: 30 * time.Minute, Lifetime: accountant.LifetimeMinimum}
	}
	// Falls through to entryFacts' own starting value: an older OpenAI model's retention is
	// undocumented, so the library must not treat a gap as provably cold (CacheLifetimeUnknown).
	return accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeUnknown}
}

// gpt56OrLater reports whether an OpenAI model id names GPT-5.6 or later, the gate
// modelinfo.GPT56OrLater uses (internal/modelinfo/cachefrac.go), ported here because Cortex does
// not import context-guru. A routed model id (azure/gpt-5.6-luna) is supported: the match is the
// LAST "gpt-" substring, because a routing prefix may itself contain an older GPT name and the
// final model token is the one the request actually selects.
func gpt56OrLater(model string) bool {
	m := strings.ToLower(model)
	i := strings.LastIndex(m, "gpt-")
	if i < 0 {
		return false
	}
	v := m[i+4:]
	major, n := 0, 0
	for n < len(v) && v[n] >= '0' && v[n] <= '9' {
		major = major*10 + int(v[n]-'0')
		n++
	}
	if n == 0 || major != 5 {
		return major > 5
	}
	if n == len(v) || v[n] != '.' {
		return false
	}
	v = v[n+1:]
	minor, digits := 0, 0
	for digits < len(v) && v[digits] >= '0' && v[digits] <= '9' {
		minor = minor*10 + int(v[digits]-'0')
		digits++
	}
	return digits > 0 && minor >= 6
}
