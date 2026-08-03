//go:build windows

package spnego

import (
	"io/fs"
	"syscall"
)

// fileRevisionID returns the file's creation time, which Windows already
// supplies through os.Stat at no extra syscall cost.
//
// It is not a true identity the way a Unix inode is, but it discriminates the
// case size and modification time miss: a keytab staged as a separate file and
// renamed into place was created separately, so its creation time differs even
// when a copy preserved the modification time. A real identity — volume serial
// plus file index — requires opening the file, which would put a syscall on
// every authenticated request.
func fileRevisionID(info fs.FileInfo) (fileID, bool) {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fileID{}, false
	}
	return fileID{a: uint64(data.CreationTime.Nanoseconds())}, true
}
