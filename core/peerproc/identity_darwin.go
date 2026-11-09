//go:build darwin

package peerproc

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The proc_info flavors processIdentity reads, and the layouts it reads out of them.
// Offsets are from <sys/proc_info.h>; both structs have kept their size since 2013.
const (
	procPIDTBSDInfo           = 3   // PROC_PIDTBSDINFO: struct proc_bsdinfo
	procBSDInfoSize           = 136 // sizeof(struct proc_bsdinfo)
	procBSDInfoStatusOff      = 4   // pbi_status, uint32
	procBSDInfoPIDOff         = 12  // pbi_pid, uint32
	procBSDInfoStartSecOff    = 120 // pbi_start_tvsec, uint64
	procBSDInfoStartUsecOff   = 128 // pbi_start_tvusec, uint64
	procPIDUniqIdentifierInfo = 17  // PROC_PIDUNIQIDENTIFIERINFO: struct proc_uniqidentifierinfo
	procUniqInfoSize          = 56  // sizeof(struct proc_uniqidentifierinfo)
	procUniqInfoUniqueIDOff   = 16  // p_uniqueid, uint64, after the 16-byte executable UUID
	sZomb                     = 5   // SZOMB in <sys/proc.h>
)

// processIdentity reads pid as Codex's macOS reader does (read_process_details): the
// unique id, then the BSD info, then the unique id again from the pid the BSD info names,
// so a pid reused between the two reads is an error rather than a mixed identity. arg 1
// asks for zombies too. The unique-id flavor does not require owning the process; the
// BSD info flavor does, for another user's process, so when only that read fails the
// error comes with BootID and UniqueID filled in: enough to tell the process is not a
// recorded one, which is all such a caller can learn.
func processIdentity(pid int32) (Identity, error) {
	if pid <= 0 {
		return Identity{}, ErrNotFound
	}
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil || boot == "" {
		return Identity{}, fmt.Errorf("%w: kern.bootsessionuuid: %v", ErrUnsupported, err)
	}
	unique, err := uniqueID(pid)
	if err != nil {
		return Identity{BootID: boot}, err
	}
	buf := make([]byte, procBSDInfoSize)
	if n, err := pidInfo(pid, procPIDTBSDInfo, buf); err != nil || n != procBSDInfoSize {
		return Identity{BootID: boot, UniqueID: unique},
			fmt.Errorf("%w: proc_bsdinfo for pid %d: %d bytes, %v", ErrNotFound, pid, n, err)
	}
	named := int32(binary.LittleEndian.Uint32(buf[procBSDInfoPIDOff:]))
	if again, err := uniqueID(named); err != nil || again != unique {
		return Identity{}, fmt.Errorf("%w: pid %d changed while its identity was read", ErrNotFound, pid)
	}
	return Identity{
		BootID:            boot,
		UniqueID:          unique,
		StartSeconds:      binary.LittleEndian.Uint64(buf[procBSDInfoStartSecOff:]),
		StartMicroseconds: binary.LittleEndian.Uint64(buf[procBSDInfoStartUsecOff:]),
		Zombie:            binary.LittleEndian.Uint32(buf[procBSDInfoStatusOff:]) == sZomb,
	}, nil
}

// uniqueID is pid's p_uniqueid.
func uniqueID(pid int32) (uint64, error) {
	buf := make([]byte, procUniqInfoSize)
	if n, err := pidInfo(pid, procPIDUniqIdentifierInfo, buf); err != nil || n != procUniqInfoSize {
		return 0, fmt.Errorf("%w: unique id for pid %d: %d bytes, %v", ErrNotFound, pid, n, err)
	}
	return binary.LittleEndian.Uint64(buf[procUniqInfoUniqueIDOff:]), nil
}

// pidInfo is proc_pidinfo(pid, flavor, 1, buf, len(buf)), returning the bytes written.
func pidInfo(pid int32, flavor int, buf []byte) (int, error) {
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), uintptr(flavor), 1,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}
