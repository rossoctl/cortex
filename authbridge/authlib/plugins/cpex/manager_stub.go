//go:build !cpex

package cpex

import "errors"

// errNoCpexBuild is what NewManager returns when the binary was
// compiled without -tags cpex. The plugin's Configure surfaces this
// error to the operator at boot, so they don't get a silent
// no-op cpex plugin in production.
//
// Operators hitting this should switch to the authbridge-cpex image
// (or rebuild their custom binary with `-tags cpex`).
var errNoCpexBuild = errors.New("cpex plugin: this binary was not built with -tags cpex; use the authbridge-cpex image, or rebuild with -tags cpex")

// NewManager (stub) always returns errNoCpexBuild. The real
// implementation lives in manager_cpex.go (//go:build cpex).
//
// Returning an error here — rather than panicking or registering a
// silent no-op — preserves the fail-loud guarantee from the design
// doc: if you configured the cpex plugin in YAML, you want a
// pipeline build failure if the binary can't honor it.
func NewManager(_ ManagerOptions) (Manager, error) {
	return nil, errNoCpexBuild
}
