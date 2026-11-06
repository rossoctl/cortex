package inferencerouter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// trimAbove and trimTo are the share of a window a request may fill before its
	// oldest turns are dropped, and the share it is brought back under. The gap keeps
	// one cut for many turns, so the start of the request, and the server's prompt
	// cache for it, stay the same from turn to turn.
	trimAbove = 0.85
	trimTo    = 0.60

	// bytesPerToken estimates tokens from a body's length. Text runs nearer 4 bytes a
	// token, so the estimate is high, and a request is trimmed a little early rather
	// than refused.
	bytesPerToken = 3

	trimNote = "[Cortex: %d earlier messages were removed so the conversation fits this model's context window.]"
)

// outputFields are where the request formats put the output limit: Anthropic messages
// and chat completions, chat completions' newer name, and responses.
var outputFields = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// fitResult is a body fitted to a model's limits, and what fitting changed.
type fitResult struct {
	body       []byte
	field      string // the output field lowered, "" for none
	from, to   int
	dropped    int // messages removed
	cut        int // index into the client's messages where the kept tail starts
	estimate   int // tokens, after fitting
	unfittable bool
}

func estimate(body []byte) int { return len(body) / bytesPerToken }

// fitBody fits body to l: its oldest turns dropped when it nears the window, then its
// output field lowered to the cap and, for a window that counts both, to what the
// prompt leaves. cut is the session's last cut, 0 for none.
func fitBody(body []byte, l limits, cut int) fitResult {
	r := fitResult{body: body}
	if l.Window > 0 {
		r = trim(r, l.Window, cut)
	}
	r.estimate = estimate(r.body)
	return capOutput(r, l)
}

// trim drops whole messages after the first user message, oldest first, by the
// package rules: the session's last cut is kept while it still brings a request that
// needs trimming under trimAbove, else a new cut brings the body under trimTo. A cut
// is made only where a user message without a tool result starts. A body it
// cannot bring within the window is left as it was.
func trim(r fitResult, window, cut int) fitResult {
	items, head := conversation(r.body)
	if head < 0 {
		return r
	}
	var starts []int
	for i := head + 1; i < len(items); i++ {
		if cutPoint(items[i]) {
			starts = append(starts, i)
		}
	}
	size := func(k int) int { // the estimate with items[head:k] dropped and the note added
		dropped := 0
		for _, m := range items[head:k] {
			dropped += len(m.Raw) + 1
		}
		return (len(r.body) - dropped + len(trimNote)) / bytesPerToken
	}
	high, low := int(float64(window)*trimAbove), int(float64(window)*trimTo)
	total := estimate(r.body)
	k := 0
	switch {
	case total <= high:
		return r
	case contains(starts, cut) && size(cut) <= high:
		k = cut
	default:
		for _, s := range starts {
			if size(s) <= low {
				k = s
				break
			}
		}
		if k == 0 && len(starts) > 0 {
			k = starts[len(starts)-1]
		}
	}
	if k == 0 || size(k) > window {
		r.unfittable = total > window
		return r
	}
	raws := make([]string, 0, head+len(items)-k)
	for _, m := range items[:head] {
		raws = append(raws, m.Raw)
	}
	raws = append(raws, noted(items[k], fmt.Sprintf(trimNote, k-head)))
	for _, m := range items[k+1:] {
		raws = append(raws, m.Raw)
	}
	body, err := sjson.SetRawBytes(r.body, "messages", []byte("["+strings.Join(raws, ",")+"]"))
	if err != nil {
		return r
	}
	r.body, r.dropped, r.cut = body, k-head, k
	return r
}

// conversation is body's messages and the index after its first user message, which
// trimming always keeps; -1 when there is no user message.
func conversation(body []byte) ([]gjson.Result, int) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil, -1
	}
	items := msgs.Array()
	for i, m := range items {
		if m.Get("role").String() == "user" {
			return items, i + 1
		}
	}
	return items, -1
}

