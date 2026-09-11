package toolprune

import (
	"github.com/tidwall/gjson"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// pruneEvent is the per-request record published under "tool-prune/event", so a
// consumer can show what this one request saved instead of only an aggregate.
//
// It carries FACTS ONLY — what was removed and how big the body was. No rates, no dollars.
//
// It used to carry rates so a consumer could do the arithmetic, because the dollar amount
// depends on which prompt-cache tier the saving came out of and that is only known from the
// response. The conclusion was right and the remedy was backwards: the money step belongs
// where both halves are in hand, which is the cost owner on the response side. It publishes
// the priced saving in costevent.Event.Avoided, attributed to this plugin, so no consumer
// needs rates and none of them can disagree about the figure.
//
// No body content: the session store is unauthenticated, so this holds counts,
// tool names the operator themselves configured, and rates.
type pruneEvent struct {
	ToolsRemoved []string `json:"toolsRemoved,omitempty"`
	BytesRemoved int      `json:"bytesRemoved"`
	// BodyBytesAfter is the size of the body actually SENT upstream, which is
	// not the pruned size under on_error: observe — there SetBody is a no-op and
	// the original goes out. A consumer divides the response's prompt-token
	// count by this to get tokens-per-byte, so using the pruned size while the
	// original was billed would inflate that ratio and overstate the saving.
	BodyBytesAfter int `json:"bodyBytesAfter"`
	// Projected marks a saving that was measured but NOT applied — observe mode.
	// The bytes were not actually removed from the request, so a consumer must
	// present this as "would have saved", never as money already not spent.
	Projected bool   `json:"projected,omitempty"`
	Model     string `json:"model,omitempty"`
}

func (p *ToolPrune) publish(pctx *pipeline.Context, ev pruneEvent) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix] = ev
}

// inferenceModel returns the model the parser recorded, or "" when no parser has
// run — in which case rate lookup falls through to the flat fallback.
func inferenceModel(pctx *pipeline.Context) string {
	if pctx.Extensions.Inference == nil {
		return ""
	}
	return pctx.Extensions.Inference.Model
}

// toolsCitedByHistory returns the tool names the conversation already used, from
// tool_use blocks in assistant messages.
//
// Those tools must survive pruning: a provider may reject a request whose history
// references a tool the manifest no longer defines. Enabling the plugin
// mid-conversation is exactly when this arises, because the config hot-reloads and
// the scan's window can propose a tool that was used earlier in the same session.
//
// Scanned with gjson paths rather than a full unmarshal — a Claude Code body runs
// to hundreds of KB and this is the request hot path.
func toolsCitedByHistory(body []byte) map[string]struct{} {
	out := map[string]struct{}{}
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "tool_use" {
				return true
			}
			if n := block.Get("name"); n.Type == gjson.String && n.String() != "" {
				out[n.String()] = struct{}{}
			}
			return true
		})
		return true
	})
	return out
}
