// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credinject

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestDenyResolverFailsClosed(t *testing.T) {
	if v, ok := (DenyResolver{}).Resolve(context.Background(), "ANYTHING"); ok || v != "" {
		t.Errorf("DenyResolver.Resolve = (%q, %v), want (\"\", false)", v, ok)
	}
}

func TestMapResolver(t *testing.T) {
	m := MapResolver{"K": "v"}
	if v, ok := m.Resolve(context.Background(), "K"); !ok || v != "v" {
		t.Errorf("hit = (%q,%v), want (v,true)", v, ok)
	}
	if _, ok := m.Resolve(context.Background(), "MISS"); ok {
		t.Error("miss should be ok=false")
	}
}

func TestFileResolverReadsDirectChild(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "TOKEN"), []byte("  sk-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, ok := FileResolver{Dir: dir}.Resolve(context.Background(), "TOKEN")
	if !ok || v != "sk-real" { // ReadCredentialFile trims surrounding whitespace
		t.Errorf("Resolve = (%q,%v), want (sk-real,true)", v, ok)
	}
}

func TestFileResolverRejectsTraversalAndSeparators(t *testing.T) {
	dir := t.TempDir()
	// A secret one level up that must never be reachable.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "SECRET"), []byte("leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	fr := FileResolver{Dir: dir}
	for _, key := range []string{"../SECRET", "sub/TOKEN", "/etc/passwd", "", "a/../../SECRET"} {
		if v, ok := fr.Resolve(context.Background(), key); ok {
			t.Errorf("Resolve(%q) = (%q,true), want ok=false (containment)", key, v)
		}
	}
}

func TestSafeHeaderValue(t *testing.T) {
	for _, ok := range []string{"sk-abc123", "Bearer x.y.z", ""} {
		if !SafeHeaderValue(ok) {
			t.Errorf("SafeHeaderValue(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"a\rb", "a\nb", "a\x00b", "tok\r\nInjected: 1"} {
		if SafeHeaderValue(bad) {
			t.Errorf("SafeHeaderValue(%q) = true, want false", bad)
		}
	}
}

func TestSafeSetHeader(t *testing.T) {
	h := http.Header{}
	if !SafeSetHeader(h, "Authorization", "Bearer ok") {
		t.Fatal("safe value should set")
	}
	if got := h.Get("Authorization"); got != "Bearer ok" {
		t.Errorf("Authorization = %q", got)
	}
	h2 := http.Header{}
	if SafeSetHeader(h2, "Authorization", "Bearer bad\r\nX: y") {
		t.Fatal("unsafe value should not set")
	}
	if h2.Get("Authorization") != "" {
		t.Error("header must be unmodified on unsafe value")
	}
}
