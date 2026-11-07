package a2aparser

import (
	"context"
	"log/slog"

	"github.com/rossoctl/cortex/core/parse/a2a"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// A2AParser parses A2A JSON-RPC 2.0 request bodies and populates
// pctx.Extensions.A2A with the parsed method, session ID, message parts,
// and role for downstream policy plugins (e.g., guardrails).
type A2AParser struct{}

func NewA2AParser() *A2AParser { return &A2AParser{} }

func init() {
	plugins.RegisterPlugin("a2a-parser", func() pipeline.Plugin { return NewA2AParser() })
}

func (p *A2AParser) Name() string { return "a2a-parser" }

func (p *A2AParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:   true,
		Description: "Parses A2A messages into pctx.Extensions.A2A for downstream plugins.",
	}
}

func (p *A2AParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if ext := a2a.ParseRequest(pctx.Path, pctx.Headers, pctx.Body); ext != nil {
		pctx.Extensions.A2A = ext
		pctx.Observe("matched_" + ext.Method)
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse is the legacy buffered-path response hook. Because this
// plugin implements StreamingResponder, pipeline.RunResponse skips it
// and OnResponseFrame is the dispatch path under all listeners — this
// method is unreachable from a normal listener. Kept for tests and
// hypothetical pipelines that call OnResponse directly without going
// through RunResponse, with a defensive guard against re-recording if
// the streaming path has already populated state.
func (p *A2AParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Extensions.A2A == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.A2A
	// If OnResponseFrame already finalized (any response field set),
	// don't re-record on the buffered path.
	if ext.FinalStatus != "" || ext.Artifact != "" || ext.ErrorMessage != "" {
		return pipeline.Action{Type: pipeline.Continue}
	}
	if len(pctx.ResponseBody) == 0 {
		pctx.Skip("no_response_body")
		return pipeline.Action{Type: pipeline.Continue}
	}

	a2a.ParseResponse(pctx.ResponseBody, ext)
	logA2AFinalized(ext)
	pctx.Observe("matched_" + ext.Method + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponseFrame folds each message/stream SSE event into the
// running A2A response state. Final-status and artifact text are
// accumulated across frames; the public extension fields are
// finalized on last=true so the session event sees a single
// consistent snapshot.
//
// application/json (message/send) responses are delivered as a
// single last=true frame carrying the full envelope — the same path
// extracts the response summary from it.
func (p *A2AParser) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if pctx.Extensions.A2A == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.A2A

	// Per-frame fold. message/stream events arrive one per frame;
	// message/send arrives as a single last=true frame. A buffered
	// message/stream (a body-writing plugin is in the chain) arrives as
	// one last=true frame that holds the whole SSE body.
	if len(frame) > 0 {
		a2a.ParseResponse(frame, ext)
	}

	if last {
		// Empty stream and we never recorded anything on the request
		// side — record a Skip so the response row is paired with the
		// request row in agentop.
		if ext.FinalStatus == "" && ext.Artifact == "" && ext.ErrorMessage == "" {
			pctx.Skip("no_response_body")
			return pipeline.Action{Type: pipeline.Continue}
		}
		logA2AFinalized(ext)
		pctx.Observe("matched_" + ext.Method + "_response")
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// logA2AFinalized emits the operator-facing debug log once a response
// is finalized — shared by OnResponse and OnResponseFrame so the two
// paths log identically.
func logA2AFinalized(ext *pipeline.A2AExtension) {
	slog.Debug("a2a-parser: response parsed",
		"sessionId", pipeline.SanitizeLogValue(ext.SessionID),
		"finalStatus", pipeline.SanitizeLogValue(ext.FinalStatus),
		"artifactLen", len(ext.Artifact),
		"error", pipeline.SanitizeLogValue(ext.ErrorMessage),
	)
}
