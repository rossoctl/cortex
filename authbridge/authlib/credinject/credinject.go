// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package credinject provides reusable primitives for resolving a credential
// and safely injecting it into an HTTP header — the shared core behind
// credential-injecting plugins (e.g. placeholder-resolve, and a future
// host-keyed injector). It is transport-agnostic and imports no plugin, so
// plugins depend on it rather than the reverse.
package credinject

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
)

// Resolver maps a lookup key to a real credential value. ok is false when the
// key is unknown or unavailable — callers MUST fail closed on a false (never
// forward the unresolved key).
type Resolver interface {
	Resolve(ctx context.Context, key string) (value string, ok bool)
}

// LifecycleResolver is implemented by resolvers that need background warm-up
// (e.g. a remote/cached source). Static resolvers omit it and are treated as
// always ready.
type LifecycleResolver interface {
	Start(ctx context.Context) error
	Ready() bool
	Stop(ctx context.Context) error
}

// DenyResolver resolves nothing. It is the fail-closed default for an
// unconfigured source, so a missing source never silently resolves from an
// ambient source (e.g. the process environment).
type DenyResolver struct{}

// Resolve always fails closed.
func (DenyResolver) Resolve(context.Context, string) (string, bool) { return "", false }

// MapResolver resolves from an inline map. For tests/dev only — it holds
// cleartext credentials in process memory and is not a production source.
type MapResolver map[string]string

// Resolve returns the mapped value for key, if present.
func (m MapResolver) Resolve(_ context.Context, key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

// FileResolver resolves each key by reading <Dir>/<key> as a credential file
// (whitespace-trimmed via config.ReadCredentialFile). The key is path-
// contained: any key that is not a direct child filename of Dir (i.e. contains
// a separator or "..") is rejected, so the resolver is safe for arbitrary,
// caller-supplied keys such as a destination host.
type FileResolver struct{ Dir string }

// Resolve reads <Dir>/<key>, rejecting keys that would escape Dir.
func (f FileResolver) Resolve(_ context.Context, key string) (string, bool) {
	if key == "" {
		return "", false
	}
	p := filepath.Join(f.Dir, key)
	// The joined path must be a direct child of Dir — this rejects separators,
	// absolute keys, and "../" traversal regardless of the key's grammar.
	if filepath.Dir(p) != filepath.Clean(f.Dir) {
		return "", false
	}
	v, err := config.ReadCredentialFile(p)
	if err != nil {
		return "", false
	}
	return v, true
}

// SafeHeaderValue reports whether v is safe to place in an HTTP header value.
// It rejects CR, LF, and NUL to prevent header/response splitting (CWE-113)
// via a poisoned credential store.
func SafeHeaderValue(v string) bool {
	return !strings.ContainsAny(v, "\r\n\x00")
}

// SafeSetHeader sets h[name]=value only when value is header-safe. It returns
// false and leaves h unmodified when value is unsafe, so the caller can fail
// closed rather than forward a poisoned value.
func SafeSetHeader(h http.Header, name, value string) bool {
	if !SafeHeaderValue(value) {
		return false
	}
	h.Set(name, value)
	return true
}
