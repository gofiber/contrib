package utils

import (
	"errors"
	"io"
	"os"
	"path"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/types"
	"github.com/stretchr/testify/require"
)

type mockFileOperator struct {
	flag int
}

const (
	// readOnlyFileOperator creates the file but hands back an unwritable handle, so the
	// write fails while the close succeeds — where a reused err would drop the write's.
	readOnlyFileOperator = 0x04
	// closeFailsFileOperator writes successfully and fails only on close, the
	// one combination a real *os.File cannot produce.
	closeFailsFileOperator = 0x08
	// removeFailsFileOperator refuses to remove the existing file, which is
	// where a stale keytab at the wrong mode would otherwise be written into.
	removeFailsFileOperator = 0x10
)

// errCloseFailed is what closeFailsFileOperator reports, distinct from any
// error the OS would produce so the assertion cannot pass by coincidence.
var errCloseFailed = errors.New("close refused")

// failingCloser writes through to the file and then refuses to close.
type failingCloser struct{ *os.File }

func (f failingCloser) Close() error {
	_ = f.File.Close()
	return errCloseFailed
}

func (m mockFileOperator) OpenFile(filename string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	if m.flag&0x01 != 0 {
		return nil, os.ErrPermission
	}
	if m.flag&readOnlyFileOperator != 0 {
		flag = os.O_RDONLY | os.O_CREATE
	}
	file, err := os.OpenFile(filename, flag, perm)
	if err != nil {
		return nil, err
	}
	if m.flag&0x02 != 0 {
		file.Close()
	}
	if m.flag&closeFailsFileOperator != 0 {
		return failingCloser{File: file}, nil
	}
	return file, nil
}

func (m mockFileOperator) Remove(filename string) error {
	if m.flag&removeFailsFileOperator != 0 {
		return os.ErrPermission
	}
	return os.Remove(filename)
}

