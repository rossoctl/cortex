package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// configurablePlugin is a stubPlugin that also implements Configurable so
// tests can verify Configure is invoked and its error is surfaced.
type configurablePlugin struct {
	stubPlugin
	configureCalled bool
	receivedRaw     json.RawMessage
	returnErr       error
}

func (c *configurablePlugin) Configure(raw json.RawMessage) error {
	c.configureCalled = true
	c.receivedRaw = raw
	return c.returnErr
}

func TestConfigurable_InterfaceIsOptional(t *testing.T) {
	// A plugin that doesn't implement Configurable must not be rejected.
	// This is the whole reason Configurable exists as an interface rather
	// than a method on Plugin — parsers and other config-free plugins
	// shouldn't need a boilerplate Configure stub.
	p := &stubPlugin{name: "no-config"}

	// Type assertion that callers (Build) will use.
	if _, ok := any(p).(Configurable); ok {
		t.Error("stubPlugin must NOT implement Configurable; otherwise this test is vacuous")
	}
}

func TestConfigurable_ErrorPropagates(t *testing.T) {
	// A Configure error is surfaced to the caller. Pipeline
	// construction should not silently continue when a plugin refuses
	// its config — Build-level coverage that the error actually
	// aborts a pipeline build lives in
	// plugins_test.go:TestBuild_ConfigureError.
	p := &configurablePlugin{
		stubPlugin: stubPlugin{name: "bad"},
		returnErr:  errors.New("bad config"),
	}

	err := p.Configure(json.RawMessage(`{"x":1}`))
	if err == nil {
		t.Fatal("expected error from Configure, got nil")
	}
	if !p.configureCalled {
		t.Error("Configure was never invoked")
	}
}

func TestConfigurable_RawIsPassedThrough(t *testing.T) {
	// The framework must not interpret or re-marshal the raw config — the
	// plugin receives the exact bytes from its config sub-tree. This lets
	// plugins do their own DisallowUnknownFields decode without the
	// framework swallowing stray keys.
	p := &configurablePlugin{stubPlugin: stubPlugin{name: "ok"}}
	in := json.RawMessage(`{"issuer":"http://example","extra":"keep"}`)

	if err := p.Configure(in); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if string(p.receivedRaw) != string(in) {
		t.Errorf("raw = %s, want %s", p.receivedRaw, in)
	}
}

// The Plugin interface itself is unchanged; a plugin can implement
// Configurable alongside the usual hooks without any adapter wiring.
func TestConfigurable_DoesNotAlterPluginSurface(t *testing.T) {
	p := &configurablePlugin{stubPlugin: stubPlugin{name: "x"}}
	// Plugin surface still works.
	if got := p.Name(); got != "x" {
		t.Errorf("Name() = %q, want x", got)
	}
	action := p.OnRequest(context.Background(), &Context{})
	if action.Type != Continue {
		t.Errorf("OnRequest = %v, want Continue", action.Type)
	}
}
