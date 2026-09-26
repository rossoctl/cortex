//go:build !unix

package claude

// lockMetadata is a no-op where flock is unavailable.
//
// Windows has equivalent primitives, but reaching them needs golang.org/x/sys, and no release
// artifact targets it — release-binaries.yaml builds linux and darwin only. A no-op leaves those
// platforms exactly where every platform was before the lock existed: the recovery pass below the
// save, and best-effort merge semantics. Worth having as a stub rather than a build failure so the
// package still compiles for anyone cross-checking on Windows.
func lockMetadata(string) (func(), error) {
	return func() {}, nil
}
