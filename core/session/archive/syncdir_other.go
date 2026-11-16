//go:build !windows

package archive

import "os"

// openDir opens a directory so that syncDir can fsync it.
func openDir(dir string) (*os.File, error) {
	return os.Open(dir)
}
