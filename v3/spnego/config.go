package spnego

import (
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
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

// UnauthorizedHandler is invoked in place of SPNEGO's own response when a
// presented Kerberos ticket is rejected. It is not called for the opening
// challenge or for a continuation token; see Config.Unauthorized.
//
// The headers SPNEGO wrote are already on the response when this runs, so a
// handler that only changes the status code or body leaves them in place.
// Removing the WWW-Authenticate header will break negotiation with the client.
type UnauthorizedHandler func(ctx fiber.Ctx) error

// Config holds the configuration for the SPNEGO middleware
// It includes the keytab lookup function and a logger
type Config struct {
	// Next, when it returns true, skips this middleware for that request and
	// passes it straight down the chain. Useful for exempting health probes or
	// CORS preflights from authentication.
	Next func(ctx fiber.Ctx) bool
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
	// Unauthorized customizes the response sent when a client's Kerberos
	// ticket is rejected. It does not run for the opening challenge or for a
	// continuation token, which are legs of a handshake that can still succeed
	// and which clients act on only when they arrive untouched. When nil,
	// SPNEGO's own response is returned unmodified.
	Unauthorized UnauthorizedHandler
	// FallbackToSystemKeytab loads the host's system keytab when KeytabLookup
	// is nil, instead of rejecting the configuration. See
	// NewSystemKeytabLookupFunc for how the path is resolved.
	FallbackToSystemKeytab bool
}

// fileStamp identifies a keytab file revision cheaply enough to check on every
// request. It is not exact: a rewrite that keeps the file the same size and
// lands within one filesystem timestamp tick looks unchanged.
type fileStamp struct {
	size    int64
	modTime int64
}

// keytabSnapshot is an immutable view of the merged keytab and the file
// revisions it was built from, published as a unit so readers never see a
// half-updated cache.
type keytabSnapshot struct {
	stamps []fileStamp
	merged *keytab.Keytab
}

// defaultKeytabStaleGrace bounds how long a keytab that no longer parses keeps
// being served. Long enough to cover a file caught mid-rewrite, short enough
// that a rotation to a permanently corrupt keytab surfaces quickly.
const defaultKeytabStaleGrace = 30 * time.Second

// errKeytabUnparsable marks a keytab that was read but could not be parsed, as
// opposed to one that could not be read at all.
var errKeytabUnparsable = errors.New("keytab did not parse")

// keytabRetryEvery bounds how often a keytab revision already known to be
// unparsable is re-read. Without it every request during an outage re-reads and
// re-parses every keytab file, serialised behind the reload mutex.
const keytabRetryEvery = time.Second

// degradedState records an episode in which the keytab on disk no longer
// parses. All of it is guarded by keytabFileCache.mu.
type degradedState struct {
	since        time.Time
	stamps       []fileStamp
	cause        error
	lastAttempt  time.Time
	expiryLogged bool
}

// keytabFileCache holds the merged keytab for a fixed set of files, reloading
// it only when one of those files changes on disk.
type keytabFileCache struct {
	files      []string
	staleGrace time.Duration
	retryEvery time.Duration
	nowFn      func() time.Time

	// snapshot is read without holding mu so the cache-hit path, which runs on
	// every authenticated request, does not serialise concurrent requests.
	snapshot atomic.Pointer[keytabSnapshot]
	// degraded mirrors whether deg is populated. The hit path reads it rather
	// than writing anything, so the common healthy case leaves this cache line
	// clean for the concurrent snapshot readers sharing it.
	degraded atomic.Bool

	// mu guards reloads so the file I/O is done once, not once per waiter.
	mu  sync.Mutex
	deg degradedState
}

// newKeytabFileCache builds a cache with the production defaults.
func newKeytabFileCache(files []string) *keytabFileCache {
	return &keytabFileCache{
		files:      slices.Clone(files),
		staleGrace: defaultKeytabStaleGrace,
		retryEvery: keytabRetryEvery,
	}
}

// now reports the current time, defaulting to the wall clock so a zero-value
// cache is usable. Tests substitute nowFn to drive the grace window.
func (c *keytabFileCache) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

// grace reports how long an unparsable keytab may be covered for.
func (c *keytabFileCache) grace() time.Duration {
	if c.staleGrace <= 0 {
		return defaultKeytabStaleGrace
	}
	return c.staleGrace
}

// retry reports how often an already-failing revision is re-read.
func (c *keytabFileCache) retry() time.Duration {
	if c.retryEvery <= 0 {
		return keytabRetryEvery
	}
	return c.retryEvery
}

// stat collects the current revision of every file. A file that cannot be
// stat'ed is an error rather than a reason to serve the cache: that is how a
// deleted or revoked keytab has to behave.
func (c *keytabFileCache) stat() ([]fileStamp, error) {
	stamps := make([]fileStamp, len(c.files))
	for i, keytabFile := range c.files {
		info, err := os.Stat(keytabFile)
		if err != nil {
			return nil, fmt.Errorf("%w: file %s load failed: %w", ErrLoadKeytabFileFailed, keytabFile, err)
		}
		stamps[i] = fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano()}
	}
	return stamps, nil
}

// readAll re-reads and merges every keytab file. A parse failure is wrapped
// with errKeytabUnparsable so the caller can tell it apart from an I/O failure.
func (c *keytabFileCache) readAll() (*keytab.Keytab, error) {
	// keytab.New rather than the zero value: it sets the format version that
	// Marshal writes, so the merged keytab stays serialisable.
	mergeKeytab := keytab.New()
	for _, keytabFile := range c.files {
		raw, err := os.ReadFile(keytabFile) //nolint:gosec // path comes from the caller's own configuration
		if err != nil {
			return nil, fmt.Errorf("%w: file %s load failed: %w", ErrLoadKeytabFileFailed, keytabFile, err)
		}
		kt := keytab.New()
		if err = kt.Unmarshal(raw); err != nil {
			return nil, fmt.Errorf("%w: file %s load failed: %w: %w", ErrLoadKeytabFileFailed, keytabFile, errKeytabUnparsable, err)
		}
		mergeKeytab.Entries = append(mergeKeytab.Entries, kt.Entries...)
	}
	return mergeKeytab, nil
}

