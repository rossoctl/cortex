package toolprune

import (
	"github.com/tidwall/gjson"

	"github.com/rossoctl/cortex/core/pipeline"
)

// pruneEvent is the per-request record published under "tool-prune/event", so a
// consumer can show what this one request saved instead of only an aggregate.
//
// It carries FACTS ONLY — what was removed and how big the body was. No rates, no dollars.
//
// The money is not here. The accountant prices the saving from the ops this plugin reports
// (pctx.ReportOps) and the response's usage, and inference-parser publishes it in
// event.Event.Avoided, attributed to this plugin, so no consumer needs rates and none of them can
// disagree about the figure. These facts stay on the session timeline for a reader of one
// request; nothing prices them.
//
// No body content: the session store is unauthenticated, so this holds counts,
// tool names the operator themselves configured, and rates.
type pruneEvent struct {
	ToolsRemoved []string `json:"toolsRemoved,omitempty"`
	BytesRemoved int      `json:"bytesRemoved"`
	// BodyBytesAfter is the size of the body as this plugin left it, which is
	// not the pruned size under on_error: observe — there SetBody is a no-op and
	// the body it was handed goes on. A fact for a reader; nothing prices it.
	BodyBytesAfter int `json:"bodyBytesAfter"`
	// Projected marks a saving that was measured but NOT applied — observe mode.
	// The bytes were not actually removed from the request, so a consumer must
	// present this as "would have saved", never as money already not spent.
	// Set from this plugin's own SetBody result, not from BodyMutated, which
	// another writer's applied bytes would make true.
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
