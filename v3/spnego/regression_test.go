package spnego

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/credentials"
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
		got = requestForSPNEGO(c, false)
		return nil
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodPatch)
	ctx.Request.SetRequestURI("/a%2Fb/c%20d?q=1&x=%41")
	ctx.Request.Header.Set(fiber.HeaderHost, "sso.example.com:8443")
	ctx.Request.Header.Set(fiber.HeaderAuthorization, "Negotiate abc")
	ctx.Request.Header.Set(fiber.HeaderCookie, "session=abc123")
	ctx.SetRemoteAddr(&net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 55123})
	app.Handler()(ctx)

	require.NotNil(t, got)
	require.Equal(t, fiber.MethodPatch, got.Method)
	require.Equal(t, "Negotiate abc", got.Header.Get(fiber.HeaderAuthorization))
	// Cookies are withheld unless a session manager needs them, so gokrb5 is not
	// handed every cookie the site sets on requests that have no use for them.
	require.Empty(t, got.Header.Get(fiber.HeaderCookie),
		"cookies must not be forwarded without a session manager")
	// Host sits behind the same gate: nothing reads it without a session
	// manager, and asking Fiber for it walks the headers once TrustProxy is on.
	require.Empty(t, got.Host, "the host is only for a session manager")

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

	// URL carries at most a scheme — and not even that here, since this request
	// is built without a session manager. It is never a reconstruction of the
	// target: the path could not be faithful for every request, so nothing in
	// it should claim to describe one.
	require.NotNil(t, got.URL)
	require.Empty(t, got.URL.Path)
	require.Empty(t, got.URL.RawQuery)
	// Scheme and TLS serve a session manager and nothing else, so a request
	// built without one pays for neither. Reading the TLS state copies and
	// heap-allocates a tls.ConnectionState, and ctx.Scheme walks every header
	// once TrustProxy is on — per request, for a field gokrb5 never looks at.
	require.Empty(t, got.URL.Scheme,
		"the scheme is only for a session manager")
	require.Nil(t, got.TLS,
		"the TLS state is only for a session manager")
}

// TestRequestForSPNEGOHasANonNilBody pins the invariant net/http states for
// server requests. A Config.SessionManager is ordinary net/http code and may
// hold the usual "defer r.Body.Close()"; a nil Body turns that into a panic
// inside the authentication middleware, and Fiber installs no recover by
// default, so the connection would simply drop.
func TestRequestForSPNEGOHasANonNilBody(t *testing.T) {
	build := func(t *testing.T, forSessionManager bool) *http.Request {
		t.Helper()
		var got *http.Request
		app := fiber.New()
		app.All("/*", func(c fiber.Ctx) error {
			got = requestForSPNEGO(c, forSessionManager)
			return nil
		})
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/authenticate")
		ctx.Request.Header.Set(fiber.HeaderHost, "sso.example.com")
		app.Handler()(ctx)
		require.NotNil(t, got)
		return got
	}

	// Both shapes: the field is set on the literal, so a future refactor that
	// moves it behind the session-manager gate would leave the other nil.
	for _, forSessionManager := range []bool{false, true} {
		got := build(t, forSessionManager)
		require.NotNil(t, got.Body, "net/http guarantees a server request has a body")
		require.NotPanics(t, func() {
			require.NoError(t, got.Body.Close(), "the idiom a manager will use")
		})
		read, err := io.ReadAll(got.Body)
		require.NoError(t, err)
		require.Empty(t, read, "there is no body to offer; fasthttp's is not an io.ReadCloser")
	}
}

// TestRequestForSPNEGOCopiesOutOfTheRequestBuffer pins that nothing handed to a
// session manager points back into fasthttp's storage.
//
// Fiber returns header and URI strings as views into the request buffer unless
// Config.Immutable is set, and fasthttp reuses that buffer for the next request
// on the same worker. A manager that keeps r.Host — ordinary enough for one
// recording which host it issued a session for — would then read back another
// connection's bytes, and write a session record naming the wrong client.
//
// The property is asserted on the strings' storage rather than by serving a
// second request and looking for corruption. That is the aliasing itself and not
// a reproduction of it: whether a reused buffer happens to land on the same
// bytes depends on fasthttp's pooling, so a value test would pass or fail for
// reasons unrelated to this middleware.
func TestRequestForSPNEGOCopiesOutOfTheRequestBuffer(t *testing.T) {
	raw := "POST /authenticate HTTP/1.1\r\n" +
		"Host: sso.example.com:8443\r\n" +
		"Authorization: Negotiate dGlja2V0\r\n" +
		"X-Forwarded-Proto: https\r\n" +
		"Cookie: spnego-session=opaque\r\n\r\n"
	// Host carries a port on purpose: net/http documents a server request's
	// Host as "host or host:port", so it must be Fiber's Host and not its
	// Hostname, which drops the port. A session manager scoping sessions by
	// r.Host would otherwise conflate two ports on one name, and one calling
	// net.SplitHostPort would get "missing port in address".

	ctx := &fasthttp.RequestCtx{}
	require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))
	ctx.SetRemoteAddr(&net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 55123})

	var got *http.Request
	// TrustProxy on, because that is the only configuration where Scheme comes
	// out of the request buffer at all: without it Fiber answers with a
	// constant, and the check on Scheme below would hold whatever it liked.
	app := fiber.New(fiber.Config{
		TrustProxy: true,
		TrustProxyConfig: fiber.TrustProxyConfig{
			Proxies: []string{"203.0.113.7"},
		},
	})
	app.All("/*", func(c fiber.Ctx) error {
		got = requestForSPNEGO(c, true)
		return nil
	})
	app.Handler()(ctx)
	require.NotNil(t, got)

	require.Equal(t, "sso.example.com:8443", got.Host,
		"the port must survive: this is Host, not Hostname")
	require.Equal(t, "Negotiate dGlja2V0", got.Header.Get(fiber.HeaderAuthorization))
	require.Equal(t, "https", got.URL.Scheme,
		"the proxy must be trusted, or Scheme is a constant and proves nothing")

	// Every byte range Fiber could have handed out a view of. A copied string
	// starts outside all of them; an aliasing one starts inside its own.
	buffers := [][]byte{
		ctx.Request.Header.Peek(fiber.HeaderAuthorization),
		ctx.Request.Header.Peek(fiber.HeaderHost),
		ctx.Request.Header.Peek(fiber.HeaderXForwardedProto),
		ctx.Request.URI().Host(),
		ctx.Request.URI().FullURI(),
		ctx.Request.Header.Method(),
		ctx.Request.Header.Protocol(),
		ctx.Request.Header.Header(),
	}
	for _, tc := range []struct{ field, value string }{
		// Method is here despite not being cloned: Fiber answers it from its
		// own table of constants, and if that ever stops being true this is
		// where it shows up.
		{field: "Method", value: got.Method},
		{field: "Host", value: got.Host},
		{field: "the Authorization value", value: got.Header.Get(fiber.HeaderAuthorization)},
		{field: "URL.Scheme", value: got.URL.Scheme},
		{field: "Proto", value: got.Proto},
		{field: "the Cookie value", value: got.Header.Get(fiber.HeaderCookie)},
		{field: "RemoteAddr", value: got.RemoteAddr},
	} {
		require.NotEmpty(t, tc.value, "%s must be set, or the check below is vacuous", tc.field)
		for _, buffer := range buffers {
			// Not require: every field is worth reporting, and stopping at the
			// first would hide how many of them alias.
			if sharesStorage(tc.value, buffer) {
				t.Errorf("%s aliases the request buffer; a session manager that keeps it "+
					"would read back the next request's bytes", tc.field)
				break
			}
		}
	}

	// Without a session manager nothing outlives the handler, so the copy is not
	// made — a Kerberos Authorization header runs to kilobytes and arrives as
	// often as an unauthenticated caller cares to send one. Asserted rather than
	// assumed, because the gate is otherwise invisible from the outside.
	//
	// This is the one assertion here that pins an optimisation rather than a
	// safety property, so it is the one to delete rather than work around: if a
	// gokrb5 upgrade starts retaining the request, copy unconditionally and drop
	// this. The half above is what must keep holding.
	var bare *http.Request
	bareCtx := &fasthttp.RequestCtx{}
	require.NoError(t, bareCtx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))
	bareApp := fiber.New()
	bareApp.All("/*", func(c fiber.Ctx) error {
		bare = requestForSPNEGO(c, false)
		return nil
	})
	bareApp.Handler()(bareCtx)
	require.NotNil(t, bare)

	require.True(t,
		sharesStorage(bare.Header.Get(fiber.HeaderAuthorization),
			bareCtx.Request.Header.Peek(fiber.HeaderAuthorization)),
		"without a session manager the Authorization value has no reason to be copied; "+
			"if gokrb5 now retains the request, clone it unconditionally and delete this")
	require.Empty(t, bare.Host, "the host is only for a session manager")
}

