package plugins

// This file is intentionally NOT named with a _test.go suffix so that
// UnregisterPlugin is importable by tests in OTHER packages (e.g.,
// cmd/authbridge/listener tests that want to register a fake plugin).
// It IS, however, clearly-named and documented as a test affordance —
// callers must not use it in production code paths.

// UnregisterPlugin removes a plugin factory from the registry. Intended
// for test isolation: a test registers a fake plugin, runs, and uses
// t.Cleanup to unregister so parallel tests aren't poisoned by the
// leftover entry.
//
// Do not call from production code. The registry is intended to be
// written exactly once per plugin per process, at init() time; runtime
// deregistration has no valid use case in a running authbridge binary
// and would make the /config endpoint lie about pipeline composition.
//
// Returns true when the name was registered (and is now removed), false
// when it wasn't. Callers ignoring the return value are common and
// correct — Cleanup doesn't care.
func UnregisterPlugin(name string) bool {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[name]; !ok {
		return false
	}
	delete(registry, name)
	return true
}
