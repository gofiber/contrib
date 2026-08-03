package spnego

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/service"
	gospnego "github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/jcmturner/gokrb5/v8/types"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TestUnauthorizedNotCalledOnContinueNeeded covers the "continue needed" leg of
// the handshake. It is a 401, but it tells the client to retry with KRB5 rather
// than reporting a failure, so handing it to Config.Unauthorized would let the
// status or body change and strand clients that only renegotiate on an
// untouched 401.
func TestUnauthorizedNotCalledOnContinueNeeded(t *testing.T) {
	var unauthorizedCalled bool
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		Unauthorized: func(c fiber.Ctx) error {
			unauthorizedCalled = true
			return c.Status(fiber.StatusForbidden).SendString("denied")
		},
	}, func(ctx *fasthttp.RequestCtx) {
		// An NTLMSSP token is a Negotiate header gokrb5 cannot parse as SPNEGO
		// or raw KRB5, which is exactly the path that asks the client to
		// renegotiate.
		ctx.Request.Header.Set(fiber.HeaderAuthorization,
			"Negotiate "+base64.StdEncoding.EncodeToString([]byte("NTLMSSP\x00\x01\x00\x00\x00")))
	})

	require.False(t, unauthorizedCalled, "Unauthorized must not run on the continue-needed leg")
	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Equal(t, spnegoContinueNeeded, string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)),
		"the continuation token must reach the client untouched")
}

// TestUnauthorizedNotCalledOnOpeningChallenge covers the leg that bootstraps
// every handshake: a request with no Authorization header gets 401 plus a bare
// "Negotiate". curl --negotiate and every major browser start SPNEGO only when
// that arrives untouched, so letting a caller rewrite it into, say, a 403 is a
// permanent silent deny-all.
func TestUnauthorizedNotCalledOnOpeningChallenge(t *testing.T) {
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		Unauthorized: func(c fiber.Ctx) error {
			return c.Status(fiber.StatusForbidden).SendString("denied")
		},
	})

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Equal(t, "Negotiate", string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)),
		"the opening challenge must reach the client untouched")
	require.Contains(t, string(ctx.Response.Body()), "Unauthorised",
		"SPNEGO's own body must reach the client, not just its headers")
}

// TestIsRejection pins which of gokrb5's three 401 shapes is treated as a
// failure. Only the rejection is; the other two are legs of a negotiation that
// can still succeed.
func TestIsRejection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   bool
	}{
		{"opening challenge", "Negotiate", false},
		{"continue needed", spnegoContinueNeeded, false},
		{"accept completed", "Negotiate oRQwEqADCgEAoQsGCSqGSIb3EgECAg==", false},
		{"rejected", spnegoRejected, true},
		{"no header", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(http.Header)
			if tc.header != "" {
				headers.Set(fiber.HeaderWWWAuthenticate, tc.header)
			}
			require.Equal(t, tc.want, isRejection(headers))
		})
	}
}

// TestKeytabLookupErrorNotLeakedToClient checks that the keytab path and the
// underlying OS error stay out of the response body. Fiber's default error
// handler echoes err.Error() straight to the client, and the lookup error names
// the keytab file.
func TestKeytabLookupErrorNotLeakedToClient(t *testing.T) {
	secretPath := path.Join(t.TempDir(), "super-secret-location.keytab")
	ctx := serveProtected(t, Config{
		KeytabLookup: func() (*keytab.Keytab, error) {
			return nil, fmt.Errorf("open %s: permission denied", secretPath)
		},
	})

	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode())
	require.NotContains(t, string(ctx.Response.Body()), secretPath)
	require.NotContains(t, string(ctx.Response.Body()), "permission denied")
}

// TestNilKeytabFromLookup covers a caller-supplied lookup that reports success
// but returns no keytab. gokrb5 dereferences it unconditionally, so passing it
// through panics the process on an unauthenticated request.
func TestNilKeytabFromLookup(t *testing.T) {
	var ctx *fasthttp.RequestCtx
	require.NotPanics(t, func() {
		ctx = serveProtected(t, Config{
			KeytabLookup: func() (*keytab.Keytab, error) { return nil, nil },
		})
	})
	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode())
}

// TestNextSkipsMiddleware covers the standard opt-out hook.
func TestNextSkipsMiddleware(t *testing.T) {
	filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")
	lookupFunc, err := NewKeytabFileLookupFunc(filename)
	require.NoError(t, err)
	middleware, err := New(Config{
		KeytabLookup: lookupFunc,
		Next:         func(c fiber.Ctx) bool { return c.Path() == "/healthz" },
	})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/healthz", middleware, func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/healthz")
	app.Handler()(ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	require.Equal(t, "ok", string(ctx.Response.Body()))
}

// TestResolveLoggerNilFiberLogger covers the nil interface Fiber's
// DefaultLogger returns when the registered logger is not backed by a
// *log.Logger. Calling Logger on it panics, which would take down New at
// startup for any app using the zap or zerolog adapters.
func TestResolveLoggerNilFiberLogger(t *testing.T) {
	t.Run("nil fiber logger does not panic", func(t *testing.T) {
		require.NotPanics(t, func() {
			require.Nil(t, resolveLogger(Config{UseFiberLogger: true}, nil))
		})
	})

	t.Run("gokrb5 logging is off unless asked for", func(t *testing.T) {
		require.Nil(t, resolveLogger(Config{}, flog.DefaultLogger[*log.Logger]()))
	})

	t.Run("explicit logger wins", func(t *testing.T) {
		want := log.New(os.Stderr, "", 0)
		require.Same(t, want, resolveLogger(Config{Log: want}, nil))
	})

	t.Run("fiber logger is used when opted in", func(t *testing.T) {
		require.NotNil(t, resolveLogger(Config{UseFiberLogger: true}, flog.DefaultLogger[*log.Logger]()))
	})
}

// TestKeytabCacheSurvivesTornRead checks that a keytab that fails to parse —
// as happens while a rotation is mid-write — does not take out authentication
// when a good keytab is already cached.
func TestKeytabCacheSurvivesTornRead(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	fn, err := NewKeytabFileLookupFunc(filename)
	require.NoError(t, err)

	good, err := fn()
	require.NoError(t, err)
	require.NotNil(t, good)

	// Simulate the window where the file has been truncated but not yet
	// rewritten. Its stamp changes, so the cache tries to reload and fails.
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))

	served, err := fn()
	require.NoError(t, err, "a torn read must not fail the request")
	require.Same(t, good, served, "the last known good keytab should be served")

	// Once the rotation completes, the new keytab is picked up.
	replacement := writeMockKeytab(t, dir, "rotated.keytab", "HTTP/rotated.example.com")
	contents, err := os.ReadFile(replacement)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filename, contents, 0o600))

	after, err := fn()
	require.NoError(t, err)
	require.NotSame(t, good, after)
}

// TestKeytabCacheReportsFirstLoadFailure checks that the fallback does not hide
// a keytab that never loaded in the first place.
func TestKeytabCacheReportsFirstLoadFailure(t *testing.T) {
	broken := path.Join(t.TempDir(), "broken.keytab")
	require.NoError(t, os.WriteFile(broken, []byte("12"), 0o600))

	fn, err := NewKeytabFileLookupFunc(broken)
	require.NoError(t, err)
	_, err = fn()
	require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
}