// sharesStorage reports whether s begins inside b, which is what a string
// handed out as a view of a request buffer looks like. Comparing the start is
// enough: Fiber hands out whole values, never a copy that happens to land in
// the middle of one.
func sharesStorage(s string, b []byte) bool {
	if len(s) == 0 || len(b) == 0 {
		return false
	}
	start := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	low := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	return start >= low && start < low+uintptr(len(b))
}

// TestRequestForSPNEGOCarriesFibersContext pins the context a session manager
// gets. A manager is ordinary net/http code, so its store call is likely to be
// a QueryContext or an outbound request taking r.Context(); left unset that is
// context.Background(), which means no deadline, no cancellation when the
// client disconnects, and nothing joined to the application's trace. A session
// store that hangs would then hold the request goroutine for its own driver
// timeout rather than for Fiber's.
func TestRequestForSPNEGOCarriesFibersContext(t *testing.T) {
	type key struct{}

	build := func(t *testing.T, forSessionManager bool) *http.Request {
		t.Helper()
		var got *http.Request
		app := fiber.New()
		app.All("/*", func(c fiber.Ctx) error {
			c.SetContext(context.WithValue(context.Background(), key{}, "from the application"))
			got = requestForSPNEGO(c, forSessionManager)
			return nil
		})
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/authenticate")
		ctx.Request.Header.Set(fiber.HeaderHost, "sso.example.com")
		app.Handler()(ctx)
		require.NotNil(t, got)
		return got
	}

	require.Equal(t, "from the application", build(t, true).Context().Value(key{}),
		"a session manager must reach its store under the application's context")

	// Not carried without one, like everything else only a manager reads:
	// WithContext shallow-copies the whole request, and nothing on that path
	// would look at it.
	require.Nil(t, build(t, false).Context().Value(key{}),
		"the context is only for a session manager")
}

// TestRequestForSPNEGOStatesTheProtocol pins Proto against its numbers. A
// manager that compares ProtoAtLeast with Proto must not be told two different
// things, and fasthttp serves HTTP/1.0 as well as 1.1.
//
// It also pins the gate. A session manager is the only reader — gokrb5 looks at
// none of Proto, Scheme or TLS — so a request built without one must not pay to
// copy the protocol out of the request buffer and parse it.
func TestRequestForSPNEGOStatesTheProtocol(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		wantProto   string
		wantMajor   int
		wantMinor   int
		wantAtLeast bool
	}{
		{
			raw:       "GET /authenticate HTTP/1.1\r\nHost: sso.example.com\r\n\r\n",
			wantProto: "HTTP/1.1", wantMajor: 1, wantMinor: 1, wantAtLeast: true,
		},
		{
			raw:       "GET /authenticate HTTP/1.0\r\nHost: sso.example.com\r\n\r\n",
			wantProto: "HTTP/1.0", wantMajor: 1, wantMinor: 0, wantAtLeast: false,
		},
	} {
		t.Run(tc.wantProto, func(t *testing.T) {
			build := func(forSessionManager bool) *http.Request {
				ctx := &fasthttp.RequestCtx{}
				require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(tc.raw))))

				var got *http.Request
				app := fiber.New()
				app.All("/*", func(c fiber.Ctx) error {
					got = requestForSPNEGO(c, forSessionManager)
					return nil
				})
				app.Handler()(ctx)
				require.NotNil(t, got)
				return got
			}

			got := build(true)
			require.Equal(t, tc.wantProto, got.Proto)
			require.Equal(t, tc.wantMajor, got.ProtoMajor)
			require.Equal(t, tc.wantMinor, got.ProtoMinor)
			require.Equal(t, tc.wantAtLeast, got.ProtoAtLeast(1, 1),
				"the numbers must agree with the string")
			// RequestURI stays empty along with the rest of the path: it cannot
			// be reconstructed faithfully, so it is not claimed.
			require.Empty(t, got.RequestURI)

			bare := build(false)
			require.Empty(t, bare.Proto, "the protocol is only for a session manager")
			require.Zero(t, bare.ProtoMajor)
			require.Zero(t, bare.ProtoMinor)
		})
	}
}

// TestRequestForSPNEGOForwardsCookiesForSessionManager covers the other half of
// that rule. A session manager reads its session out of the cookies, so
// withholding them would make every session lookup miss and quietly reduce the
// feature to full validation on every request.
func TestRequestForSPNEGOForwardsCookiesForSessionManager(t *testing.T) {
	var got *http.Request
	app := fiber.New()
	app.All("/*", func(c fiber.Ctx) error {
		got = requestForSPNEGO(c, true)
		return nil
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	ctx.Request.Header.Set(fiber.HeaderCookie, "session=abc123")
	app.Handler()(ctx)

	require.NotNil(t, got)
	require.Equal(t, "session=abc123", got.Header.Get(fiber.HeaderCookie))
	cookie, err := (&http.Request{Header: got.Header}).Cookie("session")
	require.NoError(t, err, "gokrb5's session manager reads cookies off this request")
	require.Equal(t, "abc123", cookie.Value)
}

// TestRequestForSPNEGOForwardsEveryCookie covers a client that sends more than
// one Cookie line.
//
// The header is parsed from a raw request rather than built with Add, because
// the two behave differently and only the raw form reproduces the bug: Add
// merges into one line, so Peek returns everything, while two real lines leave
// Peek returning just the first until something in the chain makes fasthttp
// collect the rest. Copying the raw header would therefore forward a subset
// that varies with whatever unrelated middleware ran earlier, and a session
// cookie on the second line would go missing — which reads as "no session" and
// mints a fresh one per request.
func TestRequestForSPNEGOForwardsEveryCookie(t *testing.T) {
	raw := "GET /authenticate HTTP/1.1\r\n" +
		"Host: sso.example.com\r\n" +
		"Cookie: unrelated=1\r\n" +
		"Cookie: spnego-session=opaque\r\n\r\n"

	ctx := &fasthttp.RequestCtx{}
	require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))
	require.Equal(t, "unrelated=1", string(ctx.Request.Header.Peek(fiber.HeaderCookie)),
		"the fixture must actually reproduce the split-header case")

	var got *http.Request
	app := fiber.New()
	app.All("/*", func(c fiber.Ctx) error {
		got = requestForSPNEGO(c, true)
		return nil
	})
	app.Handler()(ctx)

	require.NotNil(t, got)
	forwarded := &http.Request{Header: got.Header}
	session, err := forwarded.Cookie("spnego-session")
	require.NoError(t, err, "a session cookie on a later line must still arrive")
	require.Equal(t, "opaque", session.Value)
	unrelated, err := forwarded.Cookie("unrelated")
	require.NoError(t, err)
	require.Equal(t, "1", unrelated.Value)
}

// TestRequestForSPNEGODropsValuelessCookies covers a cookie sent with no "=".
// fasthttp parses it as an empty key with the text as the value, which leaves
// the rebuild no way to render it faithfully: emitting "=flag" gives net/http
// an invalid name to drop, and emitting "flag" invents a cookie named "flag"
// that the client never sent. Neither can be looked up by name, which is all a
// session manager does with them, so it is dropped instead — forwarding either
// form could only mislead.
func TestRequestForSPNEGODropsValuelessCookies(t *testing.T) {
	raw := "GET /authenticate HTTP/1.1\r\n" +
		"Host: sso.example.com\r\n" +
		"Cookie: legacyflag\r\n" +
		"Cookie: spnego-session=opaque\r\n\r\n"

	ctx := &fasthttp.RequestCtx{}
	require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))

	var got *http.Request
	app := fiber.New()
	app.All("/*", func(c fiber.Ctx) error {
		got = requestForSPNEGO(c, true)
		return nil
	})
	app.Handler()(ctx)

	require.NotNil(t, got)
	forwarded := got.Header.Get(fiber.HeaderCookie)
	require.NotContains(t, forwarded, "legacyflag",
		"a nameless cookie cannot be looked up, so forwarding it can only mislead")
	require.NotContains(t, forwarded, "=legacyflag")
	// And the real session cookie still survives alongside it.
	session, err := (&http.Request{Header: got.Header}).Cookie("spnego-session")
	require.NoError(t, err)
	require.Equal(t, "opaque", session.Value)
}

