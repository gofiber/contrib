package spnego

import (
	"os"
	"path"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/stretchr/testify/require"
)

// writeMockKeytab writes a keytab holding a single principal and returns its path.
func writeMockKeytab(t *testing.T, dir, name, principal string) string {
	t.Helper()
	filename := path.Join(dir, name)
	_, clean, err := utils.NewMockKeytab(
		utils.WithPrincipal(principal),
		utils.WithRealm("TEST.LOCAL"),
		utils.WithFilename(filename),
		utils.WithPairs(utils.EncryptTypePair{
			Version:     2,
			EncryptType: 18,
			CreateTime:  time.Now(),
		}),
	)
	require.NoError(t, err)
	t.Cleanup(clean)
	return filename
}

func TestKeytabFileLookupCaching(t *testing.T) {
	t.Run("reuses the merged keytab while the files are unchanged", func(t *testing.T) {
		filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")
		fn, err := NewKeytabFileLookupFunc(filename)
		require.NoError(t, err)

		first, err := fn()
		require.NoError(t, err)
		second, err := fn()
		require.NoError(t, err)

		// A stable pointer is what lets the middleware reuse its SPNEGO handler.
		require.Same(t, first, second)
	})

	t.Run("reloads after the keytab changes on disk", func(t *testing.T) {
		dir := t.TempDir()
		filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
		fn, err := NewKeytabFileLookupFunc(filename)
		require.NoError(t, err)

		before, err := fn()
		require.NoError(t, err)
		require.Len(t, utils.GetKeytabInfo(before), 1)

		// Rotate the keytab in place with a different principal. The mock helper
		// truncates, so the stamp changes even if the size happens to match.
		replacement := writeMockKeytab(t, dir, "rotated.keytab", "HTTP/rotated.example.com")
		contents, err := os.ReadFile(replacement)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filename, contents, 0o600))
		require.NoError(t, os.Chtimes(filename, time.Now().Add(time.Second), time.Now().Add(time.Second)))

		after, err := fn()
		require.NoError(t, err)
		require.NotSame(t, before, after)

		info := utils.GetKeytabInfo(after)
		require.Len(t, info, 1)
		require.Equal(t, "HTTP/rotated.example.com@TEST.LOCAL", info[0].PrincipalName)
	})

	t.Run("reports a missing keytab file", func(t *testing.T) {
		fn, err := NewKeytabFileLookupFunc(path.Join(t.TempDir(), "absent.keytab"))
		require.NoError(t, err)
		_, err = fn()
		require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
	})
}

func TestNewSystemKeytabLookupFunc(t *testing.T) {
	t.Run("loads the keytab named by KRB5_KTNAME", func(t *testing.T) {
		filename := writeMockKeytab(t, t.TempDir(), "system.keytab", "HTTP/system.example.com")
		t.Setenv("KRB5_KTNAME", filename)

		fn, err := NewSystemKeytabLookupFunc()
		require.NoError(t, err)
		kt, err := fn()
		require.NoError(t, err)

		info := utils.GetKeytabInfo(kt)
		require.Len(t, info, 1)
		require.Equal(t, "HTTP/system.example.com@TEST.LOCAL", info[0].PrincipalName)
	})

	t.Run("accepts the FILE: prefix MIT Kerberos allows", func(t *testing.T) {
		filename := writeMockKeytab(t, t.TempDir(), "system.keytab", "HTTP/system.example.com")
		t.Setenv("KRB5_KTNAME", "FILE:"+filename)

		fn, err := NewSystemKeytabLookupFunc()
		require.NoError(t, err)
		_, err = fn()
		require.NoError(t, err)
	})

	t.Run("falls back to the default path when KRB5_KTNAME is unset", func(t *testing.T) {
		t.Setenv("KRB5_KTNAME", "")

		fn, err := NewSystemKeytabLookupFunc()
		require.NoError(t, err)
		// The default path is almost never present in CI, so assert on the
		// resolved target rather than on a successful load.
		if _, statErr := os.Stat(DefaultSystemKeytabPath); statErr != nil {
			_, err = fn()
			require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
			require.Contains(t, err.Error(), DefaultSystemKeytabPath)
		}
	})
}

func TestHandlerCache(t *testing.T) {
	var built int
	cache := &handlerCache{
		build: func(*keytab.Keytab) fiber.Handler {
			built++
			return func(fiber.Ctx) error { return nil }
		},
	}

	first := &keytab.Keytab{}
	second := &keytab.Keytab{}

	cache.get(first)
	cache.get(first)
	require.Equal(t, 1, built, "the handler should be built once per keytab")

	cache.get(second)
	require.Equal(t, 2, built, "a new keytab should rebuild the handler")

	cache.get(first)
	require.Equal(t, 3, built, "the cache holds a single entry")
}
