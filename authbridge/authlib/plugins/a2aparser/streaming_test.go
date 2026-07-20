package a2aparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

func TestA2AParser_OnResponseFrame_FoldsArtifactAndFinalStatus(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/stream"},
		},
	}

	frames := [][]byte{
		[]byte(`{"result":{"kind":"artifact-update","taskId":"task-1","artifact":{"parts":[{"kind":"text","text":"Hello "}]}}}`),
		[]byte(`{"result":{"kind":"artifact-update","taskId":"task-1","artifact":{"parts":[{"kind":"text","text":"world"}]}}}`),
		[]byte(`{"result":{"kind":"status-update","final":true,"status":{"state":"completed"}}}`),
	}
	for _, f := range frames {
		if action := p.OnResponseFrame(context.Background(), pctx, f, false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.A2A
	if ext.Artifact != "Hello world" {
		t.Errorf("Artifact = %q, want %q", ext.Artifact, "Hello world")
	}
	if ext.FinalStatus != "completed" {
		t.Errorf("FinalStatus = %q, want completed", ext.FinalStatus)
	}
	if ext.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want task-1", ext.TaskID)
	}
}

func TestA2AParser_OnResponseFrame_FailedStatusErrorMessage(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/stream"},
		},
	}
	frame := []byte(`{"result":{"kind":"status-update","final":true,"status":{"state":"failed","message":{"parts":[{"kind":"text","text":"upstream timeout"}]}}}}`)
	p.OnResponseFrame(context.Background(), pctx, frame, false)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	if pctx.Extensions.A2A.FinalStatus != "failed" {
		t.Errorf("FinalStatus = %q, want failed", pctx.Extensions.A2A.FinalStatus)
	}
	if pctx.Extensions.A2A.ErrorMessage != "upstream timeout" {
		t.Errorf("ErrorMessage = %q, want upstream timeout", pctx.Extensions.A2A.ErrorMessage)
	}
}

func TestA2AParser_OnResponseFrame_ApplicationJSONOneShot(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/send"},
		},
	}
	body := []byte(`{"result":{"taskId":"t-2","status":{"state":"completed"},"artifacts":[{"parts":[{"kind":"text","text":"answer"}]}]}}`)
	action := p.OnResponseFrame(context.Background(), pctx, body, true)
	if action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext.FinalStatus != "completed" {
		t.Errorf("FinalStatus = %q", ext.FinalStatus)
	}
	if ext.Artifact != "answer" {
		t.Errorf("Artifact = %q, want answer", ext.Artifact)
	}
	if ext.TaskID != "t-2" {
		t.Errorf("TaskID = %q, want t-2", ext.TaskID)
	}
}

func TestA2AParser_OnResponseFrame_CapturesContextID(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/stream"},
		},
	}
	// Request had no contextId; first stream event reveals it.
	frame := []byte(`{"result":{"kind":"status-update","contextId":"ctx-7","final":false,"status":{"state":"running"}}}`)
	p.OnResponseFrame(context.Background(), pctx, frame, false)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	if pctx.Extensions.A2A.SessionID != "ctx-7" {
		t.Errorf("SessionID = %q, want ctx-7", pctx.Extensions.A2A.SessionID)
	}
}

func TestA2AParser_OnResponseFrame_RequestContextIDPreserved(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			A2A: &pipeline.A2AExtension{Method: "message/stream", SessionID: "from-request"},
		},
	}
	frame := []byte(`{"result":{"kind":"status-update","contextId":"different","final":false,"status":{"state":"running"}}}`)
	p.OnResponseFrame(context.Background(), pctx, frame, false)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	if pctx.Extensions.A2A.SessionID != "from-request" {
		t.Errorf("SessionID = %q, want preserved from-request", pctx.Extensions.A2A.SessionID)
	}
}