// TestRequestForSPNEGODropsLeadingEqualsCookies is the other half of that rule.
// fasthttp parses "Cookie: =sneaky" to the same empty key as a bare flag, so
// rendering it as "sneaky" would hand the session manager a cookie named
// "sneaky" that the client never sent — net/http drops it from the wire form
// for having an invalid name.
func TestRequestForSPNEGODropsLeadingEqualsCookies(t *testing.T) {
	raw := "GET /authenticate HTTP/1.1\r\n" +
		"Host: sso.example.com\r\n" +
		"Cookie: =sneaky\r\n" +
		"Cookie: spnego-session=opaque\r\n\r\n"

	ctx := &fasthttp.RequestCtx{}
	require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))

	var got *http.Request
	app := fiber.New()
	app.All("/*", func(c fiber.Ctx) error {
		got = requestForSPNEGO(c, true)
		return nil
	})
	app.Handler()(ctx)

	require.NotNil(t, got)
	_, err := (&http.Request{Header: got.Header}).Cookie("sneaky")
	require.Error(t, err, "a cookie the client never named must not be invented")
	session, err := (&http.Request{Header: got.Header}).Cookie("spnego-session")
	require.NoError(t, err)
	require.Equal(t, "opaque", session.Value)
}

// TestRequestForSPNEGOSkipsNamelessCookiesWithoutLeavingAGap covers the
// nameless cookie arriving somewhere other than first, which is where dropping
// it and writing the separator can go wrong independently.
//
// Both other nameless-cookie tests put it at the head, where the separator is
// not written at all, so neither notices a separator emitted before the skip.
// That ordering yields "a=1; ; b=2" — a stray empty element every cookie parser
// downstream then has to make sense of.
func TestRequestForSPNEGOSkipsNamelessCookiesWithoutLeavingAGap(t *testing.T) {
	raw := "GET /authenticate HTTP/1.1\r\n" +
		"Host: sso.example.com\r\n" +
		"Cookie: first=one\r\n" +
		"Cookie: =sneaky\r\n" +
		"Cookie: last=two\r\n\r\n"

	ctx := &fasthttp.RequestCtx{}
	require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))

	var got *http.Request
	app := fiber.New()
	app.All("/*", func(c fiber.Ctx) error {
		got = requestForSPNEGO(c, true)
		return nil
	})
	app.Handler()(ctx)

	require.NotNil(t, got)
	// Asserted on the header text rather than on parsed cookies: Go's parser
	// discards an empty element silently, so it would report the same cookies
	// either way and the gap would survive.
	require.Equal(t, "first=one; last=two", got.Header.Get("Cookie"))

	named := &http.Request{Header: got.Header}
	for name, want := range map[string]string{"first": "one", "last": "two"} {
		cookie, err := named.Cookie(name)
		require.NoError(t, err)
		require.Equal(t, want, cookie.Value)
	}
	_, err := named.Cookie("sneaky")
	require.Error(t, err, "a cookie the client never named must not be invented")
}

// TestRequestForSPNEGOCarriesSchemeAndTLS pins the two fields a session store
// reads to decide whether its cookie may be marked Secure. Leaving them unset
// would issue the session — which the middleware treats as a credential —
// without that attribute on an HTTPS listener.
//
// The TLS case needs a context backed by a real *tls.Conn: fasthttp derives
// TLSConnectionState from the connection, so nothing short of one distinguishes
// "read from the connection" from "hardcoded nil".
func TestRequestForSPNEGOCarriesSchemeAndTLS(t *testing.T) {
	build := func(t *testing.T, ctx *fasthttp.RequestCtx) *http.Request {
		t.Helper()
		var got *http.Request
		app := fiber.New()
		app.All("/*", func(c fiber.Ctx) error {
			got = requestForSPNEGO(c, true)
			return nil
		})
		ctx.Request.Header.SetMethod(fiber.MethodGet)
		ctx.Request.SetRequestURI("/authenticate")
		app.Handler()(ctx)
		require.NotNil(t, got)
		return got
	}

	t.Run("plaintext", func(t *testing.T) {
		got := build(t, &fasthttp.RequestCtx{})
		require.NotNil(t, got.URL)
		require.Equal(t, "http", got.URL.Scheme)
		require.Nil(t, got.TLS, "a plaintext connection must not look secure")
	})

	t.Run("tls", func(t *testing.T) {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
		// Never handshaken, which is fine: ConnectionState is readable either
		// way, and what is under test is that the field is taken from the
		// connection at all.
		ctx := &fasthttp.RequestCtx{}
		ctx.Init2(tls.Server(server, &tls.Config{MinVersion: tls.VersionTLS12}), nil, true)

		got := build(t, ctx)
		require.NotNil(t, got.TLS, "a TLS connection must be visible to a session store")
	})
}

// TestServiceSettingsPassesOnlyWhatWasSet pins the translation from Config into
// gokrb5's options. The zero value of each field has to leave gokrb5's own
// default standing rather than pinning a number here — MaxClockSkew in
// particular resolves to five minutes inside gokrb5, and PAC decoding is on
// unless it is explicitly turned off.
func TestServiceSettingsPassesOnlyWhatWasSet(t *testing.T) {
	t.Run("an empty config sets nothing", func(t *testing.T) {
		opts := serviceSettings(Config{})
		settings := service.NewSettings(nil, opts...)

		// This is where the spare capacity was largest, and it is why the slice
		// is clipped: it goes to gokrb5 variadically and is closed over by the
		// handler, so its backing array is shared by every concurrent request.
		// A gokrb5 that appended to it rather than to a fresh slice of its own
		// would be writing into that array from several goroutines at once.
		require.Equal(t, len(opts), cap(opts),
			"a config that sets nothing must not hand gokrb5 room to append into")

		require.Empty(t, settings.KeytabPrincipal())
		require.Equal(t, 5*time.Minute, settings.MaxClockSkew(),
			"a zero skew must leave gokrb5's default, not pin one here")
		require.True(t, settings.DecodePAC(),
			"PAC decoding is what populates group SIDs; it must stay on by default")
		require.False(t, settings.RequireHostAddr())
		require.Nil(t, settings.SessionManager())
		require.Nil(t, settings.Logger())
	})

	t.Run("every field reaches gokrb5", func(t *testing.T) {
		manager := &recordingSessionManager{}
		logger := log.New(io.Discard, "", 0)
		opts := serviceSettings(Config{
			Log:                logger,
			KeytabPrincipal:    "HTTP/other.example.com",
			MaxClockSkew:       90 * time.Second,
			DisablePACDecoding: true,
			RequireHostAddress: true,
			SessionManager:     manager,
		})
		settings := service.NewSettings(nil, opts...)

		// SName is deliberately never set: gokrb5's SPNEGO path does not read
		// it, so wiring it up would promise a restriction that does nothing.
		require.Empty(t, settings.SName(),
			"SName must stay unset — VerifyAPREQ ignores it")
		// gokrb5 parses the override into a PrincipalName rather than keeping
		// the string, so the assertion has to meet it there.
		require.NotNil(t, settings.KeytabPrincipal())
		require.Equal(t, []string{"HTTP", "other.example.com"},
			settings.KeytabPrincipal().NameString)
		require.Equal(t, 90*time.Second, settings.MaxClockSkew())
		require.False(t, settings.DecodePAC())
		require.True(t, settings.RequireHostAddr())
		// Wrapped rather than passed through, so that a New which refuses is
		// known as a fact instead of being read back out of the response the
		// manager itself was just holding. The caller's manager has to be
		// reachable underneath, or nothing it stores would be stored.
		probe, ok := settings.SessionManager().(sessionManagerProbe)
		require.True(t, ok, "the session manager must be wrapped in the probe")
		require.Same(t, manager, probe.delegate)
		require.Same(t, logger, settings.Logger())
	})
}

// recordingSessionManager is a gokrb5 service.SessionMgr that keeps what it was
// handed, so a test can tell whether the middleware wired it up at all.
type recordingSessionManager struct {
	mu     sync.Mutex
	stored []byte
	gets   int
}

// New satisfies service.SessionMgr and nothing more. gokrb5 calls it only from
// newSession, which runs after a ticket has been validated — so reaching it
// needs a KDC, and no test here can. Tests that need the manager-writes-a-cookie
// behaviour drive it through the stub instead, which is where gokrb5 would have
// written one.
func (m *recordingSessionManager) New(_ http.ResponseWriter, _ *http.Request, _ string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stored = v
	return nil
}

func (m *recordingSessionManager) Get(_ *http.Request, _ string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	return m.stored, nil
}

// getCalls reports how often gokrb5 consulted the session, which is what shows
// the manager was wired through at all.
func (m *recordingSessionManager) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gets
}

