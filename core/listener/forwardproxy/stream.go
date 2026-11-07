package forwardproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/sseframe"
	"github.com/rossoctl/cortex/core/pipeline"
)

// classifyBrokenStream classifies a mid-body read error on a relay path
// (#1543). fail is what the response row should carry — client_canceled
// when the client hung up first, the transport kind otherwise, with the
// upstream's status as error.code exactly as the buffered path records it.
// Two of the errors that reach here are the proxy's own cuts, not the
// upstream's — the idle read cutoff and the per-frame size cap — and both
// record proxy_error, the kind docs/session-api.md reserves for the proxy's
// own errors, exactly as the buffered path records its own size limit. An
// operator reading an upstream_error row should be looking at the network,
// never at streamReadIdleTimeout.
//
// abort says the client's connection must be reset — the caller panics with
// http.ErrAbortHandler — instead of letting the handler return normally: a
// normal return makes net/http write the terminating chunk (h1) or
// END_STREAM (h2), handing the client a COMPLETE response off a stream the
// upstream cut off. ErrAbortHandler is the standard library's own abort
// signal: deferred finalization still runs, and net/http does not log it as
// a panic. abort is false only when the client hung up first — there is
// nobody left to abort for.
// pctx.StatusCode goes back to zero, as the buffered path does on a failed
// read: the deferred RunFinish derives the outcome from it after the handler
// returns, and a 200 there would report OutcomeAllow for an exchange the
// client saw reset — lineage labelling "ok" a failed one, the exact
// misreading the buffered path's zero exists to prevent. The row is
// unaffected: it records resp.StatusCode, the status already on the wire.
func classifyBrokenStream(pctx *pipeline.Context, r *http.Request, resp *http.Response, err error) (fail *pipeline.EventError, abort bool) {
	switch {
	case errors.Is(err, errIdleCut):
		fail = &pipeline.EventError{Kind: "proxy_error", Message: fmt.Sprintf("stream idle over the proxy's %s read cutoff", streamReadIdleTimeout)}
	case errors.Is(err, sseframe.ErrFrameTooLarge):
		fail = &pipeline.EventError{Kind: "proxy_error", Message: fmt.Sprintf("single SSE frame over the %d-byte frame cap", maxBodySize)}
	default:
		_, fail = pipeline.ExchangeFailure(r.Context(), err)
	}
	fail.Code = strconv.Itoa(resp.StatusCode)
	pctx.StatusCode = 0
	if r.Context().Err() != nil {
		slog.Debug("forward-proxy: streaming response ended by client hangup", "host", pipeline.SanitizeLogValue(r.Host), "error", err)
		return fail, false
	}
	slog.Warn("forward-proxy: streaming response read error — aborting the client connection",
		"host", pipeline.SanitizeLogValue(r.Host), "kind", fail.Kind, "error", fail.Message)
	return fail, true
}

// isKnownMislabeledSSE reports whether host+path is a known endpoint that sends real SSE
// wire bytes (event:/data: lines) under the wrong Content-Type.
//
// Confirmed on live traffic: Codex's chatgpt.com-hosted Responses API gateway sends
// event-stream content labeled Content-Type: application/json. Content-Type is otherwise
// the single signal this function's caller trusts to decide streaming vs. buffered — see
// isEventStream — and with the wrong header, this response would never reach
// handleStreamingResponse at all, forcing the buffered path (and losing live delivery to
// the client) regardless of what any plugin declares. inference-parser's own dispatch has
// a second, independent content-sniff fallback (carriesSSEFraming) for the case where the
// buffered path is still what runs — this is the other half, letting the response stream
// live in the first place when the one plugin that cares (inference-parser) supports it.
//
// Deliberately a narrow, named-endpoint allowlist rather than sniffing every response's
// body to detect SSE by content: that would mean reading ahead on EVERY response regardless
// of provider, to work around one provider's one wrong header. Add a line here if another
// endpoint turns out to have the same defect.
func isKnownMislabeledSSE(host, path string) bool {
	return host == "chatgpt.com" && path == "/backend-api/codex/responses"
}

