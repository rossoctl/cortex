// Package wire reads an inference request body into the shape github.com/rossoctl/accountant
// hashes: tools, then system, then messages, one []byte per position, in the provider's cache
// order (accountant/docs/cortex-wiring.md W6.3). It ports the context-guru proxy's accParse
// (proxy/accountant_savings_body.go) to Cortex, so both hosts build the same accountant.Body for
// the same bytes, with four deliberate departures from accParse: scan keeps reading for a
// Responses input when messages is not an array; a top-level cache_control (Anthropic's automatic
// caching) counts as a mark and gives a breakpoint; and only cache_control keys in cache-metadata
// locations leave the hash, so a tool parameter named cache_control stays in it.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rossoctl/accountant"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Format is the wire shape of an inference request body: which top-level fields hold its tools,
// system prompt and conversation turns.
type Format int

const (
	FormatAnthropic Format = iota // POST /v1/messages: tools, system, messages
	FormatChat                    // POST /v1/chat/completions: tools, messages (no top-level system)
	FormatResponses               // POST /v1/responses: tools, instructions, input
)

// The three inference dialects, matched by the end of the path, the same way
// core/parse/inference's dialectFor does (dialect.go) and for the same reason: providers and
// gateways mount these endpoints under prefixes of their own.
const (
	anthropicMessagesPath = "/v1/messages"
	chatCompletionsPath   = "/v1/chat/completions"
	responsesPath         = "/v1/responses"
)

// FormatOf reports the wire format of an inference endpoint from its path, matched by how the
// path ends, like core/parse/inference's dialectFor. ok is false when the path is not one of the
// three endpoints this package reads.
func FormatOf(path string) (Format, bool) {
	switch {
	case strings.HasSuffix(path, anthropicMessagesPath):
		return FormatAnthropic, true
	case strings.HasSuffix(path, chatCompletionsPath):
		return FormatChat, true
	case strings.HasSuffix(path, responsesPath):
		return FormatResponses, true
	}
	return 0, false
}

// errNotJSON is wrapped into the error Parse returns for a body that is not valid JSON.
var errNotJSON = errors.New("wire: body is not valid JSON")

// sections are the top-level values scan reads, found in one pass over the body. instructions and
// input are the Responses wire's system and messages; responsesSections folds them in only when
// the body has no messages array (W6.3), the same way the proxy's accSections does.
type sections struct {
	tools, system, messages gjson.Result
	instructions, input     gjson.Result
	// auto is the body's top-level cache_control: Anthropic's automatic caching, which puts one
	// breakpoint on the last cacheable block of the request.
	auto gjson.Result
}

// Parse splits raw into the sections accountant.NewBody wants, and lists its cache breakpoints.
// It returns an error only when raw is not valid JSON; a body that parses but has no messages (or
// input) array gives an empty accountant.Body, as the proxy's accBodies does for the same case.
//
// The size of each position keeps the cache_control bytes; only the hash leaves them out (W6.3,
// accountant#21): the provider's own cache key ignores the marks, and a client moves its marks
// every turn, so a hash that included them would never find a common prefix with the previous
// request.
//
// f is not used to pick which top-level fields are read: like the proxy's accScan and
// responsesSections, Parse detects the body's own shape (does it have a messages array, or an
// input array/string instead), not the endpoint it arrived on. A body whose shape does not match
// the dialect its path would suggest is still read by its actual shape, which is what the
// provider itself will read. f stays a parameter for symmetry with FormatOf and for a caller that
// wants to record which endpoint this body came from.
func Parse(f Format, raw []byte) (accountant.Body, []accountant.Breakpoint, error) {
	if !json.Valid(raw) {
		return accountant.Body{}, nil, fmt.Errorf("%w", errNotJSON)
	}
	sec := scan(raw)
	if !sec.messages.IsArray() {
		return accountant.NewBody(nil, nil, nil), nil, nil
	}
	var tools, system, msgs [][]byte
	var sizes []int // the wire size of each position, in hash order
	add := func(dst *[][]byte, v gjson.Result) {
		*dst = append(*dst, withoutCacheMarks(raw, v))
		sizes = append(sizes, len(v.Raw))
	}
	sec.tools.ForEach(func(_, v gjson.Result) bool { add(&tools, v); return true })
	if sec.system.IsArray() {
		sec.system.ForEach(func(_, v gjson.Result) bool { add(&system, v); return true })
	} else if sec.system.Exists() {
		add(&system, sec.system)
	}
	sec.messages.ForEach(func(_, v gjson.Result) bool { add(&msgs, v); return true })

	built := accountant.NewBody(tools, system, msgs)
	for i := range built.Pos {
		built.Pos[i].Bytes = int32(sizes[i])
	}
	// Built again from Start and Pos only (as the proxy does): this drops the unexported
	// cumulative-bytes table NewBody built from the mark-free input, so every byte count the
	// library reads afterward is the wire size, consistent with the operation sizes a context
	// operator reports (W6.3).
	body := accountant.Body{Start: built.Start, Pos: built.Pos}
	return body, breakpointsOf(sec), nil
}

