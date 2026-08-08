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
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/service"
)

type contextKey string

// contextKeyOfIdentity is the key used to store the authenticated identity in the Fiber context
const contextKeyOfIdentity contextKey = "middleware.spnego.Identity"

// DefaultSystemKeytabPath is the keytab consulted by NewSystemKeytabLookupFunc
// when the KRB5_KTNAME environment variable is not set.
const DefaultSystemKeytabPath = "/etc/krb5.keytab"

// KeytabLookupFunc returns a keytab. Called on every request the middleware
// handles, unauthenticated ones included, so cache and refresh out of band.
type KeytabLookupFunc func() (*keytab.Keytab, error)

// UnauthorizedHandler replaces SPNEGO's response when a ticket is rejected, but
// not for the challenge legs. Removing WWW-Authenticate breaks negotiation.
type UnauthorizedHandler func(ctx fiber.Ctx) error

// Config holds the configuration for the SPNEGO middleware
// It includes the keytab lookup function and a logger
type Config struct {
	// Next, when it returns true, skips this middleware for that request, for
	// exempting health probes or CORS preflights from authentication.
	Next func(ctx fiber.Ctx) bool
	// KeytabLookup retrieves the keytab. Whatever error it returns is logged and
	// passed to OnError, so it must not carry gokrb5's parse error — that
	// interpolates the buffer, which is the key. See the README for the safe form.
	KeytabLookup KeytabLookupFunc
	// Log receives gokrb5's diagnostics, which carry no level of their own and
	// run to a line per request carrying a Negotiate token. Leaving it nil keeps
	// all of that off the log.
	Log *log.Logger
	// UseFiberLogger sends gokrb5's diagnostics to Fiber's default logger when
	// Log is nil, if that logger is backed by a *log.Logger. Resolved once in
	// New, so register yours first; it bypasses Fiber's log level.
	UseFiberLogger bool
	// Unauthorized customizes the response sent when a ticket is rejected. It
	// does not run for the opening challenge or a continuation token, which
	// clients act on only when they arrive untouched.
	//
	// Optional. Default: nil (SPNEGO's own response is returned unmodified).
	Unauthorized UnauthorizedHandler
	// FallbackToSystemKeytab loads the host's system keytab when KeytabLookup is
	// nil, instead of rejecting the configuration. See NewSystemKeytabLookupFunc.
	FallbackToSystemKeytab bool
	// KeytabPrincipal selects which principal's key decrypts an incoming ticket.
	// gokrb5 otherwise infers it from the ticket, so a keytab merged from several
	// services accepts a ticket for any of them. New rejects an "@REALM" suffix.
	//
	// Optional. Default: "" (inferred from the ticket).
	KeytabPrincipal string
	// MaxClockSkew is how far a ticket's issue time may sit from this host's
	// clock; drift is the usual cause of a service rejecting every ticket. Must
	// not be negative, which New rejects rather than silently ignoring.
	//
	// Optional. Default: 0, which leaves gokrb5's own default of 5 minutes.
	MaxClockSkew time.Duration
	// DisablePACDecoding turns off decoding of the Privilege Attribute Certificate
	// Active Directory embeds in a ticket. Spelled negatively so the zero value
	// keeps gokrb5's default, which populates the SIDs behind Identity.Authorized.
	//
	// Optional. Default: false (PAC decoded when present).
	DisablePACDecoding bool
	// RequireHostAddress rejects a ticket carrying no host addresses. Note the
	// address checked is the connection's peer, which behind a reverse proxy is
	// the proxy rather than the client.
	//
	// Optional. Default: false.
	RequireHostAddress bool
	// SessionManager lets gokrb5 serve later requests from a session instead of
	// revalidating the ticket. That makes the session a credential in its own
	// right, so what is stored must be unguessable and bound to the client.
	//
	// The README documents what the request it is handed carries, which parts of
	// its response are replayed, and why a failed New fails the request while a
	// failed Get degrades silently to full validation.
	//
	// Optional. Default: nil (every request is validated in full).
	SessionManager service.SessionMgr
	// OnSuccess runs once per authenticated request, after the identity is in
	// the Fiber context and before the next handler. For metrics and audit; it
	// cannot change the outcome.
	//
	// Optional. Default: nil.
	OnSuccess func(ctx fiber.Ctx, identity goidentity.Identity)
	// OnError runs when the middleware itself fails, just before the request is
	// answered with 500 — not for authentication outcomes, which are
	// Unauthorized's business.
	//
	// Match on ErrLookupKeytabFailed or ErrSPNEGOHandlerFailed rather than on
	// text, and treat what arrives as internal diagnostics: it names keytab
	// paths, OS errors, and whatever a session manager wrote.
	//
	// Optional. Default: nil.
	OnError func(ctx fiber.Ctx, err error)
}

