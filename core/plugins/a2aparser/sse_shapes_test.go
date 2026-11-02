package a2aparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The two tests in this file pin issue #1567 item 2 for A2A. The event-stream format
// allows a leading UTF-8 BOM and CR-only line endings, and the buffered message/stream
// scanner used to split on LF over the raw body — so a stream shaped that way matched
// no "data:" line, the A2A extension stayed empty, and IBAC treated the exchange as
// unclassified, which passes by default. inferenceparser already normalizes these
// shapes (normalizeSSE); these tests hold the A2A side to the same bar.

// bufferedStreamResponse builds a message/stream body in the wire-legal shape named
// by bom and crOnly. The body starts directly with a data: line when bom is set —
// that is the case the BOM breaks: a BOM before an event: or comment line leaves the
// data: lines untouched, while a BOM before data: defeats the prefix check on the
// first line.
func bufferedStreamResponse(bom, crOnly bool) []byte {
	eol := "\n"
	if crOnly {
		eol = "\r"
	}
	body := `data: {"result":{"kind":"artifact-update","taskId":"task-1","artifact":{"parts":[{"kind":"text","text":"Hello world"}]}}}` + eol + eol
	body += `data: {"result":{"kind":"status-update","final":true,"status":{"state":"completed"}}}` + eol + eol
	if bom {
		body = "\xef\xbb\xbf" + body
	}
	return []byte(body)
}

func TestA2AParser_OnResponse_BufferedSSELeadingBOM(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{A2A: &pipeline.A2AExtension{Method: "message/stream"}},
		ResponseBody: bufferedStreamResponse(true, false),
	}
	if action := p.OnResponse(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext.Artifact != "Hello world" {
		t.Errorf("Artifact = %q, want %q — a leading BOM is wire-legal SSE and must not "+
			"hide the events from the buffered scanner", ext.Artifact, "Hello world")
	}
	if ext.FinalStatus != "completed" {
		t.Errorf("FinalStatus = %q, want %q", ext.FinalStatus, "completed")
	}
}

func TestA2AParser_OnResponse_BufferedSSECROnlyLineEndings(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{A2A: &pipeline.A2AExtension{Method: "message/stream"}},
		ResponseBody: bufferedStreamResponse(false, true),
	}
	if action := p.OnResponse(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext.Artifact != "Hello world" {
		t.Errorf("Artifact = %q, want %q — CR-only line endings are wire-legal SSE; a body "+
			"so shaped is ONE line to an LF split and every data: check missed it", ext.Artifact, "Hello world")
	}
	if ext.FinalStatus != "completed" {
		t.Errorf("FinalStatus = %q, want %q", ext.FinalStatus, "completed")
	}
}
