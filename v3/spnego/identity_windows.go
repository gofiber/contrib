//go:build windows

package spnego

import (
	"io/fs"
	"syscall"
)

// fileRevisionID returns the file's creation time, which Windows already
// supplies through os.Stat at no extra syscall cost.
//
// This is a best-effort hint rather than a true identity. It usually
// distinguishes a keytab staged separately and renamed into place, because the
// replacement was created separately, but two cases defeat it: NTFS file
// tunneling restores the superseded file's creation time to a replacement made
// at the same name within about fifteen seconds, and FAT, exFAT and several
// network redirectors report no creation time at all. Both fall back to size
// and modification time.
//
// A dependable identity — volume serial plus file index — requires opening the
// file, which would put a syscall on every authenticated request. If that is
// ever adopted, flip identityDetectsRename in identity_rename_windows_test.go
// so the rotation test stops skipping here.
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
