package storage

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var testSeq atomic.Int64

type nopStore struct{}

func (nopStore) Get(context.Context, string) (string, error)                    { return "", nil }
func (nopStore) Set(context.Context, string, string, time.Duration) error       { return nil }
func (nopStore) Incr(context.Context, string, int64) (int64, error)             { return 0, nil }
func (nopStore) HashIncr(context.Context, string, string, int64) (int64, error) { return 0, nil }
func (nopStore) HashGet(context.Context, string) (map[string]string, error)     { return nil, nil }
func (nopStore) HashSetNX(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (nopStore) Expire(context.Context, string, time.Duration) error { return nil }
func (nopStore) Close() error                                        { return nil }

func TestRegisterAndOpen(t *testing.T) {
	scheme := fmt.Sprintf("test-scheme-%d", testSeq.Add(1))
	Register(scheme, func(url string) (Store, error) {
		return nopStore{}, nil
	})

	s, err := Open(scheme, scheme+"://addr")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestOpen_UnknownScheme(t *testing.T) {
	_, err := Open("nonexistent", "foo://bar")
	if err == nil {
		t.Error("expected error for unknown scheme")
	}
}

func TestRegister_Duplicate(t *testing.T) {
	scheme := fmt.Sprintf("dup-scheme-%d", testSeq.Add(1))
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate Register")
		}
	}()
	Register(scheme, func(url string) (Store, error) { return nopStore{}, nil })
	Register(scheme, func(url string) (Store, error) { return nopStore{}, nil })
}
