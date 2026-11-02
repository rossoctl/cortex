// Package peerproc names the process at the other end of a TCP connection this process
// accepted, so a proxy on the same host can tell its clients apart by process rather
// than by what they claim in their requests.
//
// It answers three questions — who holds the client end of a connection, who listens on
// an address, and who a process's parents are — from the kernel's own tables: on macOS
// the net.inet.tcp.pcblist_n sysctl and kinfo_proc; on Linux /proc. Pure
// Go, no cgo, no root. It sees only what the caller may inspect: on Linux another user's
// process, or one in another PID namespace, is not found.
// Environ also reads the environment a process started with.
//
// New runs a self-test and returns an error instead of a Resolver when lookups do not
// work here, so a caller can fall back once at startup rather than failing per request.
package peerproc

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"
)

// Proc is one process.
type Proc struct {
	PID  int32
	PPID int32
	// Start is when the process started. PIDs are reused, so PID and Start together
	// name one process for its whole lifetime. Start is comparable between lookups made
	// through one Resolver; on Linux it is derived from a boot time read once in New, so
	// it must not be compared across Resolvers or persisted. attribution.Attributor.Restore
	// compares a persisted Start with a live one anyway: after a clock step its claims do
	// not match, and it accepts that.
	Start time.Time
	// Exe is the absolute, symlink-resolved path of the process's executable, or "" when
	// it is not known: the caller may not read it.
	Exe string
}

// Resolver answers process questions about TCP connections on this host. Safe for
// concurrent use.
type Resolver interface {
	// ConnOwner is the process holding the client end of the connection between client
	// and server — for a connection this process accepted, its RemoteAddr and
	// LocalAddr. hints are PIDs to look at first; only Linux uses them, to avoid
	// scanning every process. ErrNotFound when no process the caller may inspect holds
	// it, which includes a connection that has already closed.
	ConnOwner(client, server netip.AddrPort, hints ...int32) (Proc, error)
	// ListenerOwner is the process listening on the socket a connection to addr would
	// reach, chosen on addr's port in the kernel's order:
	//
	//  1. a listener bound to exactly addr;
	//  2. else a wildcard listener of addr's family: 0.0.0.0 for IPv4, [::] for IPv6;
	//  3. else, for an IPv4 addr only, a [::] listener, which takes IPv4 too unless it
	//     set IPV6_V6ONLY. macOS records that flag and skips such a listener; Linux's
	//     /proc does not, so there this answer can name a listener that would refuse
	//     the connection.
	//
	// A socket bound but not listening never counts, nor does one whose owner the kernel
	// did not record (pid 0 on macOS, no inode on Linux). Where SO_REUSEPORT lets several
	// listeners share a rank, macOS names the one its kernel picks — the newest exact
	// bind, the oldest wildcard — and Linux, which spreads connections across such a
	// group, names one of them. hints are as ConnOwner's. ErrNotFound when no listener
	// matches, or the one chosen belongs to a process the caller may not inspect.
	ListenerOwner(addr netip.AddrPort, hints ...int32) (Proc, error)
	// Ancestry is pid followed by its parents, nearest first, stopping before PID 1 and
	// after max entries (at least one). A parent that cannot be read ends the walk.
	// ErrNotFound when pid itself cannot be read, or is 1 or below.
	Ancestry(pid int32, max int) ([]Proc, error)
}

// Environ is the environment process pid was exec'd with, as "NAME=value" strings. It is
// the start-time environment: a change the process made through setenv does not show. A
// process that wrote over its own argument memory (Node's process.title, setproctitle) can
// show no environment at all, so an empty result means unknown, not absent.
//
// It returns ErrNotFound when the environment cannot be read: another user's process, one
// that has exited, a Linux process that is not dumpable, or, on macOS, a process whose
// environment the kernel withholds (every Apple system process, and some others). It
// returns ErrUnsupported on platforms with no implementation.
func Environ(pid int32) ([]string, error) { return environ(pid) }

var (
	// ErrNotFound means no socket or process matched that the caller may inspect.
	ErrNotFound = errors.New("peerproc: not found")
	// ErrUnsupported means lookups are not implemented, or do not work, on this host.
	ErrUnsupported = errors.New("peerproc: unsupported on this host")
)

// New returns this host's Resolver once it has checked that the Resolver works: it dials
// a listener of its own and requires both lookups to name this process, and ConnOwner to
// name its parent too. Any failure is ErrUnsupported, wrapped with what went wrong.
func New() (Resolver, error) {
	r, err := newPlatform()
	if err != nil {
		return nil, err
	}
	if err := selfTest(r); err != nil {
		return nil, fmt.Errorf("%w: self-test: %v", ErrUnsupported, err)
	}
	return r, nil
}

func selfTest(r Resolver) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return err
	}
	defer c.Close()
	s, ok := <-accepted
	if !ok {
		return errors.New("accept failed")
	}
	defer s.Close()

	self := int32(os.Getpid())
	client := s.RemoteAddr().(*net.TCPAddr).AddrPort()
	server := s.LocalAddr().(*net.TCPAddr).AddrPort()
	p, err := r.ConnOwner(client, server, self)
	if err != nil {
		return fmt.Errorf("ConnOwner: %w", err)
	}
	if p.PID != self {
		return fmt.Errorf("ConnOwner named pid %d, want %d", p.PID, self)
	}
	// The parent pid checks the process-record parsing (kinfo_proc, /proc/<pid>/stat)
	// that the pid alone does not reach.
	if ppid := int32(os.Getppid()); p.PPID != ppid {
		return fmt.Errorf("ConnOwner named parent pid %d, want %d", p.PPID, ppid)
	}
	if p, err = r.ListenerOwner(server, self); err != nil {
		return fmt.Errorf("ListenerOwner: %w", err)
	}
	if p.PID != self {
		return fmt.Errorf("ListenerOwner named pid %d, want %d", p.PID, self)
	}
	return nil
}

// unmapAddrPort is ap with an IPv4-mapped IPv6 address turned back into IPv4, and with no
// zone. net.TCPAddr.AddrPort maps every IPv4 address, while the kernel tables record an
// IPv4 socket's address as IPv4, so both sides of every comparison go through this.
func unmapAddrPort(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
}

// ancestry implements Resolver.Ancestry over a platform's single-process lookup.
func ancestry(info func(int32) (Proc, error), pid int32, max int) ([]Proc, error) {
	if max < 1 {
		max = 1
	}
	var out []Proc
	for pid > 1 && len(out) < max {
		p, err := info(pid)
		if err != nil {
			break
		}
		out = append(out, p)
		if p.PPID == pid {
			break // a self-parented entry would loop forever
		}
		pid = p.PPID
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}
