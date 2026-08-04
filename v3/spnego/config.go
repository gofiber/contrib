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

// KeytabLookupFunc is a function type that returns a keytab or an error
// It's used to look up the keytab dynamically when needed
// This design allows for extensibility, enabling keytab retrieval from various sources
// such as databases, remote services, or other custom implementations beyond static files
//
// The middleware calls this function on every request it does not skip, which
// includes wholly unauthenticated ones: the lookup runs before the ticket is
// examined, so a client with no credentials at all still drives one call per
// request. An implementation that contacts an external system should therefore
// cache its result and refresh it out of band rather than doing the work
// inline, and size any rate limit for unauthenticated traffic.
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
	// Log receives gokrb5's diagnostics, which carry no level of their own.
	// gokrb5 writes a line for every request carrying a Negotiate token — one
	// on success, one on a refusal, one on a token it cannot parse — so on a
	// busy service this is a line per request, not a rare event. It is silent
	// only for requests it declines to negotiate at all: no Authorization
	// header, or one for a different scheme.
	//
	// One case escapes that rule. gokrb5 parses the client address before it
	// looks at the token, and logs when it cannot, so a listener whose remote
	// addresses are not host:port — a Unix socket, say — produces a line on
	// every request including the opening challenge.
	//
	// Leaving this nil keeps all of that off the log.
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
	// KeytabPrincipal selects which principal's key decrypts an incoming
	// ticket, out of a keytab that holds several. gokrb5 otherwise infers it
	// from the ticket itself, which means a ticket for *any* principal in the
	// keytab is accepted.
	//
	// That inference is the right behaviour for one service with one SPN, and
	// the wrong one as soon as a keytab is merged from several: a client
	// entitled to one of them gets into all of them. Setting this pins the key,
	// so a ticket minted for a different principal fails to decrypt and is
	// refused. It is the only control gokrb5's SPNEGO path offers here —
	// service.SName exists but is read solely by gokrb5's Basic-auth
	// authenticator, never by SPNEGO acceptance.
	//
	// The realm is not part of it: gokrb5 parses this with types.ParseSPNString,
	// which silently drops anything after an "@". New rejects a value
	// containing one rather than letting it look effective.
	//
	// Optional. Default: "" (inferred from the ticket).
	KeytabPrincipal string
	// MaxClockSkew is how far a ticket's issue time may sit from this host's
	// clock. Kerberos is clock-sensitive, and the usual cause of a service
	// rejecting every ticket is drift rather than misconfiguration.
	//
	// Must not be negative; New rejects that rather than silently ignoring it.
	//
	// Optional. Default: 0, which leaves gokrb5's own default of 5 minutes.
	MaxClockSkew time.Duration
	// DisablePACDecoding turns off decoding of the Privilege Attribute
	// Certificate that Active Directory embeds in a ticket.
	//
	// It is spelled negatively so the zero value preserves gokrb5's default,
	// which is to decode. Decoding is what populates the group SIDs behind
	// Identity.Authorized, so disabling it trades authorization data for a
	// little less work per request — worth it only for a service that never
	// inspects groups.
	//
	// Optional. Default: false (PAC decoded when present).
	DisablePACDecoding bool
	// RequireHostAddress rejects a ticket that carries no host addresses,
	// mapping to gokrb5's service.RequireHostAddr.
	//
	// Note that the address a ticket is checked against is the connection's
	// peer, which behind a reverse proxy is the proxy rather than the client.
	//
	// Optional. Default: false.
	RequireHostAddress bool
	// SessionManager lets gokrb5 establish a session after the first successful
	// authentication and serve later requests from it, skipping full ticket
	// validation. It is the single largest saving available on an authenticated
	// hot path.
	//
	// Setting it changes the trust model: the session becomes a credential in
	// its own right, so whatever the implementation stores must be
	// unguessable, bound to the client, and expired deliberately. The
	// middleware forwards the request's Cookie header to gokrb5 only when this
	// is set, so a cookie-backed manager can find its session; anything the
	// manager writes — Set-Cookie included — is replayed onto the Fiber
	// response.
	//
	// Optional. Default: nil (every request is validated in full).
	SessionManager service.SessionMgr
	// OnSuccess runs once per request that authenticated, after the identity is
	// in the Fiber context and before the next handler. Intended for metrics
	// and audit; it cannot change the outcome.
	//
	// Optional. Default: nil.
	OnSuccess func(ctx fiber.Ctx, identity goidentity.Identity)
	// OnError runs when the middleware itself fails — today, only when the
	// keytab lookup does — just before the request is answered with 500. It is
	// not called for authentication outcomes: a rejected ticket is Unauthorized's
	// business, and a challenge is not a failure.
	//
	// The error it receives wraps ErrLookupKeytabFailed and the underlying
	// cause, which names keytab paths and OS errors, so treat it as internal
	// diagnostics rather than something to echo to a client.
	//
	// Optional. Default: nil.
	OnError func(ctx fiber.Ctx, err error)
}

