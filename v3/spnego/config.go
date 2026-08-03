package spnego

import (
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/jcmturner/gokrb5/v8/keytab"
)

type contextKey string

// contextKeyOfIdentity is the key used to store the authenticated identity in the Fiber context
const contextKeyOfIdentity contextKey = "middleware.spnego.Identity"

// DefaultSystemKeytabPath is the keytab consulted by NewSystemKeytabLookupFunc
// when the KRB5_KTNAME environment variable is not set.
const DefaultSystemKeytabPath = "/etc/krb5.keytab"

// KeytabLookupFunc is a function type that returns a keytab or an error
// It's used to look up the keytab dynamically when needed
// This design allows for extensibility, enabling keytab retrieval from various sources
// such as databases, remote services, or other custom implementations beyond static files
//
// The middleware calls this function on every authenticated request, so an
// implementation that contacts an external system should cache its result and
// refresh it out of band rather than doing the work inline.
type KeytabLookupFunc func() (*keytab.Keytab, error)

// UnauthorizedHandler is invoked when SPNEGO authentication does not succeed,
// in place of the default challenge response.
//
// The Negotiate challenge headers written by the SPNEGO layer are already set
// on the response when this runs, so a handler that only changes the status
// code or body keeps the negotiation flow intact. Removing the
// WWW-Authenticate header will break Kerberos negotiation with the client.
type UnauthorizedHandler func(ctx fiber.Ctx) error

// Config holds the configuration for the SPNEGO middleware
// It includes the keytab lookup function and a logger
type Config struct {
	// KeytabLookup is a function that retrieves the keytab
	KeytabLookup KeytabLookupFunc
	// Log receives gokrb5's diagnostics. gokrb5 logs once per unauthenticated
	// request and has no level of its own, so leaving this nil keeps an
	// unauthenticated client from driving log volume. Note that the first leg
	// of every Negotiate handshake is unauthenticated.
	Log *log.Logger
	// UseFiberLogger sends gokrb5's diagnostics to Fiber's default logger when
	// Log is nil. It only takes effect when that logger is backed by a
	// *log.Logger, and it bypasses Fiber's log level, so the caveat on Log
	// applies here too.
	UseFiberLogger bool
	// Unauthorized customizes the response sent when authentication fails.
	// When nil, the SPNEGO layer's own 401 challenge response is returned
	// unmodified.
	Unauthorized UnauthorizedHandler
	// FallbackToSystemKeytab loads the host's system keytab when KeytabLookup
	// is nil, instead of rejecting the configuration. See
	// NewSystemKeytabLookupFunc for how the path is resolved.
	FallbackToSystemKeytab bool
}

// fileStamp identifies a keytab file revision cheaply enough to check on every
// request. Size and modification time change whenever a keytab is rotated in
// place or replaced.
type fileStamp struct {
	size    int64
	modTime int64
}

// keytabFileCache holds the merged keytab for a fixed set of files, reloading
// it only when one of those files changes on disk.
type keytabFileCache struct {
	files []string

	mu     sync.Mutex
	stamps []fileStamp
	merged *keytab.Keytab
}

// lastGood returns the previously loaded keytab, if there is one, so that a
// transient read failure does not take down authentication. Rotating a keytab
// is not atomic: a stat can see the new size while the write is still in
// flight, and re-parsing the half-written file fails. The cached keytab is
// still valid in that window, and the stamps are deliberately left untouched so
// the next request retries the load.
func (c *keytabFileCache) lastGood(err error) (*keytab.Keytab, error) {
	if c.merged != nil {
		return c.merged, nil
	}
	return nil, err
}

// load returns the merged keytab, re-reading the files only when their size or
// modification time differs from the cached revision. The returned pointer is
// stable across calls for as long as the files are unchanged, which lets a
// caller reuse anything it derived from the keytab.
func (c *keytabFileCache) load() (*keytab.Keytab, error) {
	stamps := make([]fileStamp, len(c.files))
	for i, keytabFile := range c.files {
		info, err := os.Stat(keytabFile)
		if err != nil {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.lastGood(fmt.Errorf("%w: file %s load failed: %w", ErrLoadKeytabFileFailed, keytabFile, err))
		}
		stamps[i] = fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano()}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.merged != nil && slices.Equal(c.stamps, stamps) {
		return c.merged, nil
	}

	var mergeKeytab keytab.Keytab
	for _, keytabFile := range c.files {
		kt, err := keytab.Load(keytabFile)
		if err != nil {
			return c.lastGood(fmt.Errorf("%w: file %s load failed: %w", ErrLoadKeytabFileFailed, keytabFile, err))
		}
		mergeKeytab.Entries = append(mergeKeytab.Entries, kt.Entries...)
	}
	c.merged = &mergeKeytab
	c.stamps = stamps
	return c.merged, nil
}

// NewKeytabFileLookupFunc creates a new KeytabLookupFunc that loads keytab files
// It accepts one or more keytab file paths and returns a function that loads them
//
// The merged keytab is cached and reused until one of the files changes size or
// modification time, so rotating a keytab on disk still takes effect without
// re-reading and re-parsing every file on every request.
func NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error) {
	if len(keytabFiles) == 0 {
		return nil, ErrConfigInvalidOfAtLeastOneKeytabFileRequired
	}
	cache := &keytabFileCache{files: slices.Clone(keytabFiles)}
	return cache.load, nil
}

// NewSystemKeytabLookupFunc creates a KeytabLookupFunc that loads the host's
// system keytab: the path in the KRB5_KTNAME environment variable when set,
// otherwise DefaultSystemKeytabPath. A "FILE:" prefix, which MIT Kerberos
// allows in KRB5_KTNAME, is accepted and stripped.
//
// Note that this is a keytab, not a credential cache: SPNEGO acceptance
// requires the service's long-term keys, so a client credential cache
// (KRB5CCNAME) cannot be used to accept incoming tickets.
func NewSystemKeytabLookupFunc() (KeytabLookupFunc, error) {
	keytabPath := os.Getenv("KRB5_KTNAME")
	if keytabPath == "" {
		keytabPath = DefaultSystemKeytabPath
	}
	// MIT Kerberos writes KRB5_KTNAME as an optional "TYPE:residual" pair. Only
	// file-backed types can be read here, so anything else is rejected up front
	// rather than being treated as a literal path that fails on every request.
	if resolved, err := resolveKeytabResidual(keytabPath); err != nil {
		return nil, err
	} else if keytabPath = resolved; keytabPath == "" {
		return nil, ErrConfigInvalidOfAtLeastOneKeytabFileRequired
	}
	return NewKeytabFileLookupFunc(keytabPath)
}

// fileKeytabTypes are the MIT keytab residual types backed by a plain file.
var fileKeytabTypes = []string{"FILE", "WRFILE"}

// resolveKeytabResidual strips a supported "TYPE:" prefix from a keytab name
// and rejects the types that are not plain files. A name with no recognised
// prefix is returned unchanged, so absolute paths and Windows drive letters
// still work.
func resolveKeytabResidual(name string) (string, error) {
	prefix, residual, found := strings.Cut(name, ":")
	if !found {
		return name, nil
	}
	// Not a type prefix: an absolute path, or a Windows drive letter such as C:\.
	if strings.ContainsAny(prefix, `/\`) || len(prefix) < 2 {
		return name, nil
	}
	if slices.Contains(fileKeytabTypes, strings.ToUpper(prefix)) {
		return residual, nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnsupportedKeytabResidualType, prefix)
}
