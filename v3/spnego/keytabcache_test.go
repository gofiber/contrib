package spnego

import (
	"os"
	"path"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/spnego/utils"
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

		// A stable pointer means an unchanged keytab was not re-read and
		// re-parsed, and lets a caller reuse anything derived from it.
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

	t.Run("accepts a lowercase file type", func(t *testing.T) {
		filename := writeMockKeytab(t, t.TempDir(), "system.keytab", "HTTP/system.example.com")
		t.Setenv("KRB5_KTNAME", "file:"+filename)

		fn, err := NewSystemKeytabLookupFunc()
		require.NoError(t, err)
		_, err = fn()
		require.NoError(t, err, "type names are case-insensitive")
	})

	t.Run("falls back to the default path when KRB5_KTNAME is unset", func(t *testing.T) {
		t.Setenv("KRB5_KTNAME", "")

		fn, err := NewSystemKeytabLookupFunc()
		require.NoError(t, err)
		require.NotNil(t, fn)

		// The resolved path is not exposed, so it is observed through a load:
		// the failure names the file it tried. A host that really has a system
		// keytab would load it instead, and is not a useful place to assert
		// this, so the check is skipped there rather than made conditional on
		// an outcome.
		if _, statErr := os.Stat(DefaultSystemKeytabPath); statErr != nil {
			_, loadErr := fn()
			require.ErrorContains(t, loadErr, DefaultSystemKeytabPath,
				"an unset KRB5_KTNAME must resolve to DefaultSystemKeytabPath")
		}
	})

	t.Run("the default path is MIT Kerberos's standard location", func(t *testing.T) {
		// Pinned as a literal because every other assertion here is written in
		// terms of the constant and so holds whatever it says. This is exported
		// API, and moving it silently relocates every host relying on the
		// fallback.
		require.Equal(t, "/etc/krb5.keytab", DefaultSystemKeytabPath)
	})

	t.Run("accepts the WRFILE: prefix", func(t *testing.T) {
		filename := writeMockKeytab(t, t.TempDir(), "system.keytab", "HTTP/system.example.com")
		t.Setenv("KRB5_KTNAME", "WRFILE:"+filename)

		fn, err := NewSystemKeytabLookupFunc()
		require.NoError(t, err)
		_, err = fn()
		require.NoError(t, err)
	})

	t.Run("rejects keytab types that are not plain files", func(t *testing.T) {
		for _, name := range []string{
			"KEYRING:persistent:0:0",
			"MEMORY:mykeytab",
			"ANY:FILE:/tmp/a",
			"DIR:/etc/krb5kt",
			"SRVTAB:/etc/srvtab",
			"keyring:persistent:0:0", // type names are case-insensitive
		} {
			t.Setenv("KRB5_KTNAME", name)
			_, err := NewSystemKeytabLookupFunc()
			require.ErrorIs(t, err, ErrUnsupportedKeytabResidualType, name)
		}
	})

	t.Run("treats a path as a path, not a residual type", func(t *testing.T) {
		// resolveKeytabResidual never touches the filesystem, so these are just
		// names; nothing here needs to exist.
		for _, name := range []string{
			path.Join(t.TempDir(), "system.keytab"), // POSIX absolute path
			`C:\krb5.keytab`,                        // Windows drive letter
			`c:/krb5.keytab`,                        // lowercase, forward slashes
			`\\host\share\kt`,                       // UNC path
			"backup:2024.keytab",                    // relative name that happens to contain a colon
		} {
			resolved, err := resolveKeytabResidual(name)
			require.NoError(t, err, name)
			require.Equal(t, name, resolved, name)
		}
	})

	t.Run("rejects a file type with no residual", func(t *testing.T) {
		t.Setenv("KRB5_KTNAME", "FILE:")
		_, err := NewSystemKeytabLookupFunc()
		require.ErrorIs(t, err, ErrConfigInvalidOfAtLeastOneKeytabFileRequired)
	})
}