// ConfigDefault records what an unset field does, for callers who would rather
// start from it than from a zero Config. Every field is optional except
// KeytabLookup, which has no usable default: it is required unless
// FallbackToSystemKeytab is set.
//
// It is deliberately equal to the zero Config, and New applies these defaults
// by treating a zero field as unset rather than by reading this variable. So
// copying it is supported and mutating it is not — a package-level write would
// change nothing and TestConfigDefault would start failing.
var ConfigDefault = Config{
	Next:                   nil,
	KeytabLookup:           nil,
	Log:                    nil,
	UseFiberLogger:         false,
	Unauthorized:           nil,
	FallbackToSystemKeytab: false,
	KeytabPrincipal:        "",
	// Zero rather than 5 minutes: gokrb5 substitutes its own default for a zero
	// value, and naming a number here would silently pin it if gokrb5 ever
	// changed.
	MaxClockSkew:       0,
	DisablePACDecoding: false,
	RequireHostAddress: false,
	SessionManager:     nil,
	OnSuccess:          nil,
	OnError:            nil,
}

// fileStamp identifies a keytab file revision cheaply enough to check on every
// request. Identity is recorded alongside size and modification time where the
// platform exposes it, so on Unix, replacing a keytab by rename is detected
// even when the staging tool preserved the timestamp of a same-sized file.
// Elsewhere identity is a hint at best — see the fileRevisionID
// implementations — and detection falls back to size and modification time.
//
// It is deliberately plain and comparable: the os.FileInfo it came from is not
// retained, so a snapshot stays immutable and readable without locking.
//
// Detection is still not exact. An in-place rewrite that keeps the file the
// same size and lands within one filesystem timestamp tick looks unchanged,
// because the file's identity does not change either. Neither does a
// permissions change, which alters only ctime.
type fileStamp struct {
	size    int64
	modTime int64
	id      fileID
}