// ConfigDefault is the zero Config. New reads the fields rather than this, so
// copying it is supported and mutating it is not.
var ConfigDefault = Config{}

// fileStamp identifies a keytab revision cheaply enough to check per request.
// An in-place rewrite at the same length and mtime may not be — rotate by rename.
type fileStamp struct {
	size    int64
	modTime int64
	id      fileID
}

// fileID is an opaque, comparable file identity; zero where unavailable.
type fileID struct {
	a uint64
	b uint64
}

// keytabSnapshot pairs the merged keytab with the revisions it was built from,
// published as a unit so readers never see a half-updated cache.
type keytabSnapshot struct {
	stamps []fileStamp
	merged *keytab.Keytab
}

// defaultKeytabStaleGrace bounds how long an unparsable keytab keeps being
// served: long enough for a mid-rewrite read, short enough to surface a fault.
const defaultKeytabStaleGrace = 30 * time.Second

// errKeytabUnparsable marks a keytab that was read but could not be parsed.
var errKeytabUnparsable = errors.New("keytab did not parse")

// keytabRetryEvery bounds how often a known-bad revision is re-read.
const keytabRetryEvery = time.Second

// degradedState records an episode of an unparsable keytab. Guarded by mu.
type degradedState struct {
	since        time.Time
	stamps       []fileStamp
	cause        error
	lastAttempt  time.Time
	expiryLogged bool
}

// keytabFileCache holds the merged keytab, reloading only when a file changes.
// Episode lines are queued under mu and written after release, so not ordered.
type keytabFileCache struct {
	files      []string
	staleGrace time.Duration
	retryEvery time.Duration
	nowFn      func() time.Time

	// Read without mu, so the cache-hit path does not serialise requests.
	snapshot atomic.Pointer[keytabSnapshot]
	// Mirrors deg.cause != nil. Read-only on the hit path, so the cache line
	// stays clean for the concurrent snapshot readers.
	degraded atomic.Bool

	// Guards reloads, so the file I/O happens once, not once per waiter.
	mu  sync.Mutex
	deg degradedState
	// Episode lines from this locked section, oldest first; drained by emit.
	queue []func()
}

// newKeytabFileCache builds a cache with the production defaults.
func newKeytabFileCache(files []string) *keytabFileCache {
	return &keytabFileCache{
		files:      slices.Clone(files),
		staleGrace: defaultKeytabStaleGrace,
		retryEvery: keytabRetryEvery,
	}
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

// stat collects every file's current revision. A file that cannot be stat'ed is
// an error, not a reason to serve the cache — that is how revocation works.
func (c *keytabFileCache) stat() ([]fileStamp, error) {
	stamps := make([]fileStamp, len(c.files))
	for i, keytabFile := range c.files {
		info, err := os.Stat(keytabFile)
		if err != nil {
			return nil, fmt.Errorf("%w: file %s load failed: %w", ErrLoadKeytabFileFailed, keytabFile, err)
		}
		stamps[i] = fileStamp{
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
			id:      fileRevisionID(info),
		}
	}
	return stamps, nil
}

// statMatches reports whether every file still carries the recorded stamp. Same
// os.Stat count as stat, without the slice the hit path would throw away.
func (c *keytabFileCache) statMatches(want []fileStamp) (bool, error) {
	// No length guard: want comes from a snapshot stat built, and c.files is fixed.
	for i, keytabFile := range c.files {
		info, err := os.Stat(keytabFile)
		if err != nil {
			return false, fmt.Errorf("%w: file %s load failed: %w", ErrLoadKeytabFileFailed, keytabFile, err)
		}
		got := fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano(), id: fileRevisionID(info)}
		if got != want[i] {
			return false, nil
		}
	}
	return true, nil
}

