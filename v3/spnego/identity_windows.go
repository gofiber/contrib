//go:build windows

package spnego

import (
	"io/fs"
	"syscall"
)

// fileRevisionID returns the creation time os.Stat already supplies. A hint,
// not an identity: NTFS tunneling and FAT both defeat it, falling back to mtime.
func fileRevisionID(info fs.FileInfo) fileID {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fileID{}
	}
	// Filetime.Nanoseconds maps an unset time to a constant that looks real.
	if data.CreationTime.LowDateTime == 0 && data.CreationTime.HighDateTime == 0 {
		return fileID{}
	}
	return fileID{a: uint64(data.CreationTime.Nanoseconds())}
}
