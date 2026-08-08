package utils

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jcmturner/gokrb5/v8/keytab"
)

// KeytabInfo describes one principal in a keytab.
type KeytabInfo struct {
	// Fully qualified, realm included: "HTTP/svc.example.com@EXAMPLE.COM".
	// Realm repeats that suffix, so appending it prints the realm twice.
	PrincipalName string
	Realm         string            // The Kerberos realm (e.g., EXAMPLE.COM)
	Pairs         []EncryptTypePair // List of encryption type pairs for this principal
}

// EncryptTypePair is one encryption-type entry in a keytab.
type EncryptTypePair struct {
	// gokrb5 prefers the 32-bit form, so this is wider than NewMockKeytab takes.
	Version     uint32
	EncryptType int32     // The encryption type (e.g., 18 for AES-256-CTS-HMAC-SHA1-96)
	CreateTime  time.Time // The timestamp when this key was created
}

// MultiKeytabInfo is a set of principals from a keytab.
type MultiKeytabInfo []KeytabInfo

// GetKeytabInfo groups a keytab's entries by principal and sorts them by name.
// A nil keytab yields an empty slice rather than nil.
func GetKeytabInfo(kt *keytab.Keytab) MultiKeytabInfo {
	keytabMap := make(map[string]KeytabInfo)
	if kt != nil {
		for _, entry := range kt.Entries {
			// Principal.String allocates, so build it once per entry.
			name := entry.Principal.String()
			item, ok := keytabMap[name]
			if !ok {
				item = KeytabInfo{
					PrincipalName: name,
					Realm:         entry.Principal.Realm,
				}
			}
			item.Pairs = append(item.Pairs, EncryptTypePair{
				Version:     entry.KVNO,
				EncryptType: entry.Key.KeyType,
				CreateTime:  entry.Timestamp,
			})
			keytabMap[name] = item
		}
	}
	// Not slices.SortedFunc: it collects onto nil, marshalling as null not [].
	mk := slices.AppendSeq(make(MultiKeytabInfo, 0, len(keytabMap)), maps.Values(keytabMap))
	slices.SortFunc(mk, func(a, b KeytabInfo) int {
		return strings.Compare(a.PrincipalName, b.PrincipalName)
	})
	return mk
}
