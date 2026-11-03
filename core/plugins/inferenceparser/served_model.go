package inferenceparser

import (
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
)

// servedModelHeader is the header a LiteLLM gateway names its deployment's model in,
// for example "bedrock/us.anthropic.claude-opus-4-6-v1" for a request that asked for
// "claude-opus-4-6". LiteLLM's documentation does not list it, but every gateway this
// was measured on (LiteLLM 1.105.1) sends it on chat completions and on /v1/messages.
// The body's "model" is no help there: LiteLLM repeats the requested name in it.
const servedModelHeader = "X-Litellm-Model-Name"

// maxServedModelLen bounds a model name read off the wire. A real one is well under
// it; the cap only stops an upstream from storing an arbitrarily long string on
// every event.
const maxServedModelLen = 256

// noteBodyModel keeps the response body's "model" field as the served model when no
// header has named one. A provider API can answer with a dated version of the name
// it was asked for; a gateway usually repeats the requested name, which servedModel
// then drops as no news.
func noteBodyModel(ext *pipeline.InferenceExtension, model string) {
	if ext == nil || model == "" || ext.ServedModel != "" {
		return
	}
	ext.ServedModel = cleanServedModel(model)
}

// settleServedModel decides ServedModel at the end of a response: the gateway
// header when there is one, else what noteBodyModel kept, and nothing when the
// result only repeats Model.
func settleServedModel(pctx *pipeline.Context) {
	ext := pctx.Extensions.Inference
	if ext == nil {
		return
	}
	if v := pctx.ResponseHeaders.Get(servedModelHeader); v != "" {
		ext.ServedModel = cleanServedModel(v)
	}
	if ext.ServedModel == ext.Model {
		ext.ServedModel = ""
	}
}

// cleanServedModel makes an upstream-supplied name safe to store and print: control
// runes replaced, length capped.
func cleanServedModel(s string) string {
	s = pipeline.SanitizeControlRunes(s)
	if len(s) > maxServedModelLen {
		s = strings.ToValidUTF8(s[:maxServedModelLen], "") // the cut may split a rune
	}
	return s
}
