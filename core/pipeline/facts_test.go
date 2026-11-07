package pipeline

import "testing"

// The 3 rules of LastIntent and LastInference, in order.
func TestContext_SessionFacts(t *testing.T) {
	intentEv := SessionEvent{Seq: 1, Direction: Inbound, Phase: SessionRequest, A2A: &A2AExtension{Method: "message/send"}}
	inferEv := SessionEvent{Seq: 2, Direction: Outbound, Phase: SessionRequest, Inference: &InferenceExtension{Model: "m"}}
	other := SessionEvent{Seq: 3, Direction: Outbound, Phase: SessionResponse, Inference: &InferenceExtension{}}

	t.Run("2: the facts the listener set win", func(t *testing.T) {
		set := SessionFacts{LastIntent: SessionFact{State: FactNone}, LastInference: SessionFact{Event: &inferEv, State: FactPresent}}
		c := &Context{Session: &SessionView{ID: "s", Events: []SessionEvent{intentEv}}}
		c.SetSessionFacts(set)
		if got := c.LastIntent(); got != set.LastIntent {
			t.Errorf("LastIntent = %+v, want the set %+v", got, set.LastIntent)
		}
		if got := c.LastInference(); got != set.LastInference {
			t.Errorf("LastInference = %+v, want the set %+v", got, set.LastInference)
		}
	})

	t.Run("1: no session", func(t *testing.T) {
		c := &Context{}
		if got := c.LastIntent(); got != (SessionFact{State: FactNoSession}) {
			t.Errorf("LastIntent = %+v, want FactNoSession", got)
		}
		if got := c.LastInference(); got != (SessionFact{State: FactNoSession}) {
			t.Errorf("LastInference = %+v, want FactNoSession", got)
		}
	})

	t.Run("1: facts set, Session nil", func(t *testing.T) {
		c := &Context{}
		c.SetSessionFacts(SessionFacts{LastIntent: SessionFact{State: FactNone}, LastInference: SessionFact{State: FactNone}})
		if got := c.LastIntent(); got != (SessionFact{State: FactNoSession}) {
			t.Errorf("LastIntent = %+v, want FactNoSession", got)
		}
		if got := c.LastInference(); got != (SessionFact{State: FactNoSession}) {
			t.Errorf("LastInference = %+v, want FactNoSession", got)
		}
	})

	t.Run("3: derived from the view", func(t *testing.T) {
		c := &Context{Session: &SessionView{ID: "s", Events: []SessionEvent{intentEv, inferEv, other}}}
		if got := c.LastIntent(); got.State != FactPresent || got.Event != &c.Session.Events[0] {
			t.Errorf("LastIntent = %+v, want FactPresent at Events[0]", got)
		}
		if got := c.LastInference(); got.State != FactPresent || got.Event != &c.Session.Events[1] {
			t.Errorf("LastInference = %+v, want FactPresent at Events[1]", got)
		}

		empty := &Context{Session: &SessionView{ID: "s", Events: []SessionEvent{other}}}
		if got := empty.LastIntent(); got != (SessionFact{State: FactNone}) {
			t.Errorf("empty view: LastIntent = %+v, want FactNone", got)
		}
		if got := empty.LastInference(); got != (SessionFact{State: FactNone}) {
			t.Errorf("empty view: LastInference = %+v, want FactNone", got)
		}
	})
}