func TestNewMockKeytab(t *testing.T) {
	t.Run("test add keytab entry failed", func(t *testing.T) {
		_, _, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}, EncryptTypePair{
				Version:     3,
				EncryptType: 0xffff,
				CreateTime:  time.Now(),
			}),
		)
		require.Error(t, err)
	})
	t.Run("test none file created", func(t *testing.T) {
		tm := time.Now()
		kt, clean, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  tm,
			}),
		)
		require.NoError(t, err)
		t.Cleanup(clean)
		_, kv, err := kt.GetEncryptionKey(types.NewPrincipalName(1, "HTTP/sso.example.com"), "TEST.LOCAL", 3, 18)
		require.NoError(t, err)
		require.Equal(t, 3, kv)
	})
	t.Run("test file open failed", func(t *testing.T) {
		prevFileOperator := defaultFileOperator
		defaultFileOperator = mockFileOperator{flag: 0x01}
		t.Cleanup(func() {
			defaultFileOperator = prevFileOperator
		})
		filename := path.Join(t.TempDir(), "temp.keytab")
		_, _, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}),
			WithFilename(filename),
		)
		require.ErrorIs(t, err, os.ErrPermission)
		require.NoFileExists(t, filename)
	})
	t.Run("test file write failed", func(t *testing.T) {
		prevFileOperator := defaultFileOperator
		defaultFileOperator = mockFileOperator{flag: 0x02}
		t.Cleanup(func() {
			defaultFileOperator = prevFileOperator
		})
		filename := path.Join(t.TempDir(), "temp.keytab")
		_, _, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}),
			WithFilename(filename),
		)
		require.ErrorIs(t, err, os.ErrClosed)
		require.NoFileExists(t, filename)
		// Both fail with the same error, so only the wording separates them: the write is
		// the cause and the close an aside. Reporting only the close would hide it.
		require.ErrorContains(t, err, "error writing to file")
		require.ErrorContains(t, err, "also failed to close file")
	})
	t.Run("a real open failure is reported", func(t *testing.T) {
		// Runs against the default file operator, so the production open path's own error
		// branch is exercised rather than the mock standing in for it.
		filename := path.Join(t.TempDir(), "no-such-directory", "temp.keytab")
		_, _, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}),
			WithFilename(filename),
		)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.ErrorContains(t, err, "error opening file")
		require.NoFileExists(t, filename)
	})
	t.Run("a failed write is reported even when the close succeeds", func(t *testing.T) {
		prevFileOperator := defaultFileOperator
		defaultFileOperator = mockFileOperator{flag: readOnlyFileOperator}
		t.Cleanup(func() {
			defaultFileOperator = prevFileOperator
		})
		filename := path.Join(t.TempDir(), "temp.keytab")
		_, _, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}),
			WithFilename(filename),
		)
		require.ErrorContains(t, err, "error writing to file",
			"a successful close must not overwrite the write error")
		require.NotContains(t, err.Error(), "error closing file")
		require.NoFileExists(t, filename,
			"a half-written keytab must not be left on disk")
	})
	t.Run("a failed close is reported and the file removed", func(t *testing.T) {
		// A close that fails after a good write can still leave a truncated keytab, so
		// success here would hand back a keytab that does not match the file.
		prevFileOperator := defaultFileOperator
		defaultFileOperator = mockFileOperator{flag: closeFailsFileOperator}
		t.Cleanup(func() {
			defaultFileOperator = prevFileOperator
		})
		filename := path.Join(t.TempDir(), "temp.keytab")
		_, _, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}),
			WithFilename(filename),
		)
		require.ErrorIs(t, err, errCloseFailed)
		require.ErrorContains(t, err, "error closing file")
		require.NoFileExists(t, filename)
	})
	t.Run("the keytab file is readable only by its owner", func(t *testing.T) {
		// Keytabs hold long-term key material. Widening this would expose it to
		// every local account, and nothing else in the suite looks at the mode.
		filename := path.Join(t.TempDir(), "temp.keytab")
		_, clean, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}),
			WithFilename(filename),
		)
		require.NoError(t, err)
		t.Cleanup(clean)

		info, err := os.Stat(filename)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})
	t.Run("test file created", func(t *testing.T) {
		filename := path.Join(t.TempDir(), "temp.keytab")
		tm := time.Now()
		_, clean, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  tm,
			}),
			WithFilename(filename),
		)
		require.NoError(t, err)
		t.Cleanup(clean)
		require.FileExists(t, filename)
		kt, err := keytab.Load(filename)
		require.NoError(t, err)
		_, kv, err := kt.GetEncryptionKey(types.NewPrincipalName(1, "HTTP/sso.example.com"), "TEST.LOCAL", 3, 18)
		require.NoError(t, err)
		require.Equal(t, 3, kv)
	})
}

func TestWithFilename(t *testing.T) {
	opts := mockOptions{}
	require.Empty(t, opts.Filename)
	WithFilename("/tmp/test.keytab")(&opts)
	require.Equal(t, "/tmp/test.keytab", opts.Filename)
}

func TestWithPairs(t *testing.T) {
	opts := mockOptions{}
	tm := time.Now()
	require.Len(t, opts.Pairs, 0)
	WithPairs(EncryptTypePair{
		Version:     2,
		EncryptType: 17,
		CreateTime:  tm.Add(-time.Minute),
	}, EncryptTypePair{
		Version:     2,
		EncryptType: 18,
		CreateTime:  tm.Add(-time.Minute),
	})(&opts)
	require.Len(t, opts.Pairs, 2)
	WithPairs(EncryptTypePair{
		Version:     3,
		EncryptType: 18,
		CreateTime:  tm,
	})(&opts)
	require.Len(t, opts.Pairs, 3)
	require.Equal(t, []EncryptTypePair{
		{Version: 2, EncryptType: 17, CreateTime: tm.Add(-time.Minute)},
		{Version: 2, EncryptType: 18, CreateTime: tm.Add(-time.Minute)},
		{Version: 3, EncryptType: 18, CreateTime: tm},
	}, opts.Pairs)
}