// rejectedNegotiateHeader builds an Authorization value that gokrb5 parses as a
// well-formed SPNEGO NegTokenInit advertising KRB5, but whose mech token fails
// validation. That is the one path that produces an outright rejection rather
// than another leg of the handshake.
func rejectedNegotiateHeader(t *testing.T) string {
	t.Helper()
	token := gospnego.SPNEGOToken{
		Init: true,
		NegTokenInit: gospnego.NegTokenInit{
			MechTypes:      []asn1.ObjectIdentifier{gssapi.OIDKRB5.OID()},
			MechTokenBytes: []byte{0x60, 0x05, 0x06, 0x03, 0x2a, 0x03, 0x04},
		},
	}
	raw, err := token.Marshal()
	require.NoError(t, err)
	return "Negotiate " + base64.StdEncoding.EncodeToString(raw)
}

// TestUnauthorizedCalledOnRejection is the positive case for Config.Unauthorized:
// a client presented a ticket and the service refused it.
func TestUnauthorizedCalledOnRejection(t *testing.T) {
	var called bool
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		Unauthorized: func(c fiber.Ctx) error {
			called = true
			return c.Status(fiber.StatusForbidden).SendString("denied")
		},
	}, func(ctx *fasthttp.RequestCtx) {
		ctx.Request.Header.Set(fiber.HeaderAuthorization, rejectedNegotiateHeader(t))
	})

	require.True(t, called, "Unauthorized must run when a ticket is rejected")
	require.Equal(t, fiber.StatusForbidden, ctx.Response.StatusCode())
	require.Equal(t, "denied", string(ctx.Response.Body()))
	// The godoc promises a handler that only sets a status and body leaves
	// SPNEGO's headers in place.
	require.Equal(t, spnegoRejected, string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)),
		"a custom handler must not lose the WWW-Authenticate header")
}

// TestRejectionWithoutHandlerPassesThrough checks the default: with no handler
// configured, SPNEGO's own rejection reaches the client unchanged.
func TestRejectionWithoutHandlerPassesThrough(t *testing.T) {
	ctx := serveProtected(t, Config{KeytabLookup: testKeytabLookup(t)},
		func(ctx *fasthttp.RequestCtx) {
			ctx.Request.Header.Set(fiber.HeaderAuthorization, rejectedNegotiateHeader(t))
		})

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Equal(t, spnegoRejected, string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)))
	require.Contains(t, string(ctx.Response.Body()), "Unauthorised",
		"SPNEGO's own body must reach the client, not just its headers")
}

// TestLookupErrorRemainsInspectable checks that hiding the detail from the
// client did not also hide it from the application's own error handler.
func TestLookupErrorRemainsInspectable(t *testing.T) {
	middleware, err := New(Config{
		KeytabLookup: func() (*keytab.Keytab, error) {
			return nil, errors.New("boom")
		},
	})
	require.NoError(t, err)

	var handlerErr error
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, e error) error {
			handlerErr = e
			return c.SendStatus(fiber.StatusInternalServerError)
		},
	})
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	app.Handler()(ctx)

	require.ErrorIs(t, handlerErr, ErrLookupKeytabFailed)
	require.Equal(t, "Internal Server Error", handlerErr.Error(),
		"the message shown to a client must not carry the cause")
}

// TestLookupErrorCannotChooseTheResponseStatus is why clientSafeError
// implements Is and not Unwrap. Fiber's asFiberError resolves the status with
// errors.As, which walks the Unwrap chain, so publishing the cause would hand a
// caller's KeytabLookupFunc control of this middleware's status code. A lookup
// returning fiber.ErrUnauthorized would answer an infrastructure fault with a
// bare 401 — which a SPNEGO client reads as "renegotiate", not "this server is
// broken" — and it would do so with no WWW-Authenticate header to renegotiate
// against.
//
// Adding the Unwrap method that Go style would otherwise expect is the whole
// regression, and it is invisible without this test.
func TestLookupErrorCannotChooseTheResponseStatus(t *testing.T) {
	ctx := serveProtected(t, Config{
		KeytabLookup: func() (*keytab.Keytab, error) {
			// A caller-chosen error that Fiber knows how to turn into a status.
			return nil, fiber.ErrUnauthorized
		},
	})

	require.Equal(t, fiber.StatusInternalServerError, ctx.Response.StatusCode(),
		"a keytab lookup failure is a 500 whatever error the lookup returned")
	require.Empty(t, ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate),
		"nothing here should look like a challenge the client can answer")
	require.Equal(t, "Internal Server Error", string(ctx.Response.Body()),
		"the cause must not reach the client through the body either")
}

// TestKeytabStaleGraceExpires checks that a keytab which stays unparsable stops
// being covered for. Serving it forever would keep a revoked key alive.
func TestKeytabStaleGraceExpires(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")

	now := time.Now()
	cache := &keytabFileCache{
		files:      []string{filename},
		staleGrace: 30 * time.Second,
		// A tiny retry window keeps the throttle out of the way; load() maps a
		// non-positive value back to the 1s default, so 0 would not disable it.
		retryEvery: time.Nanosecond,
		nowFn:      func() time.Time { return now },
	}

	good, err := cache.load()
	require.NoError(t, err)

	// The keytab is replaced by something that will never parse.
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))

	served, err := cache.load()
	require.NoError(t, err, "within the grace window the last good keytab is served")
	require.Same(t, good, served)

	// Exactly at the boundary the keytab is still covered for; one tick past it
	// the fallback expires.
	now = now.Add(30 * time.Second)
	_, err = cache.load()
	require.NoError(t, err, "the grace window is inclusive of its own length")

	now = now.Add(time.Nanosecond)
	_, err = cache.load()
	require.ErrorIs(t, err, ErrLoadKeytabFileFailed, "the fallback must not outlive the grace window")
}

// TestKeytabStaleGraceResetsAfterRecovery covers a degraded episode that ends
// via the cache-hit path: restoring a keytab with its original size and mtime
// (cp -p, rsync -a) leaves the stamps matching, so the recovery has to be
// noticed there too. Otherwise the stale clock keeps running and a later,
// unrelated torn read gets no grace at all.
func TestKeytabStaleGraceResetsAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	original, err := os.ReadFile(filename)
	require.NoError(t, err)
	info, err := os.Stat(filename)
	require.NoError(t, err)

	now := time.Now()
	cache := &keytabFileCache{
		files:      []string{filename},
		staleGrace: 30 * time.Second,
		nowFn:      func() time.Time { return now },
	}
	good, err := cache.load()
	require.NoError(t, err)

	// Corrupt it, and confirm the degraded episode has started.
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	_, err = cache.load()
	require.NoError(t, err)
	require.True(t, cache.degraded.Load())

	// Restore byte-for-byte, preserving size and mtime.
	require.NoError(t, os.WriteFile(filename, original, 0o600))
	require.NoError(t, os.Chtimes(filename, info.ModTime(), info.ModTime()))
	served, err := cache.load()
	require.NoError(t, err)
	require.Same(t, good, served)
	require.False(t, cache.degraded.Load(), "recovery must clear the degraded episode")

	// A genuine torn read much later still gets the full grace window.
	now = now.Add(10 * time.Minute)
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	served, err = cache.load()
	require.NoError(t, err, "a later torn read must get its own grace window")
	require.Same(t, good, served)
}

