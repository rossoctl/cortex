package cpex

import (
	"context"
	"sync"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// FakeManager is a programmable Manager for unit tests. Set Hooks,
// InvokeErr, etc. before the test action; read LoadedYAML,
// Initialized, ShutdownCalled, Invokes after.
//
// Zero-value FakeManager is ready to use. Concurrent Invoke calls
// are safe.
//
// The fake lives outside _test.go so future integration suites in
// cmd/authbridge-cpex/ or downstream tools can reuse it. The cost is
// that it ships in the authbridge-cpex binary; no caller path
// instantiates it from production code, so the risk is just dead
// bytes. If binary size becomes a concern this moves to
// fake_manager_test.go and stays in-package.
type FakeManager struct {
	mu sync.Mutex

	// Hooks maps hook name → canned Result. Invoke returns the
	// canned Result when the hook name matches; otherwise it
	// returns DecisionAllow with a "no hook configured" reason.
	Hooks map[string]Result

	// InvokeErr, if non-nil, is returned from every Invoke before
	// the Hooks lookup. Use for fail_open tests.
	InvokeErr error

	// LoadErr / InitErr are returned from LoadConfig and Initialize.
	LoadErr error
	InitErr error

	// KnownHooks overrides HasHook's reply. When nil/empty, HasHook
	// falls back to the keyset of Hooks. Set explicitly when you
	// want HasHook to report a hook that isn't in Hooks (covers the
	// "Invoke is dispatched but returns Allow" path).
	KnownHooks []string

	// Recorded state — read after the test action.
	LoadedYAML     string
	Initialized   bool
	ShutdownCalled bool
	Invokes       []FakeInvoke
}

// FakeInvoke records one Invoke call on a FakeManager.
type FakeInvoke struct {
	Hook string
	// Pctx is the pipeline.Context the plugin built and handed us.
	// Inspect after the test to verify the right inputs were passed
	// (identity, headers, body, classification).
	Pctx *pipeline.Context
}

// Static interface assertion: FakeManager satisfies Manager. If
// Manager gains a method, this line forces FakeManager to be
// updated in the same commit (compile error in default tests).
var _ Manager = (*FakeManager)(nil)

// LoadConfig records yaml and returns LoadErr.
func (f *FakeManager) LoadConfig(yaml string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LoadedYAML = yaml
	return f.LoadErr
}

// Initialize records the call and returns InitErr (if set).
func (f *FakeManager) Initialize(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.InitErr != nil {
		return f.InitErr
	}
	f.Initialized = true
	return nil
}

// Shutdown records the call.
func (f *FakeManager) Shutdown(_ context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ShutdownCalled = true
}

// HasHook returns whether the hook is reportedly known. KnownHooks
// takes precedence over the Hooks map.
func (f *FakeManager) HasHook(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.KnownHooks) > 0 {
		for _, h := range f.KnownHooks {
			if h == name {
				return true
			}
		}
		return false
	}
	_, ok := f.Hooks[name]
	return ok
}

// Invoke records the call and returns InvokeErr (if set), the canned
// Result for hookName (if in Hooks), or a default DecisionAllow.
func (f *FakeManager) Invoke(_ context.Context, hookName string, pctx *pipeline.Context) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Invokes = append(f.Invokes, FakeInvoke{Hook: hookName, Pctx: pctx})
	if f.InvokeErr != nil {
		return Result{}, f.InvokeErr
	}
	if r, ok := f.Hooks[hookName]; ok {
		return r, nil
	}
	return Result{Decision: DecisionAllow, Reason: "fake: no hook configured"}, nil
}
