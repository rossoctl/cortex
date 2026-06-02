package pipeline

import (
	"context"
	"encoding/json"
	"testing"
)

// fakePlugin is a minimal Plugin implementation for testing the wrapper.
// It records call counts so pass-through can be asserted.
type fakePlugin struct {
	name      string
	caps      PluginCapabilities
	requests  int
	responses int
}

func (f *fakePlugin) Name() string                  { return f.name }
func (f *fakePlugin) Capabilities() PluginCapabilities { return f.caps }
func (f *fakePlugin) OnRequest(ctx context.Context, pctx *Context) Action {
	f.requests++
	return Action{}
}
func (f *fakePlugin) OnResponse(ctx context.Context, pctx *Context) Action {
	f.responses++
	return Action{}
}

func TestConfiguredPluginRawConfig(t *testing.T) {
	raw := json.RawMessage(`{"issuer":"http://idp"}`)
	cp := WrapConfigured(&fakePlugin{name: "jwt-validation"}, raw)
	rc, ok := cp.(RawConfigProvider)
	if !ok {
		t.Fatal("wrapper should expose RawConfig() via type-assertion")
	}
	got := string(rc.RawConfig())
	if got != `{"issuer":"http://idp"}` {
		t.Fatalf("RawConfig: %q want %q", got, `{"issuer":"http://idp"}`)
	}
}

func TestConfiguredPluginPassesThroughPluginMethods(t *testing.T) {
	fake := &fakePlugin{
		name: "jwt-validation",
		caps: PluginCapabilities{ReadsBody: true, Description: "test plugin"},
	}
	cp := WrapConfigured(fake, json.RawMessage(`{}`))

	if cp.Name() != "jwt-validation" {
		t.Fatalf("Name pass-through broken: %q", cp.Name())
	}
	caps := cp.Capabilities()
	if !caps.ReadsBody {
		t.Fatalf("Capabilities pass-through broken: ReadsBody should be true, got %+v", caps)
	}
	if caps.Description != "test plugin" {
		t.Fatalf("Capabilities pass-through broken: Description mismatch, got %+v", caps)
	}
	cp.OnRequest(context.Background(), nil)
	cp.OnResponse(context.Background(), nil)
	if fake.requests != 1 || fake.responses != 1 {
		t.Fatalf("OnRequest/OnResponse pass-through broken: req=%d resp=%d",
			fake.requests, fake.responses)
	}
}

// fakeInitializer extends fakePlugin with Initializer.
type fakeInitializer struct {
	fakePlugin
	initCalls int
	initErr   error
}

func (f *fakeInitializer) Init(ctx context.Context) error {
	f.initCalls++
	return f.initErr
}

// fakeShutdowner extends fakePlugin with Shutdowner.
type fakeShutdowner struct {
	fakePlugin
	shutdownCalls int
}

func (f *fakeShutdowner) Shutdown(ctx context.Context) error {
	f.shutdownCalls++
	return nil
}

// fakeFinisher extends fakePlugin with Finisher.
type fakeFinisher struct {
	fakePlugin
	finishCalls int
}

func (f *fakeFinisher) OnFinish(ctx context.Context, pctx *Context) {
	f.finishCalls++
}

// fakeReadier extends fakePlugin with Readier.
type fakeReadier struct {
	fakePlugin
	ready bool
}

func (f *fakeReadier) Ready() bool { return f.ready }

func TestConfiguredPluginForwardsInit(t *testing.T) {
	fake := &fakeInitializer{fakePlugin: fakePlugin{name: "x"}}
	cp := WrapConfigured(fake, nil)
	init, ok := cp.(Initializer)
	if !ok {
		t.Fatal("wrapper should implement Initializer (unconditional forwarding)")
	}
	if err := init.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if fake.initCalls != 1 {
		t.Fatalf("Init not forwarded: calls=%d want 1", fake.initCalls)
	}
}

func TestConfiguredPluginInitNoOpForNonInitializer(t *testing.T) {
	cp := WrapConfigured(&fakePlugin{name: "x"}, nil)
	init, ok := cp.(Initializer)
	if !ok {
		t.Fatal("wrapper always implements Initializer")
	}
	if err := init.Init(context.Background()); err != nil {
		t.Fatalf("no-op Init should return nil, got %v", err)
	}
}

func TestConfiguredPluginForwardsShutdown(t *testing.T) {
	fake := &fakeShutdowner{fakePlugin: fakePlugin{name: "x"}}
	cp := WrapConfigured(fake, nil)
	sh, ok := cp.(Shutdowner)
	if !ok {
		t.Fatal("wrapper should implement Shutdowner")
	}
	if err := sh.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if fake.shutdownCalls != 1 {
		t.Fatalf("Shutdown not forwarded: calls=%d want 1", fake.shutdownCalls)
	}
}

func TestConfiguredPluginShutdownNoOpForNonShutdowner(t *testing.T) {
	cp := WrapConfigured(&fakePlugin{name: "x"}, nil)
	sh, ok := cp.(Shutdowner)
	if !ok {
		t.Fatal("wrapper always implements Shutdowner")
	}
	if err := sh.Shutdown(context.Background()); err != nil {
		t.Fatalf("no-op Shutdown should return nil, got %v", err)
	}
}

func TestConfiguredPluginForwardsFinish(t *testing.T) {
	fake := &fakeFinisher{fakePlugin: fakePlugin{name: "x"}}
	cp := WrapConfigured(fake, nil)
	fin, ok := cp.(Finisher)
	if !ok {
		t.Fatal("wrapper should implement Finisher")
	}
	fin.OnFinish(context.Background(), nil)
	if fake.finishCalls != 1 {
		t.Fatalf("OnFinish not forwarded: calls=%d want 1", fake.finishCalls)
	}
}

func TestConfiguredPluginFinishNoOpForNonFinisher(t *testing.T) {
	cp := WrapConfigured(&fakePlugin{name: "x"}, nil)
	fin, ok := cp.(Finisher)
	if !ok {
		t.Fatal("wrapper always implements Finisher")
	}
	// Should not panic.
	fin.OnFinish(context.Background(), nil)
}

func TestConfiguredPluginForwardsReady(t *testing.T) {
	// Plugin that reports not-ready: wrapper must report not-ready too.
	fake := &fakeReadier{fakePlugin: fakePlugin{name: "x"}, ready: false}
	cp := WrapConfigured(fake, nil)
	r, ok := cp.(Readier)
	if !ok {
		t.Fatal("wrapper should implement Readier")
	}
	if r.Ready() {
		t.Fatal("Ready should forward false from underlying plugin")
	}
	// Now set ready=true; wrapper reflects.
	fake.ready = true
	if !r.Ready() {
		t.Fatal("Ready should forward true from underlying plugin")
	}
}

func TestConfiguredPluginReadyDefaultsTrueForNonReadier(t *testing.T) {
	// Matches the existing Pipeline.Ready() semantics: plugins without
	// Readier are considered always-ready (pipeline.go:287-289).
	cp := WrapConfigured(&fakePlugin{name: "x"}, nil)
	r, ok := cp.(Readier)
	if !ok {
		t.Fatal("wrapper always implements Readier")
	}
	if !r.Ready() {
		t.Fatal("non-Readier wrapped plugin should default to ready=true")
	}
}
