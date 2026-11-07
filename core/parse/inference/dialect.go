package inference

import "strings"

// dialect is the wire format an inference endpoint speaks, which decides how both its
// request and its response are read.
type dialect int

const (
	dialectNone      dialect = iota // not an endpoint this parser reads
	dialectOpenAI                   // OpenAI chat completions, or legacy completions
	dialectAnthropic                // Anthropic Messages
	dialectResponses                // OpenAI Responses API
)

// anthropicMessagesPath is the Anthropic Messages endpoint, and the ending that marks the
// dialect under any prefix: Anthropic's own /v1/messages, OpenCode Zen's /zen/v1/messages
// and /zen/go/v1/messages, and the /anthropic/v1/messages that gateways mount. Clients
// such as Claude Code POST here instead of to /v1/chat/completions, so the parser must
// recognise both dialects.
const anthropicMessagesPath = "/v1/messages"

// completionsSuffix ends every OpenAI-dialect inference path: /v1/chat/completions and the
// legacy /v1/completions, at the root or under any prefix — IBM Bob's /inference, OpenCode
// Zen's /zen and /zen/go, OpenRouter's /api, Groq's /openai, Azure's
// /openai/deployments/<d>.
const completionsSuffix = "/completions"

// responsesSuffix ends every OpenAI Responses-API path: the public /v1/responses, and
// Codex's chatgpt.com-hosted gateway at /backend-api/codex/responses — both end in
// "/responses" even though they share no common prefix, which is why this is a suffix of
// just the last segment rather than anchored to "/v1" the way completionsSuffix could
// afford to be loose about its own prefix. The same "match by ending, verify by body"
// split applies as it does for the other two dialects: a path that happens to end this
// way but whose body carries no `input` array is rejected by parseResponsesRequest, not
// here.
const responsesSuffix = "/responses"

// dialectFor reports which dialect path speaks, from how it ends. One function for the
// request and every response-side call site, so a request and its response cannot be read
// as different dialects.
//
// THE END OF THE PATH, NOT THE WHOLE OF IT. Providers mount these APIs under prefixes of
// their own, and matching whole paths needed a code change for each one. What keeps an
// unrelated endpoint that happens to end the same way out of the inference record is the
// body check in parseOpenAIRequest, parseAnthropicRequest and parseResponsesRequest, not
// this function.
//
// path must already be query-free (endpointPath). A trailing slash is not trimmed: no
// client is known to send one, and TestInferenceParser_BobNonInferencePathsAreIgnored pins
// it as not inference.
func dialectFor(path string) dialect {
	switch {
	case strings.HasSuffix(path, anthropicMessagesPath):
		return dialectAnthropic
	case strings.HasSuffix(path, completionsSuffix):
		return dialectOpenAI
	case strings.HasSuffix(path, responsesSuffix):
		return dialectResponses
	}
	return dialectNone
}