// fileID is an opaque, comparable file identity. What it holds is
// platform-specific — see the fileRevisionID implementations — and it is the
// zero value where the platform exposes nothing, in which case change detection
// falls back to size and modification time.
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
//
// Episode logging follows one rule: the line is queued with announce while mu
// is held, next to the state change it describes, and emit writes whatever this
// goroutine queued once mu is released. Queueing beside the mutation means a
// transition cannot be marked as announced without its line being queued, and
// writing after the unlock keeps the sink off the reload path that every
// request queues on during a degraded episode.
//
// Each goroutine writes only its own lines, and nothing orders them against
// each other. A goroutine preempted between releasing mu and writing can
// therefore let a later transition's line reach the sink first — an all-clear
// printed above the warning it clears. That window is a few instructions wide,
// against another goroutine that has to acquire mu and read every keytab file
// to overtake it.
//
// A shared flush lock was tried and removed. It made the ordering exact, but
// every request on the reload path had to take it, so one goroutine inside a
// slow sink stalled all of them — and a sink that re-entered the cache
// deadlocked on it, since a sync.Mutex is not reentrant. A cosmetic
// misordering of two rare lines is a better trade than a reproducible stall on
// the authentication path.
type keytabFileCache struct {
	files      []string
	staleGrace time.Duration
	retryEvery time.Duration
	nowFn      func() time.Time

	// snapshot is read without holding mu so the cache-hit path, which runs on
	// every request the middleware handles, does not serialise them.
	snapshot atomic.Pointer[keytabSnapshot]
	// degraded mirrors whether deg is populated. The hit path reads it rather
	// than writing anything, so the common healthy case leaves this cache line
	// clean for the concurrent snapshot readers sharing it.
	degraded atomic.Bool

	// mu guards reloads so the file I/O is done once, not once per waiter.
	mu  sync.Mutex
	deg degradedState
	// queue holds the episode lines this locked section produced, oldest first.
	// Guarded by mu; taken and cleared by emit.
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
		// Once per episode rather than per request or per revision. An episode
		// begins only when the files on disk actually change, so the rate is
		// bounded by rotations, not by request volume.
		grace := c.grace()
		c.announce(func() {
			flog.Warnf("spnego: %v; serving the last keytab that parsed for up to %s", cause, grace)
		})
		return snap.merged, nil
	}
	if now.Sub(c.deg.since) > c.grace() {
		if !c.deg.expiryLogged {
			c.deg.expiryLogged = true
			// The transition from degraded to failing is the one an operator
			// most needs to see, so it gets its own line at error level.
			grace := c.grace()
			c.announce(func() {
				flog.Errorf("spnego: keytab still unusable after %s, failing requests: %v", grace, cause)
			})
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
	// Only announce a recovery for an episode that was announced. An episode
	// that began before any keytab ever loaded never emitted the warning, so an
	// all-clear would refer to nothing.
	//
	// At the same level as the warning it clears: an operator filtering below
	// Warn would otherwise see every alert open and none of them close.
	if !c.deg.since.IsZero() {
		c.announce(func() { flog.Warnf("spnego: keytab loads cleanly again") })
	}
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
	defer c.emit()
	if c.deg.cause == nil {
		// Another goroutine closed the episode between the lock-free check and
		// this lock. Skipping the stat below is the only effect: clearDegraded
		// would find nothing to do either, since degraded and deg.cause always
		// move together under this mutex. Purely an early-out, so its absence
		// is not observable.
		return
	}
	if fresh, err := c.stat(); err == nil && slices.Equal(fresh, expected) {
		c.clearDegraded()
	}
}

// announce queues an episode line to be written once mu is released. Callers
// must hold mu. Queueing rather than replacing means a locked section that
// grows a second transition cannot silently drop the first one's line.
func (c *keytabFileCache) announce(write func()) {
	c.queue = append(c.queue, write)
}

// emit releases mu and writes whatever this locked section queued, oldest
// first. It is called with mu held and returns with it released. Clearing the
// queue rather than resharing it means a goroutine only ever writes its own
// lines and nothing is retained afterwards. See the note on keytabFileCache.
func (c *keytabFileCache) emit() {
	writes := c.queue
	c.queue = nil
	c.mu.Unlock()
	for _, write := range writes {
		write()
	}
}

// load returns the merged keytab, re-reading the files only when their
// revision differs from the cached one. The returned pointer is stable across
// calls for as long as the files are unchanged, which lets a caller reuse
// anything it derived from the keytab.
//
// Because change detection is inexact (see fileStamp), rotate a keytab by
// writing a new file and renaming it over the old one. On Unix that changes the
// file's identity, so it is picked up even when size and modification time are
// preserved, whereas an in-place rewrite of identical length may not be. On
// other platforms make sure the replacement differs in size or modification
// time, since identity there cannot be relied on.
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
	// Re-stat under the lock. The caller's stamps were taken before mu was
	// acquired, and this goroutine may have waited behind another's readAll, so
	// pairing those stamps with content read now could record one revision as
	// holding another's bytes — which would both clobber a correct snapshot and,
	// if the rotation were later rolled back in place, pin the wrong keytab.
	stamps, err = c.stat()
	if err != nil {
		return nil, err
	}
	// Another goroutine may have reloaded while this one waited for the lock.
	if snap := c.snapshot.Load(); snap != nil && slices.Equal(snap.stamps, stamps) {
		// The revision on disk is the cached one again, so any episode is over.
		// The fast path reaches the same conclusion through endEpisodeIfCurrent;
		// here the stamps were just taken under the lock, so no re-stat is
		// needed.
		c.clearDegraded()
		return snap.merged, nil
	}

	now := nowOr(c.nowFn)
	// This exact revision already failed recently; do not re-read every file
	// again on every request while the fault persists. This covers a file that
	// cannot be read as well as one that cannot be parsed — both would
	// otherwise put a full read of every keytab on every request, serialised
	// behind this mutex.
	if c.deg.cause != nil && slices.Equal(c.deg.stamps, stamps) && now.Sub(c.deg.lastAttempt) < c.retry() {
		if errors.Is(c.deg.cause, errKeytabUnparsable) {
			return c.serveStale(c.deg.cause, now)
		}
		return nil, c.deg.cause
	}

	merged, err := c.readAll()
	if err != nil {
		// Record the failing revision either way, so the retry throttle applies
		// to unreadable files too. deg.since is deliberately left alone: an
		// episode is bounded by its first failure, not by the latest revision,
		// so a keytab being rewritten badly over and over cannot extend the
		// grace window indefinitely and keep superseded keys alive.
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

// NewKeytabFileLookupFunc creates a new KeytabLookupFunc that loads keytab files
// It accepts one or more keytab file paths and returns a function that loads them
//
// The merged keytab is cached and reused until one of the files changes, so
// rotating a keytab on disk still takes effect without re-reading and
// re-parsing every file on every request. A file counts as changed when its
// size, modification time or identity differs; see fileStamp for what that
// does and does not catch.
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
// file-backed types, FILE and WRFILE, are accepted and stripped. The known
// types that cannot be read as a file — KEYRING, MEMORY, DIR, ANY, SRVTAB —
// are rejected with ErrUnsupportedKeytabResidualType rather than being treated
// as a literal path that would fail on every request.
//
// A prefix matching no known type is taken as part of the filename, since a
// relative path may legitimately contain a colon. A typo such as
// "FIEL:/etc/krb5.keytab" therefore resolves as a path and fails when the file
// cannot be found.
//
// Note that this is a keytab, not a credential cache: SPNEGO acceptance
// requires the service's long-term keys, so a client credential cache
// (KRB5CCNAME) cannot be used to accept incoming tickets.
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

// nonFileKeytabTypes are the MIT keytab residual types that exist but cannot be
// read as a file. Anything outside both lists is a filename, not a type: a
// relative path may legitimately contain a colon.
var nonFileKeytabTypes = []string{"KEYRING", "MEMORY", "DIR", "ANY", "SRVTAB"}

// resolveKeytabResidual strips a supported "TYPE:" prefix from a keytab name
// and rejects the known types that are not plain files. A name whose prefix
// matches no known type is returned unchanged and treated as a path, so
// absolute paths, Windows drive letters and relative names that happen to
// contain a colon all still work.
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
		// Not a known type, so the colon belongs to the name: an absolute path,
		// a Windows drive letter such as C:\, or a relative name that simply
		// contains one.
		return name, nil
	}
}
