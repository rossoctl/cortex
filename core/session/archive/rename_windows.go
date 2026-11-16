//go:build windows

package archive

// closeWriterBeforeRename closes s's open segment before rename moves its directory.
// Windows will not rename a directory while a file in it is open. The next event opens a
// new segment under the new directory. A reader that holds a segment open (a Page read)
// still makes the rename fail: the history then stays in the old directory, under the new
// id, until adopt moves it at the next start.
func (a *Archive) closeWriterBeforeRename(s *sessionState) {
	a.closeWriter(s)
}