// TestOnSuccessSeesTheIdentity covers the success hook. It runs before the rest
// of the chain, so a slow downstream handler cannot delay a metric, and it is
// handed the same identity the handler will read.
func TestOnSuccessSeesTheIdentity(t *testing.T) {
	user := goidentity.NewUser("alice")
	user.SetDomain("EXAMPLE.LOCAL")
	stubAuthenticate(t, func(http.ResponseWriter, *http.Request) goidentity.Identity {
		return &user
	})

	var (
		order    []string
		seen     goidentity.Identity
		hookRuns int
	)
	middleware, err := New(Config{
		KeytabLookup: testKeytabLookup(t),
		OnSuccess: func(_ fiber.Ctx, identity goidentity.Identity) {
			hookRuns++
			seen = identity
			order = append(order, "hook")
		},
	})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		order = append(order, "handler")
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	app.Handler()(ctx)

	require.Equal(t, 1, hookRuns, "the hook must run once per authenticated request")
	require.NotNil(t, seen)
	require.Equal(t, "alice", seen.UserName())
	require.Equal(t, []string{"hook", "handler"}, order,
		"the hook must not wait on the rest of the chain")
}

// TestOnSuccessNotCalledWithoutAuthentication keeps the hook honest: a
// challenge, a continuation and a rejection are all not-authenticated, and a
// metric that counted them would overstate successful logins.
func TestOnSuccessNotCalledWithoutAuthentication(t *testing.T) {
	var hookRuns int
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		OnSuccess:    func(fiber.Ctx, goidentity.Identity) { hookRuns++ },
	})

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Zero(t, hookRuns, "an opening challenge is not a successful authentication")
}

// TestOnErrorReceivesTheInternalCause covers the failure hook. It gets the
// wrapped cause rather than the client-safe wrapper, since a hook handed
// "Internal Server Error" would have nothing to report.
func TestOnErrorReceivesTheInternalCause(t *testing.T) {
	secretPath := path.Join(t.TempDir(), "super-secret-location.keytab")
	var (
		hookErr  error
		hookRuns int
	)
	ctx := serveProtected(t, Config{
		KeytabLookup: func() (*keytab.Keytab, error) {
			return nil, fmt.Errorf("open %s: permission denied", secretPath)
		},
		OnError: func(_ fiber.Ctx, err error) {
			hookRuns++
			hookErr = err
		},
	})

	require.Equal(t, 1, hookRuns)
	require.ErrorIs(t, hookErr, ErrLookupKeytabFailed)
	require.ErrorContains(t, hookErr, secretPath,
		"the hook is internal diagnostics and needs the cause")
	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode())
	require.NotContains(t, string(ctx.Response.Body()), secretPath,
		"handing the cause to the hook must not also hand it to the client")
}

// TestOnErrorNotCalledOnAuthenticationFailure separates the two kinds of
// not-authenticated. A rejected ticket is Unauthorized's business; treating it
// as an internal error would page whoever watches OnError for every bad client.
func TestOnErrorNotCalledOnAuthenticationFailure(t *testing.T) {
	var hookRuns int
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		OnError:      func(fiber.Ctx, error) { hookRuns++ },
	}, func(ctx *fasthttp.RequestCtx) {
		ctx.Request.Header.Set(fiber.HeaderAuthorization, rejectedNegotiateHeader(t))
	})

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Equal(t, spnegoRejected, string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)))
	require.Zero(t, hookRuns, "a refused ticket is not an internal failure")
}

// TestSessionManagerServesFromSessionWithoutATicket drives gokrb5's real
// session path. It does not stub authenticate: SPNEGOKRB5Authenticate consults
// the session manager before it looks at the Authorization header, so a request
// carrying no ticket at all is served from the session — which is the whole
// point of the feature, and the only part of it reachable without a KDC.
//
// What it covers is that service.SessionManager is passed through at all: this
// recordingSessionManager ignores the request, so cookie forwarding is not
// exercised here — TestCookieForwardingFollowsTheSessionManager does that.
func TestSessionManagerServesFromSessionWithoutATicket(t *testing.T) {
	established := credentials.New("alice", "EXAMPLE.LOCAL")
	established.SetAuthenticated(true)
	marshalled, err := established.Marshal()
	require.NoError(t, err)

	manager := &recordingSessionManager{stored: marshalled}
	middleware, err := New(Config{
		KeytabLookup:   testKeytabLookup(t),
		SessionManager: manager,
	})
	require.NoError(t, err)

	var seen goidentity.Identity
	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		seen, _ = GetAuthenticatedIdentityFromContext(c)
		return c.SendString("authenticated")
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	ctx.Request.Header.SetCookie("spnego-session", "opaque-session-id")
	app.Handler()(ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	require.Equal(t, "authenticated", string(ctx.Response.Body()))
	require.NotNil(t, seen, "the session identity must reach the handler")
	require.Equal(t, "alice", seen.UserName())

	require.Positive(t, manager.getCalls(),
		"gokrb5 must have consulted the session manager")

	// The premise copyHeadersTo's comment rests on, checked against live gokrb5
	// rather than asserted in prose: a resumed session carries no
	// accept-completed token, because the token proves an exchange and this
	// request had none. The manager cannot have written one either — the resume
	// path calls only Get, which is handed the request and not the writer. So
	// there is nothing at all to replay here, and a gokrb5 that started sending
	// a token before serving an established session would land right here.
	require.Empty(t, ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate),
		"a session served without an exchange has no token to prove one")
}

// TestSPNEGOInternalFailureIsReportedNotSwallowed covers the 5xx the session
// feature makes reachable. gokrb5 answers 500 from inside its own handler when
// a session cannot be stored — spnegoInternalServerError, via newSession at
// spnego/http.go:365 and :371 — and returns without calling the inner handler.
// Replaying that as an ordinary response would leave a broken session store
// failing every authenticated request with nothing logged and OnError silent.
//
// The 500 is written by the stub rather than by a real session manager because
// gokrb5 only reaches newSession after validating a ticket, which needs a KDC.
// What is under test is this middleware's handling of a 5xx from the handler,
// which is the part it owns; the stub writes exactly what
// spnegoInternalServerError does.
func TestSPNEGOInternalFailureIsReportedNotSwallowed(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return nil
	})

	var (
		hookErr  error
		hookRuns int
		reached  bool
	)
	middleware, err := New(Config{
		KeytabLookup: testKeytabLookup(t),
		OnError: func(_ fiber.Ctx, err error) {
			hookRuns++
			hookErr = err
		},
	})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		reached = true
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	app.Handler()(ctx)

	require.False(t, reached, "a failing handler must not authenticate anyone")
	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode())
	require.Equal(t, 1, hookRuns, "a 5xx from inside SPNEGO must reach OnError")
	require.ErrorIs(t, hookErr, ErrSPNEGOHandlerFailed)
	require.Contains(t, logged.String(), "spnego:",
		"and must reach the log, not just the client")
}

// TestSessionCookieIsReplayedOnSuccess pins the promise Config.SessionManager's
// godoc and the README both make: whatever the manager writes reaches the
// client. A session that is stored but whose Set-Cookie never leaves the server
// is a session the client can never present, so every request would re-establish
// one.
//
// Nothing else in the suite looks at Set-Cookie. Deleting copyHeadersTo
// outright would also fail TestAuthenticatedRequestPropagatesIdentityAndError,
// which pins the accept-completed header, but moving it after the response is
// written or switching Add to Set would leave only this test to notice.
func TestSessionCookieIsReplayedOnSuccess(t *testing.T) {
	user := goidentity.NewUser("alice")
	stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
		// The order gokrb5 uses: the session cookie is written before the
		// accept-completed header, and both before the inner handler runs.
		http.SetCookie(w, &http.Cookie{Name: "spnego-session", Value: "opaque-session-id", Path: "/"})
		w.Header().Set(fiber.HeaderWWWAuthenticate, "Negotiate accepted")
		return &user
	})

	ctx := serveProtected(t, Config{
		KeytabLookup:   testKeytabLookup(t),
		SessionManager: &recordingSessionManager{},
	})

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	require.Contains(t, string(ctx.Response.Header.Peek(fiber.HeaderSetCookie)), "opaque-session-id",
		"a session the client never receives is a session it can never present")
	require.Equal(t, "Negotiate accepted", string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)))
}