// TestKeytabRetryThrottle checks that a revision already known to be unparsable
// is not re-read on every request while the fault persists.
func TestKeytabRetryThrottle(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")

	now := time.Now()
	cache := &keytabFileCache{
		files:      []string{filename},
		staleGrace: 30 * time.Second,
		retryEvery: time.Second,
		nowFn:      func() time.Time { return now },
	}
	_, err := cache.load()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	_, err = cache.load()
	require.NoError(t, err)
	// deg is mutex-guarded, so read it the way the cache does.
	lastAttempt := func() time.Time {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.deg.lastAttempt
	}
	firstAttempt := lastAttempt()

	// Within the retry window the file is not read again.
	now = now.Add(100 * time.Millisecond)
	_, err = cache.load()
	require.NoError(t, err)
	require.Equal(t, firstAttempt, lastAttempt(), "should not re-read within the retry window")

	// Past it, one more attempt is made.
	now = now.Add(2 * time.Second)
	_, err = cache.load()
	require.NoError(t, err)
	require.NotEqual(t, firstAttempt, lastAttempt(), "should retry once the window elapses")
}

// TestNewKeytabFileCacheWiresDefaults pins what the production constructor
// configures, which the hand-built caches above deliberately override.
func TestNewKeytabFileCacheWiresDefaults(t *testing.T) {
	filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")

	cache := newKeytabFileCache([]string{filename})
	require.Equal(t, defaultKeytabStaleGrace, cache.staleGrace)
	require.Equal(t, keytabRetryEvery, cache.retryEvery)
	require.Positive(t, cache.grace(), "a zero grace would defeat the torn-read fallback")
	require.Positive(t, cache.retry(), "a zero retry interval would re-read on every request")

	// The constructor copies its input, so a caller mutating the slice
	// afterwards cannot repoint the cache at a different keytab.
	files := []string{filename}
	cache = newKeytabFileCache(files)
	files[0] = "/nonexistent"
	_, err := cache.load()
	require.NoError(t, err)

	// A zero-value cache must still work: now falls back to the wall clock and
	// grace to the default, so no nil call and no zero window.
	zero := &keytabFileCache{files: []string{filename}}
	require.NotPanics(t, func() {
		_, loadErr := zero.load()
		require.NoError(t, loadErr)
	})
	require.Equal(t, defaultKeytabStaleGrace, zero.grace())
	require.Equal(t, keytabRetryEvery, zero.retry())
}

// TestKeytabEpisodeBoundedAcrossRevisions covers a rotation script that keeps
// writing a broken keytab. Each attempt is a new revision, and restarting the
// grace window on every one would cover a superseded — possibly revoked —
// keytab forever.
func TestKeytabEpisodeBoundedAcrossRevisions(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")

	now := time.Now()
	cache := &keytabFileCache{
		files:      []string{filename},
		staleGrace: 30 * time.Second,
		retryEvery: time.Nanosecond,
		nowFn:      func() time.Time { return now },
	}
	_, err := cache.load()
	require.NoError(t, err)

	// Rewrite a different broken keytab every 20 seconds.
	var lastErr error
	for i := range 10 {
		require.NoError(t, os.WriteFile(filename, []byte(strings.Repeat("x", i+2)), 0o600))
		now = now.Add(20 * time.Second)
		_, lastErr = cache.load()
	}
	require.Error(t, lastErr, "a keytab that keeps failing must stop being covered for")
	require.ErrorIs(t, lastErr, ErrLoadKeytabFileFailed)
}

// TestEndEpisodeIfCurrentRejectsStaleView drives the guard directly: a request
// whose stat predates a rotation another goroutine already found broken must
// not cancel that goroutine's episode.
func TestEndEpisodeIfCurrentRejectsStaleView(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")

	cache := newKeytabFileCache([]string{filename})
	_, err := cache.load()
	require.NoError(t, err)
	goodStamps := cache.snapshot.Load().stamps

	// Simulate the state another goroutine would have left behind.
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	cache.mu.Lock()
	cache.deg = degradedState{since: time.Now(), cause: errKeytabUnparsable}
	cache.degraded.Store(true)
	cache.mu.Unlock()

	// A caller holding the pre-rotation view must not clear that episode: the
	// keytab on disk is still broken.
	cache.endEpisodeIfCurrent(goodStamps)
	require.True(t, cache.degraded.Load(), "a stale view must not end a live episode")

	// Once the keytab really is back, the episode ends.
	restored := writeMockKeytab(t, dir, "restored.keytab", "HTTP/sso.example.com")
	contents, err := os.ReadFile(restored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filename, contents, 0o600))
	fresh, err := cache.stat()
	require.NoError(t, err)
	cache.endEpisodeIfCurrent(fresh)
	require.False(t, cache.degraded.Load(), "a current view must end the episode")
}

// TestKeytabRotationByRenameDetected covers the rotation the README recommends,
// staged by a tool that preserves the timestamp of a same-sized file. Size and
// mtime alone would call that unchanged and keep serving the rotated-out keys.
func TestKeytabRotationByRenameDetected(t *testing.T) {
	if !identityDetectsRename {
		t.Skip("platform has no dependable file identity; detection falls back to size and mtime")
	}

	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	info, err := os.Stat(filename)
	require.NoError(t, err)

	cache := newKeytabFileCache([]string{filename})
	before, err := cache.load()
	require.NoError(t, err)

	// A different principal of the same length, staged with the original's
	// mtime, then renamed into place.
	staged := writeMockKeytab(t, dir, "staged.keytab", "HTTP/sso.example.org")
	stagedInfo, err := os.Stat(staged)
	require.NoError(t, err)
	require.Equal(t, info.Size(), stagedInfo.Size(), "same-length keytabs make this the interesting case")
	require.NoError(t, os.Chtimes(staged, info.ModTime(), info.ModTime()))
	require.NoError(t, os.Rename(staged, filename))

	after, err := cache.load()
	require.NoError(t, err)
	require.NotSame(t, before, after, "a renamed-in keytab must be picked up")
	info2 := utils.GetKeytabInfo(after)
	require.Len(t, info2, 1)
	require.Equal(t, "HTTP/sso.example.org@TEST.LOCAL", info2[0].PrincipalName)
}

// TestKeytabUnreadableIsNotCoveredFor checks the branch that separates a file
// that cannot be read from one that cannot be parsed. Only the latter is a
// rotation caught mid-write; an unreadable keytab must surface immediately, or
// revoking one by making it unreachable would be masked by the cache.
//
// A directory in place of the file yields EISDIR portably; chmod is not usable
// because the suite may run as root.
func TestKeytabUnreadableIsNotCoveredFor(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")

	cache := newKeytabFileCache([]string{filename})
	good, err := cache.load()
	require.NoError(t, err)
	require.NotNil(t, good)

	// Replace the keytab with a directory: it stats fine but cannot be read.
	require.NoError(t, os.Remove(filename))
	require.NoError(t, os.Mkdir(filename, 0o700))

	_, err = cache.load()
	require.Error(t, err, "an unreadable keytab must not be covered for by the cache")
	require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
	require.NotErrorIs(t, err, errKeytabUnparsable)

	// The failing revision is still recorded, so the retry throttle applies.
	cache.mu.Lock()
	recorded := cache.deg.cause != nil
	cache.mu.Unlock()
	require.True(t, recorded, "an unreadable revision must be recorded for throttling")
	require.True(t, cache.degraded.Load())
}

