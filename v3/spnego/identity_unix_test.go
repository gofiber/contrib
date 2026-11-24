//go:build unix

package spnego

import (
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gofiber/contrib/v3/spnego/utils"
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

// A proxy for a reused inode number: same inode, size and mtime, new keys.
func TestKeytabRewriteWithRestoredStampDetected(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	info, err := os.Stat(filename)
	require.NoError(t, err)

	cache := newKeytabFileCache([]string{filename})
	_, err = cache.load()
	require.NoError(t, err)

	replacement, err := os.ReadFile(writeMockKeytab(t, dir, "new.keytab", "HTTP/sso.example.org"))
	require.NoError(t, err)
	require.Len(t, replacement, int(info.Size()))
	// Linux stamps ctime from a coarse clock, so rewrite until it lands on a later tick.
	for start := time.Now(); ; time.Sleep(time.Millisecond) {
		require.NoError(t, os.WriteFile(filename, replacement, 0o600))
		require.NoError(t, os.Chtimes(filename, info.ModTime(), info.ModTime()))
		rewritten, err := os.Stat(filename)
		require.NoError(t, err)
		if fileRevisionID(rewritten) != fileRevisionID(info) || time.Since(start) > 2*time.Second {
			break
		}
	}

	after, err := cache.load()
	require.NoError(t, err)
	require.Equal(t, "HTTP/sso.example.org@TEST.LOCAL", utils.GetKeytabInfo(after)[0].PrincipalName)
}
