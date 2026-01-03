package spnego

import (
	"os"
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
		err := os.WriteFile("./invalid.keytab", []byte("12345"), 0600)
		require.NoError(t, err)
		t.Cleanup(func() {
			os.Remove("./invalid.keytab")
		})
		fn, err := NewKeytabFileLookupFunc("./invalid.keytab")
		require.NoError(t, err)
		_, err = fn()
		require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
	})
	t.Run("test one keytab file", func(t *testing.T) {
		tm := time.Now()
		_, clean, err := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso.example.com"),
			utils.WithRealm("TEST.LOCAL"),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
			utils.WithFilename("./temp.keytab"),
		)
		require.NoError(t, err)
		t.Cleanup(clean)
		fn, err := NewKeytabFileLookupFunc("./temp.keytab")
		require.NoError(t, err)
		kt1, err := fn()
		require.NoError(t, err)
		info := utils.GetKeytabInfo(kt1)
		require.Len(t, info, 1)
		require.Equal(t, info[0].PrincipalName, "HTTP/sso.example.com@TEST.LOCAL")
		require.Equal(t, info[0].Realm, "TEST.LOCAL")
		require.Len(t, info[0].Pairs, 1)
		require.Equal(t, info[0].Pairs[0].Version, uint8(2))
		require.Equal(t, info[0].Pairs[0].EncryptType, int32(18))
		// Note: The creation time of keytab is only accurate to the second.
		require.Equal(t, info[0].Pairs[0].CreateTime.Unix(), tm.Unix())
	})
	t.Run("test multiple keytab file but has invalid keytab", func(t *testing.T) {
		tm := time.Now()
		_, clean, err := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso.example.com"),
			utils.WithRealm("TEST.LOCAL"),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
			utils.WithFilename("./temp.keytab"),
		)
		require.NoError(t, err)
		t.Cleanup(clean)
		err = os.WriteFile("./invalid1.keytab", []byte("12345"), 0600)
		require.NoError(t, err)
		t.Cleanup(func() {
			os.Remove("./invalid1.keytab")
		})
		fn, err := NewKeytabFileLookupFunc("./temp.keytab", "./invalid1.keytab")
		require.NoError(t, err)
		_, err = fn()
		require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
	})
	t.Run("test multiple keytab file", func(t *testing.T) {
		tm := time.Now()
		_, clean1, err1 := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso.example1.com"),
			utils.WithRealm("TEST.LOCAL"),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
			utils.WithFilename("./temp1.keytab"),
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
			utils.WithFilename("./temp2.keytab"),
		)
		require.NoError(t, err2)
		t.Cleanup(clean2)
		fn, err := NewKeytabFileLookupFunc("./temp1.keytab", "./temp2.keytab")
		require.NoError(t, err)
		kt2, err := fn()
		require.NoError(t, err)
		info := utils.GetKeytabInfo(kt2)
		require.Len(t, info, 2)
		require.Equal(t, info[0].PrincipalName, "HTTP/sso.example1.com@TEST.LOCAL")
		require.Equal(t, info[0].Realm, "TEST.LOCAL")
		require.Len(t, info[0].Pairs, 1)
		require.Equal(t, info[0].Pairs[0].Version, uint8(2))
		require.Equal(t, info[0].Pairs[0].EncryptType, int32(18))
		require.Equal(t, info[0].Pairs[0].CreateTime.Unix(), tm.Unix())
		require.Equal(t, info[1].PrincipalName, "HTTP/sso.example2.com@TEST.LOCAL")
		require.Equal(t, info[1].Realm, "TEST.LOCAL")
		require.Len(t, info[1].Pairs, 2)
		require.Equal(t, info[1].Pairs[0].Version, uint8(2))
		require.Equal(t, info[1].Pairs[0].EncryptType, int32(17))
		require.Equal(t, info[1].Pairs[0].CreateTime.Unix(), tm.Unix())
		require.Equal(t, info[1].Pairs[1].Version, uint8(2))
		require.Equal(t, info[1].Pairs[1].EncryptType, int32(18))
		require.Equal(t, info[1].Pairs[1].CreateTime.Unix(), tm.Unix())
	})
}
