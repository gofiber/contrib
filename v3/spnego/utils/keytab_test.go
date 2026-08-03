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
	require.Equal(t, "HTTP/sso-test.example.com@EXAMPLE.LOCAL", info[0].PrincipalName)
	require.Equal(t, "EXAMPLE.LOCAL", info[0].Realm)
	require.Len(t, info[0].Pairs, 2)
	require.Equal(t, uint8(3), info[0].Pairs[0].Version)
	require.Equal(t, int32(17), info[0].Pairs[0].EncryptType)
	require.Equal(t, tm.Unix(), info[0].Pairs[0].CreateTime.Unix())
	require.Equal(t, uint8(3), info[0].Pairs[1].Version)
	require.Equal(t, int32(18), info[0].Pairs[1].EncryptType)
	require.Equal(t, tm.Unix(), info[0].Pairs[1].CreateTime.Unix())
	require.Equal(t, "HTTP/sso-test2.example.com@EXAMPLE.LOCAL", info[1].PrincipalName)
	require.Equal(t, "EXAMPLE.LOCAL", info[1].Realm)
	require.Len(t, info[1].Pairs, 1)
	require.Equal(t, uint8(2), info[1].Pairs[0].Version)
	require.Equal(t, int32(18), info[1].Pairs[0].EncryptType)
	require.Equal(t, tm.Add(-time.Minute).Unix(), info[1].Pairs[0].CreateTime.Unix())
}
