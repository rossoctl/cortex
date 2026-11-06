package inferencerouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stripReasoning drops from body the reasoning artifacts drop reports true for —
// the ones only their producer can validate — and returns the body it made with
// how many it dropped. drop is given each artifact's fingerprint. Anthropic
// messages carry them as thinking and redacted_thinking blocks in an assistant
// message's content; responses carries them as items of type reasoning in input.
// Chat completions carries reasoning as plain-text fields every measured server
// accepts, and matches neither shape, so it is left alone — the artifacts name
// themselves; no format is sniffed. An assistant message the drop empties goes
// whole: an empty content array is refused at Anthropic, and a thinking-only
// turn carries no tool_use for a tool_result to reference.
func stripReasoning(body []byte, drop func(fingerprint string) bool) ([]byte, int) {
	conv := reasoningConversation(body)
	dropped := 0
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		arr := msgs.Array()
		for i := len(arr) - 1; i >= 0; i-- { // reverse: a deletion shifts no later path
			m := arr[i]
			if m.Get("role").String() != "assistant" {
				continue
			}
			content := m.Get("content")
			if !content.IsArray() {
				continue
			}
			blocks := content.Array()
			kept := make([]string, 0, len(blocks))
			for j, b := range blocks {
				if fp, ok := fingerprint(b, conv, i, j); ok && drop(fp) {
					dropped++
					continue
				}
				kept = append(kept, b.Raw)
			}
			if len(kept) == len(blocks) {
				continue
			}
			if len(kept) == 0 {
				body, _ = sjson.DeleteBytes(body, "messages."+strconv.Itoa(i))
				continue
			}
			body, _ = sjson.SetRawBytes(body, "messages."+strconv.Itoa(i)+".content",
				[]byte("["+strings.Join(kept, ",")+"]"))
		}
	}
	if items := gjson.GetBytes(body, "input"); items.IsArray() {
		arr := items.Array()
		for i := len(arr) - 1; i >= 0; i-- {
			if fp, ok := fingerprint(arr[i], conv, i, 0); ok && drop(fp) {
				body, _ = sjson.DeleteBytes(body, "input."+strconv.Itoa(i))
				dropped++
			}
		}
	}
	return body, dropped
}

// fingerprint names a reasoning artifact by what its producer signed: a thinking
// block's signature, a redacted_thinking block's data, a reasoning item's id and
// content. ok is false for anything that is not one. An unsigned thinking block —
// Ollama writes them, often empty — is named by its conversation, its text and
// where it sits (message i, block j), since two of them can share a text; a client
// appends to its history, so an earlier block keeps its place.
func fingerprint(b gjson.Result, conv string, i, j int) (string, bool) {
	var key string
	switch b.Get("type").String() {
	case "thinking":
		if key = b.Get("signature").String(); key == "" {
			key = "unsigned " + conv + " " + strconv.Itoa(i) + " " + strconv.Itoa(j) + " " + b.Get("thinking").String()
		}
	case "redacted_thinking":
		key = b.Get("data").String()
	case "reasoning":
		key = b.Get("id").String() + " " + b.Get("encrypted_content").String()
	default:
		return "", false
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12]), true
}

// reasoningConversation names the conversation body carries by the text of its
// first user turn: a session's main conversation, each subagent and each one-shot
// open differently, and a client's history never rewrites that turn's text. Text
// only, because the cache marker a client moves along its history changes the
// turn's bytes from one request to the next.
func reasoningConversation(body []byte) string {
	first := gjson.GetBytes(body, `messages.#(role=="user")`)
	if !first.Exists() {
		first = gjson.GetBytes(body, `input.#(role=="user")`)
	}
	var text strings.Builder
	if c := first.Get("content"); c.IsArray() {
		for _, block := range c.Array() {
			text.WriteString(block.Get("text").String())
		}
	} else {
		text.WriteString(c.String())
	}
	sum := sha256.Sum256([]byte(text.String()))
	return hex.EncodeToString(sum[:8])
}

// reasoningPrefix is where the router keeps a moved session's reasoningRecord. It
// lives where pins do, with the pin's TTL, renewed with the pin, and X does not
// clear it.
const reasoningPrefix = "inference-router/reasoning/"