// TestRequestForSPNEGO checks the hand-built request the middleware hands to
// gokrb5. A field populated wrongly is worse than one left nil, because nil
// fails loudly.
func TestRequestForSPNEGO(t *testing.T) {
	var got *http.Request
	app := fiber.New()
	// Registered for every verb and driven with one that is not the zero-ish
	// default: asserting a GET against a hardcoded "GET" would hold however the
	// method was derived.
	app.All("/*", func(c fiber.Ctx) error {
		got = requestForSPNEGO(c)
		return nil
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodPatch)
	ctx.Request.SetRequestURI("/a%2Fb/c%20d?q=1&x=%41")
	ctx.Request.Header.Set(fiber.HeaderHost, "sso.example.com:8443")
	ctx.Request.Header.Set(fiber.HeaderAuthorization, "Negotiate abc")
	ctx.SetRemoteAddr(&net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 55123})
	app.Handler()(ctx)

	require.NotNil(t, got)
	require.Equal(t, fiber.MethodPatch, got.Method)
	require.Equal(t, "Negotiate abc", got.Header.Get(fiber.HeaderAuthorization))
	// net/http documents a server request's Host as "host or host:port", so the
	// port must survive.
	require.Equal(t, "sso.example.com:8443", got.Host)

	// RemoteAddr is the one field gokrb5 reads: it parses it with
	// types.GetHostAddress and, on success, constrains ticket verification to
	// that client address (spnego/http.go:252, service/APExchange.go:14).
	// Leaving it empty would reject every address-restricted service ticket and
	// log a parse failure per request, so both the value and its parseability
	// are pinned.
	require.Equal(t, "203.0.113.7:55123", got.RemoteAddr)
	addr, err := types.GetHostAddress(got.RemoteAddr)
	require.NoError(t, err, "gokrb5 must be able to parse the address it is handed")
	require.Equal(t, net.IPv4(203, 0, 113, 7).To4(), net.IP(addr.Address))

	// URL is present purely so a dereference cannot panic. It is deliberately
	// empty rather than a reconstruction, which could not be faithful for every
	// request, so nothing here should claim to describe the target.
	require.NotNil(t, got.URL)
	require.Empty(t, got.URL.Path)
	require.Empty(t, got.URL.RawQuery)
}

// TestLogThrottle pins all three properties: the first event passes, repeats
// inside the window do not, and the window reopens. The last one needs a clock
// seam — without it, hoisting the timestamp update out of the guard would turn
// the throttle into "never log again while failures keep arriving" and no test
// would notice.
func TestLogThrottle(t *testing.T) {
	now := time.Now()
	throttle := &logThrottle{every: 30 * time.Second, nowFn: func() time.Time { return now }}
	var runs int
	fire := func() { throttle.do(func() { runs++ }) }

	fire()
	require.Equal(t, 1, runs, "the first event must always run")
	fire()
	require.Equal(t, 1, runs, "an immediate repeat must not")

	now = now.Add(29 * time.Second)
	fire()
	require.Equal(t, 1, runs, "still inside the window")

	now = now.Add(2 * time.Second)
	fire()
	require.Equal(t, 2, runs, "the window must reopen")
	fire()
	require.Equal(t, 2, runs, "and close again behind it")
}

// TestLogThrottleZeroWindow checks that a throttle built without a window
// still throttles. Treating zero as "no interval" would let every event
// through, which is the flood the type exists to prevent.
func TestLogThrottleZeroWindow(t *testing.T) {
	throttle := &logThrottle{}
	var runs int
	for range 5 {
		throttle.do(func() { runs++ })
	}
	require.Equal(t, 1, runs)
	require.Equal(t, internalErrorLogEvery, throttle.window())
}

// TestInternalErrorLogsOncePerFault checks the wiring: a repeating keytab
// failure reaches the log exactly once, not once per request.
func TestInternalErrorLogsOncePerFault(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	middleware, err := New(Config{
		KeytabLookup: func() (*keytab.Keytab, error) {
			return nil, errors.New("keytab boom")
		},
	})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	handler := app.Handler()
	for range 4 {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fiber.MethodGet)
		ctx.Request.SetRequestURI("/authenticate")
		handler(ctx)
		require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode())
	}

	require.Equal(t, 1, strings.Count(logged.String(), "keytab boom"),
		"a repeating fault must be logged once, not per request")
}

// TestKeytabEpisodeLogging pins the three operator-facing lines the README
// promises, and that each is emitted once per episode rather than per request.
// Without this, dropping the log calls entirely leaves the suite green.
func TestKeytabEpisodeLogging(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	original, err := os.ReadFile(filename)
	require.NoError(t, err)

	now := time.Now()
	cache := &keytabFileCache{
		files:      []string{filename},
		staleGrace: 30 * time.Second,
		retryEvery: time.Nanosecond,
		nowFn:      func() time.Time { return now },
	}
	_, err = cache.load()
	require.NoError(t, err)
	require.Empty(t, logged.String(), "a healthy keytab must not log")

	// Enter the degraded state.
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	_, err = cache.load()
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(logged.String(), "serving the last keytab that parsed"))

	// Repeats inside the window add nothing.
	now = now.Add(time.Second)
	_, err = cache.load()
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(logged.String(), "serving the last keytab that parsed"),
		"the warning is per episode, not per request")

	// Expire the grace window.
	now = now.Add(31 * time.Second)
	_, err = cache.load()
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(logged.String(), "failing requests"))
	now = now.Add(time.Second)
	_, err = cache.load()
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(logged.String(), "failing requests"),
		"the expiry line is emitted once, not per request")

	// Recover.
	require.NoError(t, os.WriteFile(filename, original, 0o600))
	now = now.Add(time.Second)
	_, err = cache.load()
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(logged.String(), "loads cleanly again"))

	// The all-clear must carry the level of the warning it clears, or an
	// operator filtering below Warn sees every alert open and none of them
	// close.
	require.Contains(t, logged.String(), "[Warn] spnego: keytab loads cleanly again")
	require.Regexp(t, `\[Warn\] spnego: .+; serving the last keytab that parsed`, logged.String())
	require.Regexp(t, `\[Error\] spnego: keytab still unusable`, logged.String())
}

// lockProbeWriter records whether the cache's reload mutex was free while the
// log line was being written.
type lockProbeWriter struct {
	cache     *keytabFileCache
	buf       *bytes.Buffer
	sawLocked bool
}

func (w *lockProbeWriter) Write(p []byte) (int, error) {
	if w.cache.mu.TryLock() {
		w.cache.mu.Unlock()
	} else {
		w.sawLocked = true
	}
	return w.buf.Write(p)
}

