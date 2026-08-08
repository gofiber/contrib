//go:build unix

package spnego

import (
	"io/fs"
	"syscall"
)

// fileRevisionID returns the device and inode, so a rotation by rename is
// detected even at the same size and mtime.
func fileRevisionID(info fs.FileInfo) fileID {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}
	}
	return fileID{a: uint64(stat.Dev), b: uint64(stat.Ino)}
}
