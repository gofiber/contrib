package utils

import (
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/jcmturner/gokrb5/v8/keytab"
)

// ErrInvalidKeyVersion is returned when a requested key version cannot be
// represented in the keytab format gokrb5 writes.
var ErrInvalidKeyVersion = errors.New("invalid key version")

// mockOptions contains configuration parameters for creating a mock keytab
// It allows customization of principal name, realm, password, filename, and encryption type pairs
// used for testing SPNEGO authentication middleware
type mockOptions struct {
	PrincipalName string            // Kerberos principal name
	Realm         string            // Kerberos realm
	Password      string            // Password for generating encryption keys
	Filename      string            // Optional filename to write the mock keytab
	Pairs         []EncryptTypePair // Encryption type pairs to add to the keytab
}

// apply applies the given options to the mockOptions
// This method iterates over all provided options and applies them in sequence
// allowing for flexible configuration of the mock keytab
func (m *mockOptions) apply(opts ...MockOption) {
	for _, opt := range opts {
		opt(m)
	}
}

// WithPrincipal sets the Kerberos principal name for the mock keytab
// Example: WithPrincipal("HTTP/service.example.com")
func WithPrincipal(principalName string) MockOption {
	return func(options *mockOptions) {
		options.PrincipalName = principalName
	}
}

// WithRealm sets the Kerberos realm for the mock keytab
// Example: WithRealm("EXAMPLE.COM")
func WithRealm(realm string) MockOption {
	return func(options *mockOptions) {
		options.Realm = realm
	}
}

// WithFilename specifies the filename to write the mock keytab to
// If provided, the keytab will be written to this file
// Example: WithFilename("test.keytab")
func WithFilename(filename string) MockOption {
	return func(options *mockOptions) {
		options.Filename = filename
	}
}

// WithPairs adds encryption type pairs to the mock keytab
// Each pair specifies an encryption type and associated parameters
// Example: WithPairs(EncryptTypePair{EncryptType: 18, CreateTime: time.Now(), Version: 1})
func WithPairs(pairs ...EncryptTypePair) MockOption {
	return func(options *mockOptions) {
		options.Pairs = append(options.Pairs, pairs...)
	}
}

// WithPassword sets the password used to generate encryption keys
// This password is used with the principal name and realm to create keys
// Example: WithPassword("securePassword123")
func WithPassword(password string) MockOption {
	return func(options *mockOptions) {
		options.Password = password
	}
}

// MockOption defines a function type for configuring mockOptions
// Used to implement the option pattern for flexible configuration
type MockOption func(*mockOptions)

// newDefaultMockOptions creates mockOptions with default values
// Default realm is "TEST.LOCAL" and default password is "abcdef"
// These defaults can be overridden using WithXXX option functions
func newDefaultMockOptions() *mockOptions {
	return &mockOptions{
		Realm:    "TEST.LOCAL",
		Password: "abcdef",
	}
}

type fileOperator interface {
	OpenFile(filename string, flag int, perm os.FileMode) (*os.File, error)
	Remove(filename string) error
}

type myFileOperator struct{}

func (m myFileOperator) OpenFile(filename string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(filename, flag, perm)
}

func (m myFileOperator) Remove(filename string) error {
	return os.Remove(filename)
}

var defaultFileOperator fileOperator = myFileOperator{}

// NewMockKeytab creates a mock keytab for testing purposes
// It allows customization through option functions and returns:
//   - A keytab.Keytab instance populated with the specified entries
//   - A cleanup function that removes any created files
//   - An error if the keytab creation fails
//
// Example usage:
//
//	kt, cleanup, err := NewMockKeytab(
//	  WithPrincipal("HTTP/service.example.com"),
//	  WithRealm("EXAMPLE.COM"),
//	  WithPassword("secret"),
//	  WithFilename("test.keytab"),
//	  WithPairs(EncryptTypePair{EncryptType: 18})
//	)
//	if err != nil {
//	  // handle error; cleanup is nil on failure, so do not defer it above
//	}
//	defer cleanup()
func NewMockKeytab(opts ...MockOption) (*keytab.Keytab, func(), error) {
	opt := newDefaultMockOptions()
	opt.apply(opts...)
	kt := keytab.New()
	var err error
	for _, pair := range opt.Pairs {
		// gokrb5 only accepts an 8-bit key version when building a keytab, so a
		// wider value cannot be represented here. Reject it rather than
		// silently writing a truncated version.
		if pair.Version > math.MaxUint8 {
			return nil, nil, fmt.Errorf("%w: key version %d exceeds the maximum of %d", ErrInvalidKeyVersion, pair.Version, math.MaxUint8)
		}
		if err = kt.AddEntry(opt.PrincipalName, opt.Realm, opt.Password, pair.CreateTime, uint8(pair.Version), pair.EncryptType); err != nil {
			return nil, nil, fmt.Errorf("error adding entry: %w", err)
		}
	}
	var clean = func() {}
	if len(opt.Filename) > 0 {
		// Keytabs hold key material, so keep them readable only by the owner.
		file, err := defaultFileOperator.OpenFile(opt.Filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("error opening file: %w", err)
		}
		clean = func() {
			_ = defaultFileOperator.Remove(opt.Filename)
		}
		if _, writeErr := kt.Write(file); writeErr != nil {
			// Keep the write error in its own variable: reusing err would lose it
			// whenever the subsequent Close succeeds.
			if closeErr := file.Close(); closeErr != nil {
				clean()
				return nil, nil, fmt.Errorf("error writing to file: %w (also failed to close file: %w)", writeErr, closeErr)
			}
			clean()
			return nil, nil, fmt.Errorf("error writing to file: %w", writeErr)
		}
		if err = file.Close(); err != nil {
			clean()
			return nil, nil, fmt.Errorf("error closing file: %w", err)
		}
		return kt, clean, nil
	}
	return kt, clean, nil
}