// TestKeytabEpisodeLogWritesOffReloadLock pins the half of the logging rule
// that line counts cannot see: the sink is written with the reload mutex
// released, so a blocking sink cannot stall every authenticated request queued
// behind a reload.
func TestKeytabEpisodeLogWritesOffReloadLock(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")

	cache := newKeytabFileCache([]string{filename})
	probe := &lockProbeWriter{cache: cache, buf: &bytes.Buffer{}}
	flog.SetOutput(probe)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	_, err := cache.load()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	_, err = cache.load()
	require.NoError(t, err)

	require.Contains(t, probe.buf.String(), "serving the last keytab that parsed",
		"the episode warning must actually reach the sink")
	require.False(t, probe.sawLocked, "the log write must happen with the reload lock released")
}

// TestKeytabCacheConcurrentReload drives the cache from many goroutines across
// a corrupt-then-restore cycle. Nothing else in the package runs it
// concurrently, so without this the -race build has no two goroutines to
// compare and the locking around the episode state is unverified.
func TestKeytabCacheConcurrentReload(t *testing.T) {
	var logged bytes.Buffer
	var logMu sync.Mutex
	flog.SetOutput(&lockedWriter{mu: &logMu, w: &logged})
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	good, err := os.ReadFile(filename)
	require.NoError(t, err)

	cache := newKeytabFileCache([]string{filename})
	_, err = cache.load()
	require.NoError(t, err)

	// Enter a degraded episode deterministically, so the assertion below does
	// not depend on how the flipping goroutine interleaves.
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	_, err = cache.load()
	require.NoError(t, err)
	require.True(t, cache.degraded.Load())

	logMu.Lock()
	beforeConcurrent := logged.Len()
	logMu.Unlock()

	// Every restore below is stamped with this same mtime. What opens the
	// cache-hit recovery path through endEpisodeIfCurrent is that restores are
	// stamp-identical to each other — once one has been snapshotted, the next
	// matches it — not that they match the keytab's original mtime.
	restoreStamp, err := os.Stat(filename)
	require.NoError(t, err)

	// Now flip the file between broken and good while readers hammer the cache.
	// The two interlock: the flipper waits for the readers to have loaded since
	// its last write, and the readers run until the flipper is done. Neither
	// side bounding itself independently works — at GOMAXPROCS=1 the flipper
	// runs every write before a reader is first scheduled, so the concurrent
	// phase contains no observed transition at all and the assertion below
	// becomes vacuous.
	const (
		readerCount = 16
		flips       = 100
		// One token per reader can describe a load that stat'd before the write
		// it is counted against: a reader holds at most one undelivered token,
		// and progress is unbuffered so none are stored in the channel either.
		// The token after those therefore comes from a load that began after
		// the write, and so saw the revision it made.
		loadsPerFlip = readerCount + 1
	)
	// Both sides block on the channel rather than polling. A non-blocking send
	// here would let a reader that found no receiver loop straight back into
	// load, which at GOMAXPROCS=1 holds the only P until async preemption fires
	// and turns a 0.2s test into a 7s one.
	progress := make(chan struct{})
	flipsDone := make(chan struct{})
	// Verdicts come back over a channel: require.* calls runtime.Goexit, which
	// on a worker would let Wait proceed as though it had finished.
	bad := make(chan string, readerCount+1)
	report := func(msg string) {
		select {
		case bad <- msg:
		default:
		}
	}

	var flipper, readers sync.WaitGroup
	flipper.Add(1)
	go func() {
		defer flipper.Done()
		defer close(flipsDone)
		for i := range flips {
			for range loadsPerFlip {
				<-progress
			}
			// A failed write leaves the revision unchanged, which surfaces
			// downstream as "no episode lines" — a report about the cache
			// rather than about the write that actually failed.
			if i%2 == 0 {
				if writeErr := os.WriteFile(filename, []byte("12"), 0o600); writeErr != nil {
					report(fmt.Sprintf("flip %d: write broken keytab: %v", i, writeErr))
					return
				}
				continue
			}
			if writeErr := os.WriteFile(filename, good, 0o600); writeErr != nil {
				report(fmt.Sprintf("flip %d: write good keytab: %v", i, writeErr))
				return
			}
			// Restoring the original mtime makes the stamps match the
			// snapshot, so recovery goes through the cache-hit path.
			if timeErr := os.Chtimes(filename, restoreStamp.ModTime(), restoreStamp.ModTime()); timeErr != nil {
				report(fmt.Sprintf("flip %d: restore mtime: %v", i, timeErr))
				return
			}
		}
	}()
	for range readerCount {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				kt, loadErr := cache.load()
				// Either a keytab or an error, never both and never neither.
				if (loadErr == nil) == (kt == nil) {
					report(fmt.Sprintf("load returned keytab=%v err=%v", kt != nil, loadErr))
				}
				// Reporting a violation does not end the loop: the flipper is
				// waiting on these sends, and a reader dropping out would
				// starve it into a deadlock rather than a failure.
				select {
				case progress <- struct{}{}:
				case <-flipsDone:
					return
				}
			}
		}()
	}
	flipper.Wait()
	readers.Wait()
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}

	// Measured before the convergence load below, not after: that load closes
	// the episode still open at this point and writes its own all-clear, which
	// on its own would satisfy the assertion no matter what the concurrent
	// phase did.
	logMu.Lock()
	afterConcurrent := logged.Len()
	logMu.Unlock()
	require.Greater(t, afterConcurrent, beforeConcurrent,
		"the concurrent phase must produce episode lines of its own")

	// Whatever the interleaving, the file ends good and the cache converges.
	require.NoError(t, os.WriteFile(filename, good, 0o600))
	kt, err := cache.load()
	require.NoError(t, err)
	require.NotNil(t, kt)
	require.False(t, cache.degraded.Load(), "the episode must close once the keytab is good")
}

// waitUntilBlockedOnMutex blocks until some goroutine is inside sync.Mutex.Lock
// beneath the named function. The caller must already hold that mutex, which is
// what makes the observation stable: the goroutine cannot leave Lock until the
// caller releases it.
//
// It exists because the tests covering load's under-lock re-stat have to know
// the loader is past its pre-lock stat before they disturb the files. A sleep
// only makes that likely — and on a miss the pre-lock stat fails instead,
// producing the same error and leaving the branch under test unexercised. A
// goroutine sitting in Lock has provably run everything before the Lock call.
//
// Frames are matched rather than the goroutine's header state, which the
// runtime has renamed across releases; the frames below Lock are elided for a
// parked goroutine, so they cannot be matched either.
//
// The budget is seconds rather than tens of seconds because the loader only has
// to be scheduled and run two syscalls. A caller loops over this — should the
// frame match ever stop holding, thirty generous waits would blow the package's
// own 10-minute timeout and bury this diagnostic under a goroutine dump.
func waitUntilBlockedOnMutex(t *testing.T, symbol string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, g := range strings.Split(goroutineDump(), "\n\n") {
			if strings.Contains(g, symbol) && strings.Contains(g, "sync.(*Mutex).Lock(") {
				return true
			}
		}
		return false
	}, 5*time.Second, 200*time.Microsecond,
		"no goroutine blocked on a mutex in %s", symbol)
}

