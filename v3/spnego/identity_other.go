//go:build !unix

package spnego

import "io/fs"

// fileIdentity reports no identity on platforms that do not expose one through
// fs.FileInfo, so keytab change detection falls back to size and modification
// time alone. os.SameFile is deliberately not used instead: it resolves a
// Windows file's identity lazily by re-opening the recorded path, which would
// both add a syscall to every request and compare two stamps against whatever
// currently lives at that path rather than against what was stat'ed.
func fileIdentity(fs.FileInfo) (dev, ino uint64, ok bool) {
	return 0, 0, false
}
