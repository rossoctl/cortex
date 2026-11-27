package main

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestTags_ReproduceTodaysDefaults is the invariant for this refactor: the
// convention change must not alter what any published artifact contains. Each
// want list is the set that binary links today — proxy's 15 default-on tagged
// plugins, and envoy's and cpex's tagged-plus-unconditional imports. If one of
// these drifts, a shipped artifact silently gained or lost a plugin.
func TestTags_ReproduceTodaysDefaults(t *testing.T) {
	for _, tc := range []struct {
		profile string
		want    []string
	}{
		{"full", []string{
			"a2aparser", "contextguru", "evolve", "ibac", "inferenceparser", "inferencerouter", "jwtvalidation", "lineage",
			"litellm_budgettrack", "mcpparser", "opa", "script", "sparc", "staticinject",
			"tokenbroker", "tokenexchange", "tokenexchangeaiac", "toolprune",
		}},
		// envoy gained litellm_budgettrack and staticinject in #1577: nothing
		// recorded a reason to leave them out, and only one binary had their files.
		{"envoy", []string{
			"a2aparser", "ibac", "inferenceparser", "jwtvalidation", "lineage", "litellm_budgettrack",
			"mcpparser", "opa", "sparc", "staticinject", "tokenbroker", "tokenexchange",
			"tokenexchangeaiac", "toolprune",
		}},
		{"cpex", []string{
			"a2aparser", "cpex", "ibac", "inferenceparser", "jwtvalidation",
			"mcpparser", "sparc", "tokenbroker", "tokenexchange", "tokenexchangeaiac",
		}},
		{"fullcpex", []string{
			"a2aparser", "contextguru", "cpex", "evolve", "ibac", "inferenceparser", "inferencerouter", "jwtvalidation",
			"lineage", "litellm_budgettrack", "mcpparser", "opa", "script", "sparc", "staticinject",
			"tokenbroker", "tokenexchange", "tokenexchangeaiac", "toolprune",
		}},
		// lite is the retired lite-tags generator's keep set plus
		// tokenexchangeaiac, the route-free alternative to tokenexchange, which
		// ships wherever that does. It is a sidecar minimum and shares no plugin
		// with `local`.
		{"lite", []string{
			"jwtvalidation", "litellm_budgettrack", "staticinject", "tokenexchange", "tokenexchangeaiac",
		}},
		// A full tag names a driver; a bare name is a plugin.
		{"hostegress", []string{"include_driver_moca", "staticinject"}},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			got, err := Tags(ProfileName(tc.profile))
			if err != nil {
				t.Fatalf("Tags(%s): %v", tc.profile, err)
			}
			var wantTags []string
			for _, n := range tc.want {
				if !strings.HasPrefix(n, "include_") {
					n = "include_plugin_" + n
				}
				wantTags = append(wantTags, n)
			}
			sort.Strings(wantTags)
			if strings.Join(got, ",") != strings.Join(wantTags, ",") {
				t.Errorf("profile %s changed\n got: %s\nwant: %s",
					tc.profile, strings.Join(got, ","), strings.Join(wantTags, ","))
			}
		})
	}
}

// TestTags_FullCpexIsFullPlusCpex keeps the cgo laptop build at parity with the
// default one: a plugin added to full must reach fullcpex too.
func TestTags_FullCpexIsFullPlusCpex(t *testing.T) {
	full, err := Tags(ProfileFull)
	if err != nil {
		t.Fatalf("Tags(full): %v", err)
	}
	got, err := Tags(ProfileFullCpex)
	if err != nil {
		t.Fatalf("Tags(fullcpex): %v", err)
	}
	want := append(append([]string{}, full...), "include_plugin_cpex")
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("fullcpex is not full plus cpex\n got: %s\nwant: %s",
			strings.Join(got, ","), strings.Join(want, ","))
	}
}

// TestTags_UnknownProfile — a typo in CI must fail the build, not silently
// produce an empty tag list that links no plugins at all.
func TestTags_UnknownProfile(t *testing.T) {
	if _, err := Tags(ProfileName("desktop")); err == nil {
		t.Fatal("want error for an undefined profile, got nil")
	}
}

// TestTags_DriverOnlyProfile — a profile with a driver and no plugins resolves,
// and one with neither fails closed.
func TestTags_DriverOnlyProfile(t *testing.T) {
	savedProfiles, savedDrivers := allProfiles, drivers
	t.Cleanup(func() { allProfiles, drivers = savedProfiles, savedDrivers })
	allProfiles = append(slices.Clone(allProfiles), "probe", "empty")
	drivers = map[DriverName][]ProfileName{"moca": {"probe"}}

	if tags, err := Tags("probe"); err != nil || !slices.Equal(tags, []string{"include_driver_moca"}) {
		t.Errorf("probe: got %v, %v; want [include_driver_moca]", tags, err)
	}
	if _, err := Tags("empty"); err == nil {
		t.Error("empty: want an error for a profile with no plugins or drivers")
	}
}
