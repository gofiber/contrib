package spnego

import (
	"fmt"
	"log"

	"github.com/jcmturner/gokrb5/v8/keytab"
)

type contextKey string

// contextKeyOfIdentity is the key used to store the authenticated identity in the Fiber context
const contextKeyOfIdentity contextKey = "middleware.spnego.Identity"

// KeytabLookupFunc is a function type that returns a keytab or an error
// It's used to look up the keytab dynamically when needed
// This design allows for extensibility, enabling keytab retrieval from various sources
// such as databases, remote services, or other custom implementations beyond static files
type KeytabLookupFunc func() (*keytab.Keytab, error)

// Config holds the configuration for the SPNEGO middleware
// It includes the keytab lookup function and a logger
type Config struct {
	// KeytabLookup is a function that retrieves the keytab
	KeytabLookup KeytabLookupFunc
	// Log is the logger used for middleware logging
	Log *log.Logger
}

// NewKeytabFileLookupFunc creates a new KeytabLookupFunc that loads keytab files
// It accepts one or more keytab file paths and returns a function that loads them
func NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error) {
	if len(keytabFiles) == 0 {
		return nil, ErrConfigInvalidOfAtLeastOneKeytabFileRequired
	}
	return func() (*keytab.Keytab, error) {
		var mergeKeytab keytab.Keytab
		for _, keytabFile := range keytabFiles {
			kt, err := keytab.Load(keytabFile)
			if err != nil {
				return nil, fmt.Errorf("%w: file %s load failed: %w", ErrLoadKeytabFileFailed, keytabFile, err)
			}
			mergeKeytab.Entries = append(mergeKeytab.Entries, kt.Entries...)
		}
		return &mergeKeytab, nil
	}, nil
}