// handleStreamingResponse forwards a text/event-stream response to the
// downstream client block by block. Each SSE block carrying data is
// delivered to the pipeline's StreamingResponder plugins (recording-only
// today), and then every block — data or not — is written exactly as the
// upstream sent it and flushed. End-of-stream
// is signaled to plugins with one final last=true call so aggregating
// plugins (inference-parser, a2a-parser) can finalize their running
// state. RunResponse is intentionally NOT invoked on this path —
// streaming-aware plugins move their finalization logic into
// OnResponseFrame(last=true), and legacy non-migrated plugins are
// not called on streaming responses (cleaner contract; no fragmented
// double-dispatch).
func (s *Server) handleStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, pctx *pipeline.Context) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flusher means the downstream connection can't deliver
		// bytes incrementally — fall back to buffered. http.Flusher
		// is supported by net/http's default ResponseWriter, so this
		// is a defensive guard for exotic wrappers (httptest with a
		// custom recorder, embedded servers).
		slog.Warn("forward-proxy: ResponseWriter does not support flushing — falling back to buffered for streaming response", "host", pipeline.SanitizeLogValue(r.Host))
		s.streamFallbackBuffered(w, r, resp, pctx)
		return
	}

	// Set by the read-error arm below; the deferred recorder puts it on the
	// response row so a stream the upstream broke mid-body stops showing as
	// a clean 200 in agentop (#1543).
	var streamFail *pipeline.EventError

	// Defer the final last=true dispatch + session-event recording so
	// every exit path (normal EOF, upstream read error, downstream
	// client-write error, the ErrAbortHandler panic below) finalizes
	// aggregating plugins and records the response event. Without this, a
	// client disconnect mid-stream leaves inference/a2a stuck in an
	// unfinalized state and emits no SessionResponse row to agentop.
	defer func() {
		// Use a detached, BOUNDED context for finalization: the client may have
		// cancelled the request context after reading the full stream, but
		// aggregating plugins (inference-parser, session-budget) still need their
		// last=true dispatch to finalize state — and detaching alone would leave that
		// dispatch with nothing that could ever stop it. See httpx.TeardownContext.
		finalCtx, cancelFinal := httpx.TeardownContext(r.Context())
		defer cancelFinal()
		// DELIVERED: the headers and every frame are on the wire by now, which the Warn below
		// already says. Marking it keeps a late refusal from becoming this request's OUTCOME as
		// well — the same statement ext_proc's flush and the reverse proxy's finalize make.
		pctx.MarkResponseDelivered()
		finalAction := s.OutboundPipeline.RunResponseFrame(finalCtx, pctx, nil, true)
		if finalAction.Type == pipeline.Reject {
			// Headers already sent; we can't promote to 502, but
			// surface the policy violation so operators see it.
			slog.Warn("forward-proxy: streaming response rejected on finalization (headers already sent)",
				"host", pipeline.SanitizeLogValue(r.Host), "violation", finalAction.Violation)
		}
		s.recordOutboundResponseEvent(pctx, resp.StatusCode, streamFail)
	}()

	// Forward headers and the streaming status code BEFORE the first
	// frame is written. Strip Content-Length since we'll be writing
	// chunked; copyResponseHeaders clears the hop-by-hop set as net/http would.
	copyResponseHeaders(w, resp)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	reader := sseframe.NewReader(idleReader(resp.Body, streamReadIdleTimeout), maxBodySize)
	for {
		block, err := reader.ReadBlock()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Read error or oversized single frame. We can't promote this to
			// 502 — headers are sent — so the client's only honest signal is a
			// broken connection: returning normally would write the terminating
			// chunk and hand it a COMPLETE stream off a cut-off one (#1543).
			// The deferred finalize + recorder above still run, both on the
			// panic path and the plain break.
			// bytesRead, not the bytesWritten this used to keep in a local: the
			// local was declared after the recording defer and never reached the
			// session event, so the figure is now taken off the counting wrapper
			// around resp.Body — which is bytes received, not frames relayed.
			var abort bool
			streamFail, abort = classifyBrokenStream(pctx, r, resp, err)
			if abort {
				panic(http.ErrAbortHandler)
			}
			break
		}

		// Plugins see the payload BEFORE the block's bytes are written, so a
		// Reject still keeps the rejected block off the wire. Blocks with no
		// data line — heartbeats, a lone retry: — are not shown to plugins,
		// exactly as ReadFrame never returned them.
		if block.HasData {
			respAction := s.OutboundPipeline.RunResponseFrame(r.Context(), pctx, block.Data, false)
			if respAction.Type == pipeline.Reject {
				// Headers + earlier frames already on the wire — log and
				// stop forwarding. The downstream client sees a truncated
				// stream, which is the best we can do without inspect-
				// before-forward semantics.
				slog.Warn("forward-proxy: streaming response rejected mid-stream by plugin",
					"host", pipeline.SanitizeLogValue(r.Host), "violation", respAction.Violation)
				break
			}
		}

		// The block exactly as the upstream sent it. Nothing here needs a
		// rebuilt frame: StreamingResponders only read, and a response-body
		// writer forces the buffered path before this one is chosen. A rebuild
		// used to drop event:, id:, retry: and comment lines, which clients
		// type, resume and keep alive by.
		if _, err := w.Write(block.Raw); err != nil {
			slog.Debug("forward-proxy: streaming write error", "host", pipeline.SanitizeLogValue(r.Host), "error", err)
			break
		}
		flusher.Flush()
	}
}

