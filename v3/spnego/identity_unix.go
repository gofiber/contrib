//go:build unix

package spnego

import (
	"io/fs"
	"syscall"
)

// fileIdentity reports the device and inode a file lives at, which change when
// a keytab is replaced by rename even if the replacement has the same size and
// modification time.
func fileIdentity(info fs.FileInfo) (dev, ino uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}
