//go:build unix

package spnego

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

// An fs.FileInfo whose Sys carries something other than a *syscall.Stat_t.
type foreignSysFileInfo struct{ fs.FileInfo }

func (foreignSysFileInfo) Sys() any { return struct{}{} }

// Unreachable here, but the parameter is an interface and the alternative is a panic.
//
// The zero value is what non-Unix builds return: fall back to size and mtime.
func TestFileRevisionIDWithoutStatT(t *testing.T) {
	require.Equal(t, fileID{}, fileRevisionID(foreignSysFileInfo{}),
		"a FileInfo that did not come from os.Stat must degrade, not panic")
}
