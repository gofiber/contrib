package utils

import (
	"errors"
	"fmt"
	"io"
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

// fileOperator is the seam the tests substitute to drive the failure paths of
// writing a keytab out. It yields an io.WriteCloser because a concrete *os.File
// cannot fail its Close independently of its Write.
type fileOperator interface {
	OpenFile(filename string, flag int, perm os.FileMode) (io.WriteCloser, error)
	Remove(filename string) error
}

type myFileOperator struct{}

func (m myFileOperator) OpenFile(filename string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	file, err := os.OpenFile(filename, flag, perm) //nolint:gosec // path comes from the caller's own options
	if err != nil {
		// Returned explicitly: a nil *os.File in a non-nil interface would not
		// compare equal to nil at the call site.
		return nil, err //nolint:wrapcheck // the caller wraps this with its own context
	}
	return file, nil
}

func (m myFileOperator) Remove(filename string) error {
	return os.Remove(filename) //nolint:wrapcheck // the caller does not inspect this
}

var defaultFileOperator fileOperator = myFileOperator{}

// NewMockKeytab builds a keytab for testing, optionally writing it to the file
// named by WithFilename, and returns a cleanup function that removes it. The
// cleanup is nil on error, so do not defer it before checking.
func NewMockKeytab(opts ...MockOption) (*keytab.Keytab, func(), error) {
	opt := newDefaultMockOptions()
	opt.apply(opts...)
	kt := keytab.New()
	var err error
	for _, pair := range opt.Pairs {
		// gokrb5 accepts only an 8-bit key version here, so reject a wider one
		// rather than silently writing a truncated version.
		if pair.Version > math.MaxUint8 {
			return nil, nil, fmt.Errorf("%w: key version %d exceeds the maximum of %d", ErrInvalidKeyVersion, pair.Version, math.MaxUint8)
		}
		if err = kt.AddEntry(opt.PrincipalName, opt.Realm, opt.Password, pair.CreateTime, uint8(pair.Version), pair.EncryptType); err != nil {
			return nil, nil, fmt.Errorf("error adding entry: %w", err)
		}
	}
	if opt.Filename == "" {
		return kt, func() {}, nil
	}
	// Removed then created exclusively, not truncated: OpenFile applies its
	// permission argument only on create, so writing a keytab into a path left
	// at 0644 would hand the key out. O_EXCL leaves no wrong-mode window.
	if err = defaultFileOperator.Remove(opt.Filename); err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("error removing existing file: %w", err)
	}
	file, err := defaultFileOperator.OpenFile(opt.Filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("error opening file: %w", err)
	}
	clean := func() { _ = defaultFileOperator.Remove(opt.Filename) }
	if err = writeAndClose(kt, file); err != nil {
		clean()
		return nil, nil, err
	}
	return kt, clean, nil
}

// writeAndClose writes the keytab out and closes the file, reporting whichever
// of the two failed. Close runs even when the write failed, and its error is
// kept separately so a successful close cannot swallow the write's.
func writeAndClose(kt *keytab.Keytab, file io.WriteCloser) error {
	_, writeErr := kt.Write(file)
	closeErr := file.Close()
	switch {
	case writeErr != nil && closeErr != nil:
		return fmt.Errorf("error writing to file: %w (also failed to close file: %w)", writeErr, closeErr)
	case writeErr != nil:
		return fmt.Errorf("error writing to file: %w", writeErr)
	case closeErr != nil:
		return fmt.Errorf("error closing file: %w", closeErr)
	}
	return nil
}