// reasoningRecord says which server wrote each reasoning artifact in a moved
// session's history. It is kept only once a session has moved: until then every
// artifact in it is its one server's, and nothing is dropped or stored.
//
// An artifact first appears in the request after the answer that carried it, the
// next request of the same conversation, so its writer is where that
// conversation's previous request went: Last, or Base for a conversation not seen
// since the first move.
type reasoningRecord struct {
	// Base is the server the session ran on before its first move, "" for the
	// client's own provider.
	Base string `json:"base"`
	// Writers is each artifact's fingerprint, to the server that wrote it.
	Writers map[string]string `json:"writers"`
	// Last is each conversation's latest destination since the first move.
	Last map[string]string `json:"last"`
}

// keepOwnReasoning drops from a request the reasoning artifacts another server
// wrote — only their writer can read them, and Bedrock 4.6 stops thinking with
// another's in its history — and keeps the ones dest wrote, so a session moved back
// keeps its first server's. dest is where the request goes: a server's name, or ""
// for a host no server has. moving says this request moves the session, from
// where it ran. It runs before the request is sent, so a move costs no second
// request, and on requests the router leaves unrouted, so a move to the client's
// own provider is covered too.
func (p *Router) keepOwnReasoning(pctx *pipeline.Context, dest string, moving bool, from string) {
	key := reasoningKey(pctx)
	if key == "" || pctx.Observing() {
		return
	}
	p.reasoningMu.Lock()
	defer p.reasoningMu.Unlock()
	rec, ok := p.loadReasoning(pctx, key)
	if !ok {
		if !moving {
			return
		}
		rec = reasoningRecord{Base: from, Writers: map[string]string{}, Last: map[string]string{}}
	}
	conv := reasoningConversation(pctx.Body)
	writer, seen := rec.Last[conv]
	if !seen {
		writer = rec.Base
	}
	changed := !ok || writer != dest
	body, dropped := stripReasoning(pctx.Body, func(fp string) bool {
		w, known := rec.Writers[fp]
		if !known {
			w, rec.Writers[fp], changed = writer, writer, true
		}
		return w != dest
	})
	rec.Last[conv] = dest
	if changed {
		p.saveReasoning(pctx, key, rec)
	}
	if dropped > 0 && pctx.SetBody(body) {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "reasoning_stripped",
			Details: map[string]string{"blocks": strconv.Itoa(dropped)}})
	}
}

// renewReasoning keeps a moved session's record for another pinTTL, beside its pin.
func (p *Router) renewReasoning(pctx *pipeline.Context) {
	key := reasoningKey(pctx)
	if key == "" {
		return
	}
	p.reasoningMu.Lock()
	defer p.reasoningMu.Unlock()
	if rec, ok := p.loadReasoning(pctx, key); ok {
		p.saveReasoning(pctx, key, rec)
	}
}

// reasoningKey is the store key for pctx's session's record, "" when it has none a
// record could name: a synthetic session holds many conversations.
func reasoningKey(pctx *pipeline.Context) string {
	if pctx.SessionID == "" || synthetic(pctx.SessionID) {
		return ""
	}
	return reasoningPrefix + pctx.SessionID
}

func (p *Router) loadReasoning(pctx *pipeline.Context, key string) (reasoningRecord, bool) {
	var raw string
	if p.store != nil {
		raw, _ = p.store.Get(context.Background(), key)
	} else if pctx.Shared != nil {
		if v, ok := pctx.Shared.Get(key); ok {
			raw, _ = v.(string)
		}
	}
	var rec reasoningRecord
	if raw == "" || json.Unmarshal([]byte(raw), &rec) != nil || rec.Writers == nil || rec.Last == nil {
		return reasoningRecord{}, false
	}
	return rec, true
}

func (p *Router) saveReasoning(pctx *pipeline.Context, key string, rec reasoningRecord) {
	b, _ := json.Marshal(rec)
	if p.store != nil {
		if err := p.store.Set(context.Background(), key, string(b), pinTTL); err != nil {
			slog.Warn("inference-router: could not record which server wrote a moved session's reasoning", "error", err)
		}
		return
	}
	if pctx.Shared != nil {
		pctx.Shared.Put(key, string(b), pinTTL)
	}
}
