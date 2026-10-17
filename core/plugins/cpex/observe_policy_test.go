package cpex

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDenyCodes(t *testing.T) {
	for s, want := range map[string][]string{
		`deny('secret in a tool result', 'secret_in_tool_result')`:       {"secret_in_tool_result"},
		`security.labels contains "secret": deny("a, b (c)", "tainted")`: {"tainted"},
		`deny('it\'s', 'c1') ; deny( 'x' , 'c2' )`:                       {"c1", "c2"},
		`deny`:                          nil,
		`deny('reason only')`:           nil,
		`!x: deny`:                      nil,
		`nodeny('a', 'b')`:              nil,
		`require(role.hr)`:              nil,
		`x.deny('a', 'b')`:              nil,
		`deny('unterminated, 'b'`:       nil,
		`deny('a', code_without_quote)`: nil,
	} {
		if got := denyCodes(s); !reflect.DeepEqual(got, want) {
			t.Errorf("denyCodes(%q) = %q, want %q", s, got, want)
		}
	}
}

const observeLastStep = `
global:
  pdp:
    - kind: cel
routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - "require(authenticated)"
        - cel:
            expr: "custom.tool_result_count == 0"
            on_deny:
              - "taint(secret, session)"
              - "deny('secret in a tool result', 'secret_in_tool_result')"
  - tool: http_egress
    authorization:
      pre_invocation:
        - "security.labels contains \"secret\": deny('tainted', 'session_tainted')"
`

func TestCheckObservedCodes(t *testing.T) {
	cases := []struct {
		name, policy string
		codes        []string
		wantErr      string // "" accepts
	}{
		{"last step, unique", observeLastStep, []string{"secret_in_tool_result"}, ""},
		{"namespaced code", observeLastStep, []string{"cpex.secret_in_tool_result"}, ""},
		{"single-step route", observeLastStep, []string{"session_tainted"}, ""},
		{"not last step", `
routes:
  - tool: t
    authorization:
      pre_invocation:
        - "x: deny('a', 'observed')"
        - "require(role.hr)"
`, []string{"observed"}, "not the last entry"},
		{"used by two steps", `
routes:
  - tool: a
    authorization:
      pre_invocation: ["x: deny('a', 'observed')"]
  - tool: b
    authorization:
      post_invocation: ["y: deny('b', 'observed')"]
`, []string{"observed"}, "used by 2 steps"},
		{"codes equal once sanitized", `
routes:
  - tool: a
    authorization:
      pre_invocation: ["x: deny('a', 'pii.detected')"]
  - tool: b
    authorization:
      pre_invocation: ["y: deny('b', 'pii_detected')"]
`, []string{"pii_detected"}, "used by 2 steps"},
		{"when/do, deny not last", `
routes:
  - tool: t
    authorization:
      pre_invocation:
        - "require(authenticated)"
        - when: "args.amount > 10"
          do:
            - "deny('big', 'observed')"
            - "taint(big, session)"
`, []string{"observed"}, "not the last entry"},
		{"when/do step not last", `
routes:
  - tool: t
    authorization:
      pre_invocation:
        - when: "args.amount > 10"
          do: ["deny('big', 'observed')"]
        - "require(role.hr)"
`, []string{"observed"}, "not the last entry"},
		{"when/do, last throughout", `
routes:
  - tool: t
    authorization:
      pre_invocation:
        - "require(role.hr)"
        - when: "args.amount > 10"
          do: ["taint(big, session)", "deny('big', 'observed')"]
`, []string{"observed"}, ""},
		{"on_deny effect not last", `
routes:
  - tool: t
    authorization:
      pre_invocation:
        - opa:
            query: data.x.allow
            on_deny: ["deny('no', 'observed')", "taint(x, session)"]
`, []string{"observed"}, "not the last entry"},
		{"global step", `
global:
  authorization:
    pre_invocation: ["x: deny('a', 'observed')"]
routes:
  - tool: t
    authorization:
      pre_invocation: ["require(role.hr)"]
`, []string{"observed"}, "only a route's own step"},
		{"group step", `
groups:
  pii:
    authorization:
      pre_invocation: ["x: deny('a', 'observed')"]
routes:
  - tool: t
    groups: [pii]
    authorization:
      pre_invocation: ["require(role.hr)"]
`, []string{"observed"}, "only a route's own step"},
		{"code the policy never names", observeLastStep, []string{"cedar_default_deny"}, "not the code of a deny() step"},
		{"code reused through an alias", `
routes:
  - tool: a
    authorization:
      pre_invocation: [&s "x: deny('a', 'observed')"]
  - tool: b
    authorization:
      pre_invocation: [*s]
`, []string{"observed"}, "used by 2 steps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkObservedCodes(tc.policy, tc.codes)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// Configure applies the check to the policy it loads, inline or from a
// file, and only when observe_codes is set.
func TestConfigure_ObserveCodesCheckedAgainstPolicy(t *testing.T) {
	notLast := `
routes:
  - tool: t
    authorization:
      pre_invocation:
        - "x: deny('a', 'observed')"
        - "require(role.hr)"
`
	raw := func(cfg map[string]any) []byte {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if err := setupFake(&FakeManager{}).Configure(raw(map[string]any{"config": notLast})); err != nil {
		t.Fatalf("no observe_codes: %v", err)
	}
	if err := setupFake(&FakeManager{}).Configure(raw(map[string]any{"config": notLast, "observe_codes": []string{"observed"}})); err == nil {
		t.Fatal("inline policy: want error")
	}
	file := t.TempDir() + "/cpex.yaml"
	if err := os.WriteFile(file, []byte(notLast), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setupFake(&FakeManager{}).Configure(raw(map[string]any{"config_file": file, "observe_codes": []string{"observed"}})); err == nil {
		t.Fatal("config_file policy: want error")
	}
	if err := setupFake(&FakeManager{}).Configure(raw(map[string]any{"config": observeLastStep, "observe_codes": []string{"secret_in_tool_result"}})); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
}
