//go:build unix

package spnego

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

// foreignSysFileInfo is an fs.FileInfo whose Sys carries something other than
// the *syscall.Stat_t that os.Stat returns on Unix.
type foreignSysFileInfo struct{ fs.FileInfo }

func (foreignSysFileInfo) Sys() any { return struct{}{} }

// TestFileRevisionIDWithoutStatT covers the type-assertion guard. Nothing in
// this package reaches it — os.Stat on Unix always carries a *syscall.Stat_t —
// but fileRevisionID takes the fs.FileInfo interface, so a wrapped or
// caller-supplied one can arrive, and the alternative to returning the zero
// identity is a panic on the request path.
//
// The zero value is also the right answer rather than merely a safe one: it is
// what the non-Unix builds return, and it makes change detection fall back to
// size and modification time.
func TestFileRevisionIDWithoutStatT(t *testing.T) {
	require.Equal(t, fileID{}, fileRevisionID(foreignSysFileInfo{}),
		"a FileInfo that did not come from os.Stat must degrade, not panic")
}
