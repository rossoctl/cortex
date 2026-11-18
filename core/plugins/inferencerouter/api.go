package inferencerouter

import (
	"net/http"
	"slices"
	"strings"

	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/translate"
)

// planKind is what the server's api map does to a request (rules 4 to 7, C1).
type planKind int

const (
	planAsIs        planKind = iota // the agent's protocol, to the agent's path
	planPath                        // the agent's protocol, to the api map's path
	planTranslate                   // converted to target, sent to path
	planRefuse                      // no protocol of the map can be translated to (rule 4a)
	planCountTokens                 // answered here with a 404 (rule 6)
)

type apiPlan struct {
	kind                 planKind
	source, target, path string
}

// countTokensSuffix ends Anthropic's token-count path.
const countTokensSuffix = "/v1/messages/count_tokens"

// translationServerDoc is the fallback doc that the rule 4a error names.
const translationServerDoc = "https://github.com/rossoctl/cortex/blob/main/docs/translation-server.md"

// plan says how a request to path goes to r, from r's api map.
func (r route) plan(path string) apiPlan {
	if len(r.api) == 0 {
		return apiPlan{kind: planAsIs}
	}
	if _, ok := r.api[translate.Anthropic]; !ok && strings.HasSuffix(path, countTokensSuffix) {
		return apiPlan{kind: planCountTokens}
	}
	src := inference.ProtocolFor(path)
	if p, ok := r.api[src]; ok {
		return apiPlan{kind: planPath, path: p}
	}
	if translate.Decodes(src) {
		tgt, ok := translate.Target(apiKeys(r))
		if !ok {
			return apiPlan{kind: planRefuse, source: src}
		}
		return apiPlan{kind: planTranslate, source: src, target: tgt, path: r.api[tgt]}
	}
	return apiPlan{kind: planAsIs}
}

// apiKeys are the protocols of r's api map, sorted.
func apiKeys(r route) []string {
	keys := make([]string, 0, len(r.api))
	for k := range r.api {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// cannotTranslate is the rule 4a message (decision 13).
func cannotTranslate(source string, r route) string {
	return "Cortex cannot translate " + source + " to " + strings.Join(apiKeys(r), " or ") +
		" yet. Use a translation server for this pair (see " + translationServerDoc + ")."
}

// refusePlan records and gives the local answer for planRefuse and planCountTokens.
func refusePlan(pctx *pipeline.Context, r route, pl apiPlan, details map[string]string) pipeline.Action {
	if pl.kind == planCountTokens {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "count_tokens_local", Details: details})
		const msg = "this inference server does not count tokens"
		return pipeline.Action{Type: pipeline.Reject, Violation: &pipeline.Violation{
			Code: "inference.count_tokens_unavailable", Reason: msg, Status: http.StatusNotFound,
			Body:     translate.ErrorBody(translate.Anthropic, http.StatusNotFound, "not_found_error", msg),
			BodyType: "application/json",
		}}
	}
	details["from"] = pl.source
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "cannot_translate", Details: details})
	msg := cannotTranslate(pl.source, r)
	return pipeline.Action{Type: pipeline.Reject, Violation: &pipeline.Violation{
		Code: "inference.translation_unavailable", Reason: msg, Status: http.StatusBadGateway,
		Body:     translate.ErrorBody(pl.source, http.StatusBadGateway, "api_error", msg),
		BodyType: "application/json",
	}}
}

// authHeaderNames are the headers an agent can carry its key in.
var authHeaderNames = []string{"Authorization", "X-Api-Key", "Api-Key"}

// setAuth puts r's key on the request (rule C2). translatedTo is the target protocol,
// "" when the request is not translated. With no auth_header and no translation, it
// is setKey: the header the agent used.
func setAuth(pctx *pipeline.Context, r route, translatedTo string) {
	name := r.authHeader
	switch {
	case name == "" && translatedTo == translate.Chat:
		name = "authorization"
	case name == "" && translatedTo == translate.Anthropic:
		name = "x-api-key"
	case name == "":
		setKey(pctx, r.key)
		return
	}
	if pctx.Headers == nil {
		pctx.Headers = http.Header{}
	}
	for _, h := range authHeaderNames {
		pctx.Headers.Del(h)
	}
	if strings.EqualFold(name, "authorization") {
		pctx.Headers.Set("Authorization", "Bearer "+r.key)
		return
	}
	pctx.Headers.Set(name, r.key)
}
