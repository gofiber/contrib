package utils

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/jcmturner/gokrb5/v8/keytab"
)

// ErrInvalidKeyVersion is returned for a key version gokrb5 cannot write.
var ErrInvalidKeyVersion = errors.New("invalid key version")

// mockOptions configures NewMockKeytab.
type mockOptions struct {
	PrincipalName string            // Kerberos principal name
	Realm         string            // Kerberos realm
	Password      string            // Password for generating encryption keys
	Filename      string            // Optional filename to write the mock keytab
	Pairs         []EncryptTypePair // Encryption type pairs to add to the keytab
}

// apply runs the options in order.
func (m *mockOptions) apply(opts ...MockOption) {
	for _, opt := range opts {
		opt(m)
	}
}

// WithPrincipal sets the principal, e.g. "HTTP/service.example.com".
func WithPrincipal(principalName string) MockOption {
	return func(options *mockOptions) {
		options.PrincipalName = principalName
	}
}

// WithRealm sets the Kerberos realm, e.g. "EXAMPLE.COM".
func WithRealm(realm string) MockOption {
	return func(options *mockOptions) {
		options.Realm = realm
	}
}

// WithFilename writes the keytab to this path.
func WithFilename(filename string) MockOption {
	return func(options *mockOptions) {
		options.Filename = filename
	}
}

// WithPairs adds encryption-type entries to the keytab.
func WithPairs(pairs ...EncryptTypePair) MockOption {
	return func(options *mockOptions) {
		options.Pairs = append(options.Pairs, pairs...)
	}
}

// WithPassword sets the password the keys are derived from.
func WithPassword(password string) MockOption {
	return func(options *mockOptions) {
		options.Password = password
	}
}

// MockOption configures a mock keytab.
type MockOption func(*mockOptions)

// newDefaultMockOptions returns the TEST.LOCAL / "abcdef" defaults.
func newDefaultMockOptions() *mockOptions {
	return &mockOptions{
		Realm:    "TEST.LOCAL",
		Password: "abcdef",
	}
}

// fileOperator is the seam tests substitute to drive the write failure paths.
// An io.WriteCloser, since *os.File cannot fail Close independently of Write.
type fileOperator interface {
	OpenFile(filename string, flag int, perm os.FileMode) (io.WriteCloser, error)
	Remove(filename string) error
}

type myFileOperator struct{}

func (m myFileOperator) OpenFile(filename string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	file, err := os.OpenFile(filename, flag, perm) //nolint:gosec // path comes from the caller's own options
	if err != nil {
		// Explicit: a nil *os.File in a non-nil interface is not nil.
		return nil, err //nolint:wrapcheck // the caller wraps this with its own context
	}
	return file, nil
}

func (m myFileOperator) Remove(filename string) error {
	return os.Remove(filename) //nolint:wrapcheck // the caller does not inspect this
}

var defaultFileOperator fileOperator = myFileOperator{}

// NewMockKeytab builds a test keytab, optionally writing it to WithFilename,
// and returns a cleanup that removes it. Cleanup is nil on error.
func NewMockKeytab(opts ...MockOption) (*keytab.Keytab, func(), error) {
	opt := newDefaultMockOptions()
	opt.apply(opts...)
	kt := keytab.New()
	var err error
	for _, pair := range opt.Pairs {
		// gokrb5 writes an 8-bit version here; reject rather than truncate.
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
	// Removed then created O_EXCL, not truncated: OpenFile applies perm only on
	// create, so an existing 0644 path would keep that mode and leak the key.
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

// writeAndClose writes and closes, reporting whichever failed. Close runs even
// after a failed write, and its error is kept separately.
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