// goroutineDump returns every goroutine's stack, growing the buffer until the
// dump fits. A truncated dump could cut out the very block being matched, which
// would turn a scheduling wait into a timeout.
func goroutineDump() string {
	for size := 1 << 16; ; size *= 2 {
		buf := make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			return string(buf[:n])
		}
	}
}

// lockedWriter serialises writes so the test can read the buffer safely.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestNoAllClearWithoutWarning covers the guard that suppresses a recovery line
// for an episode that never announced one. A process whose keytab is already
// corrupt at startup has no cached keytab to fall back on, so serveStale
// returns before setting the episode start and no warning is ever emitted;
// announcing an all-clear afterwards would refer to nothing.
func TestNoAllClearWithoutWarning(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	dir := t.TempDir()
	filename := path.Join(dir, "sso.keytab")
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))

	cache := newKeytabFileCache([]string{filename})
	_, err := cache.load()
	require.Error(t, err, "a keytab that never loaded must surface its error")
	require.NotContains(t, logged.String(), "serving the last keytab that parsed",
		"there is nothing to serve, so no warning opens the episode")

	// Now make it good. The episode ends, but it never announced itself.
	good := writeMockKeytab(t, dir, "good.keytab", "HTTP/sso.example.com")
	contents, err := os.ReadFile(good)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filename, contents, 0o600))

	_, err = cache.load()
	require.NoError(t, err)
	require.NotContains(t, logged.String(), "loads cleanly again",
		"an all-clear must not be emitted for a warning that never fired")
}

// TestAnnounceQueuesInOrder pins the queue semantics directly. No locked
// section announces twice today, so nothing else would notice announce going
// back to overwriting — which is the trap the queue exists to remove.
func TestAnnounceQueuesInOrder(t *testing.T) {
	cache := newKeytabFileCache(nil)

	var written []string
	cache.mu.Lock()
	cache.announce(func() { written = append(written, "first") })
	cache.announce(func() { written = append(written, "second") })
	cache.emit()

	require.Equal(t, []string{"first", "second"}, written,
		"queued lines must all be written, oldest first")

	// The queue was cleared, so a second locked section writes nothing.
	cache.mu.Lock()
	cache.emit()
	require.Len(t, written, 2)
}

// TestEmitDoesNotHoldReloadLock pins the lock discipline: a queued line is
// written after mu is released, so a blocking sink cannot stall the reload
// path every authenticated request queues on during a degraded episode.
func TestEmitDoesNotHoldReloadLock(t *testing.T) {
	cache := newKeytabFileCache(nil)

	locked := false
	cache.mu.Lock()
	cache.announce(func() {
		if cache.mu.TryLock() {
			cache.mu.Unlock()
		} else {
			locked = true
		}
	})
	cache.emit()

	require.False(t, locked, "a queued line must be written with the reload lock released")
}

// stubAuthenticate replaces the SPNEGO acceptance step for the duration of a
// test, so the authenticated branch can be driven without a KDC. accept writes
// whatever gokrb5 would have written and returns the identity to hand the inner
// handler, or nil to decline.
//
// The identity is attached here rather than by accept because gokrb5 attaches
// it to a request *derived* from the one it was given
// (goidentity.AddToHTTPRequestContext, spnego/http.go:298) and never to the one
// the middleware built. Passing the middleware's own request straight through
// would make the two indistinguishable, and reading the identity off the wrong
// one would pass under test while returning nothing in production.
func stubAuthenticate(t *testing.T, accept func(w http.ResponseWriter, r *http.Request) goidentity.Identity) {
	t.Helper()
	previous := authenticate
	authenticate = func(inner http.Handler, _ *keytab.Keytab, _ ...func(*service.Settings)) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := accept(w, r)
			if id == nil {
				return
			}
			inner.ServeHTTP(w, goidentity.AddToHTTPRequestContext(id, r))
		})
	}
	t.Cleanup(func() { authenticate = previous })
}

// TestAuthenticatedRequestPropagatesIdentityAndError covers the success branch:
// the identity reaches the handler, the accept-completed header the client
// needs for mutual authentication is replayed onto the response, and an error
// from the downstream handler is returned rather than swallowed.
func TestAuthenticatedRequestPropagatesIdentityAndError(t *testing.T) {
	user := goidentity.NewUser("alice")
	user.SetDomain("EXAMPLE.LOCAL")
	stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
		// SPNEGO writes the accept-completed header before serving the inner
		// handler, so the recorder holds it by the time the middleware replays.
		w.Header().Set(fiber.HeaderWWWAuthenticate, "Negotiate accepted")
		return &user
	})

	filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")
	lookupFunc, err := NewKeytabFileLookupFunc(filename)
	require.NoError(t, err)
	middleware, err := New(Config{KeytabLookup: lookupFunc})
	require.NoError(t, err)

	downstream := errors.New("downstream failed")
	var seen goidentity.Identity
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, e error) error {
			require.ErrorIs(t, e, downstream)
			return c.SendStatus(fiber.StatusTeapot)
		},
	})
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		seen, _ = GetAuthenticatedIdentityFromContext(c)
		return downstream
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	app.Handler()(ctx)

	require.NotNil(t, seen, "the authenticated identity must reach the handler")
	require.Equal(t, "alice", seen.UserName())
	require.Equal(t, "EXAMPLE.LOCAL", seen.Domain())
	require.Equal(t, "Negotiate accepted", string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)),
		"the accept-completed header must be replayed for mutual authentication")
	require.Equal(t, fiber.StatusTeapot, ctx.Response.StatusCode(),
		"the downstream error must be returned, not swallowed")
}

// TestKeytabUnreadableNotCoveredWithinRetryWindow covers the throttled branch:
// a second request inside the retry window must still get the error rather than
// the cached keytab, or making a keytab unreadable would be masked for the
// whole grace window.
func TestKeytabUnreadableNotCoveredWithinRetryWindow(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")

	now := time.Now()
	cache := &keytabFileCache{
		files:      []string{filename},
		staleGrace: 30 * time.Second,
		retryEvery: time.Second,
		nowFn:      func() time.Time { return now },
	}
	_, err := cache.load()
	require.NoError(t, err)

	// A directory stats fine but cannot be read.
	require.NoError(t, os.Remove(filename))
	require.NoError(t, os.Mkdir(filename, 0o700))

	_, err = cache.load()
	require.ErrorIs(t, err, ErrLoadKeytabFileFailed)

	// Inside the retry window the recorded cause is reused — and must still be
	// an error, not the stale keytab.
	now = now.Add(100 * time.Millisecond)
	kt, err := cache.load()
	require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
	require.Nil(t, kt, "an unreadable keytab must never be served from cache")
}