// TestEveryValueOfARepeatedHeaderIsReplayed pins Add over Set in copyHeadersTo.
//
// Set-Cookie alone would not: fasthttp routes it through its special-header
// path, where Set appends like Add does, so it survives either way. Any other
// repeated header does not — a manager emitting two Vary lines before the
// request authenticates would have all but the last silently dropped, and the
// response would then be cached against the wrong key.
func TestEveryValueOfARepeatedHeaderIsReplayed(t *testing.T) {
	user := goidentity.NewUser("alice")
	stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
		w.Header().Add(fiber.HeaderVary, fiber.HeaderCookie)
		w.Header().Add(fiber.HeaderVary, fiber.HeaderAuthorization)
		w.Header().Set(fiber.HeaderWWWAuthenticate, "Negotiate accepted")
		return &user
	})

	ctx := serveProtected(t, Config{
		KeytabLookup:   testKeytabLookup(t),
		SessionManager: &recordingSessionManager{},
	})

	var vary []string
	for key, value := range ctx.Response.Header.All() {
		if string(key) == fiber.HeaderVary {
			vary = append(vary, string(value))
		}
	}
	require.ElementsMatch(t, []string{fiber.HeaderCookie, fiber.HeaderAuthorization}, vary,
		"every value of a repeated header must survive the replay, not just the last")
}

// TestSPNEGOInternalFailureDoesNotLeakTheHandlerBody covers what the 5xx branch
// withholds. gokrb5 hands the session manager the raw ResponseWriter, so a
// manager reporting its own failure — a DSN, a host, a driver error — has that
// captured by the recorder. Replaying it would hand an unauthenticated caller
// exactly what clientSafeError exists to keep from them.
//
// A Set-Cookie written before the failure is dropped for the same reason: it
// would advertise a session that was never stored.
func TestSPNEGOInternalFailureDoesNotLeakTheHandlerBody(t *testing.T) {
	flog.SetOutput(io.Discard)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	const secret = "postgres://user:hunter2@db.internal:5432/sessions"
	stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
		http.SetCookie(w, &http.Cookie{Name: "spnego-session", Value: "never-stored"})
		http.Error(w, "could not reach "+secret, http.StatusInternalServerError)
		return nil
	})

	var handlerErr error
	middleware, err := New(Config{
		KeytabLookup:   testKeytabLookup(t),
		SessionManager: &recordingSessionManager{},
	})
	require.NoError(t, err)

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, e error) error {
			handlerErr = e
			return fiber.DefaultErrorHandler(c, e)
		},
	})
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	app.Handler()(ctx)

	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode())
	require.NotContains(t, string(ctx.Response.Body()), secret,
		"the handler's own message must not reach the client")
	require.NotContains(t, string(ctx.Response.Body()), "hunter2")
	require.Equal(t, "Internal Server Error", string(ctx.Response.Body()))
	require.Empty(t, ctx.Response.Header.Peek(fiber.HeaderSetCookie),
		"a session that failed to store must not be advertised to the client")

	// Unlike the previous behaviour, which returned nil and never reached here.
	require.ErrorIs(t, handlerErr, ErrSPNEGOHandlerFailed,
		"the application's ErrorHandler must see it, as it does a keytab failure")
}

// TestSPNEGOHandlerFailureCaughtWhenItWritesBeforeFailing covers the shape a
// real session manager produces, and the one a 5xx-only test misses entirely.
//
// gokrb5 hands the manager the raw ResponseWriter, so a manager that reports
// its own trouble writes a body first and only then returns the error gokrb5
// turns into a 500. The recorder keeps the first status, so that Write pins it
// at 200 and the eventual 500 is dropped — leaving a request that never
// authenticated answered 200, with the manager's text and a Set-Cookie for a
// session that was never stored. Testing "not an authentication outcome"
// rather than "5xx" is what catches it.
func TestSPNEGOHandlerFailureCaughtWhenItWritesBeforeFailing(t *testing.T) {
	flog.SetOutput(io.Discard)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	const secret = "postgres://user:hunter2@db.internal:5432/sessions"
	stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
		http.SetCookie(w, &http.Cookie{Name: "spnego-session", Value: "never-stored"})
		// Body first — this is what pins the recorder at 200.
		_, _ = fmt.Fprintf(w, "could not reach %s", secret)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return nil
	})

	var hookRuns int
	ctx := serveProtected(t, Config{
		KeytabLookup:   testKeytabLookup(t),
		SessionManager: &recordingSessionManager{},
		OnError:        func(fiber.Ctx, error) { hookRuns++ },
	})

	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode(),
		"a request that did not authenticate must never be answered 200")
	require.Equal(t, "Internal Server Error", string(ctx.Response.Body()))
	require.NotContains(t, string(ctx.Response.Body()), "hunter2")
	require.Empty(t, ctx.Response.Header.Peek(fiber.HeaderSetCookie))
	require.Equal(t, 1, hookRuns)
}

// TestInternalFailureKindsThrottleIndependently pins that the two internal
// failures have separate throttles. Sharing one would let either suppress the
// other for a full window, so an operator chasing a broken session store could
// find only a keytab line and conclude the store was fine.
func TestInternalFailureKindsThrottleIndependently(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	// Fails once, then succeeds, so one request takes the keytab path and the
	// next reaches the SPNEGO handler.
	lookup := testKeytabLookup(t)
	var calls int
	failing := func() (*keytab.Keytab, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("keytab boom")
		}
		return lookup()
	}
	stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return nil
	})

	middleware, err := New(Config{KeytabLookup: failing})
	require.NoError(t, err)
	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	handler := app.Handler()
	for range 2 {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fiber.MethodGet)
		ctx.Request.SetRequestURI("/authenticate")
		handler(ctx)
	}

	// Both land inside one throttle window, so a shared throttle would show
	// only the first.
	require.Contains(t, logged.String(), "keytab boom",
		"the keytab failure must be logged")
	require.Contains(t, logged.String(), ErrSPNEGOHandlerFailed.Error(),
		"the handler failure must not be suppressed by the keytab one")
}

// TestChallengeIsNotAnInternalFailure keeps the 5xx rule from swallowing the
// ordinary outcomes: a 401 challenge must leave the log and OnError alone, or
// every unauthenticated request would look like a service fault.
func TestChallengeIsNotAnInternalFailure(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	var hookRuns int
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		OnError:      func(fiber.Ctx, error) { hookRuns++ },
	})

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Zero(t, hookRuns)
	require.NotContains(t, logged.String(), "spnego:")
}

// TestAuthenticatedIdentityCarriesADGroups pins the capability the README
// documents: against Active Directory, PAC decoding puts the caller's group
// SIDs on the identity, so Authorized answers group questions without the
// application parsing anything itself.
//
// The identity here is the *credentials.Credentials gokrb5 itself stores, so
// this exercises the real type rather than a stand-in. What the test cannot do
// is mint a PAC — that needs a KDC — so it sets the AD credentials the way
// gokrb5's own APExchange path does.
func TestAuthenticatedIdentityCarriesADGroups(t *testing.T) {
	const admins = "S-1-5-21-1004336348-1177238915-682003330-512"

	creds := credentials.New("alice", "EXAMPLE.LOCAL")
	creds.SetADCredentials(credentials.ADCredentials{
		EffectiveName:       "alice",
		LogonDomainName:     "EXAMPLE",
		GroupMembershipSIDs: []string{admins},
	})
	stubAuthenticate(t, func(http.ResponseWriter, *http.Request) goidentity.Identity {
		return creds
	})

	var (
		isAdmin bool
		isOther bool
		attrs   []string
	)
	middleware, err := New(Config{KeytabLookup: testKeytabLookup(t)})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		identity, ok := GetAuthenticatedIdentityFromContext(c)
		require.True(t, ok)
		isAdmin = identity.Authorized(admins)
		isOther = identity.Authorized("S-1-5-21-0000000000-0000000000-0000000000-999")
		attrs = identity.AuthzAttributes()
		return c.SendString("authenticated")
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	app.Handler()(ctx)

	require.True(t, isAdmin, "a group the caller is in must authorize")
	require.False(t, isOther, "a group the caller is not in must not")
	require.Contains(t, attrs, admins)
}

// TestCookieForwardingFollowsTheSessionManager covers the wiring between the
// two, which asserting on requestForSPNEGO directly cannot reach: that helper
// takes the decision as an argument, so only an end-to-end request shows
// whether Config.SessionManager is what decides it.
func TestCookieForwardingFollowsTheSessionManager(t *testing.T) {
	for _, tc := range []struct {
		name    string
		manager service.SessionMgr
		want    string
	}{
		{
			name:    "withheld without a session manager",
			manager: nil,
			want:    "",
		},
		{
			name:    "forwarded for a session manager",
			manager: &recordingSessionManager{},
			want:    "session=abc123",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var handed *http.Request
			stubAuthenticate(t, func(_ http.ResponseWriter, r *http.Request) goidentity.Identity {
				handed = r
				return nil
			})

			serveProtected(t, Config{
				KeytabLookup:   testKeytabLookup(t),
				SessionManager: tc.manager,
			}, func(ctx *fasthttp.RequestCtx) {
				ctx.Request.Header.Set(fiber.HeaderCookie, "session=abc123")
			})

			require.NotNil(t, handed, "the stub must have seen the built request")
			require.Equal(t, tc.want, handed.Header.Get(fiber.HeaderCookie))
		})
	}
}

