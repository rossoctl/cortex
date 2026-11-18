package contextguru

import (
	"hash/fnv"
	"strconv"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/expand"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The envelope is the part of a request the engine's own operation list does not describe: the
// tools array and the system section. apply changes it in several places — toolschema and
// toolfilter rewrite or drop declarations, cachesplit splits the system block at its volatile
// tail (every preset), headttl relabels a mark to 1h — and reports none of them in
// apply.Result.Trace.Ops. The library's sum check then fails on the tools or system section and
// the request is recorded with no saving.
//
// envelopeOps reports each change from the two bodies instead: the body this plugin was handed
// and the body it returns. It is a port of the context-guru proxy's reporter
// (proxy/accountant_envelope.go: envelopeOps, toolsOps, systemOps and their helpers), with the
// same operator names (context-guru.<component>) and the same ChangeID scheme, so the two hosts
// attribute and replay the same change alike. Diffing this plugin's own input and output keeps
// the attribution with context-guru: a declaration another plugin (tool-prune) removed before or
// after it is not in this diff, so the two operators' ops never cover the same bytes.
//
// Not ported: the proxy's tooldefer re-attribution (envelopeOpsFor) and messageOps, both for edits
// the proxy makes after its pipeline, which the plugin does not make.
//
// The forms are the shared cases' (accountant test/cases):
//   - a declaration the output added is a rewrite at its output index, SizeBefore 0;
//   - declarations it removed are one rewrite with a span for each, at the input indexes,
//     SizeAfter 0;
//   - a declaration both bodies carry with different bytes is a rewrite in place;
//   - a system block split in two is one rewrite on the input block, its SizeAfter the bytes of
//     both halves, with a cg:cachesplit: ChangeID, the prefix the library recognises a split by;
//   - a system block whose bytes changed in place (a ttl label, a moved mark) is a rewrite in place.
//
// Every ChangeID is derived from what changed, never from the turn, so the same change gets the
// same ChangeID on every request.

// envelopeOwner names the operator of a tools change. expand and adjudicate inject a declaration
// under a name only they use; every other tools change is apply's envelope rewrite, attributed to
// toolschema (a rewrite in place) or toolfilter (a removal).
func envelopeOwner(name string, added, removed bool) string {
	switch {
	case added && name == expand.ToolName:
		return "expand"
	case added && name == adjudicateToolName:
		return "adjudicate"
	case removed:
		return "toolfilter"
	default:
		return "toolschema"
	}
}

// adjudicateToolName is context-guru's internal/adjudicate.ToolName, which an outside module
// cannot import.
const adjudicateToolName = "context_guru_adjudicate"

// envelopeOps lists the operations that explain the tools and system sections' difference between
// in and out. It returns nil when either body has no conversation. On the Responses wire the system
// section is instructions.
func envelopeOps(in, out []byte) []accountant.Operation {
	if !scanSections(in).messages.IsArray() || !scanSections(out).messages.IsArray() {
		return nil
	}
	ops := toolsOps(gjson.GetBytes(in, "tools").Array(), gjson.GetBytes(out, "tools").Array())
	return append(ops, systemOps(systemBlocks(in), systemBlocks(out))...)
}

// toolsOps matches declarations by name. An unnamed declaration is matched by its position among
// the unnamed ones. A name that repeats is matched by its occurrence. A removal among repeated
// names cannot be placed, so the reporter returns nil and the sum check sends the request to the
// no-saving fallback, as for any change it cannot explain.
func toolsOps(cl, fw []gjson.Result) []accountant.Operation {
	repeated := false
	key := func(arr []gjson.Result) ([]string, map[string]int) {
		keys, at := make([]string, len(arr)), map[string]int{}
		seen := map[string]int{}
		for i, d := range arr {
			base := d.Get("name").String()
			if base == "" {
				base = "\x00" // unnamed declarations share one counter
			}
			k := base
			if n := seen[base]; n > 0 {
				k = base + "\x00" + strconv.Itoa(n)
			}
			seen[base]++
			repeated = repeated || seen[base] > 1
			keys[i] = k
			at[k] = i
		}
		return keys, at
	}
	ck, cat := key(cl)
	fk, fat := key(fw)
	var ops []accountant.Operation
	var removed []accountant.Span
	removedBytes, removedHash := 0, fnv.New64a()
	for i, k := range ck {
		if _, ok := fat[k]; ok {
			continue
		}
		n := len(cl[i].Raw)
		removed = append(removed, accountant.Span{Section: accountant.SectionTools, From: i, To: i,
			SizeBefore: accountant.Size{Bytes: n}, SizeAfter: accountant.Size{Bytes: 0}})
		removedBytes += n
		removedHash.Write([]byte(k))
		removedHash.Write([]byte{0})
	}
	if len(removed) > 0 && repeated {
		return nil
	}
	if len(removed) > 0 {
		op := envelopeOp("toolfilter", accountant.Position{Section: accountant.SectionTools, Index: removed[0].From},
			removedBytes, 0, "decl-removed:"+hexOf(removedHash.Sum64()))
		if len(removed) > 1 {
			op.Spans = removed
		}
		ops = append(ops, op)
	}
	for i, k := range fk {
		j, inClient := cat[k]
		switch {
		case !inClient:
			ops = append(ops, envelopeOp(envelopeOwner(k, true, false),
				accountant.Position{Section: accountant.SectionTools, Index: i}, 0, len(fw[i].Raw), "decl-added:"+hashOf(k)))
		case cl[j].Raw != fw[i].Raw:
			at := accountant.Position{Section: accountant.SectionTools, Index: j}
			// apply rewrites the declaration first and relabels its mark to 1h last, so the rewrite
			// ends at the output bytes without the ttl, and the relabel is its own headttl op.
			if mid, ok := withoutHeadTTL(cl[j].Raw, fw[i].Raw); ok {
				if mid != cl[j].Raw {
					ops = append(ops, envelopeOp(envelopeOwner(k, false, false), at, len(cl[j].Raw), len(mid),
						"decl-rewritten:"+hashOf(k)))
				}
				ops = append(ops, envelopeOpID("headttl", at, len(mid), len(fw[i].Raw), "headttl:"+hashOf(k)))
				continue
			}
			ops = append(ops, envelopeOp(envelopeOwner(k, false, false), at, len(cl[j].Raw), len(fw[i].Raw),
				"decl-rewritten:"+hashOf(k)))
		}
	}
	return ops
}

// systemBlocks is the system section as the accountant hashes it: a string system prompt is one
// block; a Responses body's instructions stand in for system.
func systemBlocks(raw []byte) []string {
	s := scanSections(raw).system
	switch {
	case s.IsArray():
		var out []string
		s.ForEach(func(_, v gjson.Result) bool { out = append(out, v.Raw); return true })
		return out
	case s.Exists():
		return []string{s.Raw}
	}
	return nil
}

// systemOps explains the system section. The output has the input's blocks, one of them possibly
// split in two (cachesplit), and any block possibly relabelled in place (headttl). The split is
// found as the one input block whose text equals the text of two adjacent output blocks.
func systemOps(cl, fw []string) []accountant.Operation {
	var ops []accountant.Operation
	split := -1
	if len(fw) == len(cl)+1 {
		for i := range cl {
			if i+1 < len(fw) && textOf(cl[i]) != "" && textOf(cl[i]) == textOf(fw[i])+textOf(fw[i+1]) {
				split = i
				break
			}
		}
		if split < 0 {
			return nil // a count change this reporter cannot explain: the sum check says so
		}
	} else if len(fw) != len(cl) {
		return nil
	}
	for i, j := 0, 0; i < len(cl); i, j = i+1, j+1 {
		pos := accountant.Position{Section: accountant.SectionSystem, Index: i}
		if i == split {
			before := cl[i]
			if relabelled := withHeadTTL(cl[i], fw[j]); relabelled != cl[i] {
				ops = append(ops, envelopeOpID("headttl", pos, len(cl[i]), len(relabelled), "headttl:"+hashOf(textOf(cl[i]))))
				before = relabelled
			}
			ops = append(ops, envelopeOpID("cachesplit", pos, len(before), len(fw[j])+len(fw[j+1]),
				"cg:cachesplit:"+hashOf(textOf(cl[i]))))
			j++
			continue
		}
		switch {
		case cl[i] == fw[j]:
		case headTTLRelabelled(cl[i], fw[j]):
			ops = append(ops, envelopeOpID("headttl", pos, len(cl[i]), len(fw[j]), "headttl:"+hashOf(textOf(cl[i]))))
		default:
			ops = append(ops, envelopeOp("toolfilter", pos, len(cl[i]), len(fw[j]), "system-rewritten:"+hashOf(textOf(cl[i]))))
		}
	}
	return ops
}

// withHeadTTL is client with its cache mark relabelled to 1h, when forwarded carries a 1h mark and
// the client's did not; client itself otherwise.
func withHeadTTL(client, forwarded string) string {
	if !headTTLRelabelled(client, forwarded) {
		return client
	}
	out, err := sjson.Set(client, "cache_control.ttl", "1h")
	if err != nil {
		return client
	}
	return out
}

// withoutHeadTTL splits a declaration's change at the relabel: mid is the forwarded declaration
// without the 1h label. ok is false when the forwarded declaration carries no relabel.
func withoutHeadTTL(client, forwarded string) (mid string, ok bool) {
	if !headTTLRelabelled(client, forwarded) {
		return "", false
	}
	if withHeadTTL(client, forwarded) == forwarded {
		return client, true
	}
	mid, err := sjson.Delete(forwarded, "cache_control.ttl")
	if err != nil {
		return "", false
	}
	return mid, true
}

// headTTLRelabelled is true when the forwarded mark asks for 1h and the client's did not.
func headTTLRelabelled(client, forwarded string) bool {
	cc := gjson.Get(client, "cache_control")
	return cc.IsObject() && cc.Get("ttl").String() != "1h" && gjson.Get(forwarded, "cache_control.ttl").String() == "1h"
}

func textOf(raw string) string {
	r := gjson.Parse(raw)
	if r.Type == gjson.String {
		return r.String()
	}
	return r.Get("text").String()
}

// envelopeOp is one rewrite with the ChangeID cg:<component>:<key>.
func envelopeOp(component string, at accountant.Position, before, after int, key string) accountant.Operation {
	return envelopeOpID(component, at, before, after, "cg:"+component+":"+key)
}

func envelopeOpID(component string, at accountant.Position, before, after int, changeID string) accountant.Operation {
	return accountant.Operation{
		Kind:       accountant.KindRewrite,
		Operator:   "context-guru." + component,
		At:         at,
		SizeBefore: accountant.Size{Bytes: before},
		SizeAfter:  accountant.Size{Bytes: after},
		ChangeID:   changeID,
	}
}

func hashOf(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return hexOf(h.Sum64())
}

func hexOf(v uint64) string { return strconv.FormatUint(v, 16) }

// sections are the top-level values the accountant hashes, read as core/accounting/wire reads them
// (the proxy's accScan): tools, system and messages, or on the Responses wire instructions and
// input when the body has no messages array.
type sections struct {
	tools, system, messages gjson.Result
}

func scanSections(raw []byte) sections {
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return sections{}
	}
	var sec sections
	var instructions, input gjson.Result
	var seen [5]bool
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
			i, dst = 3, &instructions
		case "input":
			i, dst = 4, &input
		default:
			return true
		}
		if !seen[i] { // like gjson.Get, the first key of a name wins
			seen[i], *dst = true, v
		}
		return true
	})
	if sec.messages.IsArray() || !(input.IsArray() || input.Type == gjson.String) {
		return sec
	}
	if !sec.system.Exists() {
		sec.system = instructions
	}
	sec.messages = input
	if input.Type == gjson.String {
		sec.messages = gjson.Parse("[" + input.Raw + "]")
	}
	return sec
}
