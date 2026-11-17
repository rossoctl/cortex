// Package wire reads an inference request body into the shape github.com/rossoctl/accountant
// hashes: tools, then system, then messages, one []byte per position, in the provider's cache
// order (accountant/docs/cortex-wiring.md W6.3). It ports the context-guru proxy's accParse
// (proxy/accountant_savings_body.go) to Cortex, so both hosts build the same accountant.Body for
// the same bytes.
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

// sections are the top-level values Parse reads, found in one pass over the body. On the
// Responses wire, instructions and input stand for system and messages (W6.3).
type sections struct {
	tools, system, messages gjson.Result
}

// Parse splits raw into the sections accountant.NewBody wants, and lists its cache breakpoints.
// It returns an error only when raw is not valid JSON; a body that parses but has no messages (or
// input) array gives an empty accountant.Body, as the proxy's accBodies does for the same case.
//
// The size of each position keeps the cache_control bytes; only the hash leaves them out (W6.3,
// accountant#21): the provider's own cache key ignores the marks, and a client moves its marks
// every turn, so a hash that included them would never find a common prefix with the previous
// request.
func Parse(f Format, raw []byte) (accountant.Body, []accountant.Breakpoint, error) {
	if !json.Valid(raw) {
		return accountant.Body{}, nil, fmt.Errorf("%w", errNotJSON)
	}
	sec := scan(f, raw)
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

// scan reads the top-level sections of raw for format f. For FormatResponses it reads
// instructions and input (W6.3); for the other two formats it reads tools, system and messages
// directly — Anthropic has all three, and OpenAI Chat has no top-level system (its system prompt,
// if any, is a {"role":"system"} entry inside messages, one position like any other).
func scan(f Format, raw []byte) sections {
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return sections{}
	}
	if f == FormatResponses {
		return responsesSections(root)
	}
	var sec sections
	var seenTools, seenSystem, seenMessages bool
	root.ForEach(func(k, v gjson.Result) bool {
		switch k.String() {
		case "tools":
			if !seenTools {
				seenTools, sec.tools = true, v
			}
		case "system":
			if !seenSystem {
				seenSystem, sec.system = true, v
			}
		case "messages":
			if !seenMessages {
				seenMessages, sec.messages = true, v
			}
		}
		return !(seenTools && seenSystem && seenMessages)
	})
	return sec
}

// responsesSections reads the Responses wire: tools stays tools, instructions is the system
// section and input is the messages section. A string input is one position, wrapped as a
// one-element array so it hashes like any other message (W6.3).
func responsesSections(root gjson.Result) sections {
	var sec sections
	var instructions, input gjson.Result
	var seenTools, seenInstructions, seenInput bool
	root.ForEach(func(k, v gjson.Result) bool {
		switch k.String() {
		case "tools":
			if !seenTools {
				seenTools, sec.tools = true, v
			}
		case "instructions":
			if !seenInstructions {
				seenInstructions, instructions = true, v
			}
		case "input":
			if !seenInput {
				seenInput, input = true, v
			}
		}
		return !(seenTools && seenInstructions && seenInput)
	})
	sec.system = instructions
	sec.messages = input
	if input.Type == gjson.String {
		sec.messages = gjson.Parse("[" + input.Raw + "]")
	}
	return sec
}

// withoutCacheMarks is one position's JSON without any cache_control: its own, the ones on its
// content blocks, and ones nested deeper (a mark inside a tool_result block's own content array).
// A position with none is returned as it is, byte for byte, so the common case costs no
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

// findCacheMark returns the gjson path of the first cache_control key under v, or "" when there
// is none. It looks at objects and arrays only, never inside a string.
func findCacheMark(v gjson.Result, prefix string) string {
	var found string
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch {
	case v.IsObject():
		v.ForEach(func(k, c gjson.Result) bool {
			if k.String() == "cache_control" {
				found = join(strings.NewReplacer(".", "\\.", "*", "\\*", "?", "\\?").Replace(k.String()))
				return false
			}
			if c.IsObject() || c.IsArray() {
				ek := strings.NewReplacer(".", "\\.", "*", "\\*", "?", "\\?", "|", "\\|", "#", "\\#", "@", "\\@").Replace(k.String())
				if found = findCacheMark(c, join(ek)); found != "" {
					return false
				}
			}
			return true
		})
	case v.IsArray():
		i := 0
		v.ForEach(func(_, c gjson.Result) bool {
			if c.IsObject() || c.IsArray() {
				if found = findCacheMark(c, join(strconv.Itoa(i))); found != "" {
					return false
				}
			}
			i++
			return true
		})
	}
	return found
}

// breakpointsOf lists the cache_control marks of sec, one per position, in the same space as
// Parse's Body. A mark on the position itself and a mark on one of its content blocks can both
// name one position; it is listed once, with the longer TTL (1h over 5m), because the provider
// keeps the entry for the longest TTL asked for at it. ttl defaults to "5m" (W6.3).
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
	return out
}
