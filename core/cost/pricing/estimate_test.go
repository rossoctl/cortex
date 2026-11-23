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

// TestAvoidedUsage_KeepsTheOneHourShare: a cache-write saving carries the request's 1h
// share, so it prices at the rate the request's own writes were priced at.
func TestAvoidedUsage_KeepsTheOneHourShare(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt Usage
		want1h int
	}{
		{"all 1h", Usage{CacheWrite: 1000, CacheWrite1h: 1000}, 40},
		{"all 5m", Usage{CacheWrite: 1000}, 0},
		{"mixed, split by the same ratio", Usage{CacheWrite: 1000, CacheWrite1h: 250}, 10},
		{"mixed, rounded to the nearest token", Usage{CacheWrite: 3, CacheWrite1h: 1}, 13},
		{"an impossible 1h count is ignored", Usage{CacheWrite: 1000, CacheWrite1h: 2000}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, tier := AvoidedUsage(tc.prompt, 40)
			if tier != TierCacheWrite || got.CacheWrite != 40 {
				t.Fatalf("got %+v in tier %v, want 40 cache-write tokens", got, tier)
			}
			if got.CacheWrite1h != tc.want1h {
				t.Errorf("CacheWrite1h = %d, want %d", got.CacheWrite1h, tc.want1h)
			}
		})
	}
	// Not a cache-write saving: no 1h part, whatever the request's writes were.
	if got, _ := AvoidedUsage(Usage{CacheRead: 5000, CacheWrite: 100, CacheWrite1h: 100}, 40); got.CacheWrite1h != 0 {
		t.Errorf("a cache-read saving carries CacheWrite1h = %d, want 0", got.CacheWrite1h)
	}
}