// scan reads the six top-level fields a body may carry, in one pass, then folds the Responses
// pair (instructions, input) into (system, messages) where that applies — ported from the proxy's
// accScan (proxy/accountant_savings_body.go). Like gjson.Get, the first key of a name wins. scan
// stops early only when tools, system, cache_control and a messages ARRAY are all seen: a body
// like that has nothing left for responsesSections to change, since it only acts when messages is
// not an array. Unlike accScan, a messages value that is not an array (null, say) does not stop
// the scan, so an input after it is still read.
func scan(raw []byte) sections {
	var sec sections
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return sec
	}
	var seen [6]bool
	root.ForEach(func(k, v gjson.Result) bool {
		var i int
		var dst *gjson.Result
		switch k.String() {
		case "tools":
			i, dst = 0, &sec.tools
		case "system":
			i, dst = 1, &sec.system
		case "messages":
			i, dst = 2, &sec.messages
		case "instructions":
			i, dst = 3, &sec.instructions
		case "input":
			i, dst = 4, &sec.input
		case "cache_control":
			i, dst = 5, &sec.auto
		default:
			return true
		}
		if !seen[i] {
			seen[i], *dst = true, v
		}
		return !(seen[0] && seen[1] && seen[2] && seen[5] && sec.messages.IsArray())
	})
	return sec.responsesSections()
}

// responsesSections reads a Responses body: with no messages array and an input (an array of
// items, or one string), instructions is the system section and input the messages section. A
// string input is one position, wrapped as a one-element array so it hashes like any other
// message. system is preferred over instructions when a body states both (a hybrid gateway that
// speaks both dialects at once): only an absent system falls back to instructions. Ported
// verbatim from the proxy's (accSections).responsesSections.
func (sec sections) responsesSections() sections {
	if sec.messages.IsArray() || !(sec.input.IsArray() || sec.input.Type == gjson.String) {
		return sec
	}
	if !sec.system.Exists() {
		sec.system = sec.instructions
	}
	sec.messages = sec.input
	if sec.input.Type == gjson.String {
		sec.messages = gjson.Parse("[" + sec.input.Raw + "]")
	}
	return sec
}

// AllMarksAsk1h reports whether raw has at least one cache_control mark and every one of them
// asks for the 1h tier — ported path-for-path from the context-guru proxy's
// apply.AllBreakpointsAskExtendedTTL (apply/apply.go), which the keeper uses to decide a thread's
// keep-alive schedule and which facts.For uses for the same "is this whole entry 1h" question
// W6.4's entryFacts answers. It counts raw marks, not merged positions: a position with two marks
// (a 5m one and a 1h one) counts as two separate cache_control occurrences here, unlike
// breakpointsOf's one merged Breakpoint per position — that distinction is exactly why this
// exists apart from the Breakpoint slice Parse returns: AllBreakpointsAskExtendedTTL must see a
// lone 5m mark even when it shares a position with a 1h one and the merged Breakpoint says "1h".
//
// The top-level cache_control (automatic caching) is one more mark: a body whose only mark is a
// top-level 1h one asks for 1h, and a top-level 5m mark next to 1h block marks makes this false.
//
// Anthropic-only in practice: the gjson paths read the body's and messages/system/tools' own
// cache_control objects, fields a Chat or Responses body does not carry, so this is always false
// on those wires. No Format parameter is needed to gate that: a path that does not exist in the body
// simply matches nothing.
func AllMarksAsk1h(raw []byte) bool {
	total, extended := 0, 0
	var walk func(v gjson.Result)
	walk = func(v gjson.Result) {
		if v.IsArray() {
			v.ForEach(func(_, vv gjson.Result) bool { walk(vv); return true })
			return
		}
		if v.IsObject() {
			total++
			if v.Get("ttl").String() == "1h" {
				extended++
			}
		}
	}
	for _, path := range []string{
		"cache_control",
		"messages.#.content.#.cache_control",
		"messages.#.cache_control",
		"system.#.cache_control",
		"tools.#.cache_control",
	} {
		walk(gjson.GetBytes(raw, path))
	}
	return total > 0 && total == extended
}

// withoutCacheMarks is one position's JSON without its cache_control marks: its own, the ones on
// its content blocks, and ones nested deeper (a mark inside a tool_result block's own content
// array). A key named cache_control anywhere else is data, not a mark, and stays (see
// findCacheMark). A position with none is returned as it is, byte for byte, so the common case costs no
// allocation (ported from the proxy's withoutCacheMarks, context-guru#917).
func withoutCacheMarks(body []byte, v gjson.Result) []byte {
	raw := v.Raw
	if !strings.Contains(raw, "cache_control") {
		if i := v.Index; i > 0 && i+len(raw) <= len(body) && string(body[i:i+len(raw)]) == raw {
			return body[i : i+len(raw) : i+len(raw)]
		}
		return []byte(raw)
	}
	out := raw
	for n := 0; n < 64; n++ { // a position has at most a few marks on the wire
		path := findCacheMark(gjson.Parse(out), "")
		if path == "" {
			break
		}
		next, err := sjson.Delete(out, path)
		if err != nil {
			break
		}
		out = next
	}
	return []byte(out)
}

