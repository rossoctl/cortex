package main

import (
	"net"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// All or nothing: a port that is taken fails the reservation, names the socket, and leaves
// none of the others held — the process is about to exit, and a later attempt must find
// them free.
func TestReserveListeners_AllOrNothing(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	free := freeAddr(t)

	_, err = reserveListeners([]config.PlannedListener{{Name: "stats", Addr: free}, {Name: "health", Addr: held.Addr().String()}})
	if err == nil || !strings.Contains(err.Error(), "health") || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v, want the health socket named as in use", err)
	}
	again, err := net.Listen("tcp", free)
	if err != nil {
		t.Fatalf("the socket reserved before the failure is still held: %v", err)
	}
	_ = again.Close()
}

func TestReservedListeners_TakeAndClaimed(t *testing.T) {
	r, err := reserveListeners([]config.PlannedListener{{Name: "stats", Addr: "127.0.0.1:0"}, {Name: "health", Addr: "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.closeAll()
	if ln := r.take("stats"); ln == nil {
		t.Fatal("take returned nil for a reserved socket")
	}
	if err := r.claimed(); err == nil || !strings.Contains(err.Error(), "health") {
		t.Errorf("claimed() = %v, want health named as reserved but never served", err)
	}
	r.take("health")
	if err := r.claimed(); err != nil {
		t.Errorf("claimed() = %v after every socket was taken", err)
	}
}