// readAll re-reads and merges every keytab file. A parse failure is wrapped
// with errKeytabUnparsable so the caller can tell it apart from an I/O failure.
func (c *keytabFileCache) readAll() (*keytab.Keytab, error) {
	// keytab.New sets the format version Marshal writes; the zero value does not.
	mergeKeytab := keytab.New()
	for _, keytabFile := range c.files {
		raw, err := os.ReadFile(keytabFile) //nolint:gosec // path comes from the caller's own configuration
		if err != nil {
			return nil, fmt.Errorf("%w: file %s load failed: %w", ErrLoadKeytabFileFailed, keytabFile, err)
		}
		kt := keytab.New()
		if err = kt.Unmarshal(raw); err != nil {
			// gokrb5's message is dropped, not quoted: five branches in its keytab.go
			// (:242, :449, :464, :480, :495 at v8.4.4) interpolate the key bytes.
			return nil, fmt.Errorf("%w: file %s load failed: %w (%d bytes read)",
				ErrLoadKeytabFileFailed, keytabFile, errKeytabUnparsable, len(raw))
		}
		mergeKeytab.Entries = append(mergeKeytab.Entries, kt.Entries...)
	}
	return mergeKeytab, nil
}

// serveStale covers an unparsable keytab with the last one that parsed, since a
// read can catch a half-written file. A lasting fault still surfaces. Holds mu.
func (c *keytabFileCache) serveStale(cause error, now time.Time) (*keytab.Keytab, error) {
	snap := c.snapshot.Load()
	if snap == nil {
		return nil, cause
	}
	if c.deg.since.IsZero() {
		c.deg.since = now
		// Once per episode, so the rate is bounded by rotations, not requests.
		grace := c.grace()
		// Quoted: the cause names a path, and Unix allows a newline in one.
		detail := quoteForLog([]byte(cause.Error()))
		c.announce(func() {
			flog.Warnf("spnego: %s; serving the last keytab that parsed for up to %s", detail, grace)
		})
		return snap.merged, nil
	}
	if now.Sub(c.deg.since) > c.grace() {
		if !c.deg.expiryLogged {
			c.deg.expiryLogged = true
			// The transition an operator most needs, so it gets error level.
			grace := c.grace()
			detail := quoteForLog([]byte(cause.Error()))
			c.announce(func() {
				flog.Errorf("spnego: keytab still unusable after %s, failing requests: %s", grace, detail)
			})
		}
		return nil, cause
	}
	return snap.merged, nil
}

// clearDegraded ends the episode and logs an all-clear at the warning's level,
// but only for an episode that announced one. Callers hold mu.
func (c *keytabFileCache) clearDegraded() {
	if c.deg.cause == nil {
		c.degraded.Store(false)
		return
	}
	if !c.deg.since.IsZero() {
		c.announce(func() { flog.Warnf("spnego: keytab loads cleanly again") })
	}
	c.deg = degradedState{}
	c.degraded.Store(false)
}

// endEpisodeIfCurrent closes an episode that resolved without a reload.
// Re-stats under mu, since the caller's stat may predate a rotation.
func (c *keytabFileCache) endEpisodeIfCurrent(expected []fileStamp) {
	c.mu.Lock()
	defer c.emit()
	if c.deg.cause == nil {
		// Another goroutine closed it; clearDegraded would no-op anyway.
		return
	}
	if fresh, err := c.stat(); err == nil && slices.Equal(fresh, expected) {
		c.clearDegraded()
	}
}

// announce queues an episode line for emit. Queueing rather than replacing
// means a second transition cannot drop the first's line. Callers hold mu.
func (c *keytabFileCache) announce(write func()) {
	c.queue = append(c.queue, write)
}

// emit releases mu and writes what this locked section queued, oldest first.
// Called with mu held, returns with it released.
func (c *keytabFileCache) emit() {
	writes := c.queue
	c.queue = nil
	c.mu.Unlock()
	for _, write := range writes {
		write()
	}
}

