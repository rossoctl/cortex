package config

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/usage"
	"gopkg.in/yaml.v3"
)

func TestConfig_CostLedgerSectionParses(t *testing.T) {
	var c Config
	// 9, not 7 or 8: the floor is usage.Window7dLocalDays, and a fixture below the floor is
	// not a config this loader would accept — see
	// TestCostLedgerConfig_TheFloorCoversEveryDayTheWindowTouches.
	src := `
mode: proxy-sidecar
cost_ledger:
  enabled: true
  dir: /var/lib/cortex/cost
  retention_days: 9
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if c.CostLedger == nil {
		t.Fatal("cost_ledger section did not parse")
	}
	if c.CostLedger.Dir != "/var/lib/cortex/cost" {
		t.Errorf("dir = %q", c.CostLedger.Dir)
	}
	if c.CostLedger.RetentionDays != 9 {
		t.Errorf("retention_days = %d, want 9", c.CostLedger.RetentionDays)
	}
}

// The reason Enabled is a POINTER: an absent block and an explicit `enabled: false`
// must be different answers. With a plain bool the only way to turn the ledger off
// would be to delete the whole block, which also discards the retention setting
// beside it — and the default differs by deployment (on for --local, off in a pod),
// so "unset" cannot be collapsed onto either value.
func TestCostLedgerConfig_UnsetAndExplicitFalseDiffer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yaml      string
		defaultOn bool
		want      bool
	}{
		{"no block, local default on", "mode: proxy-sidecar\n", true, true},
		{"no block, cluster default off", "mode: proxy-sidecar\n", false, false},
		{"block present but enabled unset, local", "mode: proxy-sidecar\ncost_ledger:\n  retention_days: 9\n", true, true},
		{"block present but enabled unset, cluster", "mode: proxy-sidecar\ncost_ledger:\n  retention_days: 9\n", false, false},
		{"explicit false beats the local default", "mode: proxy-sidecar\ncost_ledger:\n  enabled: false\n", true, false},
		{"explicit true beats the cluster default", "mode: proxy-sidecar\ncost_ledger:\n  enabled: true\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &c); err != nil {
				t.Fatalf("yaml: %v", err)
			}
			// This table reaches LedgerEnabled without going through Validate, so an illegal
			// value in a fixture would not fail anything — it would just quietly teach a
			// retention the loader rejects. One of them said retention_days: 5, which stopped
			// being legal when the floor landed beside it, and two said 7, which stopped being
			// legal when the floor was corrected to the nine day files window=7d can touch in a
			// spring-forward week. This check is what caught both.
			if c.CostLedger != nil {
				if verr := c.CostLedger.Validate(); verr != nil {
					t.Errorf("fixture is not a config this loader would accept: %v", verr)
				}
			}
			if got := c.CostLedger.LedgerEnabled(tc.defaultOn); got != tc.want {
				t.Errorf("LedgerEnabled(%v) = %v, want %v", tc.defaultOn, got, tc.want)
			}
		})
	}
}

func TestCostLedgerConfig_RejectsNegativeRetention(t *testing.T) {
	c := &CostLedgerConfig{RetentionDays: -1}
	if err := c.Validate(); err == nil {
		t.Error("negative retention_days accepted")
	}
}

// A retention shorter than the longest window the API serves makes that window lie.
// window=7d is answered from these day files, so with retention_days: 2 six of the
// eight local days a 7d window spans have been deleted, and the response still says
// window:"7d", priced:true over two days of spend — indistinguishable downstream from
// a genuinely quiet week.
//
// 7 AND 8 ARE BOTH IN THIS TABLE, and they are the cases that matter, because each was
// once the floor. 7 is the number an operator reading "window=7d" types, and it is one day
// file short: the eighth date, the oldest and the one the rolling window opens on, had
// already been pruned. 8 covers an ordinary week and is short by one file in a
// spring-forward week, which is only 167 hours long — the same defect arrived at by a
// second route. See TestCostLedgerConfig_TheFloorCoversEveryDayTheWindowTouches and
// usage.Window7dLocalDays.
func TestCostLedgerConfig_RejectsARetentionShorterThanTheLongestWindow(t *testing.T) {
	for _, days := range []int{1, 2, 6, 7, 8} {
		c := &CostLedgerConfig{RetentionDays: days}
		err := c.Validate()
		if err == nil {
			t.Errorf("retention_days: %d accepted; window=7d reads %d local day files, so this "+
				"would report a partial week as a full one", days, usage.Window7dLocalDays)
			continue
		}
		// The operator who chose the number is the one who reads this, so it has to say
		// why rather than only that.
		if !strings.Contains(err.Error(), "7d") {
			t.Errorf("retention_days: %d rejected with %q, which does not say which window it breaks", days, err)
		}
	}
}

// Zero still means "the package default", so the floor must not turn an absent
// setting into a load failure — that would refuse every config that omits the field.
func TestCostLedgerConfig_ZeroRetentionIsStillTheDefault(t *testing.T) {
	for _, days := range []int{0, minCostLedgerRetentionDays, 30} {
		if err := (&CostLedgerConfig{RetentionDays: days}).Validate(); err != nil {
			t.Errorf("retention_days: %d rejected: %v", days, err)
		}
	}
}

// THE FLOOR USED TO ADMIT THE PARTIAL WEEK IT WAS ADDED TO REJECT. It was the literal
// 7, on the reading that window=7d spans seven days — but usage.ParseWindowSpec makes
// "7d" a ROLLING seven times twenty-four hours, so unless it begins exactly at
// midnight it starts part-way through one date and ends part-way through another and
// the ledger has to open EIGHT day files to answer it. retention_days: 7 therefore
// passed validation and still served window:"7d" over a partial week, which is exactly
// the case the floor exists to prevent.
//
// AND THEN IT ADMITTED A SECOND ONE AT EIGHT. Eight is what an ordinary week touches, but a
// spring-forward week is 167 hours, so a rolling 168-hour span reaches into a NINTH date and
// retention_days: 8 answered window:"7d" over a partial week on the two mornings a year that
// happens. usage.Window7dLocalDays is now a CEILING of nine rather than a count of eight.
//
// The day count is derived from ParseWindowSpec here rather than compared against
// usage.Window7dLocalDays, so that this test fails if either the window's definition
// or the floor moves. It measures an ORDINARY week — this package has no zone fixtures and
// time.Local is whatever the runner has — so it asserts the floor is not SHORT of eight; that
// the ceiling is reached by the 167-hour week is pinned next to the constant, in
// usage.TestParseWindowSpec_ASpringForwardWeekReachesTheNinthLocalDate. The equality
// assertion below is the link between the two, and is what keeps the constants from drifting
// apart again.
func TestCostLedgerConfig_TheFloorCoversEveryDayTheWindowTouches(t *testing.T) {
	// Mid-afternoon, i.e. not the midnight special case: this is the shape every real
	// request has.
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	spec, err := usage.ParseWindowSpec(usage.Window7d, now)
	if err != nil {
		t.Fatalf("ParseWindowSpec(%q): %v", usage.Window7d, err)
	}
	// The ledger walks whole local days from From to To inclusive (see
	// ledger.Writer.Query), so this counts day files, not hours.
	days := 0
	for d := dayOfTest(spec.From); !d.After(dayOfTest(spec.To)); d = d.AddDate(0, 0, 1) {
		days++
	}

	if minCostLedgerRetentionDays < days {
		t.Errorf("minCostLedgerRetentionDays = %d but window=%q touches %d local day files "+
			"(%s to %s): retention_days: %d passes validation and still answers a rolling week "+
			"over %d of them, which is the partial-week report the floor exists to refuse",
			minCostLedgerRetentionDays, usage.Window7d, days,
			spec.From.Format("2006-01-02"), spec.To.Format("2006-01-02"),
			minCostLedgerRetentionDays, minCostLedgerRetentionDays)
	}
	if minCostLedgerRetentionDays != usage.Window7dLocalDays {
		t.Errorf("minCostLedgerRetentionDays = %d, usage.Window7dLocalDays = %d — the floor must be "+
			"DERIVED from the window it protects, or the two drift again",
			minCostLedgerRetentionDays, usage.Window7dLocalDays)
	}
	// And the boundary is refused, not merely met: one day short of the FLOOR is the value an
	// operator actually types, and it is now 8 rather than 7 — the number this floor itself
	// used to be, which covers an ordinary week and is one file short of a spring-forward one.
	if verr := (&CostLedgerConfig{RetentionDays: usage.Window7dLocalDays - 1}).Validate(); verr == nil {
		t.Errorf("retention_days: %d accepted; it holds one day file fewer than the most days "+
			"window=%q can touch (%d), which is the partial week this floor refuses",
			usage.Window7dLocalDays-1, usage.Window7d, usage.Window7dLocalDays)
	}
}

// dayOfTest is local midnight of t. The ledger's own day boundary, spelled here rather than called
// through cost/ledger because the point of the test is the arithmetic rather than the wiring.
//
// Not because the import is unavailable: the file DOES import cost/ledger now, for the ceiling pin at
// the bottom. Nothing in cost/ledger, usage or pipeline imports this package, so there was never a
// cycle to route around.
func dayOfTest(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// TestMinCostLedgerRetentionDays_MatchesTheWindowItProtects is what lets
// minCostLedgerRetentionDays be a literal.
//
// The floor exists because window=7d is served from these day files, so it has to cover every
// local date that window can touch. Deriving it as usage.Window7dLocalDays said that in the
// code, and cost a layering inversion: config is the leaf every binary loads to parse its
// YAML, and importing the aggregator there drags that dependency into binaries that never
// aggregate anything.
//
// A TEST-ONLY import is the right shape for this. config does not need to KNOW about windows
// at runtime — it needs to AGREE with them, and agreement is a thing to check rather than to
// compute. What the derivation bought was that a window change which outgrows the floor fails
// loudly instead of silently admitting a partial week; that protection lives here now.
//
// It also pins the numbers spelled out in Validate's error message, which are literals for the
// same reason. A window change makes that message wrong in a test rather than wrong in front
// of an operator.
func TestMinCostLedgerRetentionDays_MatchesTheWindowItProtects(t *testing.T) {
	if got, want := minCostLedgerRetentionDays, usage.Window7dLocalDays; got != want {
		t.Errorf("minCostLedgerRetentionDays = %d, but window=%s touches %d local day files.\n"+
			"The floor must cover every date the window can open on, or retention_days = %d "+
			"passes validation and then answers window:%q over a partial week.\n"+
			"Update the constant AND the numbers spelled out in Validate's message.",
			got, usage.Window7d, want, got, usage.Window7d)
	}
	// The message says "a ROLLING 7x24h, so it reads 8 local day files rather than 7 — and 9
	// in a spring-forward week". Pin all three numbers so the prose cannot drift from the
	// constants either.
	if days := int(usage.Window7dSpan / (24 * time.Hour)); days != 7 {
		t.Errorf("Validate's message says the window is a rolling 7x24h, but it is %dx24h", days)
	}
	if usage.Window7dLocalDays != 9 {
		t.Errorf("Validate's message says a spring-forward week reads 9 local day files, but the "+
			"ceiling is %d", usage.Window7dLocalDays)
	}
	// The ordinary-week figure the message also quotes. Stated as the ceiling minus the
	// spring-forward day rather than as a bare 8, so the two cannot drift apart.
	if ordinary := usage.Window7dLocalDays - 1; ordinary != 8 {
		t.Errorf("Validate's message says an ordinary week reads 8 local day files, but the "+
			"ceiling implies %d", ordinary)
	}
}

// The ledger has no path to an event without the session store: it records by being
// registered as a Recorder on it, and the block that constructs it in
// authbridge-proxy's main is nested inside `if cfg.Session.SessionEnabled()`. This
// pair used to load, validate, report success, and write nothing — with no error, no
// warning, and no log line naming the ledger anywhere.
//
// Refused rather than warned because no deployment decision can make it work: the two
// settings contradict each other. The message has to name BOTH, since the fix is a
// choice between them.
func TestValidate_RefusesTheLedgerWithoutSessions(t *testing.T) {
	on, off := true, false
	c := &Config{
		Mode:       ModeProxySidecar,
		Listener:   forwardOnlyListener(),
		Session:    SessionConfig{Enabled: &off},
		CostLedger: &CostLedgerConfig{Enabled: &on},
	}
	err := Validate(c)
	if err == nil {
		t.Fatal("cost_ledger.enabled: true with session.enabled: false accepted; " +
			"the ledger is registered as a Recorder on the session store, so it can never see an event")
	}
	for _, want := range []string{"cost_ledger", "session"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q — the fix is a choice between the two settings", err, want)
		}
	}
}

// The refusal must be narrow. It fires on an EXPLICIT `enabled: true` only, because an
// absent `enabled` means "the caller's default" — on for a local install, off in
// Kubernetes — and failing startup over a default nobody wrote would break a
// deployment that legitimately runs with sessions off. The default-on case is a Warn
// at the call site instead (warnCostLedgerNeedsSessions in cmd/authbridge-proxy).
func TestValidate_TheLedgerSessionRefusalIsNarrow(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		cfg  *Config
	}{
		{"sessions on, ledger on", &Config{Mode: ModeProxySidecar, Listener: forwardOnlyListener(), Session: SessionConfig{Enabled: &on}, CostLedger: &CostLedgerConfig{Enabled: &on}}},
		{"sessions unset (defaults on), ledger on", &Config{Mode: ModeProxySidecar, Listener: forwardOnlyListener(), CostLedger: &CostLedgerConfig{Enabled: &on}}},
		{"sessions off, ledger block present but enabled unset", &Config{Mode: ModeProxySidecar, Listener: forwardOnlyListener(), Session: SessionConfig{Enabled: &off}, CostLedger: &CostLedgerConfig{RetentionDays: 9}}},
		{"sessions off, ledger explicitly off", &Config{Mode: ModeProxySidecar, Listener: forwardOnlyListener(), Session: SessionConfig{Enabled: &off}, CostLedger: &CostLedgerConfig{Enabled: &off}}},
		{"sessions off, no ledger block", &Config{Mode: ModeProxySidecar, Listener: forwardOnlyListener(), Session: SessionConfig{Enabled: &off}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.cfg); err != nil {
				t.Errorf("rejected a legitimate config: %v", err)
			}
		})
	}
}

// forwardOnlyListener is the --local listener shape: proxy-sidecar with the forward
// role only, which is the deployment the cost ledger actually runs in. Spelled here
// so these fixtures exercise validateCostLedger rather than tripping the reverse
// role's reverse_proxy_backend requirement first.
func forwardOnlyListener() ListenerConfig {
	return ListenerConfig{Roles: []string{RoleForward}}
}

// THE CEILING, and it is not a disk-space preference: past it the failure inverts.
//
// prune counts back with AddDate, which normalises rather than saturating, so a large enough
// retention wraps the cutoff into the FUTURE and the first prune deletes every day file
// including today. A validator that bounded only the floor admitted the one value whose
// meaning is the opposite of its effect. Both edges are asserted, because a ceiling that
// rejects its own maximum is the off-by-one this table exists to catch.
func TestCostLedgerValidate_RejectsARetentionLargeEnoughToInvertPrune(t *testing.T) {
	for _, tc := range []struct {
		name    string
		days    int
		wantErr bool
	}{
		{"the maximum itself is allowed", maxCostLedgerRetentionDays, false},
		{"one past the maximum is refused", maxCostLedgerRetentionDays + 1, true},
		{"the value that wraps the cutoff into the future", 1<<62 - 1, true},
		{"an ordinary retention still loads", 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&CostLedgerConfig{RetentionDays: tc.days}).Validate()
			if tc.wantErr && err == nil {
				t.Errorf("retention_days = %d validated: prune's cutoff wraps into the future at "+
					"that scale, so this config deletes the ledger it claims to keep", tc.days)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("retention_days = %d refused: %v", tc.days, err)
			}
		})
	}
}

// TestMaxCostLedgerRetentionDays_MatchesTheStoreItClamps is the pin cost/ledger exported
// MaxRetentionDays for, and which did not exist until now.
//
// Both sides clamp: this validator refuses a retention past the ceiling, and newStore clamps one that
// arrives any other way. cost/ledger's own comment says config "owns the derivation" and that its
// exported constant is what a config-side test asserts against — but no such test existed, so the two
// literals had nothing holding them equal. That is precisely the drift store.go narrates as having
// already happened once, when a third copy of the number in a test file made a moved constant look
// pinned.
//
// This test DOES import cost/ledger, which the helper above says it avoids. There is no cycle to
// avoid: nothing in cost/ledger, usage or pipeline imports this package, so the only thing that import
// costs is a test-only edge — cheap against a silent disagreement over how much history is deleted.
func TestMaxCostLedgerRetentionDays_MatchesTheStoreItClamps(t *testing.T) {
	if maxCostLedgerRetentionDays != ledger.MaxRetentionDays {
		t.Errorf("config ceiling = %d, ledger.MaxRetentionDays = %d: a config that loads would produce a store clamping to a different number, which deletes history the validator said was fine to keep",
			maxCostLedgerRetentionDays, ledger.MaxRetentionDays)
	}
}
