package extproc

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// TestExtProcRecordersCarryMethodAndPath is a POPULATION guard, which the
// reflection guards in pipeline are not: those catch "the field never reaches
// the wire" and "MarshalJSON dropped it", but a recording site that simply
// forgets to copy pctx.Method / pctx.Path serializes a clean empty value and
// every existing test stays green. Verified by deleting both lines from all
// six recorders below — the whole authlib suite passed.
//
// One subtest per recorder, so a failure names the site rather than the file.
func TestExtProcRecordersCarryMethodAndPath(t *testing.T) {
	const (
		wantMethod = "POST"
		wantPath   = "/v1/chat/completions"
	)

	// Each recorder needs whatever makes its own gate open; the shared context
	// supplies method, path and the invocation that qualifies an event for
	// recording.
	// Each recorder snapshots invocations for its OWN phase — the response
	// recorders filter on InvocationPhaseResponse — so the fixture carries an
	// entry in both phases. A request-phase-only fixture leaves
	// recordOutboundResponseSession's gate shut and it records nothing.
	newPctx := func(dir pipeline.Direction, phase pipeline.InvocationPhase) *pipeline.Context {
		return &pipeline.Context{
			Direction: dir,
			Method:    wantMethod,
			Host:      "api.openai.com",
			Path:      wantPath,
			StartedAt: time.Now(),
			Extensions: pipeline.Extensions{
				Invocations: &pipeline.Invocations{
					Inbound: []pipeline.Invocation{{
						Plugin: "jwt-validation", Phase: phase,
						Action: pipeline.ActionAllow, Reason: "authorized",
					}},
					Outbound: []pipeline.Invocation{{
						Plugin: "token-exchange", Phase: phase,
						Action: pipeline.ActionAllow, Reason: "exchanged",
					}},
				},
			},
		}
	}

	for _, tc := range []struct {
		name   string
		record func(s *Server, pctx *pipeline.Context)
		dir    pipeline.Direction
		phase  pipeline.InvocationPhase
	}{
		{"recordInboundSession", func(s *Server, p *pipeline.Context) { s.recordInboundSession(p) },
			pipeline.Inbound, pipeline.InvocationPhaseRequest},
		{"recordOutboundSession", func(s *Server, p *pipeline.Context) { s.recordOutboundSession(p) },
			pipeline.Outbound, pipeline.InvocationPhaseRequest},
		{"recordInboundResponseSession", func(s *Server, p *pipeline.Context) { s.recordInboundResponseSession(p) },
			pipeline.Inbound, pipeline.InvocationPhaseResponse},
		{"recordOutboundResponseSession", func(s *Server, p *pipeline.Context) { s.recordOutboundResponseSession(p) },
			pipeline.Outbound, pipeline.InvocationPhaseResponse},
		{"recordInboundReject", func(s *Server, p *pipeline.Context) {
			s.recordInboundReject(p, pipeline.Action{Type: pipeline.Reject})
		}, pipeline.Inbound, pipeline.InvocationPhaseRequest},
		{"recordOutboundReject", func(s *Server, p *pipeline.Context) {
			s.recordOutboundReject(p, pipeline.Action{Type: pipeline.Reject})
		}, pipeline.Outbound, pipeline.InvocationPhaseRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := session.New(5*time.Minute, 100, 0)
			defer store.Close()
			s := &Server{Sessions: store}

			tc.record(s, newPctx(tc.dir, tc.phase))

			v := store.View(session.DefaultSessionID)
			if v == nil || len(v.Events) == 0 {
				t.Fatalf("%s recorded no event; cannot assert population", tc.name)
			}
			ev := v.Events[0]
			if ev.HTTPMethod != wantMethod {
				t.Errorf("%s: HTTPMethod = %q, want %q", tc.name, ev.HTTPMethod, wantMethod)
			}
			if ev.HTTPPath != wantPath {
				t.Errorf("%s: HTTPPath = %q, want %q", tc.name, ev.HTTPPath, wantPath)
			}
		})
	}
}
