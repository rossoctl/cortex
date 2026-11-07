package sessionapi

import "slices"

// detailKeys is, for each extension, the fields the event detail view shows: on a request event
// at index 0, on a response event at index 1. An event of one phase carries the other phase's
// fields too, and the view hides them. The search route hides them the same way (search.go), so
// a search matches only what the operator can see.
//
// Order is not significant: the detail view sorts keys.
var detailKeys = map[string][2][]string{
	"inference": {
		{"model", "requestedModel", "messages", "temperature", "maxTokens", "topP",
			"stream", "tools", "toolChoice", "toolResults", "historyToolCalls"},
		// reasoningTokens is a SUBSET of completionTokens, not a sibling: it is the share
		// of the generated tokens the model spent reasoning, billed at the output rate.
		// Adding the two double-counts every one of them.
		//
		// THE DETAIL PANE IS WHERE THE EXACT FIGURE LIVES. The events table shows one total
		// per row and the spend drawer shows a proportion, so a reader who wants the number
		// rather than the shape comes here — which is also why the key needs no
		// abbreviating: there is room for the whole word, unlike a table column.
		{"model", "requestedModel", "completion", "finishReason", "promptTokens",
			"completionTokens", "reasoningTokens", "totalTokens", "toolCalls",
			"cacheWriteTokens", "cacheReadTokens"},
	},
	"mcp": {
		{"method", "rpcId", "params"},
		{"method", "rpcId", "result", "error"},
	},
	// A2A: OnResponse captures the server-assigned contextId plus a summary
	// of the final result (finalStatus / artifact / errorMessage / taskId).
	// Drop the request-side message fields (messageId, role, parts) on
	// response rows so the detail view reflects what the response phase
	// actually contributed.
	"a2a": {
		{"method", "rpcId", "sessionId", "messageId", "taskId", "role", "parts"},
		{"method", "rpcId", "sessionId", "taskId", "finalStatus", "artifact", "errorMessage"},
	},
}

// DetailKeys is the fields of extension ext ("inference", "mcp" or "a2a") that the event detail
// view shows: on a response event when response is true, on a request event otherwise. agentop's
// detail pane and the search route read this one list. Nil for any other ext.
func DetailKeys(ext string, response bool) []string {
	k, ok := detailKeys[ext]
	if !ok {
		return nil
	}
	return slices.Clone(k[phaseIndex(response)])
}

// detailShows reports whether the detail view shows field key of extension ext.
func detailShows(ext string, response bool, key string) bool {
	k, ok := detailKeys[ext]
	return ok && slices.Contains(k[phaseIndex(response)], key)
}

func phaseIndex(response bool) int {
	if response {
		return 1
	}
	return 0
}