// load returns the merged keytab, re-reading only when a file's revision
// changed. Detection is inexact (see fileStamp), so rotate by rename.
func (c *keytabFileCache) load() (*keytab.Keytab, error) {
	// Compared in place: this runs per request, and stat's slice is immediately
	// garbage.
	if snap := c.snapshot.Load(); snap != nil {
		match, err := c.statMatches(snap.stamps)
		if err != nil {
			return nil, err
		}
		if match {
			// Rare: an episode that ended without a reload still needs closing.
			// snap.stamps is what the files were just found to carry.
			if c.degraded.Load() {
				c.endEpisodeIfCurrent(snap.stamps)
			}
			return snap.merged, nil
		}
	}
	c.mu.Lock()
	defer c.emit()
	// Stat under the lock, and only here: stamps taken before it could pair one
	// revision with another's bytes, clobbering a correct snapshot.
	stamps, err := c.stat()
	if err != nil {
		return nil, err
	}
	// Another goroutine may have reloaded while this one waited for the lock.
	if snap := c.snapshot.Load(); snap != nil && slices.Equal(snap.stamps, stamps) {
		// Back to the cached revision, so any episode is over.
		c.clearDegraded()
		return snap.merged, nil
	}

	now := nowOr(c.nowFn)
	// This revision failed recently; do not re-read every file per request while
	// the fault lasts.
	if c.deg.cause != nil && slices.Equal(c.deg.stamps, stamps) && now.Sub(c.deg.lastAttempt) < c.retry() {
		if errors.Is(c.deg.cause, errKeytabUnparsable) {
			return c.serveStale(c.deg.cause, now)
		}
		return nil, c.deg.cause
	}

	merged, err := c.readAll()
	if err != nil {
		// deg.since is left alone: an episode is bounded by its first failure, so
		// repeated bad writes cannot keep superseded keys alive.
		c.deg.stamps, c.deg.cause, c.deg.lastAttempt = stamps, err, now
		c.degraded.Store(true)
		if errors.Is(err, errKeytabUnparsable) {
			return c.serveStale(err, now)
		}
		return nil, err
	}
	c.snapshot.Store(&keytabSnapshot{stamps: stamps, merged: merged})
	c.clearDegraded()
	return merged, nil
}

// NewKeytabFileLookupFunc loads and merges keytab files, caching until one's
// size, mtime or identity changes. At least one path, none empty.
func NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error) {
	// An empty path is what an unset environment variable expands to; refusing
	// it here fails at startup rather than on every request.
	for _, keytabFile := range keytabFiles {
		if keytabFile == "" {
			return nil, ErrConfigInvalidOfAtLeastOneKeytabFileRequired
		}
	}
	if len(keytabFiles) == 0 {
		return nil, ErrConfigInvalidOfAtLeastOneKeytabFileRequired
	}
	return newKeytabFileCache(keytabFiles).load, nil
}

// NewSystemKeytabLookupFunc loads KRB5_KTNAME, else DefaultSystemKeytabPath,
// accepting MIT's "TYPE:" prefix for FILE and WRFILE. Not a credential cache.
func NewSystemKeytabLookupFunc() (KeytabLookupFunc, error) {
	keytabPath := os.Getenv("KRB5_KTNAME")
	if keytabPath == "" {
		keytabPath = DefaultSystemKeytabPath
	}
	resolved, err := resolveKeytabResidual(keytabPath)
	if err != nil {
		return nil, err
	}
	if resolved == "" {
		return nil, ErrConfigInvalidOfAtLeastOneKeytabFileRequired
	}
	return NewKeytabFileLookupFunc(resolved)
}

// resolveKeytabResidual strips a supported "TYPE:" prefix and rejects the MIT
// types that are not plain files. An unknown prefix is part of the path.
func resolveKeytabResidual(name string) (string, error) {
	prefix, residual, found := strings.Cut(name, ":")
	if !found {
		return name, nil
	}
	switch strings.ToUpper(prefix) {
	case "FILE", "WRFILE":
		return residual, nil
	case "KEYRING", "MEMORY", "DIR", "ANY", "SRVTAB":
		return "", fmt.Errorf("%w: %s", ErrUnsupportedKeytabResidualType, prefix)
	default:
		// The colon belongs to the name: a path, a drive letter such as C:\.
		return name, nil
	}
}
