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

// KeytabLookupFunc returns a keytab, so keys can come from a file, a secrets
// manager or a database. It is called on every request the middleware handles,
// unauthenticated ones included, so cache the result and refresh it out of band.
type KeytabLookupFunc func() (*keytab.Keytab, error)

// UnauthorizedHandler replaces SPNEGO's own response when a ticket is rejected.
// It is not called for the opening challenge or a continuation token. SPNEGO's
// headers are already set; removing WWW-Authenticate will break negotiation.
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

// ConfigDefault records what an unset field does. It is deliberately equal to
// the zero Config, and New reads the fields rather than this — so copying it is
// supported and mutating it is not.
var ConfigDefault = Config{
	Next:                   nil,
	KeytabLookup:           nil,
	Log:                    nil,
	UseFiberLogger:         false,
	Unauthorized:           nil,
	FallbackToSystemKeytab: false,
	KeytabPrincipal:        "",
	// Zero rather than 5 minutes: gokrb5 substitutes its own default, and naming
	// a number here would silently pin it if gokrb5 ever changed.
	MaxClockSkew:       0,
	DisablePACDecoding: false,
	RequireHostAddress: false,
	SessionManager:     nil,
	OnSuccess:          nil,
	OnError:            nil,
}

// fileStamp identifies a keytab revision cheaply enough to check per request.
// Identity is recorded where the platform exposes it, so a rename is caught even
// when size and mtime were preserved. An in-place rewrite of the same length may
// not be — rotate by rename.
type fileStamp struct {
	size    int64
	modTime int64
	id      fileID
}

// fileID is an opaque, comparable file identity, platform-specific and zero
// where the platform exposes nothing. See the fileRevisionID implementations.
type fileID struct {
	a uint64
	b uint64
}

// keytabSnapshot is an immutable view of the merged keytab and the file
// revisions it was built from, published as a unit so readers never see a
// half-updated cache.
type keytabSnapshot struct {
	stamps []fileStamp
	merged *keytab.Keytab
}

// defaultKeytabStaleGrace bounds how long a keytab that no longer parses keeps
// being served: long enough for a file caught mid-rewrite, short enough that a
// rotation to a permanently corrupt keytab surfaces.
const defaultKeytabStaleGrace = 30 * time.Second

// errKeytabUnparsable marks a keytab that was read but could not be parsed.
var errKeytabUnparsable = errors.New("keytab did not parse")

// keytabRetryEvery bounds how often a revision already known to be unparsable
// is re-read, so an outage does not re-read every file on every request.
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
// only when one changes on disk.
//
// Episode lines are queued by announce under mu and written by emit once it is
// released, so ordering between goroutines is not exact. A shared flush lock
// fixed that and was removed: it stalled every request behind one slow sink.
type keytabFileCache struct {
	files      []string
	staleGrace time.Duration
	retryEvery time.Duration
	nowFn      func() time.Time

	// Read without holding mu so the cache-hit path does not serialise requests.
	snapshot atomic.Pointer[keytabSnapshot]
	// degraded mirrors whether deg is populated. The hit path reads it rather
	// than writing, leaving this cache line clean for the snapshot readers.
	degraded atomic.Bool

	// mu guards reloads so the file I/O is done once, not once per waiter.
	mu  sync.Mutex
	deg degradedState
	// queue holds this locked section's episode lines, oldest first. Guarded by
	// mu; taken and cleared by emit.
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
		stamps[i] = fileStamp{
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
			id:      fileRevisionID(info),
		}
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
			// gokrb5's message is dropped, not quoted: five branches in its
			// keytab.go (:242, :449, :464, :480, :495 at v8.4.4) interpolate the
			// bytes they were handed, which is the key. Re-check all five on upgrade.
			return nil, fmt.Errorf("%w: file %s load failed: %w (%d bytes read)",
				ErrLoadKeytabFileFailed, keytabFile, errKeytabUnparsable, len(raw))
		}
		mergeKeytab.Entries = append(mergeKeytab.Entries, kt.Entries...)
	}
	return mergeKeytab, nil
}