func TestWithPassword(t *testing.T) {
	opts := mockOptions{}
	require.Empty(t, opts.Password)
	WithPassword("abcd1234")(&opts)
	require.Equal(t, "abcd1234", opts.Password)
}

func TestWithPrincipal(t *testing.T) {
	opts := mockOptions{}
	require.Empty(t, opts.PrincipalName)
	WithPrincipal("HTTP/sso.example.local")(&opts)
	require.Equal(t, "HTTP/sso.example.local", opts.PrincipalName)
}

func TestWithRealm(t *testing.T) {
	opts := mockOptions{}
	require.Empty(t, opts.Realm)
	WithRealm("EXAMPLE.LOCAL")(&opts)
	require.Equal(t, "EXAMPLE.LOCAL", opts.Realm)
}

func Test_mockOptions_apply(t *testing.T) {
	opts := mockOptions{}
	require.Empty(t, opts.Filename)
	require.Empty(t, opts.Realm)
	opts.apply(WithFilename("/tmp/test.keytab"), WithRealm("TEST.LOCAL"))
	require.Equal(t, "/tmp/test.keytab", opts.Filename)
	require.Equal(t, "TEST.LOCAL", opts.Realm)
}

func TestNewMockKeytabRejectsUnrepresentableVersion(t *testing.T) {
	// gokrb5's AddEntry takes a uint8. Truncating silently would defeat the point of
	// reporting the 32-bit key version in KeytabInfo.
	_, _, err := NewMockKeytab(
		WithPrincipal("HTTP/sso.example.com"),
		WithRealm("TEST.LOCAL"),
		WithPairs(EncryptTypePair{
			Version:     256,
			EncryptType: 18,
			CreateTime:  time.Now(),
		}),
	)
	require.ErrorIs(t, err, ErrInvalidKeyVersion)
}

// TestNewMockKeytabResetsPermissionsOfAnExistingFile covers what os.OpenFile does
// not do: perm applies only on create, so an existing 0644 path leaks the key.
func TestNewMockKeytabResetsPermissionsOfAnExistingFile(t *testing.T) {
	filename := path.Join(t.TempDir(), "temp.keytab")
	require.NoError(t, os.WriteFile(filename, []byte("previous occupant"), 0o644))

	info, err := os.Stat(filename)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm(),
		"the fixture must start world-readable, or this proves nothing")

	_, clean, err := NewMockKeytab(
		WithPrincipal("HTTP/sso.example.com"),
		WithRealm("TEST.LOCAL"),
		WithPairs(EncryptTypePair{Version: 3, EncryptType: 18, CreateTime: time.Now()}),
		WithFilename(filename),
	)
	require.NoError(t, err)
	t.Cleanup(clean)

	info, err = os.Stat(filename)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"key material must not be left readable by other accounts")
}

// TestNewMockKeytabReportsAFailedRemove pins that a remove which fails for a
// reason other than "not there" is reported as itself.
//
// Without the check the exclusive create still fails, but as "file exists" — which
// points at the wrong thing. The cause is that the old keytab could not be cleared.
func TestNewMockKeytabReportsAFailedRemove(t *testing.T) {
	prevFileOperator := defaultFileOperator
	defaultFileOperator = mockFileOperator{flag: removeFailsFileOperator}
	t.Cleanup(func() { defaultFileOperator = prevFileOperator })

	filename := path.Join(t.TempDir(), "temp.keytab")
	require.NoError(t, os.WriteFile(filename, []byte("previous occupant"), 0o644))

	_, _, err := NewMockKeytab(
		WithPrincipal("HTTP/sso.example.com"),
		WithRealm("TEST.LOCAL"),
		WithPairs(EncryptTypePair{Version: 3, EncryptType: 18, CreateTime: time.Now()}),
		WithFilename(filename),
	)
	require.ErrorIs(t, err, os.ErrPermission)
	require.ErrorContains(t, err, "error removing existing file",
		"the cause is the stale keytab that could not be cleared, not the create that then found it")
}