// conversationKey names the conversation body belongs to by the messages trimming
// always keeps, so two conversations sent under one session — a main thread and a
// subagent — and a conversation its client compacted each get their own cut.
func conversationKey(body []byte) string {
	items, head := conversation(body)
	if head < 0 {
		return ""
	}
	h := sha256.New()
	for _, m := range items[:head] {
		h.Write([]byte(m.Raw))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// cutPoint reports whether a kept tail may start at m: a user message carrying no
// tool result, so a kept tool result keeps the call it answers. Chat completions puts
// tool results in role "tool", which is not "user".
func cutPoint(m gjson.Result) bool {
	if m.Get("role").String() != "user" {
		return false
	}
	content := m.Get("content")
	if !content.IsArray() {
		return true
	}
	for _, block := range content.Array() {
		if block.Get("type").String() == "tool_result" {
			return false
		}
	}
	return true
}

// noted is message m with note at the start of its content.
func noted(m gjson.Result, note string) string {
	content := m.Get("content")
	switch {
	case content.Type == gjson.String:
		out, err := sjson.Set(m.Raw, "content", note+"\n\n"+content.String())
		if err == nil {
			return out
		}
	case content.IsArray():
		block, _ := json.Marshal(map[string]string{"type": "text", "text": note})
		inner := strings.TrimSpace(content.Raw[1 : len(content.Raw)-1])
		arr := "[" + string(block) + "]"
		if inner != "" {
			arr = "[" + string(block) + "," + inner + "]"
		}
		out, err := sjson.SetRaw(m.Raw, "content", arr)
		if err == nil {
			return out
		}
	}
	return m.Raw
}

// capOutput lowers the body's output field to l's cap, and for a window that counts
// input and output together to what r's estimate leaves of it. The estimate runs
// high, so when it reaches the window the combined cap is skipped rather than asking
// for a one-token answer to a request that may well fit; the plain cap still
// applies. A value is never raised, and a field that is absent or not a number is
// left alone. Neither is one when the body's thinking.budget_tokens is at least the
// cap: Anthropic and Bedrock refuse a max_tokens that is not above the budget, so the
// field stays as the agent sent it and the server's own answer reaches the agent.
func capOutput(r fitResult, l limits) fitResult {
	limit := l.Output
	if l.Combined && l.Window > 0 && r.estimate < l.Window {
		room := l.Window - r.estimate
		if limit == 0 || room < limit {
			limit = room
		}
	}
	if limit == 0 {
		return r
	}
	if b := gjson.GetBytes(r.body, "thinking.budget_tokens"); b.Type == gjson.Number && limit <= int(b.Int()) {
		return r
	}
	for _, f := range outputFields {
		v := gjson.GetBytes(r.body, f)
		if v.Type != gjson.Number || int(v.Int()) <= limit {
			continue
		}
		body, err := sjson.SetBytes(r.body, f, limit)
		if err != nil {
			continue
		}
		r.body, r.field, r.from, r.to = body, f, int(v.Int()), limit
	}
	return r
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

const (
	// cutPrefix namespaces a session's last cut, kept in process memory only: after a
	// restart the first trim is computed afresh.
	cutPrefix = Name + "/cut/"
	cutFor    = 24 * time.Hour
)

// applyLimits fits a request routed to server to what is known about the model it is
// sent with, records what it changed, and reports whether it changed the body.
// remember keeps the cut for the session's next request; a resend passes false,
// because its body was fitted once already and its cut would not index the client's
// messages.
func (p *Router) applyLimits(pctx *pipeline.Context, server string, remember bool) bool {
	if !switched(pctx) {
		return false
	}
	model, ok := pctx.RequestModel()
	if !ok {
		return false
	}
	l := p.loadLimits(pctx, server, model)
	if l == (limits{}) {
		return false
	}
	key := ""
	if pctx.Shared != nil && pctx.SessionID != "" && !synthetic(pctx.SessionID) {
		if conv := conversationKey(pctx.Body); conv != "" {
			key = cutPrefix + pctx.SessionID + "/" + conv
		}
	}
	cut := 0
	if key != "" && remember {
		if v, ok := pctx.Shared.Get(key); ok {
			cut, _ = v.(int)
		}
	}
	r := fitBody(pctx.Body, l, cut)
	if r.unfittable {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "limit_unfittable",
			Details: map[string]string{"estimate": strconv.Itoa(estimate(pctx.Body)), "window": strconv.Itoa(l.Window)}})
	}
	if string(r.body) == string(pctx.Body) || !pctx.SetBody(r.body) {
		return false
	}
	if key != "" && remember && r.cut > 0 {
		pctx.Shared.Put(key, r.cut, cutFor)
	}
	if r.dropped > 0 {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "context_trimmed", Details: map[string]string{
			"dropped": strconv.Itoa(r.dropped), "estimate": strconv.Itoa(r.estimate), "window": strconv.Itoa(l.Window)}})
	}
	if r.field != "" {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "output_capped", Details: map[string]string{
			"field": r.field, "from": strconv.Itoa(r.from), "to": strconv.Itoa(r.to)}})
	}
	return true
}

// switched reports whether a routed request goes somewhere other than the agent
// asked, or with another model: only then can the model it reaches have limits the
// agent did not size the request for.
func switched(pctx *pipeline.Context) bool {
	ext := pctx.Extensions.Inference
	return pctx.RequestedHost() != "" || (ext != nil && ext.RequestedModel != "")
}