// streamPassthrough forwards a text/event-stream response to the downstream
// client byte-for-byte with per-write flushing. It is the streaming path when
// no StreamingResponder plugin is configured (a plain proxy pipeline). It
// relays the exact upstream bytes, as handleStreamingResponse does; the two
// differ only in whether StreamingResponders are dispatched. Without this path such a response fell
// through to an unflushed io.Copy and never reached the client until the
// upstream closed the connection (issue #642).
//
// Unlike handleStreamingResponse, the response-phase pipeline (RunResponse) IS
// run before the first byte. That is safe only because this path is reached
// exclusively when no StreamingResponder is configured, so RunResponse cannot
// double-dispatch a plugin that also handles OnResponseFrame. Running it lets
// header/status-based response gates fire on streamed responses too — e.g.
// opa's response-phase deny (status + headers) and litellm-budgettrack's cost
// accounting (a response header) — and a deny is honored before any byte is
// written. The plugins reachable here do not read pctx.ResponseBody in
// OnResponse, so leaving the body unbuffered is fine; body-level response
// inspection on a stream requires implementing StreamingResponder.
func (s *Server) streamPassthrough(w http.ResponseWriter, r *http.Request, resp *http.Response, pctx *pipeline.Context) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No incremental delivery possible on this ResponseWriter — buffer.
		// http.Flusher is implemented by net/http's default writer; this is a
		// defensive guard for test recorders and exotic wrappers.
		slog.Warn("forward-proxy: ResponseWriter does not support flushing — falling back to buffered for streaming response", "host", pipeline.SanitizeLogValue(r.Host))
		s.streamFallbackBuffered(w, r, resp, pctx)
		return
	}

	// Run the response-phase pipeline before the first byte so header/status
	// gates still fire on a streamed response and a deny short-circuits before
	// anything is written. streamFallbackBuffered runs its own RunResponse, so
	// this is done only on the flushing path to avoid double-dispatch.
	if respAction := s.OutboundPipeline.RunResponse(r.Context(), pctx); respAction.Type == pipeline.Reject {
		s.rejectResponse(w, pctx, respAction)
		return
	}

	// Set by the read-error arm below; the deferred recorder puts it on the
	// response row so a stream the upstream broke mid-body stops showing as a
	// clean 200 in agentop (#1543).
	var streamFail *pipeline.EventError

	// Record the response event on every exit path (normal EOF, upstream read
	// error, downstream write error, the ErrAbortHandler panic below) so a
	// SessionResponse row still lands. A closure, not a bare defer call:
	// defer evaluates its arguments up front, so streamFail must be read at
	// exit time, not at entry.
	defer func() {
		s.recordOutboundResponseEvent(pctx, resp.StatusCode, streamFail)
	}()

	// Forward headers + status before the first byte. Drop Content-Length since
	// we relay an open-ended chunked stream.
	copyResponseHeaders(w, resp)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	// Copy raw chunks and flush each so intermittent SSE events reach the client
	// immediately. idleReader bounds a wedged upstream; total size stays
	// unbounded so long-lived streams aren't cut off.
	body := idleReader(resp.Body, streamReadIdleTimeout)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				slog.Debug("forward-proxy: streaming write error", "host", pipeline.SanitizeLogValue(r.Host), "error", writeErr)
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				// Returning normally would write the terminating chunk and
				// hand the client a COMPLETE stream off a cut-off one (#1543);
				// the deferred recorder above still runs on the panic path.
				var abort bool
				streamFail, abort = classifyBrokenStream(pctx, r, resp, readErr)
				if abort {
					panic(http.ErrAbortHandler)
				}
			}
			return
		}
	}
}

