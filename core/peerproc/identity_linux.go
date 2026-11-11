//go:build linux

package peerproc

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processIdentity reads pid as Codex's Linux reader does (read_process_details): the
// state and starttime fields of /proc/<pid>/stat, and the boot id.
func processIdentity(pid int32) (Identity, error) {
	if pid <= 0 {
		return Identity{}, ErrNotFound
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: /proc/%d/stat: %v", ErrNotFound, pid, err)
	}
	state, ticks, err := parseStatIdentity(b)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: pid %d: %v", ErrNotFound, pid, err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	id := strings.TrimSpace(string(boot))
	if err != nil || id == "" {
		return Identity{}, fmt.Errorf("%w: boot_id: %v", ErrUnsupported, err)
	}
	return Identity{BootID: id, StartTicks: ticks, Zombie: state == "Z"}, nil
}

// parseStatIdentity reads the state (field 3) and starttime (field 22) from a
// /proc/<pid>/stat line, counting from the last ')' as parseStat does, since the command
// name may contain spaces and parentheses.
func parseStatIdentity(b []byte) (state string, startTicks uint64, err error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", 0, fmt.Errorf("stat: no ')'")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", 0, fmt.Errorf("stat: %d fields after the command name", len(f))
	}
	st, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return "", 0, err
	}
	return f[0], st, nil
}
