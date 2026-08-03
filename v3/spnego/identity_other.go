//go:build !unix && !windows

package spnego

import "io/fs"

// identityDetectsRename is false: this platform exposes no file identity.
//
//nolint:unused // read by tests; lint runs with --tests=false and cannot see that
const identityDetectsRename = false

// fileRevisionID reports no identity on platforms that expose none through
// fs.FileInfo, so keytab change detection falls back to size and modification
// time alone.
//
// os.SameFile is deliberately not used instead: on some platforms it resolves
// identity lazily by re-opening the recorded path, which would both add a
// syscall per request and compare stamps against whatever currently lives at
// that path rather than against what was stat'ed.
func fileRevisionID(fs.FileInfo) fileID {
	return fileID{}
}
