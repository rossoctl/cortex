// Package staticinject implements the static-inject outbound AuthBridge
// plugin: it swaps a placeholder credential on the outbound Authorization
// header for a real static credential, so a model-influenced workload never
// holds the real secret. See plugin.go for the pipeline.Plugin
// implementation; this file holds the self-contained credential resolver
// and header-safety helpers.
package staticinject

import (
	"context"
	"net/http"
	"path/filepath"

	credconfig "github.com/rossoctl/cortex/authbridge/authlib/config"
)

// Resolver looks up a credential value by key. ok=false means the key is
// unknown or the value is unavailable — callers MUST fail closed and never
// forward the unresolved key.
type Resolver interface {
	Resolve(ctx context.Context, key string) (value string, ok bool)
}

// MapResolver is an inline map-backed Resolver. Intended for tests and
// local/dev configurations (source: mappings) — production deployments
// should use FileResolver so the real credential never lives in the
// pipeline YAML.
type MapResolver map[string]string

// Resolve looks up key in the map. ok=false when key is absent.
func (m MapResolver) Resolve(_ context.Context, key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

// FileResolver reads a credential from a file named key inside Dir. Values
// are whitespace-trimmed, matching the repo's config.ReadCredentialFile
// convention used by other plugins for secret_dir-style sources.
type FileResolver struct {
	Dir string
}

// Resolve reads <Dir>/<key>. It is path-contained: any key that would
// resolve outside Dir as a direct child (path separators, absolute paths,
// ".." traversal) is rejected with ok=false before any filesystem access.
// Any read error (missing file, permission, empty file) also yields
// ok=false — FileResolver never distinguishes "missing" from "unreadable"
// to callers, since both must fail closed identically.
func (r FileResolver) Resolve(_ context.Context, key string) (string, bool) {
	joined := filepath.Join(r.Dir, key)
	if filepath.Dir(joined) != filepath.Clean(r.Dir) {
		return "", false
	}
	value, err := credconfig.ReadCredentialFile(joined)
	if err != nil {
		return "", false
	}
	return value, true
}

// SafeHeaderValue reports whether v is safe to place in an HTTP header
// value — false if it contains CR, LF, or NUL, which could otherwise be
// used for header/response splitting (CWE-113).
func SafeHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\r', '\n', 0:
			return false
		}
	}
	return true
}

// SafeSetHeader sets h[name] = []string{value} only when SafeHeaderValue(value)
// is true. On an unsafe value it returns false and leaves h unmodified.
func SafeSetHeader(h http.Header, name, value string) bool {
	if !SafeHeaderValue(value) {
		return false
	}
	h.Set(name, value)
	return true
}
