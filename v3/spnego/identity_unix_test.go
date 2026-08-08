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

// TestFileRevisionIDWithoutStatT covers the type-assertion guard. Unreachable here,
// but fileRevisionID takes an interface, and the alternative is a panic per request.
//
// The zero value is the right answer, not merely a safe one: it is what non-Unix
// builds return, and it falls back to size and modification time.
func TestFileRevisionIDWithoutStatT(t *testing.T) {
	require.Equal(t, fileID{}, fileRevisionID(foreignSysFileInfo{}),
		"a FileInfo that did not come from os.Stat must degrade, not panic")
}