// TestFallbackToSystemKeytabValidatesAtStartup covers the documented promise
// that a misconfigured host fails during New rather than on every request.
func TestFallbackToSystemKeytabValidatesAtStartup(t *testing.T) {
	t.Run("missing keytab fails construction", func(t *testing.T) {
		t.Setenv("KRB5_KTNAME", path.Join(t.TempDir(), "absent.keytab"))
		_, err := New(Config{FallbackToSystemKeytab: true})
		require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
	})

	t.Run("unsupported residual type fails construction", func(t *testing.T) {
		t.Setenv("KRB5_KTNAME", "KEYRING:persistent:0:0")
		_, err := New(Config{FallbackToSystemKeytab: true})
		require.ErrorIs(t, err, ErrUnsupportedKeytabResidualType)
	})
}

// TestConfigLogReachesGokrb5 covers the plumbing from Config.Log into gokrb5's
// service settings, which resolveLogger alone does not exercise.
func TestConfigLogReachesGokrb5(t *testing.T) {
	var captured bytes.Buffer
	serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		Log:          log.New(&captured, "", 0),
	}, func(ctx *fasthttp.RequestCtx) {
		// A malformed Negotiate token makes gokrb5 log its own diagnostic.
		ctx.Request.Header.Set(fiber.HeaderAuthorization, "Negotiate !!!not-base64!!!")
	})

	require.NotEmpty(t, captured.String(), "gokrb5 diagnostics must reach Config.Log")
	require.Contains(t, captured.String(), "SPNEGO")
}

// TestReloadPairsStampsWithContent covers the under-lock re-stat. A goroutine
// whose pre-lock stat predates a rotation must not record that older revision
// as holding the bytes it reads after acquiring the lock — the snapshot would
// then describe a revision it never read, and a rollback to it would be
// invisible.
//
// The window is reproduced by holding mu while a loader stats and blocks, then
// rotating the file before releasing it.
func TestReloadPairsStampsWithContent(t *testing.T) {
	dir := t.TempDir()
	source := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	original, err := os.ReadFile(source)
	require.NoError(t, err)
	rotated := writeMockKeytab(t, dir, "rotated.keytab", "HTTP/rotated.example.com")
	rotatedBytes, err := os.ReadFile(rotated)
	require.NoError(t, err)

	// Repeated so the assertions run against many stat/read interleavings, not
	// because any single iteration might miss the window: parking on mu is
	// waited for below rather than slept for.
	for range 30 {
		filename := path.Join(t.TempDir(), "sso.keytab")
		require.NoError(t, os.WriteFile(filename, original, 0o600))
		cache := newKeytabFileCache([]string{filename})

		cache.mu.Lock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = cache.load()
		}()

		// Parked on mu means the pre-lock stat has already recorded the
		// pre-rotation revision, which is the state this test is about.
		waitUntilBlockedOnMutex(t, "(*keytabFileCache).load(")
		require.NoError(t, os.WriteFile(filename, rotatedBytes, 0o600))
		cache.mu.Unlock()
		<-done

		// Whatever it read, the snapshot's stamps must describe that content.
		snap := cache.snapshot.Load()
		require.NotNil(t, snap)
		onDisk, err := cache.stat()
		require.NoError(t, err)
		require.Equal(t, onDisk, snap.stamps,
			"the snapshot must record the revision whose bytes it holds")

		info := utils.GetKeytabInfo(snap.merged)
		require.Len(t, info, 1)
		require.Equal(t, "HTTP/rotated.example.com@TEST.LOCAL", info[0].PrincipalName)
	}
}

// TestReloadReportsStatFailureUnderLock covers the error branch of the
// under-lock re-stat: a keytab removed after the pre-lock stat but before the
// lock is granted must surface, not proceed to a read with stale stamps.
//
// Removing the file before calling load would fail at the pre-lock stat and
// never reach this branch, so the window is reproduced by holding mu.
func TestReloadReportsStatFailureUnderLock(t *testing.T) {
	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")

	cache := newKeytabFileCache([]string{filename})

	cache.mu.Lock()
	var loadErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, loadErr = cache.load()
	}()
	// Waiting for the loader to park on mu is what makes this test about the
	// under-lock stat. A fixed sleep here would let the removal land first on a
	// slow or loaded machine; the pre-lock stat would then be the one that
	// failed, producing the same error and the same clear flag, and dropping
	// the under-lock check would survive. Parked on mu means the pre-lock stat
	// has already run and succeeded.
	waitUntilBlockedOnMutex(t, "(*keytabFileCache).load(")
	require.NoError(t, os.Remove(filename))
	cache.mu.Unlock()
	<-done

	require.ErrorIs(t, loadErr, ErrLoadKeytabFileFailed)
	// A stat failure records no episode. Falling through to readAll instead —
	// which is what dropping the under-lock error check would do — sets
	// deg.cause and the flag, so this is what tells the two apart.
	require.False(t, cache.degraded.Load(),
		"a stat failure must surface, not fall through to a read")
}

// TestEndEpisodeIfCurrentOnAlreadyClearedEpisode covers the guard for a second
// goroutine arriving after the first has already closed the episode. Both saw
// the lock-free degraded flag, but only one gets to announce the recovery.
func TestEndEpisodeIfCurrentOnAlreadyClearedEpisode(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	original, err := os.ReadFile(filename)
	require.NoError(t, err)
	info, err := os.Stat(filename)
	require.NoError(t, err)

	cache := newKeytabFileCache([]string{filename})
	_, err = cache.load()
	require.NoError(t, err)
	stamps := cache.snapshot.Load().stamps

	// Open an episode, then restore the keytab to the snapshotted revision.
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	_, err = cache.load()
	require.NoError(t, err)
	require.True(t, cache.degraded.Load())
	require.NoError(t, os.WriteFile(filename, original, 0o600))
	require.NoError(t, os.Chtimes(filename, info.ModTime(), info.ModTime()))

	// The first arrival closes it and announces.
	cache.endEpisodeIfCurrent(stamps)
	require.False(t, cache.degraded.Load())
	require.Equal(t, 1, strings.Count(logged.String(), "loads cleanly again"))

	// A second arrival, still holding the stale view that the episode was open,
	// must find nothing to do rather than announce again.
	cache.endEpisodeIfCurrent(stamps)
	require.Equal(t, 1, strings.Count(logged.String(), "loads cleanly again"),
		"a closed episode must not be announced twice")
}

// TestRecorderStatusRules covers how the recorder turns what SPNEGO wrote into
// a Fiber response. gokrb5 v8.4.4 always writes a status exactly once through
// http.Error, so these rules exist for a future version — or a caller's own
// wrapper — that does something else.
func TestRecorderStatusRules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		write      func(w http.ResponseWriter)
		wantStatus int
		wantBody   string
	}{
		{
			name: "the first status written wins",
			write: func(w http.ResponseWriter) {
				w.Header().Set(fiber.HeaderWWWAuthenticate, spnegoRejected)
				w.WriteHeader(http.StatusForbidden)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("denied"))
			},
			wantStatus: fiber.StatusForbidden,
			wantBody:   "denied",
		},
		{
			name:       "declining without writing anything is 401",
			write:      func(http.ResponseWriter) {},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name: "a body without a status is 200",
			write: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("body without a status"))
			},
			wantStatus: fiber.StatusOK,
			wantBody:   "body without a status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
				tc.write(w)
				return nil
			})
			ctx := serveProtected(t, Config{KeytabLookup: testKeytabLookup(t)})
			require.Equal(t, tc.wantStatus, ctx.Response.StatusCode())
			// Asserted unconditionally: the declining row expects an empty
			// body, and a "" that means "do not check" could not express that.
			require.Equal(t, tc.wantBody, string(ctx.Response.Body()))
		})
	}
}

