//go:build windows

package spnego

import (
	"io/fs"
	"syscall"
)

// fileRevisionID returns the file's creation time, which os.Stat already
// supplies at no extra syscall cost. A best-effort hint, not a true identity:
// NTFS file tunneling restores it to a same-named replacement, and FAT and some
// network redirectors report none — both fall back to size and mtime.
func fileRevisionID(info fs.FileInfo) fileID {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fileID{}
	}
	// An unset creation time carries no information, and Filetime.Nanoseconds
	// maps it to a fixed constant that would look like a real value.
	if data.CreationTime.LowDateTime == 0 && data.CreationTime.HighDateTime == 0 {
		return fileID{}
	}
	return fileID{a: uint64(data.CreationTime.Nanoseconds())}
}