// TestConfigDefault pins what the godoc promises: ConfigDefault equals the zero
// Config. New applies its defaults by treating a zero field as unset rather
// than by reading the variable, so the two diverging would leave the documented
// defaults describing something New never does.
//
// Compared whole rather than field by field. Naming fields individually is what
// let the previous version of this test check four of them and miss the rest —
// a later field with a non-zero default would have sailed through.
// reflect.DeepEqual, which require.Equal uses, treats two nil func values as
// equal, so the hooks compare cleanly.
func TestConfigDefault(t *testing.T) {
	require.Equal(t, Config{}, ConfigDefault)

	// Spelled out for the two whose default is load bearing rather than merely
	// conventional: a non-zero MaxClockSkew would override gokrb5's five
	// minutes, and a true DisablePACDecoding would strip the group SIDs behind
	// Identity.Authorized.
	require.Zero(t, ConfigDefault.MaxClockSkew)
	require.False(t, ConfigDefault.DisablePACDecoding)
}

// TestNewRejectsInvalidConfig covers the two settings that cannot be validated
// later. Both fail the same way if let through — every request refused, with
// the cause visible only if gokrb5 logging happens to be on — so they are
// caught while the caller still has a stack to blame.
func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Run("a negative clock skew", func(t *testing.T) {
		// Silently dropped by the > 0 guard otherwise, leaving gokrb5's five
		// minutes while the configuration says something tighter.
		_, err := New(Config{
			KeytabLookup: testKeytabLookup(t),
			MaxClockSkew: -30 * time.Second,
		})
		require.ErrorIs(t, err, ErrConfigInvalidOfNegativeMaxClockSkew)
	})

	t.Run("a keytab principal carrying a realm", func(t *testing.T) {
		// types.ParseSPNString drops everything after the "@" and reports
		// nothing, so this would pin a different principal than was written.
		_, err := New(Config{
			KeytabLookup:    testKeytabLookup(t),
			KeytabPrincipal: "HTTP/sso.example.com@EXAMPLE.LOCAL",
		})
		require.ErrorIs(t, err, ErrConfigInvalidOfKeytabPrincipalRealm)
	})

	t.Run("a bare keytab principal is accepted", func(t *testing.T) {
		_, err := New(Config{
			KeytabLookup:    testKeytabLookup(t),
			KeytabPrincipal: "HTTP/sso.example.com",
			MaxClockSkew:    30 * time.Second,
		})
		require.NoError(t, err)
	})
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
			// The recorder keeps the first status, and because a challenge
			// header is present this counts as an authentication outcome and is
			// replayed as written.
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
			// gokrb5 v8.4.4 always writes something, so this is the shape of a
			// handler that failed to produce a response at all rather than of
			// any authentication outcome — no challenge for the client to
			// answer, so nothing to pass through.
			name:       "declining without writing anything is a handler failure",
			write:      func(http.ResponseWriter) {},
			wantStatus: fiber.StatusInternalServerError,
			wantBody:   "Internal Server Error",
		},
		{
			// No challenge header, so whatever the status says this is not an
			// outcome the client can answer. It is the shape a session manager
			// produces when it writes its own message before failing.
			name: "a body without a challenge header is a handler failure",
			write: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("body without a status"))
			},
			wantStatus: fiber.StatusInternalServerError,
			wantBody:   "Internal Server Error",
		},
		{
			// The case both earlier status-based gates let through: a manager
			// picks its own 4xx before failing. Only the absent challenge
			// header distinguishes it.
			name: "a 4xx without a challenge header is a handler failure",
			write: func(w http.ResponseWriter) {
				http.Error(w, "session store rejected: dsn", http.StatusConflict)
			},
			wantStatus: fiber.StatusInternalServerError,
			wantBody:   "Internal Server Error",
		},
		{
			// A challenge header makes it an outcome, and it is replayed as
			// written even though the status is not 401.
			name: "a 4xx with a challenge header passes through",
			write: func(w http.ResponseWriter) {
				w.Header().Set(fiber.HeaderWWWAuthenticate, spnegoRejected)
				http.Error(w, "denied", http.StatusForbidden)
			},
			wantStatus: fiber.StatusForbidden,
			wantBody:   "denied\n",
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

// TestRecorderDefaultsToOKOnABareWrite asserts the recorder's own rule directly
// rather than through a served request. End to end, a handler that writes a
// body without a status is classified by its missing challenge header, so the
// response is the same 500 whatever default the recorder picked — which leaves
// the default itself unpinned. This is the only assertion that fails if it
// changes.
func TestRecorderDefaultsToOKOnABareWrite(t *testing.T) {
	var recorder responseRecorder

	require.Zero(t, recorder.status, "nothing written yet, so no status")

	n, err := recorder.Write([]byte("hello"))
	require.NoError(t, err)
	require.Equal(t, 5, n)
	require.Equal(t, http.StatusOK, recorder.status,
		"a body with no explicit status is a 200, as net/http defines it")
	require.Equal(t, "hello", recorder.body.String())

	// The implicit status is set once, like an explicit one: a second Write
	// appends to the body and leaves the status alone.
	_, err = recorder.Write([]byte(" again"))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.status)
	require.Equal(t, "hello again", recorder.body.String())

	// And an explicit status arriving after the body does not overwrite it,
	// which is what makes a manager's early Write mask gokrb5's later 500.
	recorder.WriteHeader(http.StatusInternalServerError)
	require.Equal(t, http.StatusOK, recorder.status)
}

// TestRecorderHeaderIsUsableBeforeAnyWrite pins the lazy map. gokrb5 calls
// Header().Set before writing anything, so returning a nil map here would panic
// on the first challenge rather than on some later edge case.
func TestRecorderHeaderIsUsableBeforeAnyWrite(t *testing.T) {
	var recorder responseRecorder

	recorder.Header().Set(fiber.HeaderWWWAuthenticate, spnegoBareChallenge)
	require.Equal(t, spnegoBareChallenge, recorder.Header().Get(fiber.HeaderWWWAuthenticate))
	require.Zero(t, recorder.status, "setting a header is not writing a response")
}

// TestRecorderFlushCommitsTheImplicitStatus pins Flush against the writer it
// stands in for. A real ResponseWriter commits the response on flush, so the
// status becomes 200 if none was chosen; httptest.ResponseRecorder does the
// same. Nothing is sent — the middleware cannot classify a response it has not
// finished reading — so the body stays where it was.
func TestRecorderFlushCommitsTheImplicitStatus(t *testing.T) {
	var recorder responseRecorder

	require.NotPanics(t, recorder.Flush)
	require.Equal(t, http.StatusOK, recorder.status)
	require.Empty(t, recorder.body.String(), "flushing sends nothing; everything stays buffered")

	// An explicit status already chosen is not overwritten, exactly as a second
	// WriteHeader would not overwrite it.
	var chosen responseRecorder
	chosen.WriteHeader(http.StatusConflict)
	chosen.Flush()
	require.Equal(t, http.StatusConflict, chosen.status)
}

// TestCopyHeadersToLeavesAnUnwrittenRecorderAlone pins the read as a read.
//
// Every request served from an established session reaches copyHeadersTo with
// nothing recorded — gokrb5 writes no token and the manager never holds the
// writer — so going through Header() would allocate the map there just to
// iterate nothing, once per request on the hot path sessions exist to make
// cheap. A nil map ranges zero times.
func TestCopyHeadersToLeavesAnUnwrittenRecorderAlone(t *testing.T) {
	var recorder responseRecorder

	app := fiber.New()
	app.All("/*", func(c fiber.Ctx) error {
		recorder.copyHeadersTo(c)
		return nil
	})
	fasthttpCtx := &fasthttp.RequestCtx{}
	fasthttpCtx.Request.SetRequestURI("/authenticate")
	app.Handler()(fasthttpCtx)

	require.Nil(t, recorder.headers,
		"replaying an empty recorder must not allocate its header map")
}

