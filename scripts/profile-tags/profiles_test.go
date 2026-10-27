package main

import (
	"sort"
	"strings"
	"testing"
)

// TestTags_Local pins the desktop profile. The set mirrors the plugin list
// cmd/cortex/local.go writes into ~/.cortex/config.yaml — the three
// parsers plus tool-prune — and script, which the preset appends there. If those diverge, a laptop either ships plugins its
// config never names or names plugins the binary cannot load, and the latter is
// a refuse-to-start (`unknown plugin ...`) rather than a degraded feature.
func TestTags_Local(t *testing.T) {
	got, err := Tags(ProfileLocal)
	if err != nil {
		t.Fatalf("Tags(local): %v", err)
	}
	want := "include_plugin_a2aparser,include_plugin_inferenceparser," +
		"include_plugin_mcpparser,include_plugin_script,include_plugin_toolprune"
	if strings.Join(got, ",") != want {
		t.Errorf("output changed\n got: %s\nwant: %s", strings.Join(got, ","), want)
	}
}

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
			"a2aparser", "ibac", "inferenceparser", "inferencerouter", "jwtvalidation", "lineage",
			"litellm_budgettrack", "mcpparser", "opa", "script", "sparc", "staticinject",
			"tokenbroker", "tokenexchange", "toolprune",
		}},
		{"envoy", []string{
			"a2aparser", "ibac", "inferenceparser", "jwtvalidation", "lineage",
			"mcpparser", "opa", "sparc", "tokenbroker", "tokenexchange", "toolprune",
		}},
		{"cpex", []string{
			"a2aparser", "cpex", "ibac", "inferenceparser", "jwtvalidation",
			"mcpparser", "sparc", "tokenbroker", "tokenexchange",
		}},
		{"fullcpex", []string{
			"a2aparser", "cpex", "ibac", "inferenceparser", "inferencerouter", "jwtvalidation",
			"lineage", "litellm_budgettrack", "mcpparser", "opa", "script", "sparc", "staticinject",
			"tokenbroker", "tokenexchange", "toolprune",
		}},
		// lite reproduces the retired lite-tags generator's keep set exactly, so
		// the published -lite artifact does not change in this refactor. It is a
		// sidecar minimum and shares no plugin with `local`.
		{"lite", []string{
			"jwtvalidation", "litellm_budgettrack", "staticinject", "tokenexchange",
		}},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			got, err := Tags(ProfileName(tc.profile))
			if err != nil {
				t.Fatalf("Tags(%s): %v", tc.profile, err)
			}
			var wantTags []string
			for _, n := range tc.want {
				wantTags = append(wantTags, "include_plugin_"+n)
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
