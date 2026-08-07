package utils

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jcmturner/gokrb5/v8/keytab"
)

// KeytabInfo represents information about a principal in a Kerberos keytab
// It contains the principal name, realm, and associated encryption type pairs
type KeytabInfo struct {
	// PrincipalName is fully qualified, realm included, as gokrb5 renders it:
	// "HTTP/service.example.com@EXAMPLE.COM". Realm repeats that suffix on its
	// own, so appending it here prints the realm twice.
	PrincipalName string
	Realm         string            // The Kerberos realm (e.g., EXAMPLE.COM)
	Pairs         []EncryptTypePair // List of encryption type pairs for this principal
}

// EncryptTypePair represents an encryption type entry in a Kerberos keytab
// It contains the version, encryption type, and creation timestamp
type EncryptTypePair struct {
	// Version is the key version number. gokrb5 prefers the 32-bit form over
	// the legacy 8-bit one, so this is wider than NewMockKeytab accepts.
	Version     uint32
	EncryptType int32     // The encryption type (e.g., 18 for AES-256-CTS-HMAC-SHA1-96)
	CreateTime  time.Time // The timestamp when this key was created
}

// MultiKeytabInfo is a slice of KeytabInfo structures
// Used to represent multiple principal entries from a keytab
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
	// AppendSeq onto a made slice, not slices.SortedFunc, which collects onto a
	// nil slice and would marshal an empty keytab as null rather than [].
	mk := slices.AppendSeq(make(MultiKeytabInfo, 0, len(keytabMap)), maps.Values(keytabMap))
	slices.SortFunc(mk, func(a, b KeytabInfo) int {
		return strings.Compare(a.PrincipalName, b.PrincipalName)
	})
	return mk
}
