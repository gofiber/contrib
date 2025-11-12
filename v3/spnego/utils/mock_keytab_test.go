package utils

import (
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

func (m mockFileOperator) OpenFile(filename string, flag int, perm os.FileMode) (*os.File, error) {
	if m.flag&0x01 != 0 {
		return nil, os.ErrPermission
	}
	file, err := os.OpenFile(filename, flag, perm)
	if err != nil {
		return nil, err
	}
	if m.flag&0x02 != 0 {
		file.Close()
	}
	return file, nil
}

func (m mockFileOperator) Remove(filename string) error {
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
		_, _, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}),
			WithFilename("./temp.keytab"),
		)
		require.ErrorIs(t, err, os.ErrPermission)
		require.NoFileExists(t, "./temp.keytab")
	})
	t.Run("test file write failed", func(t *testing.T) {
		prevFileOperator := defaultFileOperator
		defaultFileOperator = mockFileOperator{flag: 0x02}
		t.Cleanup(func() {
			defaultFileOperator = prevFileOperator
		})
		_, _, err := NewMockKeytab(
			WithPrincipal("HTTP/sso.example.com"),
			WithRealm("TEST.LOCAL"),
			WithPairs(EncryptTypePair{
				Version:     3,
				EncryptType: 18,
				CreateTime:  time.Now(),
			}),
			WithFilename("./temp.keytab"),
		)
		require.ErrorIs(t, err, os.ErrClosed)
		require.NoFileExists(t, "./temp.keytab")
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
	require.Equal(t, opts.Pairs, []EncryptTypePair{
		{Version: 2, EncryptType: 17, CreateTime: tm.Add(-time.Minute)},
		{Version: 2, EncryptType: 18, CreateTime: tm.Add(-time.Minute)},
		{Version: 3, EncryptType: 18, CreateTime: tm},
	})
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