// serveStale decides whether a keytab that no longer parses is covered for by
// the last one that did: rewriting in place is not atomic, so a read can catch
// a half-written file, but a lasting fault must surface. Callers hold mu.
func (c *keytabFileCache) serveStale(cause error, now time.Time) (*keytab.Keytab, error) {
	snap := c.snapshot.Load()
	if snap == nil {
		return nil, cause
	}
	if c.deg.since.IsZero() {
		c.deg.since = now
		// Once per episode, which begins only when the files change — so the
		// rate is bounded by rotations, not by request volume.
		grace := c.grace()
		// Quoted because the cause names the keytab file, and Unix allows a
		// newline in a path, which would otherwise forge a log line.
		detail := quoteForLog([]byte(cause.Error()))
		c.announce(func() {
			flog.Warnf("spnego: %s; serving the last keytab that parsed for up to %s", detail, grace)
		})
		return snap.merged, nil
	}
	if now.Sub(c.deg.since) > c.grace() {
		if !c.deg.expiryLogged {
			c.deg.expiryLogged = true
			// Degraded to failing is the transition an operator most needs to
			// see, so it gets its own line at error level.
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

// clearDegraded ends the current episode and logs the recovery, so an operator
// alerted by the warning gets a matching all-clear — at the same level, and
// only for an episode that was announced. Callers hold mu.
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

// endEpisodeIfCurrent closes an episode that resolved without a reload, which
// happens when a keytab is restored with its original size and mtime. The
// caller's stat may predate a rotation, so the files are re-stat'ed under mu.
func (c *keytabFileCache) endEpisodeIfCurrent(expected []fileStamp) {
	c.mu.Lock()
	defer c.emit()
	if c.deg.cause == nil {
		// Another goroutine closed the episode. Purely an early-out:
		// clearDegraded would find nothing to do either.
		return
	}
	if fresh, err := c.stat(); err == nil && slices.Equal(fresh, expected) {
		c.clearDegraded()
	}
}

// announce queues an episode line to be written once mu is released. Queueing
// rather than replacing means a second transition cannot drop the first's line.
// Callers must hold mu.
func (c *keytabFileCache) announce(write func()) {
	c.queue = append(c.queue, write)
}

// emit releases mu and writes whatever this locked section queued, oldest
// first. Called with mu held, returns with it released. Clearing the queue
// means a goroutine only ever writes its own lines.
func (c *keytabFileCache) emit() {
	writes := c.queue
	c.queue = nil
	c.mu.Unlock()
	for _, write := range writes {
		write()
	}
}

// load returns the merged keytab, re-reading only when the files' revision
// differs from the cached one. The pointer is stable while they are unchanged.
// Detection is inexact (see fileStamp), so rotate by rename.
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
	defer c.emit()
	// Re-stat under the lock: the caller's stamps predate it, so pairing them
	// with content read now could record one revision as holding another's
	// bytes, clobbering a correct snapshot.
	stamps, err = c.stat()
	if err != nil {
		return nil, err
	}
	// Another goroutine may have reloaded while this one waited for the lock.
	if snap := c.snapshot.Load(); snap != nil && slices.Equal(snap.stamps, stamps) {
		// The revision on disk is the cached one again, so any episode is over.
		// No re-stat needed: these stamps were taken under the lock.
		c.clearDegraded()
		return snap.merged, nil
	}

	now := nowOr(c.nowFn)
	// This revision already failed recently, so do not re-read every file on
	// every request while the fault lasts. The mutex still serialises them,
	// which is not worth a second copy of this state to avoid.
	if c.deg.cause != nil && slices.Equal(c.deg.stamps, stamps) && now.Sub(c.deg.lastAttempt) < c.retry() {
		if errors.Is(c.deg.cause, errKeytabUnparsable) {
			return c.serveStale(c.deg.cause, now)
		}
		return nil, c.deg.cause
	}

	merged, err := c.readAll()
	if err != nil {
		// Recorded either way, so the retry throttle covers unreadable files
		// too. deg.since is left alone: an episode is bounded by its first
		// failure, so repeated bad writes cannot keep superseded keys alive.
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

// NewKeytabFileLookupFunc returns a KeytabLookupFunc that loads and merges one
// or more keytab files, caching the result until a file's size, modification
// time or identity changes. At least one path is required, and none may be empty.
func NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error) {
	// An empty path is what an unset environment variable expands to. Refusing
	// it here turns a configuration mistake into a startup error rather than a
	// 500 on every request.
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

// NewSystemKeytabLookupFunc loads the host's system keytab: KRB5_KTNAME when
// set, otherwise DefaultSystemKeytabPath. MIT's optional "TYPE:residual" prefix
// is accepted for FILE and WRFILE and rejected for the types that are not files.
//
// This is a keytab, not a credential cache: accepting SPNEGO needs the
// service's long-term keys, so KRB5CCNAME cannot serve this role.
func NewSystemKeytabLookupFunc() (KeytabLookupFunc, error) {
	keytabPath := os.Getenv("KRB5_KTNAME")
	if keytabPath == "" {
		keytabPath = DefaultSystemKeytabPath
	}
	// See resolveKeytabResidual for how the optional "TYPE:" prefix is handled.
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

// nonFileKeytabTypes are the MIT keytab residual types that cannot be read as a
// file. Anything outside both lists is a filename, not a type.
var nonFileKeytabTypes = []string{"KEYRING", "MEMORY", "DIR", "ANY", "SRVTAB"}

// resolveKeytabResidual strips a supported "TYPE:" prefix and rejects the known
// types that are not plain files. An unrecognised prefix is treated as part of
// the path, so absolute paths, drive letters and relative names all still work.
func resolveKeytabResidual(name string) (string, error) {
	prefix, residual, found := strings.Cut(name, ":")
	if !found {
		return name, nil
	}
	switch upper := strings.ToUpper(prefix); {
	case slices.Contains(fileKeytabTypes, upper):
		return residual, nil
	case slices.Contains(nonFileKeytabTypes, upper):
		return "", fmt.Errorf("%w: %s", ErrUnsupportedKeytabResidualType, prefix)
	default:
		// The colon belongs to the name: an absolute path, a Windows drive
		// letter such as C:\, or a relative name that simply contains one.
		return name, nil
	}
}