// streamFallbackBuffered handles the rare case of a streaming
// Content-Type response on a ResponseWriter that doesn't support
// http.Flusher — buffer the whole SSE body, then re-parse it through
// sseframe so streaming-aware plugins receive one OnResponseFrame call
// per SSE event followed by last=true. Without per-frame dispatch the
// inference parser (and any future fold-and-finalize plugin) would try
// to JSON-decode the whole SSE blob as one chunk, fail, and clobber a
// correctly-parsed completion. Production ResponseWriters implement
// http.Flusher so this path is mostly hit in tests.
func (s *Server) streamFallbackBuffered(w http.ResponseWriter, r *http.Request, resp *http.Response, pctx *pipeline.Context) {
	respBody, ok := s.bufferResponseBody(w, r, resp, pctx)
	if !ok {
		return
	}
	pctx.ResponseBody = respBody
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	// The same detach as the fold and the settle below, on the site the earlier fix walked
	// past: this runs after io.ReadAll too, so a hangup during the read denies the response
	// phase and returns before anything is recorded.
	phaseCtx, cancelPhase := httpx.TeardownContext(r.Context())
	defer cancelPhase()
	respAction := s.OutboundPipeline.RunResponse(phaseCtx, pctx)
	if respAction.Type == pipeline.Reject {
		s.rejectResponse(w, pctx, respAction)
		return
	}
	if s.OutboundPipeline.ForPctx(pctx).HasStreamingResponders() {
		// Re-parse the buffered SSE body frame-by-frame so plugins see the
		// same per-event shape as the real streaming path. A Reject is
		// honored here — headers are not yet on the wire.
		//
		// ON A DETACHED, BOUNDED CONTEXT, exactly as the streaming path's finish defer is, and
		// for a reason that is easy to miss here: the whole body is already in hand by this
		// point (io.ReadAll above), so a client that hung up while it was being read leaves a
		// cancelled r.Context() — and RunResponseFrame refuses a cancelled context before
		// calling any plugin, returning a Deny this loop cannot tell apart from a policy
		// reject. It would then write a rejection and RETURN, skipping
		// recordOutboundResponseEvent below: no settled cost and no response row for a
		// response that arrived complete. Detaching also restores the meaning of a Reject
		// here — with the cancellation case gone, one can only come from a plugin.
		//
		// TWO CONTEXTS, ONE PER PIECE OF WORK. The fold below dispatches every frame through
		// the pipeline, and a plugin doing anything slow per frame would spend the budget the
		// SETTLE needs — the terminal dispatch is the one that turns the folded state into a
		// charge, and it would inherit whatever was left. Each gets its own deadline, on the
		// same rule the reverse proxy's finalize() follows: the clock starts when the work
		// does, not when its parent did.
		foldCtx, cancelFold := httpx.TeardownContext(r.Context())
		defer cancelFold()
		reader := sseframe.NewReader(bytes.NewReader(respBody), maxBodySize)
		for {
			frame, ferr := reader.ReadFrame()
			if ferr == io.EOF {
				break
			}
			if ferr != nil {
				slog.Warn("forward-proxy: streaming response read error in fallback", "host", pipeline.SanitizeLogValue(r.Host), "error", ferr)
				break
			}
			frameAction := s.OutboundPipeline.RunResponseFrame(foldCtx, pctx, frame, false)
			if frameAction.Type == pipeline.Reject {
				s.rejectResponse(w, pctx, frameAction)
				return
			}
		}
		// A FRESH DEADLINE FOR THE SETTLE. See the two-contexts note above: the fold has had
		// its own budget, and the dispatch that turns folded state into a charge gets a whole
		// one rather than the remainder.
		finalCtx, cancelFinal := httpx.TeardownContext(r.Context())
		defer cancelFinal()
		finalAction := s.OutboundPipeline.RunResponseFrame(finalCtx, pctx, nil, true)
		if finalAction.Type == pipeline.Reject {
			s.rejectResponse(w, pctx, finalAction)
			return
		}
	}
	s.recordOutboundResponseEvent(pctx, resp.StatusCode, nil)
	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Debug("response copy error", "host", pipeline.SanitizeLogValue(r.Host), "error", err)
	}
}
