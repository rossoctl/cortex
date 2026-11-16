//go:build !windows

package archive

// closeWriterBeforeRename does nothing here: a directory moves with its open files, so
// the segment stays open across a rename (rename_windows.go has the one exception).
func (a *Archive) closeWriterBeforeRename(*sessionState) {}
