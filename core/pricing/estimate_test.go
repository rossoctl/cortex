package pricing

import "testing"

func TestEstimateTokensFromBytes(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		bytesRemoved, promptTokens, bodyByt int
		want                                int
	}{
		// 10% of the body removed from a 100k-token prompt.
		{"proportional", 1000, 100_000, 10_000, 10_000},
		{"rounds to nearest", 3, 10, 7, 4}, // 3*10/7 = 4.29
		// Below half a token is reported as none: a saving too small to price must
		// not read as one priced at nothing.
		{"sub-token", 1, 1, 1000, 0},
		{"nothing removed", 0, 100, 100, 0},
		{"no response yet", 100, 0, 100, 0},
		{"no body", 100, 100, 0, 0},
		{"negative bytes", -100, 100, 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateTokensFromBytes(tc.bytesRemoved, tc.promptTokens, tc.bodyByt); got != tc.want {
				t.Errorf("= %d, want %d", got, tc.want)
			}
		})
	}
}

// The tier is the substance of the figure: the same token count differs by 12.5x between
// cache-read and cache-write, so a saving attributed to the wrong tier is not a rounding
// error.
func TestAvoidedUsage_PlacesTheSavingInThePromptsOwnTier(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt Usage
		want   Tier
		field  func(Usage) int
	}{
		{"fresh input", Usage{Input: 1000}, TierInput, func(u Usage) int { return u.Input }},
		{"cache write", Usage{CacheWrite: 1000}, TierCacheWrite, func(u Usage) int { return u.CacheWrite }},
		{"cache read", Usage{CacheRead: 1000}, TierCacheRead, func(u Usage) int { return u.CacheRead }},
		// No prompt tokens at all: nothing to attribute to, and input is the only
		// defensible default.
		{"empty", Usage{}, TierInput, func(u Usage) int { return u.Input }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, tier := AvoidedUsage(tc.prompt, 40)
			if tier != tc.want {
				t.Errorf("tier = %v, want %v", tier, tc.want)
			}
			if n := tc.field(got); n != 40 {
				t.Errorf("tokens landed in the wrong tier: %+v", got)
			}
			// And ONLY that tier: a saving counted twice would double the figure.
			if total := got.Input + got.CacheWrite + got.CacheRead + got.Output; total != 40 {
				t.Errorf("tokens total %d across tiers, want 40 in exactly one: %+v", total, got)
			}
		})
	}
}

// Output tokens are never a saving: pruning the request cannot shorten the reply, and
// attributing output cost to it would be false. (The same reasoning as
// tui/prune_saving.go's publishedRates.)
func TestAvoidedUsage_NeverAttributesOutput(t *testing.T) {
	got, tier := AvoidedUsage(Usage{Input: 100, Output: 5000}, 40)
	if got.Output != 0 {
		t.Errorf("attributed %d output tokens to a prompt saving", got.Output)
	}
	if tier == TierOutput {
		t.Error("tier is output; a prompt saving can never come out of the output tier")
	}
}

// The spelling must match the config keys, because a tier name in a cost record is meant to
// be pasted into `pricing:` without translation.
func TestTierString_MatchesConfigSpelling(t *testing.T) {
	for tier, want := range map[Tier]string{
		TierInput:      "input",
		TierCacheWrite: "cache_write",
		TierCacheRead:  "cache_read",
		TierOutput:     "output",
	} {
		if got := tier.String(); got != want {
			t.Errorf("Tier(%d).String() = %q, want %q", int(tier), got, want)
		}
	}
	if got := Tier(99).String(); got != "unknown" {
		t.Errorf("out-of-range tier = %q, want %q", got, "unknown")
	}
}
