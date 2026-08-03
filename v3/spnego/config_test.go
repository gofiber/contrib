package spnego

import (
	"os"
	"path"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/stretchr/testify/require"
)

func TestNewKeytabFileLookupFunc(t *testing.T) {
	t.Run("test didn't give any keytab files", func(t *testing.T) {
		_, err := NewKeytabFileLookupFunc()
		require.ErrorIs(t, err, ErrConfigInvalidOfAtLeastOneKeytabFileRequired)
	})
	t.Run("test not found keytab file", func(t *testing.T) {
		invalid := path.Join(t.TempDir(), "invalid.keytab")
		err := os.WriteFile(invalid, []byte("12345"), 0o600)
		require.NoError(t, err)
		fn, err := NewKeytabFileLookupFunc(invalid)
		require.NoError(t, err)
		_, err = fn()
		require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
	})
	t.Run("test one keytab file", func(t *testing.T) {
		tm := time.Now()
		temp := path.Join(t.TempDir(), "temp.keytab")
		_, clean, err := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso.example.com"),
			utils.WithRealm("TEST.LOCAL"),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
			utils.WithFilename(temp),
		)
		require.NoError(t, err)
		t.Cleanup(clean)
		fn, err := NewKeytabFileLookupFunc(temp)
		require.NoError(t, err)
		kt1, err := fn()
		require.NoError(t, err)
		info := utils.GetKeytabInfo(kt1)
		require.Len(t, info, 1)
		require.Equal(t, "HTTP/sso.example.com@TEST.LOCAL", info[0].PrincipalName)
		require.Equal(t, "TEST.LOCAL", info[0].Realm)
		require.Len(t, info[0].Pairs, 1)
		require.Equal(t, uint32(2), info[0].Pairs[0].Version)
		require.Equal(t, int32(18), info[0].Pairs[0].EncryptType)
		// Note: The creation time of keytab is only accurate to the second.
		require.Equal(t, tm.Unix(), info[0].Pairs[0].CreateTime.Unix())
	})
	t.Run("test multiple keytab file but has invalid keytab", func(t *testing.T) {
		tm := time.Now()
		dir := t.TempDir()
		temp := path.Join(dir, "temp.keytab")
		_, clean, err := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso.example.com"),
			utils.WithRealm("TEST.LOCAL"),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
			utils.WithFilename(temp),
		)
		require.NoError(t, err)
		t.Cleanup(clean)
		invalid := path.Join(dir, "invalid1.keytab")
		err = os.WriteFile(invalid, []byte("12345"), 0o600)
		require.NoError(t, err)
		fn, err := NewKeytabFileLookupFunc(temp, invalid)
		require.NoError(t, err)
		_, err = fn()
		require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
	})
	t.Run("test multiple keytab file", func(t *testing.T) {
		tm := time.Now()
		dir := t.TempDir()
		temp1 := path.Join(dir, "temp1.keytab")
		temp2 := path.Join(dir, "temp2.keytab")
		_, clean1, err1 := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso.example1.com"),
			utils.WithRealm("TEST.LOCAL"),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
			utils.WithFilename(temp1),
		)
		require.NoError(t, err1)
		t.Cleanup(clean1)
		_, clean2, err2 := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso.example2.com"),
			utils.WithRealm("TEST.LOCAL"),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 17,
				CreateTime:  tm,
			}, utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
			utils.WithFilename(temp2),
		)
		require.NoError(t, err2)
		t.Cleanup(clean2)
		fn, err := NewKeytabFileLookupFunc(temp1, temp2)
		require.NoError(t, err)
		kt2, err := fn()
		require.NoError(t, err)
		info := utils.GetKeytabInfo(kt2)
		require.Len(t, info, 2)
		require.Equal(t, "HTTP/sso.example1.com@TEST.LOCAL", info[0].PrincipalName)
		require.Equal(t, "TEST.LOCAL", info[0].Realm)
		require.Len(t, info[0].Pairs, 1)
		require.Equal(t, uint32(2), info[0].Pairs[0].Version)
		require.Equal(t, int32(18), info[0].Pairs[0].EncryptType)
		require.Equal(t, tm.Unix(), info[0].Pairs[0].CreateTime.Unix())
		require.Equal(t, "HTTP/sso.example2.com@TEST.LOCAL", info[1].PrincipalName)
		require.Equal(t, "TEST.LOCAL", info[1].Realm)
		require.Len(t, info[1].Pairs, 2)
		require.Equal(t, uint32(2), info[1].Pairs[0].Version)
		require.Equal(t, int32(17), info[1].Pairs[0].EncryptType)
		require.Equal(t, tm.Unix(), info[1].Pairs[0].CreateTime.Unix())
		require.Equal(t, uint32(2), info[1].Pairs[1].Version)
		require.Equal(t, int32(18), info[1].Pairs[1].EncryptType)
		require.Equal(t, tm.Unix(), info[1].Pairs[1].CreateTime.Unix())
	})
}