// TestMergedKeytabIsSerialisable pins why readAll builds on keytab.New rather
// than the zero value: the zero value marshals with a version byte gokrb5's own
// loader rejects, and the merged keytab is handed to callers.
func TestMergedKeytabIsSerialisable(t *testing.T) {
	dir := t.TempDir()
	first := writeMockKeytab(t, dir, "one.keytab", "HTTP/one.example.com")
	second := writeMockKeytab(t, dir, "two.keytab", "HTTP/two.example.com")

	fn, err := NewKeytabFileLookupFunc(first, second)
	require.NoError(t, err)
	merged, err := fn()
	require.NoError(t, err)

	out := path.Join(dir, "merged.keytab")
	file, err := os.OpenFile(out, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, err = merged.Write(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	reloaded, err := keytab.Load(out)
	require.NoError(t, err, "the merged keytab must be loadable by gokrb5")
	require.Len(t, utils.GetKeytabInfo(reloaded), 2)
}

// TestLogThrottleWindowBoundary pins the throttle's inclusive/exclusive edge,
// which stepping the clock well past the window leaves unspecified. The grace
// window's boundary is covered in TestKeytabStaleGraceExpires.
func TestLogThrottleWindowBoundary(t *testing.T) {
	now := time.Now()
	throttle := &logThrottle{every: 30 * time.Second, nowFn: func() time.Time { return now }}
	var runs int
	fire := func() { throttle.do(func() { runs++ }) }

	fire()
	require.Equal(t, 1, runs)

	now = now.Add(30*time.Second - time.Nanosecond)
	fire()
	require.Equal(t, 1, runs, "one tick short of the window it is still closed")

	now = now.Add(time.Nanosecond)
	fire()
	require.Equal(t, 2, runs, "at exactly the window it reopens")
}

// TestUnderLockMatchClosesEpisode covers load's under-lock snapshot match. A
// loader whose pre-lock stat saw the broken revision, but which finds the
// cached one back on disk once it holds the lock, must close the episode —
// otherwise the flag stays set and every later cache hit pays for a full stat
// under the reload mutex.
func TestUnderLockMatchClosesEpisode(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	dir := t.TempDir()
	filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
	original, err := os.ReadFile(filename)
	require.NoError(t, err)
	info, err := os.Stat(filename)
	require.NoError(t, err)

	cache := newKeytabFileCache([]string{filename})
	_, err = cache.load()
	require.NoError(t, err)

	// Open an episode on a broken revision.
	require.NoError(t, os.WriteFile(filename, []byte("12"), 0o600))
	_, err = cache.load()
	require.NoError(t, err)
	require.True(t, cache.degraded.Load())

	// A loader stats the broken revision, then blocks while the keytab is
	// restored byte-for-byte — same size, mtime and inode-preserving rewrite.
	cache.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.load()
	}()
	// Waited for rather than slept on: a loader that had not yet stat'd would
	// see the restored revision on its pre-lock stat, match the snapshot and
	// close the episode through the lock-free path instead — which satisfies
	// both assertions below without the under-lock branch running at all.
	waitUntilBlockedOnMutex(t, "(*keytabFileCache).load(")
	require.NoError(t, os.WriteFile(filename, original, 0o600))
	require.NoError(t, os.Chtimes(filename, info.ModTime(), info.ModTime()))
	cache.mu.Unlock()
	<-done

	require.False(t, cache.degraded.Load(),
		"a revision matching the snapshot under the lock must close the episode")
	require.Contains(t, logged.String(), "loads cleanly again")
}

// TestFileStampTracksSizeAndModTime pins each half of the change signal. Every
// other rotation in the suite moves both at once, so either could be dropped
// unnoticed — and each covers a case the README promises is caught.
func TestFileStampTracksSizeAndModTime(t *testing.T) {
	t.Run("a same-size rewrite is caught by modification time", func(t *testing.T) {
		dir := t.TempDir()
		first := writeMockKeytab(t, dir, "one.keytab", "HTTP/aaa.example.com")
		second := writeMockKeytab(t, dir, "two.keytab", "HTTP/bbb.example.com")
		firstBytes, err := os.ReadFile(first)
		require.NoError(t, err)
		secondBytes, err := os.ReadFile(second)
		require.NoError(t, err)
		require.Len(t, secondBytes, len(firstBytes), "the principals must be the same length")

		target := path.Join(dir, "sso.keytab")
		require.NoError(t, os.WriteFile(target, firstBytes, 0o600))
		cache := newKeytabFileCache([]string{target})
		before, err := cache.load()
		require.NoError(t, err)

		// Same size, different bytes, different mtime.
		require.NoError(t, os.WriteFile(target, secondBytes, 0o600))
		require.NoError(t, os.Chtimes(target, time.Now().Add(time.Second), time.Now().Add(time.Second)))
		after, err := cache.load()
		require.NoError(t, err)
		require.NotSame(t, before, after, "a same-size rewrite must be detected by mtime")
	})

	t.Run("a same-mtime rewrite is caught by size", func(t *testing.T) {
		dir := t.TempDir()
		single := writeMockKeytab(t, dir, "one.keytab", "HTTP/aaa.example.com")
		singleBytes, err := os.ReadFile(single)
		require.NoError(t, err)

		target := path.Join(dir, "sso.keytab")
		require.NoError(t, os.WriteFile(target, singleBytes, 0o600))
		cache := newKeytabFileCache([]string{target})
		before, err := cache.load()
		require.NoError(t, err)
		info, err := os.Stat(target)
		require.NoError(t, err)

		// A two-entry keytab is longer; restore the original mtime so only the
		// size differs.
		twoEntry := writeMockKeytab(t, dir, "two.keytab", "HTTP/a-much-longer-principal.example.com")
		twoBytes, err := os.ReadFile(twoEntry)
		require.NoError(t, err)
		require.NotEqual(t, len(singleBytes), len(twoBytes))
		require.NoError(t, os.WriteFile(target, twoBytes, 0o600))
		require.NoError(t, os.Chtimes(target, info.ModTime(), info.ModTime()))

		after, err := cache.load()
		require.NoError(t, err)
		require.NotSame(t, before, after, "a same-mtime rewrite must be detected by size")
	})
}

// testKeytabLookup returns a lookup over a throwaway mock keytab.
func testKeytabLookup(t *testing.T) KeytabLookupFunc {
	t.Helper()
	filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")
	lookup, err := NewKeytabFileLookupFunc(filename)
	require.NoError(t, err)
	return lookup
}

// serveProtected builds a middleware from cfg, puts it in front of one route,
// and drives a single GET through it, returning the response context.
func serveProtected(t *testing.T, cfg Config, decorate ...func(*fasthttp.RequestCtx)) *fasthttp.RequestCtx {
	t.Helper()
	middleware, err := New(cfg)
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	for _, d := range decorate {
		d(ctx)
	}
	app.Handler()(ctx)
	return ctx
}
