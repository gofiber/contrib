//go:build unix

package spnego

import (
	"io/fs"
	"syscall"
)

// identityDetectsRename reports whether this platform's fileRevisionID reliably
// distinguishes a file replaced by rename from the file it replaced.
//
//nolint:unused // read by tests; lint runs with --tests=false and cannot see that
const identityDetectsRename = true

// fileRevisionID returns the device and inode the file lives at. Replacing a
// keytab by rename changes the inode, so a rotation is detected even when the
// replacement has the same size and modification time.
func fileRevisionID(info fs.FileInfo) fileID {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}
	}
	return fileID{a: uint64(stat.Dev), b: uint64(stat.Ino)}
}
