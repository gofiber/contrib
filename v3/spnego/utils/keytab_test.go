package utils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGetKeytabInfo(t *testing.T) {
	tm := time.Now()
	kt, _, err := NewMockKeytab(
		WithRealm("EXAMPLE.LOCAL"),
		WithPrincipal("HTTP/sso-test.example.com"),
		WithPassword("abcd1234"),
		WithPairs(EncryptTypePair{
			Version:     3,
			EncryptType: 17,
			CreateTime:  tm,
		}, EncryptTypePair{
			Version:     3,
			EncryptType: 18,
			CreateTime:  tm,
		}),
	)
	require.NoError(t, err)
	err = kt.AddEntry("HTTP/sso-test2.example.com", "EXAMPLE.LOCAL", "qwer1234", tm.Add(-time.Minute), 2, 18)
	require.NoError(t, err)
	info := GetKeytabInfo(kt)
	require.Len(t, info, 2)
	require.Equal(t, info[0].PrincipalName, "HTTP/sso-test.example.com@EXAMPLE.LOCAL")
	require.Equal(t, info[0].Realm, "EXAMPLE.LOCAL")
	require.Len(t, info[0].Pairs, 2)
	require.Equal(t, info[0].Pairs[0].Version, uint8(3))
	require.Equal(t, info[0].Pairs[0].EncryptType, int32(17))
	require.Equal(t, info[0].Pairs[0].CreateTime.Unix(), tm.Unix())
	require.Equal(t, info[0].Pairs[1].Version, uint8(3))
	require.Equal(t, info[0].Pairs[1].EncryptType, int32(18))
	require.Equal(t, info[0].Pairs[1].CreateTime.Unix(), tm.Unix())
	require.Equal(t, info[1].PrincipalName, "HTTP/sso-test2.example.com@EXAMPLE.LOCAL")
	require.Equal(t, info[1].Realm, "EXAMPLE.LOCAL")
	require.Len(t, info[1].Pairs, 1)
	require.Equal(t, info[1].Pairs[0].Version, uint8(2))
	require.Equal(t, info[1].Pairs[0].EncryptType, int32(18))
	require.Equal(t, info[1].Pairs[0].CreateTime.Unix(), tm.Add(-time.Minute).Unix())
}
