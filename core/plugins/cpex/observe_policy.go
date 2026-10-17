package cpex

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// checkObservedCodes refuses observe_codes the policy cannot observe
// safely. CPEX stops evaluating a phase at its first denying step, so a
// deny the plugin turns into observe skips every step after it, and a
// code shared with another step silences that step's deny too. Each
// observed code must therefore be the code of exactly one deny() in the
// policy, inside a route's own pre_invocation or post_invocation list,
// and that deny must be the last entry of every list between it and the
// phase list: the last step, the last effect of its on_deny, the last
// step of its when/do. A step in global, global.defaults or a group is
// refused, because the route's own steps run after it.
//
// The check is textual: a step uses a code when the code is the second
// argument of a deny(...) in any string of the step. A code a step emits
// without naming it (a PDP's default deny, require()) is not found, and
// is refused.
func checkObservedCodes(policy string, codes []string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(policy), &doc); err != nil {
		return fmt.Errorf("observe_codes: parse policy: %w", err)
	}
	uses := map[string][]denyUse{}
	collectDenyUses(&doc, nil, stepPos{}, uses)
	for _, code := range codes {
		name := strings.TrimPrefix(observeCode(code), "cpex.")
		found := uses[name]
		switch {
		case len(found) == 0:
			return fmt.Errorf("observe_codes: %q is not the code of a deny() step in the policy", code)
		case len(found) > 1:
			where := make([]string, len(found))
			for i, u := range found {
				where[i] = u.where
			}
			return fmt.Errorf("observe_codes: %q is used by %d steps (%s); an observed code must belong to one step",
				code, len(found), strings.Join(where, ", "))
		case !found[0].route:
			return fmt.Errorf("observe_codes: %q is used at %s; only a route's own step can be observed, "+
				"since the route's steps run after it", code, found[0].where)
		case !found[0].last:
			return fmt.Errorf("observe_codes: %q is used at %s, which is not the last entry of its list; "+
				"the entries after it would not run", code, found[0].where)
		}
	}
	return nil
}

// denyUse is one deny(..., code) in the policy.
type denyUse struct {
	where string // YAML path of the string holding it
	route bool   // inside routes[*].authorization.{pre,post}_invocation
	last  bool   // last entry of every list from the phase list down
}

// stepPos tracks where the walk is relative to a phase list.
type stepPos struct {
	phase bool // inside an authorization phase list
	route bool // that phase list belongs to a route
	last  bool // every list since the phase list was entered at its last entry
}

func collectDenyUses(n *yaml.Node, path []string, pos stepPos, uses map[string][]denyUse) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			collectDenyUses(c, path, pos, uses)
		}
	case yaml.AliasNode:
		if n.Alias != nil {
			collectDenyUses(n.Alias, path, pos, uses)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			child := append(append([]string(nil), path...), key)
			cpos := pos
			if (key == "pre_invocation" || key == "post_invocation") &&
				len(path) > 0 && path[len(path)-1] == "authorization" {
				cpos = stepPos{
					phase: true,
					route: len(path) == 3 && path[0] == "routes",
					last:  true,
				}
			}
			collectDenyUses(n.Content[i+1], child, cpos, uses)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			cpos := pos
			cpos.last = pos.last && i == len(n.Content)-1
			collectDenyUses(c, append(append([]string(nil), path...), fmt.Sprintf("[%d]", i)), cpos, uses)
		}
	case yaml.ScalarNode:
		for _, code := range denyCodes(n.Value) {
			name := sanitizeReason(code)
			uses[name] = append(uses[name], denyUse{
				where: strings.ReplaceAll(strings.Join(path, "."), ".[", "["),
				route: pos.phase && pos.route,
				last:  pos.phase && pos.last,
			})
		}
	}
}

// denyCodes returns the code of every deny('reason', 'code') in s.
func denyCodes(s string) []string {
	var codes []string
	for i := 0; ; {
		j := strings.Index(s[i:], "deny")
		if j < 0 {
			return codes
		}
		start := i + j
		i = start + len("deny")
		if start > 0 && isIdentByte(s[start-1]) {
			continue
		}
		rest := strings.TrimLeft(s[i:], " \t")
		if !strings.HasPrefix(rest, "(") {
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t\r\n")
		_, rest, ok := quoted(rest)
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, " \t\r\n")
		if !strings.HasPrefix(rest, ",") {
			continue
		}
		code, _, ok := quoted(strings.TrimLeft(rest[1:], " \t\r\n"))
		if ok {
			codes = append(codes, code)
		}
	}
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '.' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// quoted reads a single- or double-quoted literal at the start of s and
// returns its content and the text after it.
func quoted(s string) (lit, rest string, ok bool) {
	if s == "" || (s[0] != '\'' && s[0] != '"') {
		return "", s, false
	}
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case q:
			return b.String(), s[i+1:], true
		default:
			b.WriteByte(s[i])
		}
	}
	return "", s, false
}
