//go:build !unix && !windows

package spnego

import "io/fs"

// fileRevisionID reports no identity here, so detection falls back to size and
// mtime. os.SameFile would re-open the path, costing a syscall per request.
func fileRevisionID(fs.FileInfo) fileID {
	return fileID{}
}