// TestRequestForSPNEGOToleratesANilContext covers the guard on WithContext,
// which panics rather than returning an error. Fiber's own Ctx never yields a
// nil context, but an application may supply its own through app.NewCtxFunc,
// and a panic here takes down the connection: Fiber installs no recover.
func TestRequestForSPNEGOToleratesANilContext(t *testing.T) {
	var got *http.Request
	app := fiber.New()
	app.All("/*", func(c fiber.Ctx) error {
		require.NotPanics(t, func() {
			got = requestForSPNEGO(nilContextCtx{Ctx: c}, true)
		})
		return nil
	})
	fasthttpCtx := &fasthttp.RequestCtx{}
	fasthttpCtx.Request.SetRequestURI("/authenticate")
	fasthttpCtx.Request.Header.Set(fiber.HeaderHost, "sso.example.com")
	app.Handler()(fasthttpCtx)

	require.NotNil(t, got)
	require.NotNil(t, got.Context(), "the request still has net/http's own default")
	require.Equal(t, "sso.example.com", got.Host, "the rest of the request is still built")
}

// nilContextCtx is a fiber.Ctx whose Context returns nil, which only a
// caller-supplied implementation could do.
type nilContextCtx struct{ fiber.Ctx }

func (nilContextCtx) Context() context.Context { return nil }

// TestRecorderSatisfiesFlusher pins why Flush exists at all: a session manager
// is ordinary net/http code, and gokrb5 hands it this recorder as the raw
// ResponseWriter. Without the method a single-value assertion panics inside the
// authentication middleware, taking the connection with it.
func TestRecorderSatisfiesFlusher(t *testing.T) {
	var recorder responseRecorder
	require.NotPanics(t, func() {
		_ = http.ResponseWriter(&recorder).(http.Flusher)
	})
}

// TestIsSPNEGOOutcome pins which WWW-Authenticate values count as gokrb5's own.
// Everything else is classified as the handler failing, so a value that merely
// looks like one must not pass: a session manager holding the raw
// ResponseWriter can set whatever it likes before it fails.
func TestIsSPNEGOOutcome(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: spnegoBareChallenge, want: true},
		{value: spnegoContinueNeeded, want: true},
		{value: spnegoRejected, want: true},
		{value: spnegoAccepted, want: true},
		{value: "", want: false},
		{value: `Basic realm="session store"`, want: false},
		{value: "Bearer", want: false},
		// The scheme alone is not enough. This is the value a scheme-matching
		// test admitted, and it is the shape a manager reaches for first if it
		// sets the header at all.
		{value: "Negotiate", want: true},
		{value: "Negotiate not-a-gokrb5-token", want: false},
		// Prefixed by the scheme but a different scheme.
		{value: "NegotiateNot", want: false},
		{value: "Negotiated " + spnegoRejected, want: false},
		// Carries a real token, but not on its own, so it is not what gokrb5
		// would have written.
		{value: spnegoRejected + ", Basic", want: false},
		{value: " " + spnegoRejected, want: false},
		// Case-sensitive on purpose: gokrb5 writes exactly one spelling, so
		// anything else did not come from it.
		{value: "negotiate oQcwBaADCgEC", want: false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			require.Equal(t, tc.want, isSPNEGOOutcome(tc.value))
		})
	}
}

// TestSessionManagerCannotForgeAnOutcomeHeader is the end-to-end consequence of
// comparing the whole value rather than the scheme. A manager that sets its own
// WWW-Authenticate and then fails would otherwise be read as an authentication
// outcome and replayed wholesale — its status, its message, and the Set-Cookie
// for the session it never stored.
//
// The bare "Negotiate" row is the one that matters: it is a value gokrb5 itself
// writes, so it defeats every check short of one that also knows the manager
// failed. What catches it is sessionManagerProbe, exercised end to end here
// through a manager whose New refuses.
func TestSessionManagerCannotForgeAnOutcomeHeader(t *testing.T) {
	for _, forged := range []string{
		`Basic realm="session store"`,
		"Negotiate",
		spnegoRejected,
		spnegoContinueNeeded,
	} {
		t.Run(forged, func(t *testing.T) {
			manager := &failingSessionManager{
				onNew: func(w http.ResponseWriter) {
					w.Header().Set(fiber.HeaderWWWAuthenticate, forged)
					w.Header().Set(fiber.HeaderSetCookie, "spnego-session=never-stored; Path=/")
					http.Error(w, "postgres://user:hunter2@db.internal/sessions is down", http.StatusConflict)
				},
			}
			// The real gokrb5 session-manager plumbing, so the probe is reached
			// the way it is in production rather than being called directly.
			stubAuthenticate(t, func(w http.ResponseWriter, r *http.Request) goidentity.Identity {
				settings := service.NewSettings(nil, serviceSettings(Config{SessionManager: manager})...)
				if err := settings.SessionManager().New(w, r, "creds", []byte("marshalled")); err != nil {
					// What gokrb5's spnegoInternalServerError does, verbatim.
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
				return nil
			})

			var hookErr error
			ctx := serveProtected(t, Config{
				KeytabLookup:   testKeytabLookup(t),
				SessionManager: manager,
				OnError:        func(_ fiber.Ctx, err error) { hookErr = err },
			})

			require.Equal(t, fiber.StatusInternalServerError, ctx.Response.StatusCode())
			require.Equal(t, "Internal Server Error", string(ctx.Response.Body()))
			require.Empty(t, ctx.Response.Header.Peek(fiber.HeaderSetCookie),
				"a session that was never stored must not be advertised")
			require.Empty(t, ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate),
				"a header the manager forged must not reach the client")
			require.ErrorIs(t, hookErr, ErrSPNEGOHandlerFailed)
			require.Contains(t, hookErr.Error(), "hunter2",
				"what the handler wrote belongs in the log, where the operator can act on it")
		})
	}
}