// serveStale decides whether a keytab that no longer parses should be covered
// for by the last one that did. Rewriting a keytab in place is not atomic, so a
// read can catch a half-written file; that window is short. A keytab that stays
// unparsable is a real fault, so the fallback expires after the grace window
// and the error surfaces. Callers must hold mu.
func (c *keytabFileCache) serveStale(cause error, now time.Time) (*keytab.Keytab, error) {
	snap := c.snapshot.Load()
	if snap == nil {
		return nil, cause
	}
	if c.deg.since.IsZero() {
		c.deg.since = now
		// Logged once per episode rather than per request, so a persistent
		// fault cannot be turned into a log flood by unauthenticated callers.
		flog.Warnf("spnego: %v; serving the last keytab that parsed for up to %s", cause, c.grace())
		return snap.merged, nil
	}
	if now.Sub(c.deg.since) > c.grace() {
		if !c.deg.expiryLogged {
			c.deg.expiryLogged = true
			// The transition from degraded to failing is the one an operator
			// most needs to see, so it gets its own line at error level.
			flog.Errorf("spnego: keytab still unusable after %s, failing requests: %v", c.grace(), cause)
		}
		return nil, cause
	}
	return snap.merged, nil
}

// clearDegraded ends the current episode and logs the recovery, so an operator
// alerted by the warning gets a matching all-clear. Callers must hold mu.
func (c *keytabFileCache) clearDegraded() {
	if c.deg.cause == nil {
		c.degraded.Store(false)
		return
	}
	flog.Infof("spnego: keytab loads cleanly again")
	c.deg = degradedState{}
	c.degraded.Store(false)
}

// endEpisodeIfCurrent closes a degraded episode that resolved without a reload,
// which happens when a keytab is restored with its original size and mtime. The
// caller's stat may predate a rotation another goroutine has already found
// broken, so the files are re-stat'ed under the lock before anything is
// cleared.
func (c *keytabFileCache) endEpisodeIfCurrent(expected []fileStamp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deg.cause == nil {
		return
	}
	fresh, err := c.stat()
	if err != nil || !slices.Equal(fresh, expected) {
		return
	}
	c.clearDegraded()
}

// load returns the merged keytab, re-reading the files only when their size or
// modification time differs from the cached revision. The returned pointer is
// stable across calls for as long as the files are unchanged, which lets a
// caller reuse anything it derived from the keytab.
//
// Because change detection is inexact (see fileStamp), rotate a keytab by
// writing a new file and renaming it over the old one: that normally moves the
// modification time, whereas an in-place rewrite of identical length may not.
func (c *keytabFileCache) load() (*keytab.Keytab, error) {
	stamps, err := c.stat()
	if err != nil {
		return nil, err
	}
	if snap := c.snapshot.Load(); snap != nil && slices.Equal(snap.stamps, stamps) {
		// Rare, and deliberately off the common path: an episode that ended
		// without a reload still has to be closed out.
		if c.degraded.Load() {
			c.endEpisodeIfCurrent(stamps)
		}
		return snap.merged, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Another goroutine may have reloaded while this one waited for the lock.
	if snap := c.snapshot.Load(); snap != nil && slices.Equal(snap.stamps, stamps) {
		return snap.merged, nil
	}

	now := c.now()
	// This exact revision already failed to parse recently; do not re-read every
	// file again on every request while the fault persists.
	if c.deg.cause != nil && slices.Equal(c.deg.stamps, stamps) && now.Sub(c.deg.lastAttempt) < c.retry() {
		return c.serveStale(c.deg.cause, now)
	}

	merged, err := c.readAll()
	if err != nil {
		if errors.Is(err, errKeytabUnparsable) {
			// A revision that has not failed before begins its own episode, so
			// a later fault gets a full grace window rather than inheriting an
			// expired one.
			if !slices.Equal(c.deg.stamps, stamps) {
				c.deg = degradedState{}
			}
			c.deg.stamps, c.deg.cause, c.deg.lastAttempt = stamps, err, now
			c.degraded.Store(true)
			return c.serveStale(err, now)
		}
		return nil, err
	}
	c.snapshot.Store(&keytabSnapshot{stamps: stamps, merged: merged})
	c.clearDegraded()
	return merged, nil
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
	return newKeytabFileCache(keytabFiles).load, nil
}

// NewSystemKeytabLookupFunc creates a KeytabLookupFunc that loads the host's
// system keytab: the path in the KRB5_KTNAME environment variable when set,
// otherwise DefaultSystemKeytabPath.
//
// MIT Kerberos writes KRB5_KTNAME as an optional "TYPE:residual" pair. The
// file-backed types, FILE and WRFILE, are accepted and stripped. Any other
// type — KEYRING, MEMORY, ANY and the like — cannot be read as a file and is
// rejected with ErrUnsupportedKeytabResidualType rather than being treated as
// a literal path that would fail on every request.
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
	resolved, err := resolveKeytabResidual(keytabPath)
	if err != nil {
		return nil, err
	}
	if resolved == "" {
		return nil, ErrConfigInvalidOfAtLeastOneKeytabFileRequired
	}
	return NewKeytabFileLookupFunc(resolved)
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
