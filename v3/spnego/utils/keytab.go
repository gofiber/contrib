package utils

import (
	"time"

	"maps"
	"sort"

	"github.com/jcmturner/gokrb5/v8/keytab"
)

// KeytabInfo represents information about a principal in a Kerberos keytab
// It contains the principal name, realm, and associated encryption type pairs
type KeytabInfo struct {
	PrincipalName string            // The Kerberos principal name (e.g., HTTP/service.example.com)
	Realm         string            // The Kerberos realm (e.g., EXAMPLE.COM)
	Pairs         []EncryptTypePair // List of encryption type pairs for this principal
}

// EncryptTypePair represents an encryption type entry in a Kerberos keytab
// It contains the version, encryption type, and creation timestamp
type EncryptTypePair struct {
	Version     uint8     // The key version number
	EncryptType int32     // The encryption type (e.g., 18 for AES-256-CTS-HMAC-SHA1-96)
	CreateTime  time.Time // The timestamp when this key was created
}

// MultiKeytabInfo is a slice of KeytabInfo structures
// Used to represent multiple principal entries from a keytab
type MultiKeytabInfo []KeytabInfo

// GetKeytabInfo extracts information from a Kerberos keytab and returns it in a structured format
// It organizes keytab entries by principal name and sorts them alphabetically
//
// Parameters:
//
//	kt - A pointer to a keytab.Keytab instance (can be nil)
//
// Returns:
//
//	MultiKeytabInfo - A sorted slice of KeytabInfo structures containing principal information
//
// Example usage:
//
//	kt, _ := keytab.Load("/path/to/keytab")
//	info := GetKeytabInfo(kt)
//	for _, principal := range info {
//	  fmt.Printf("Principal: %s@%s\n", principal.PrincipalName, principal.Realm)
//	  for _, pair := range principal.Pairs {
//	    fmt.Printf("  EncryptType: %d, Version: %d, Created: %v\n", pair.EncryptType, pair.Version, pair.CreateTime)
//	  }
//	}
func GetKeytabInfo(kt *keytab.Keytab) MultiKeytabInfo {
	keytabMap := make(map[string]KeytabInfo)
	if kt != nil {
		for _, entry := range kt.Entries {
			item, ok := keytabMap[entry.Principal.String()]
			if !ok {
				item = KeytabInfo{
					PrincipalName: entry.Principal.String(),
					Realm:         entry.Principal.Realm,
				}
			}
			item.Pairs = append(item.Pairs, EncryptTypePair{
				Version:     entry.KVNO8,
				EncryptType: entry.Key.KeyType,
				CreateTime:  entry.Timestamp,
			})
			keytabMap[entry.Principal.String()] = item
		}
	}
	var mk = make(MultiKeytabInfo, 0, len(keytabMap))
	for item := range maps.Values(keytabMap) {
		mk = append(mk, item)
	}
	sort.Slice(mk, func(i, j int) bool {
		return mk[i].PrincipalName < mk[j].PrincipalName
	})
	return mk
}