// TestSessionFailureCarriesWhatTheHandlerWroteNotWhy pins what an operator
// actually gets, which is less than "the reason the store refused" and is
// documented as such.
//
// gokrb5 sends the manager's error to its own logger and writes only "Internal
// Server Error" to the response. The middleware reports the response, so for a
// manager that writes nothing itself — the ordinary case — that boilerplate is
// the whole of it. Claiming otherwise in the docs would send someone looking
// for a cause that is not there.
func TestSessionFailureCarriesWhatTheHandlerWroteNotWhy(t *testing.T) {
	manager := &failingSessionManager{}
	stubAuthenticate(t, func(w http.ResponseWriter, r *http.Request) goidentity.Identity {
		settings := service.NewSettings(nil, serviceSettings(Config{SessionManager: manager})...)
		if err := settings.SessionManager().New(w, r, "creds", []byte("marshalled")); err != nil {
			// What gokrb5's spnegoInternalServerError does: the reason goes to
			// its logger, the response gets boilerplate.
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return nil
	})

	var hookErr error
	ctx := serveProtected(t, Config{
		KeytabLookup:   testKeytabLookup(t),
		SessionManager: manager,
		OnError:        func(_ fiber.Ctx, err error) { hookErr = err },
	})

	require.Equal(t, fiber.StatusInternalServerError, ctx.Response.StatusCode())
	require.ErrorIs(t, hookErr, ErrSPNEGOHandlerFailed)
	require.Contains(t, hookErr.Error(), "Internal Server Error")
	require.NotContains(t, hookErr.Error(), errSessionStoreDown.Error(),
		"the store's own reason never reaches the response, so it cannot be reported from there")
}

// failingSessionManager writes whatever onNew writes and then refuses, which is
// how a session store that cannot be reached behaves.
type failingSessionManager struct {
	onNew func(w http.ResponseWriter)
}

// errSessionStoreDown is the manager's own reason. gokrb5 sends it to its own
// logger and never to the response, which is why the logged body is its
// boilerplate unless the manager wrote something itself.
var errSessionStoreDown = errors.New("session store unreachable")

func (m *failingSessionManager) New(w http.ResponseWriter, _ *http.Request, _ string, _ []byte) error {
	if m.onNew != nil {
		m.onNew(w)
	}
	return errSessionStoreDown
}

func (m *failingSessionManager) Get(*http.Request, string) ([]byte, error) {
	return nil, errSessionStoreDown
}

// TestSessionManagerProbeLeavesGetAlone pins the asymmetry the probe has to
// respect. gokrb5 discards a failed Get and falls through to full ticket
// validation, so recording it as a failure would turn a slow or broken session
// cache into a 500 on every request — the exact opposite of degrading quietly.
func TestSessionManagerProbeLeavesGetAlone(t *testing.T) {
	probe := newSessionManagerProbe(&failingSessionManager{})
	var recorder responseRecorder

	_, err := probe.Get(httptest.NewRequest(http.MethodGet, "/", nil), "creds")
	require.ErrorIs(t, err, errSessionStoreDown)
	require.False(t, recorder.sessionFailed, "a failed Get is not a failed request")

	// New is the other half: its error is the signal, and it is passed back to
	// gokrb5 unchanged so gokrb5 still takes its own failure path.
	err = probe.New(&recorder, httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil)
	require.ErrorIs(t, err, errSessionStoreDown)
	require.True(t, recorder.sessionFailed)

	// A New that succeeds records nothing.
	var clean responseRecorder
	ok := newSessionManagerProbe(&recordingSessionManager{})
	require.NoError(t, ok.New(&clean, httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil))
	require.False(t, clean.sessionFailed)
}

// wrappedWriter is a ResponseWriter that hides another, the shape a future
// gokrb5 would take if it wrapped the writer to add a capability. Unwrap is the
// convention net/http itself follows in http.ResponseController.
type wrappedWriter struct {
	http.ResponseWriter
	inner http.ResponseWriter
}

func (w wrappedWriter) Unwrap() http.ResponseWriter { return w.inner }

// selfWrappingWriter unwraps to itself, which is what a cycle looks like from
// inside the walk.
type selfWrappingWriter struct{ http.ResponseWriter }

func (w *selfWrappingWriter) Unwrap() http.ResponseWriter { return w }

// TestSessionManagerProbeSeesThroughAWrappedWriter covers what happens if
// gokrb5 stops passing the ResponseWriter straight through. The recorder is
// still found, so the signal that classifies a session failure survives a
// dependency bump that adds a wrapper.
func TestSessionManagerProbeSeesThroughAWrappedWriter(t *testing.T) {
	var recorder responseRecorder
	writer := wrappedWriter{
		ResponseWriter: httptest.NewRecorder(),
		inner:          wrappedWriter{ResponseWriter: httptest.NewRecorder(), inner: &recorder},
	}

	probe := newSessionManagerProbe(&failingSessionManager{})
	err := probe.New(writer, httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil)
	require.ErrorIs(t, err, errSessionStoreDown)
	require.True(t, recorder.sessionFailed, "the walk must reach the recorder through the wrappers")
}

// TestSessionManagerProbeSurvivesAForeignWriter covers the two ways the walk can
// end without finding a recorder: a writer that does not unwrap at all, and one
// whose Unwrap cycles. Neither can be allowed to panic or spin — the signal is
// lost, which is what the log line is for, but the request still completes.
func TestSessionManagerProbeSurvivesAForeignWriter(t *testing.T) {
	// The two exits are reported apart because they point somewhere different:
	// a writer gokrb5 swapped out is a dependency change to look at, while a
	// walk that runs out has a chain it could not get to the end of. The
	// reasons are matched in full rather than by fragment, because that second
	// message has to keep naming both a deeper chain and a loop — raising the
	// bound fixes only the first, and a message that mentioned only that would
	// send whoever reads it to redeploy and get the same line back.
	for name, tc := range map[string]struct {
		writer     http.ResponseWriter
		wantReason string
	}{
		"a writer that does not unwrap": {
			writer:     httptest.NewRecorder(),
			wantReason: "gokrb5 replaced the response writer",
		},
		"a writer that unwraps to itself": {
			writer:     &selfWrappingWriter{ResponseWriter: httptest.NewRecorder()},
			wantReason: "the response writer chain either nests deeper than the unwrap limit or loops",
		},
		"a chain one hop past the limit": {
			writer:     nestedWriter(maxResponseWriterUnwraps+1, &responseRecorder{}),
			wantReason: "the response writer chain either nests deeper than the unwrap limit or loops",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var logged bytes.Buffer
			flog.SetOutput(&logged)
			t.Cleanup(func() { flog.SetOutput(os.Stderr) })

			probe := newSessionManagerProbe(&failingSessionManager{})
			done := make(chan error, 1)
			go func() {
				done <- probe.New(tc.writer, httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil)
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, errSessionStoreDown, "the error still reaches gokrb5")
			case <-time.After(5 * time.Second):
				t.Fatal("the unwrap walk did not terminate")
			}

			// Losing the signal is the hole the probe exists to close, so it
			// must not be silent.
			require.Contains(t, logged.String(), "session failure could not be recorded",
				"a lost signal must be reported, not swallowed")
			require.Contains(t, logged.String(), tc.wantReason,
				"the line must say which of the two happened")
		})
	}
}

// nestedWriter wraps inner in depth layers, so a test can put the recorder at a
// chosen distance from the writer the probe is handed.
func nestedWriter(depth int, inner http.ResponseWriter) http.ResponseWriter {
	w := inner
	for range depth {
		w = wrappedWriter{ResponseWriter: httptest.NewRecorder(), inner: w}
	}
	return w
}

// TestSessionManagerProbeUnwrapsExactlyAsManyTimesAsItSays pins the bound
// against its name. A walk that stopped one hop early would lose the signal for
// a chain the constant promises to cover, and the shortfall would only show up
// as a misclassified session failure on some future gokrb5.
func TestSessionManagerProbeUnwrapsExactlyAsManyTimesAsItSays(t *testing.T) {
	var atTheLimit responseRecorder
	require.Empty(t, recordSessionFailure(nestedWriter(maxResponseWriterUnwraps, &atTheLimit)),
		"a recorder exactly maxResponseWriterUnwraps hops away must be found")
	require.True(t, atTheLimit.sessionFailed)

	var pastTheLimit responseRecorder
	require.Equal(t,
		"the response writer chain either nests deeper than the unwrap limit or loops",
		recordSessionFailure(nestedWriter(maxResponseWriterUnwraps+1, &pastTheLimit)))
	require.False(t, pastTheLimit.sessionFailed)
}

// TestWiredSessionManagerProbeCanReportALostSignal pins the probe as production
// builds it, not as the tests construct it.
//
// The throttle is only touched when the recorder cannot be found, which no test
// going through gokrb5 can reach — it always passes the recorder through. So a
// probe built without its throttle looks fine everywhere until the day the
// signal is lost, and then panics inside the authentication middleware on a
// request that was already failing. This is the assertion that fails instead.
func TestWiredSessionManagerProbeCanReportALostSignal(t *testing.T) {
	flog.SetOutput(io.Discard)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	settings := service.NewSettings(nil, serviceSettings(Config{
		SessionManager: &failingSessionManager{},
	})...)

	require.NotPanics(t, func() {
		// A writer that is not the recorder, which is the only way to reach the
		// reporting path at all.
		err := settings.SessionManager().New(
			httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil)
		require.ErrorIs(t, err, errSessionStoreDown)
	})
}

// TestSessionManagerProbeThrottlesItsDiagnostic covers the rate. The line fires
// once per request that reaches a failing session store, so an outage would
// otherwise produce one per authenticated request for as long as it lasts —
// which is the flood every other internal-failure log in this package is
// throttled to prevent.
func TestSessionManagerProbeThrottlesItsDiagnostic(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	// The clock is installed before the probe is used and then stepped through
	// a variable, rather than the field being reassigned partway. nowFn is read
	// without the throttle's mutex — deliberately, since nothing takes it to
	// write the field either — so reassigning it once the probe is in use is
	// the pattern that becomes a race as soon as anyone drives the probe from
	// a second goroutine, which other tests in this file do.
	now := time.Now()
	probe := newSessionManagerProbe(&failingSessionManager{})
	probe.signalLost.nowFn = func() time.Time { return now }

	for range 5 {
		require.ErrorIs(t,
			probe.New(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil),
			errSessionStoreDown)
	}
	require.Equal(t, 1, strings.Count(logged.String(), "session failure could not be recorded"),
		"a repeating fault must not turn into one log line per request")

	// The window is claimed, not the line suppressed for good: winding the
	// clock past it lets the next one through, so a fault that outlasts the
	// window still reappears in the log.
	now = now.Add(2 * internalErrorLogEvery)
	require.ErrorIs(t,
		probe.New(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil),
		errSessionStoreDown)
	require.Equal(t, 2, strings.Count(logged.String(), "session failure could not be recorded"))
}

// TestSessionManagerProbeIsQuietWhenItFindsTheRecorder is the other half. The
// diagnostic above marks a dependency bump worth investigating, so emitting it
// on the ordinary path would train whoever reads the log to ignore it.
func TestSessionManagerProbeIsQuietWhenItFindsTheRecorder(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	var recorder responseRecorder
	probe := newSessionManagerProbe(&failingSessionManager{})
	require.ErrorIs(t,
		probe.New(&recorder, httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil),
		errSessionStoreDown)

	require.True(t, recorder.sessionFailed)
	require.Empty(t, logged.String(),
		"the ordinary failure is reported through OnError and the throttled log, not from here")
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
