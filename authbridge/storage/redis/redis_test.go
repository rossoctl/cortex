package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func setup(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := New("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, mr
}

func TestGetSet(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()

	val, err := c.Get(ctx, "missing")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if val != "" {
		t.Errorf("Get missing = %q, want empty", val)
	}

	if err := c.Set(ctx, "key1", "hello", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, err = c.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "hello" {
		t.Errorf("Get = %q, want hello", val)
	}
}

func TestSetWithTTL(t *testing.T) {
	c, mr := setup(t)
	ctx := context.Background()

	if err := c.Set(ctx, "ephemeral", "value", 10*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	mr.FastForward(11 * time.Second)

	val, err := c.Get(ctx, "ephemeral")
	if err != nil {
		t.Fatalf("Get after TTL: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty after TTL expiry, got %q", val)
	}
}

func TestIncr(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()

	v, err := c.Incr(ctx, "counter", 5)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if v != 5 {
		t.Errorf("Incr = %d, want 5", v)
	}

	v, err = c.Incr(ctx, "counter", 3)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if v != 8 {
		t.Errorf("Incr = %d, want 8", v)
	}
}

func TestHashIncr(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()

	v, err := c.HashIncr(ctx, "h1", "tokens", 100)
	if err != nil {
		t.Fatalf("HashIncr: %v", err)
	}
	if v != 100 {
		t.Errorf("HashIncr = %d, want 100", v)
	}

	v, err = c.HashIncr(ctx, "h1", "tokens", 50)
	if err != nil {
		t.Fatalf("HashIncr: %v", err)
	}
	if v != 150 {
		t.Errorf("HashIncr = %d, want 150", v)
	}
}

func TestHashGet(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()

	fields, err := c.HashGet(ctx, "empty")
	if err != nil {
		t.Fatalf("HashGet: %v", err)
	}
	if len(fields) != 0 {
		t.Errorf("HashGet empty = %v, want empty map", fields)
	}

	c.HashIncr(ctx, "h1", "tokens", 42)
	c.HashIncr(ctx, "h1", "calls", 3)

	fields, err = c.HashGet(ctx, "h1")
	if err != nil {
		t.Fatalf("HashGet: %v", err)
	}
	if fields["tokens"] != "42" {
		t.Errorf("tokens = %q, want 42", fields["tokens"])
	}
	if fields["calls"] != "3" {
		t.Errorf("calls = %q, want 3", fields["calls"])
	}
}

func TestHashSetNX(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()

	set, err := c.HashSetNX(ctx, "h1", "started_at", "1000")
	if err != nil {
		t.Fatalf("HashSetNX: %v", err)
	}
	if !set {
		t.Error("HashSetNX should return true on first set")
	}

	set, err = c.HashSetNX(ctx, "h1", "started_at", "2000")
	if err != nil {
		t.Fatalf("HashSetNX: %v", err)
	}
	if set {
		t.Error("HashSetNX should return false when field exists")
	}

	fields, _ := c.HashGet(ctx, "h1")
	if fields["started_at"] != "1000" {
		t.Errorf("started_at = %q, want 1000 (first value preserved)", fields["started_at"])
	}
}

func TestExpire(t *testing.T) {
	c, mr := setup(t)
	ctx := context.Background()

	c.HashIncr(ctx, "h1", "tokens", 100)
	if err := c.Expire(ctx, "h1", 5*time.Second); err != nil {
		t.Fatalf("Expire: %v", err)
	}

	mr.FastForward(6 * time.Second)

	fields, err := c.HashGet(ctx, "h1")
	if err != nil {
		t.Fatalf("HashGet after TTL: %v", err)
	}
	if len(fields) != 0 {
		t.Errorf("expected empty after expiry, got %v", fields)
	}
}

func TestNew_InvalidURL(t *testing.T) {
	_, err := New("not-a-valid-url")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}
