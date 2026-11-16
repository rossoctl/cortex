//go:build windows

package archive

import (
	"os"
	"syscall"
)

// openDir opens a directory so that syncDir can flush it. Windows flushes a directory only
// through a handle opened for writing with FILE_FLAG_BACKUP_SEMANTICS. os.Open gives a
// read-only handle, and Sync on it fails with "Access is denied".
func openDir(dir string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return os.NewFile(uintptr(h), dir), nil
}
