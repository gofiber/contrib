//go:build !unix && !windows

package spnego

import "io/fs"

// fileRevisionID reports no identity where fs.FileInfo exposes none, so change
// detection falls back to size and modification time. os.SameFile is not used
// instead: it re-opens the path, costing a syscall on every request.
func fileRevisionID(fs.FileInfo) fileID {
	return fileID{}
}