// findCacheMark returns the gjson path of the first cache-control mark under v, or "" when there
// is none. A mark is a cache_control key on v itself or on a block of a content array, at any
// depth of content nesting (a tool_result block's own content). It descends only through
// "content" arrays, so a cache_control key anywhere else stays in the hash: a tool's
// input_schema.properties.cache_control parameter, or a tool_use block's input value, is part of
// what the provider caches, and a change to it must change the hash.
func findCacheMark(v gjson.Result, prefix string) string {
	if !v.IsObject() {
		return ""
	}
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	var found string
	v.ForEach(func(k, c gjson.Result) bool {
		switch k.String() {
		case "cache_control":
			found = join("cache_control")
		case "content":
			if !c.IsArray() {
				return true
			}
			i := 0
			c.ForEach(func(_, blk gjson.Result) bool {
				found = findCacheMark(blk, join("content."+strconv.Itoa(i)))
				i++
				return found == ""
			})
		}
		return found == ""
	})
	return found
}

// breakpointsOf lists the cache_control marks of sec, one per position, in the same space as
// Parse's Body. A mark on the position itself and a mark on one of its content blocks can both
// name one position; it is listed once, with the longer TTL (1h over 5m), because the provider
// keeps the entry for the longest TTL asked for at it. ttl defaults to "5m" (W6.3).
//
// A top-level cache_control (automatic caching) adds one breakpoint at the body's last position,
// which holds the request's last cacheable block. When an explicit mark already names that
// position it is not listed twice. The provider walks back past a last block it cannot cache;
// this does not, because a position here is a whole message, not one block of it.
func breakpointsOf(sec sections) []accountant.Breakpoint {
	if !sec.messages.IsArray() {
		return nil
	}
	var out []accountant.Breakpoint
	at := map[accountant.Position]int{}
	add := func(p accountant.Position, ttl string) {
		if i, ok := at[p]; ok {
			if ttl == "1h" {
				out[i].TTL = "1h"
			}
			return
		}
		at[p] = len(out)
		out = append(out, accountant.Breakpoint{Pos: p, TTL: ttl})
	}
	ttl := func(cc gjson.Result) string {
		if cc.Get("ttl").String() == "1h" {
			return "1h"
		}
		return "5m"
	}
	mark := func(el gjson.Result) (cc, content gjson.Result) {
		if !el.IsObject() {
			return
		}
		var gotCC, gotContent bool
		el.ForEach(func(k, v gjson.Result) bool {
			switch k.String() {
			case "cache_control":
				if !gotCC {
					gotCC, cc = true, v
				}
			case "content":
				if !gotContent {
					gotContent, content = true, v
				}
			}
			return !(gotCC && gotContent)
		})
		return
	}
	scanSection := func(s accountant.Section, arr gjson.Result) {
		i := 0
		arr.ForEach(func(_, el gjson.Result) bool {
			cc, content := mark(el)
			if cc.Exists() {
				add(accountant.Position{Section: s, Index: i}, ttl(cc))
			}
			if content.IsArray() {
				content.ForEach(func(_, blk gjson.Result) bool {
					if cc, _ := mark(blk); cc.Exists() {
						add(accountant.Position{Section: s, Index: i}, ttl(cc))
					}
					return true
				})
			}
			i++
			return true
		})
	}
	scanSection(accountant.SectionTools, sec.tools)
	if sec.system.IsArray() {
		scanSection(accountant.SectionSystem, sec.system)
	}
	scanSection(accountant.SectionMessages, sec.messages)
	if sec.auto.IsObject() {
		if p, ok := lastPosition(sec); ok {
			add(p, ttl(sec.auto))
		}
	}
	return out
}

// lastPosition is the last position of the body Parse builds from sec: the last message, else
// the last system position, else the last tool. ok is false when the body has no positions.
func lastPosition(sec sections) (accountant.Position, bool) {
	count := func(v gjson.Result) int {
		if v.IsArray() {
			return len(v.Array())
		}
		return 0
	}
	if n := count(sec.messages); n > 0 {
		return accountant.Position{Section: accountant.SectionMessages, Index: n - 1}, true
	}
	n := count(sec.system)
	if !sec.system.IsArray() && sec.system.Exists() {
		n = 1
	}
	if n > 0 {
		return accountant.Position{Section: accountant.SectionSystem, Index: n - 1}, true
	}
	if n := count(sec.tools); n > 0 {
		return accountant.Position{Section: accountant.SectionTools, Index: n - 1}, true
	}
	return accountant.Position{}, false
}
