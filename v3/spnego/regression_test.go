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
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
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

// TestUnauthorizedNotCalledOnContinueNeeded covers the "continue needed" leg:
// a 401 that asks the client to retry, not a failure to hand to Unauthorized.
func TestUnauthorizedNotCalledOnContinueNeeded(t *testing.T) {
	var unauthorizedCalled bool
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		Unauthorized: func(c fiber.Ctx) error {
			unauthorizedCalled = true
			return c.Status(fiber.StatusForbidden).SendString("denied")
		},
	}, func(ctx *fasthttp.RequestCtx) {
		// An NTLMSSP token is a Negotiate header gokrb5 parses as neither SPNEGO nor
		// raw KRB5 — the path that asks the client to renegotiate.
		ctx.Request.Header.Set(fiber.HeaderAuthorization,
			"Negotiate "+base64.StdEncoding.EncodeToString([]byte("NTLMSSP\x00\x01\x00\x00\x00")))
	})

	require.False(t, unauthorizedCalled, "Unauthorized must not run on the continue-needed leg")
	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Equal(t, spnegoContinueNeeded, string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)),
		"the continuation token must reach the client untouched")
}

// TestUnauthorizedNotCalledOnOpeningChallenge covers the bootstrap leg. Clients
// start SPNEGO only on an untouched 401, so rewriting it is a silent deny-all.
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

// TestIsRejection pins which of gokrb5's three 401 shapes counts as a failure:
// only the rejection, since the other two can still succeed.
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

// TestKeytabLookupErrorNotLeakedToClient keeps the keytab path and OS error out
// of the body, which Fiber's default handler would echo verbatim.
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

// TestNilKeytabFromLookup covers a lookup reporting success with no keytab.
// gokrb5 dereferences it unconditionally, so passing it through panics.
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

// TestResolveLoggerNilFiberLogger covers the nil interface Fiber returns when
// its logger is not a *log.Logger — calling Logger on it panics in New.
func TestResolveLoggerNilFiberLogger(t *testing.T) {
	t.Run("nil fiber logger does not panic", func(t *testing.T) {
		require.NotPanics(t, func() {
			require.Same(t, discardLogger, resolveLogger(Config{UseFiberLogger: true}, nil))
		})
	})

	// "Off" discards, never nil: gokrb5 hands it to Ticket.GetPACType, which calls
	// Printf unchecked on the PAC path that is on by default.
	t.Run("gokrb5 logging is discarded unless asked for", func(t *testing.T) {
		got := resolveLogger(Config{}, flog.DefaultLogger[*log.Logger]())
		require.Same(t, discardLogger, got)
		require.NotPanics(t, func() { got.Printf("a nil logger would panic here") })
	})

	t.Run("explicit logger wins", func(t *testing.T) {
		want := log.New(os.Stderr, "", 0)
		require.Same(t, want, resolveLogger(Config{Log: want}, nil))
	})

	t.Run("fiber logger is used when opted in", func(t *testing.T) {
		require.NotNil(t, resolveLogger(Config{UseFiberLogger: true}, flog.DefaultLogger[*log.Logger]()))
	})
}

// TestKeytabCacheSurvivesTornRead: a keytab that fails to parse mid-rotation
// must not take out authentication when a good one is already cached.
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

// rejectedNegotiateHeader builds a well-formed NegTokenInit whose mech token
// fails validation — the one path producing an outright rejection.
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

// TestLookupErrorCannotChooseTheResponseStatus is why clientSafeError implements
// Is and not Unwrap: Fiber resolves status via errors.As, which walks Unwrap.
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

// TestKeytabStaleGraceResetsAfterRecovery covers an episode ending on the
// cache-hit path, when a restore (cp -p) leaves size and mtime matching.
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

// TestKeytabEpisodeBoundedAcrossRevisions: each bad write is a new revision, and
// restarting the grace window per revision would cover revoked keys forever.
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

// TestEndEpisodeIfCurrentRejectsStaleView: a request whose stat predates a
// rotation must not cancel the episode another goroutine opened.
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

// TestKeytabRotationByRenameDetected covers the rotation the README recommends.
// Size and mtime alone would call a same-sized, timestamp-preserved file unchanged.
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

// TestKeytabUnreadableIsNotCoveredFor separates unreadable from unparsable. Only
// the latter is a mid-write read; an unreadable keytab must surface at once.
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

// TestRequestForSPNEGO checks the hand-built request. A field populated wrongly
// is worse than one left nil, because nil fails loudly.
func TestRequestForSPNEGO(t *testing.T) {
	var got *http.Request
	app := fiber.New()
	// Driven with a non-default verb: asserting GET against a hardcoded "GET" would
	// hold however the method was derived.
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

	// RemoteAddr is the one field gokrb5 reads — it constrains ticket verification to
	// that address — so both the value and its parseability are pinned.
	require.Equal(t, "203.0.113.7:55123", got.RemoteAddr)
	addr, err := types.GetHostAddress(got.RemoteAddr)
	require.NoError(t, err, "gokrb5 must be able to parse the address it is handed")
	require.Equal(t, net.IPv4(203, 0, 113, 7).To4(), net.IP(addr.Address))

	// URL carries at most a scheme, and not even that without a session manager. It
	// is never a reconstruction of the target.
	require.NotNil(t, got.URL)
	require.Empty(t, got.URL.Path)
	require.Empty(t, got.URL.RawQuery)
	// Scheme and TLS serve a session manager alone, so a request without one pays
	// for neither: a tls.ConnectionState copy and a header walk per request.
	require.Empty(t, got.URL.Scheme,
		"the scheme is only for a session manager")
	require.Nil(t, got.TLS,
		"the TLS state is only for a session manager")
}

// TestRequestForSPNEGOHasANonNilBody pins net/http's server-request invariant: a
// manager holding "defer r.Body.Close()" would otherwise panic the request.
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
// Fiber hands out views into fasthttp's pooled buffer, so a manager retaining
// r.Host would read back another connection's bytes and record the wrong client.
//
// Asserted on the strings' storage, not by serving a second request: whether a
// reused buffer lands on the same bytes depends on fasthttp's pooling.
func TestRequestForSPNEGOCopiesOutOfTheRequestBuffer(t *testing.T) {
	raw := "POST /authenticate HTTP/1.1\r\n" +
		"Host: sso.example.com:8443\r\n" +
		"Authorization: Negotiate dGlja2V0\r\n" +
		"X-Forwarded-Proto: https\r\n" +
		"Cookie: spnego-session=opaque\r\n\r\n"
	// Host carries a port on purpose — net/http documents it as "host or host:port",
	// so it is Fiber's Host and not Hostname, which drops it.

	ctx := &fasthttp.RequestCtx{}
	require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))
	ctx.SetRemoteAddr(&net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 55123})

	var got *http.Request
	// TrustProxy on: it is the only configuration where Scheme comes out of the
	// request buffer at all, so the check below would otherwise hold anything.
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
		// Method is here despite not being cloned: Fiber answers from its own constants,
		// and this is where it shows up if that stops being true.
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

	// Without a manager nothing outlives the handler, so the copy is skipped — a
	// Kerberos header runs to kilobytes. Asserted, since the gate is invisible.
	//
	// The one assertion pinning an optimisation rather than a safety property: if
	// gokrb5 starts retaining the request, copy unconditionally and delete this.
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

// sharesStorage reports whether s begins inside b, which is what a view of a
// request buffer looks like. Fiber hands out whole values, never a middle.
func sharesStorage(s string, b []byte) bool {
	if len(s) == 0 || len(b) == 0 {
		return false
	}
	start := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	low := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	return start >= low && start < low+uintptr(len(b))
}

// carriedValueKey is the key both context tests store under. Unexported, as
// context.WithValue requires: an anonymous struct{}{} collides silently.
type carriedValueKey struct{}

// TestRequestForSPNEGOCarriesFibersContext pins the context a manager gets.
// Unset it is Background: no deadline, and nothing joined to the app's trace.
func TestRequestForSPNEGOCarriesFibersContext(t *testing.T) {
	build := func(t *testing.T, forSessionManager bool) *http.Request {
		t.Helper()
		var got *http.Request
		app := fiber.New()
		app.All("/*", func(c fiber.Ctx) error {
			c.SetContext(context.WithValue(context.Background(), carriedValueKey{}, "from the application"))
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

	require.Equal(t, "from the application", build(t, true).Context().Value(carriedValueKey{}),
		"a session manager must reach its store under the application's context")

	// Not carried without a manager: WithContext shallow-copies the whole request,
	// and nothing on that path would read it.
	require.Nil(t, build(t, false).Context().Value(carriedValueKey{}),
		"the context is only for a session manager")
}

// TestRequestForSPNEGOReportsEverySchemeFaithfully covers the three arms: two
// assign a constant to avoid allocating, the third copies out of the buffer.
//
// What every arm has to hold is the same: the value is what Fiber reported, and
// it does not point back into the buffer.
func TestRequestForSPNEGOReportsEverySchemeFaithfully(t *testing.T) {
	for _, tc := range []struct {
		name       string
		forwarded  string
		wantScheme string
	}{
		{name: "http", forwarded: "http", wantScheme: "http"},
		{name: "https", forwarded: "https", wantScheme: "https"},
		// Neither constant, so it takes the copying arm. Fiber passes it through, and it
		// is as much a view into the buffer as the other two.
		{name: "something else entirely", forwarded: "wss", wantScheme: "wss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "GET /authenticate HTTP/1.1\r\n" +
				"Host: sso.example.com\r\n" +
				"X-Forwarded-Proto: " + tc.forwarded + "\r\n\r\n"

			ctx := &fasthttp.RequestCtx{}
			require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))
			ctx.SetRemoteAddr(&net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 55123})

			var got *http.Request
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
			require.Equal(t, tc.wantScheme, got.URL.Scheme)
			require.False(t,
				sharesStorage(got.URL.Scheme, ctx.Request.Header.Peek(fiber.HeaderXForwardedProto)),
				"the scheme must not alias the request buffer, whichever arm it took")
		})
	}
}

// TestRequestForSPNEGOSkipsTheContextCopyWhenThereIsNothingToCarry pins the skip:
// for an app that never calls SetContext, WithContext would allocate for nothing.
//
// Measured relatively rather than against a fixed count, so it does not have to
// be revisited whenever an unrelated allocation moves.
func TestRequestForSPNEGOSkipsTheContextCopyWhenThereIsNothingToCarry(t *testing.T) {
	allocs := func(t *testing.T, setContext bool) float64 {
		t.Helper()
		var measured float64
		app := fiber.New()
		app.All("/*", func(c fiber.Ctx) error {
			if setContext {
				c.SetContext(context.WithValue(context.Background(), carriedValueKey{}, "x"))
			}
			measured = testing.AllocsPerRun(100, func() {
				_ = requestForSPNEGO(c, true)
			})
			return nil
		})
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/authenticate")
		ctx.Request.Header.Set(fiber.HeaderHost, "sso.example.com")
		app.Handler()(ctx)
		return measured
	}

	require.Less(t, allocs(t, false), allocs(t, true),
		"a context with nothing in it must not cost a copy of the request")
}

// TestRequestForSPNEGOStatesTheProtocol pins Proto against its numbers, so a
// manager comparing ProtoAtLeast with Proto is not told two different things.
//
// It also pins the gate: a manager is the only reader, so a request without one
// must not copy the protocol out of the buffer and parse it.
func TestRequestForSPNEGOStatesTheProtocol(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		// protocol overrides what the request line said, for versions fasthttp
		// will hold but not parse.
		protocol    string
		wantProto   string
		wantMajor   int
		wantMinor   int
		wantAtLeast bool
	}{
		{
			name:      "HTTP/1.1",
			raw:       "GET /authenticate HTTP/1.1\r\nHost: sso.example.com\r\n\r\n",
			wantProto: "HTTP/1.1", wantMajor: 1, wantMinor: 1, wantAtLeast: true,
		},
		{
			name:      "HTTP/1.0",
			raw:       "GET /authenticate HTTP/1.0\r\nHost: sso.example.com\r\n\r\n",
			wantProto: "HTTP/1.0", wantMajor: 1, wantMinor: 0, wantAtLeast: false,
		},
		{
			// Anything else takes the parsing arm. fasthttp records an unfamiliar version
			// on the request line rather than rejecting it, so this is not hypothetical.
			name:      "an unfamiliar version is parsed rather than assumed",
			raw:       "GET /authenticate HTTP/2.0\r\nHost: sso.example.com\r\n\r\n",
			wantProto: "HTTP/2.0", wantMajor: 2, wantMinor: 0, wantAtLeast: true,
		},
		{
			// A version net/http refuses leaves all three fields alone rather than pairing a
			// string with numbers that contradict it. Set on the header: fasthttp rejects a
			name:      "a version that will not parse is left unstated",
			protocol:  "HTTP/11.1",
			wantProto: "", wantMajor: 0, wantMinor: 0, wantAtLeast: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := func(forSessionManager bool) *http.Request {
				raw := tc.raw
				if raw == "" {
					raw = "GET /authenticate HTTP/1.1\r\nHost: sso.example.com\r\n\r\n"
				}
				ctx := &fasthttp.RequestCtx{}
				require.NoError(t, ctx.Request.Read(bufio.NewReader(bytes.NewBufferString(raw))))
				if tc.protocol != "" {
					ctx.Request.Header.SetProtocol(tc.protocol)
				}

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

// TestRequestForSPNEGOForwardsCookiesForSessionManager covers the other half: a
// manager reads its session from the cookies, so withholding them misses every time.
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
// Parsed from a raw request, not built with Add: Add merges into one line, while
// two real lines leave Peek returning only the first — which is the bug.
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
// Neither rendering is faithful, and neither is lookupable by name, so it is dropped.
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

// TestRequestForSPNEGODropsLeadingEqualsCookies is the other half: fasthttp parses
// "=sneaky" to the same empty key, so rendering it invents a cookie "sneaky".
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

// TestRequestForSPNEGOSkipsNamelessCookiesWithoutLeavingAGap covers the nameless
// cookie arriving other than first, where the separator can go wrong on its own.
//
// The other two tests put it at the head, where no separator is written, so
// neither notices "a=1; ; b=2" — a stray element every parser must handle.
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
	// Asserted on the header text, not parsed cookies: Go's parser drops an empty
	// element silently, so the gap would survive unnoticed.
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

// TestRequestForSPNEGOCarriesSchemeAndTLS pins what a store reads to decide if its
// cookie may be Secure. Unset, it would issue a credential without that attribute.
//
// The TLS case needs a real *tls.Conn: fasthttp derives the state from the
// connection, so nothing less distinguishes it from a hardcoded nil.
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
		// Never handshaken, which is fine: what is under test is that the field is taken
		// from the connection at all.
		ctx := &fasthttp.RequestCtx{}
		ctx.Init2(tls.Server(server, &tls.Config{MinVersion: tls.VersionTLS12}), nil, true)

		got := build(t, ctx)
		require.NotNil(t, got.TLS, "a TLS connection must be visible to a session store")
	})
}

// TestServiceSettingsPassesOnlyWhatWasSet pins the translation into gokrb5's
// options: a zero field must leave gokrb5's own default standing.
func TestServiceSettingsPassesOnlyWhatWasSet(t *testing.T) {
	t.Run("an empty config sets nothing", func(t *testing.T) {
		opts := serviceSettings(Config{})
		settings := service.NewSettings(nil, opts...)

		// Why the slice is clipped: it goes to gokrb5 variadically and is closed over by
		// the handler, so a future append would race across concurrent requests.
		require.Equal(t, len(opts), cap(opts),
			"a config that sets nothing must not hand gokrb5 room to append into")

		require.Empty(t, settings.KeytabPrincipal())
		require.Equal(t, 5*time.Minute, settings.MaxClockSkew(),
			"a zero skew must leave gokrb5's default, not pin one here")
		require.True(t, settings.DecodePAC(),
			"PAC decoding is what populates group SIDs; it must stay on by default")
		require.False(t, settings.RequireHostAddr())
		require.Nil(t, settings.SessionManager())
		// The one thing an empty config sets. gokrb5 calls Printf unchecked on the PAC
		// path, so "no logging" must mean a discarding logger, not none.
		require.Same(t, discardLogger, settings.Logger())
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
		// Wrapped, so a refused New is known as a fact rather than read back out of the
		// response the manager was holding. The caller's manager stays reachable underneath.
		probe, ok := settings.SessionManager().(*sessionManagerProbe)
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

// New satisfies service.SessionMgr and no more; gokrb5 calls it only after a
// ticket validates, which needs a KDC. Other tests drive the stub instead.
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

// TestOnSuccessSeesTheIdentity covers the success hook: it runs before the rest of
// the chain, and is handed the same identity the handler will read.
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

// TestOnSuccessNotCalledWithoutAuthentication keeps the hook honest — a challenge,
// a continuation and a rejection are all not-authenticated.
func TestOnSuccessNotCalledWithoutAuthentication(t *testing.T) {
	var hookRuns int
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		OnSuccess:    func(fiber.Ctx, goidentity.Identity) { hookRuns++ },
	})

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Zero(t, hookRuns, "an opening challenge is not a successful authentication")
}

// TestOnErrorReceivesTheInternalCause: the hook gets the wrapped cause, since one
// handed "Internal Server Error" would have nothing to report.
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

// TestOnErrorNotCalledOnAuthenticationFailure separates the two kinds. A rejected
// ticket is Unauthorized's business, not something to page on.
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

// TestSessionManagerServesFromSessionWithoutATicket drives gokrb5's real session
// path: it consults the manager before the header, so no ticket is needed.
//
// It covers that service.SessionManager is passed through at all; this manager
// ignores the request, so cookie forwarding is covered elsewhere.
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

	// The premise copyHeadersTo rests on, checked against live gokrb5: a resumed
	// session carries no accept-completed token and the manager never held the writer.
	require.Empty(t, ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate),
		"a session served without an exchange has no token to prove one")
}

// TestSPNEGOInternalFailureIsReportedNotSwallowed covers the 5xx sessions make
// reachable — replaying it would fail every request with nothing logged.
//
// The 500 comes from the stub because gokrb5 reaches newSession only after a
// ticket validates. The stub writes exactly what spnegoInternalServerError does.
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

// TestSessionCookieIsReplayedOnSuccess pins the promise the godoc and README make.
// A stored session whose Set-Cookie never leaves is one the client cannot present.
//
// Nothing else looks at Set-Cookie: moving copyHeadersTo after the write, or
// switching Add to Set, would leave only this test to notice.
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
// Set-Cookie alone would not: fasthttp's special-header path makes Set append.
// Any other repeated header — two Vary lines — would lose all but the last.
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
// withholds: a manager's DSN or driver error, captured by the recorder.
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
// A manager writing a body first pins the recorder's status at 200, so gokrb5's
// 500 is dropped. Testing "not an outcome" rather than "5xx" is what catches it.
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

// TestInternalFailureKindsThrottleIndependently: one shared throttle would let
// either suppress the other, hiding a broken session store behind a keytab line.
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

// TestChallengeIsNotAnInternalFailure keeps the 5xx rule off the ordinary
// outcomes, or every unauthenticated request would look like a service fault.
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

// TestAuthenticatedIdentityCarriesADGroups pins the README's claim: PAC decoding
// puts the caller's group SIDs on the identity, so Authorized answers directly.
//
// The identity is the *credentials.Credentials gokrb5 stores, so this is the real
// type. Minting a PAC needs a KDC, so the AD credentials are set as gokrb5 does.
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

// TestCookieForwardingFollowsTheSessionManager covers the wiring, which asserting
// on requestForSPNEGO cannot reach — that helper takes the decision as an argument.
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

// TestConfigDefault pins the godoc's promise. New treats a zero field as unset
// rather than reading the variable, so the two diverging would mislead.
//
// Compared whole: naming fields individually is what let an earlier version check
// four and miss the rest. DeepEqual treats two nil funcs as equal.
func TestConfigDefault(t *testing.T) {
	require.Equal(t, Config{}, ConfigDefault)

	// Spelled out for the two whose default is load bearing: MaxClockSkew would
	// override gokrb5's five minutes, DisablePACDecoding would strip the group SIDs.
	require.Zero(t, ConfigDefault.MaxClockSkew)
	require.False(t, ConfigDefault.DisablePACDecoding)
}

// TestNewRejectsInvalidConfig covers the two settings that cannot be validated
// later. Both refuse every request if let through, so they fail at construction.
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

// TestLogThrottle pins all three properties: first passes, repeats do not, the
// window reopens. The last needs a clock seam or a hoisted update goes unnoticed.
func TestLogThrottle(t *testing.T) {
	now := time.Now()
	throttle := &logThrottle{nowFn: func() time.Time { return now }}
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

// TestLogThrottleZeroWindow: a throttle built without a window still throttles.
// Treating zero as "no interval" would let through the flood it exists to stop.
func TestLogThrottleZeroWindow(t *testing.T) {
	throttle := &logThrottle{}
	var runs int
	for range 5 {
		throttle.do(func() { runs++ })
	}
	require.Equal(t, 1, runs)
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

// TestKeytabEpisodeLogging pins the three operator-facing lines, once per episode
// rather than per request. Without it, dropping the log calls leaves the suite green.
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

	// The all-clear must carry the warning's level, or an operator filtering below
	// Warn sees every alert open and none close.
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

// TestKeytabEpisodeLogWritesOffReloadLock pins what line counts cannot see: the
// sink is written with mu released, so it cannot stall requests behind a reload.
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

// TestKeytabCacheConcurrentReload drives the cache from many goroutines. Nothing
// else runs it concurrently, so the -race build otherwise has nothing to compare.
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

	// Every restore shares this mtime. What opens the cache-hit recovery path is that
	// restores are stamp-identical to each other, not to the original.
	restoreStamp, err := os.Stat(filename)
	require.NoError(t, err)

	// The flipper waits for readers to have loaded since its last write, and readers
	// run until it is done. Neither bounding itself works at GOMAXPROCS=1.
	const (
		readerCount = 16
		flips       = 100
		// One token per reader can describe a load that stat'd before the write it counts
		// against, so the token after those began later and saw the new revision.
		loadsPerFlip = readerCount + 1
	)
	// Both sides block rather than poll: a non-blocking send lets a reader spin back
	// into load, holding the only P at GOMAXPROCS=1 until preemption fires.
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
			// A failed write leaves the revision unchanged, which would surface downstream as
			// "no episode lines" — a report about the cache, not about the write.
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
				// Reporting a violation does not end the loop: the flipper is waiting on these
				// sends, so dropping out would deadlock rather than fail.
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

	// Measured before the convergence load below, which closes the open episode and
	// writes its own all-clear — satisfying the assertion whatever happened above.
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

// waitUntilBlockedOnMutex blocks until a goroutine is inside Lock beneath the
// named function. The caller holds that mutex, which makes the observation stable.
//
// The tests covering load's under-lock stat must know the loader is past its own
// stat. A sleep only makes that likely; a goroutine in Lock has provably run.
//
// Frames are matched, not the goroutine's header state, which the runtime has
// renamed across releases; frames below Lock are elided when parked.
//
// Seconds, not tens: the loader only has to be scheduled and run two syscalls. A
// caller loops, so a generous wait would bury this under the package timeout.
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

// goroutineDump grows the buffer until every stack fits: a truncated dump could
// cut out the block being matched, turning a wait into a timeout.
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

// TestNoAllClearWithoutWarning covers the guard suppressing a recovery line for an
// episode that never announced one — a keytab already corrupt at startup.
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

// TestAnnounceQueuesInOrder pins the queue semantics directly. No locked section
// announces twice today, so nothing else would notice it going back to overwriting.
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

// TestEmitDoesNotHoldReloadLock pins the lock discipline: a queued line is written
// after mu is released, so a blocking sink cannot stall the reload path.
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

// stubAuthenticate replaces the SPNEGO acceptance step, so the authenticated branch
// runs without a KDC. accept writes what gokrb5 would, or returns nil to decline.
//
// The identity is attached here, not by accept: gokrb5 attaches it to a request
// derived from ours, so passing ours through would hide reading the wrong one.
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
// identity reaches the handler, the accept header is replayed, errors propagate.
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

// TestKeytabUnreadableNotCoveredWithinRetryWindow: a second request inside the
// retry window still gets the error, or unreadability is masked for the grace.
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

// TestReloadPairsStampsWithContent covers the under-lock re-stat: a stale stamp
// paired with fresh bytes would describe a revision that was never read.
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

	// Repeated to run the assertions across many stat/read interleavings, not because
	// a single iteration might miss the window — parking on mu is waited for.
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

// TestReloadReportsStatFailureUnderLock: a keytab removed after the pre-lock stat
// but before the lock must surface, not proceed with stale stamps.
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
	// Waiting for the loader to park on mu is what makes this about the under-lock
	// stat. A sleep would let the removal land first and the check could be dropped.
	waitUntilBlockedOnMutex(t, "(*keytabFileCache).load(")
	require.NoError(t, os.Remove(filename))
	cache.mu.Unlock()
	<-done

	require.ErrorIs(t, loadErr, ErrLoadKeytabFileFailed)
	// A stat failure records no episode; falling through to readAll would set
	// deg.cause and the flag. That is what tells the two apart.
	require.False(t, cache.degraded.Load(),
		"a stat failure must surface, not fall through to a read")
}

// TestEndEpisodeIfCurrentOnAlreadyClearedEpisode covers a second goroutine
// arriving after the first closed the episode: only one announces the recovery.
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

// TestRecorderStatusRules covers how the recorder turns SPNEGO's writes into a
// response. gokrb5 always writes once, so these rules are for a future version.
func TestRecorderStatusRules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		write      func(w http.ResponseWriter)
		wantStatus int
		wantBody   string
	}{
		{
			// The recorder keeps the first status, and the challenge header makes this an
			// authentication outcome, replayed as written.
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
			// gokrb5 always writes something, so this is a handler that produced no response
			// at all — no challenge to answer, nothing to pass through.
			name:       "declining without writing anything is a handler failure",
			write:      func(http.ResponseWriter) {},
			wantStatus: fiber.StatusInternalServerError,
			wantBody:   "Internal Server Error",
		},
		{
			// A challenge with no status. 401 is the only default that keeps a negotiation
			// going; anything else, 500 especially, ends a handshake mid-invitation.
			name: "a challenge with no status is answered 401",
			write: func(w http.ResponseWriter) {
				w.Header().Set(fiber.HeaderWWWAuthenticate, spnegoBareChallenge)
			},
			wantStatus: fiber.StatusUnauthorized,
			wantBody:   "",
		},
		{
			// No challenge header, so whatever the status, the client cannot answer it — the
			// shape a session manager produces when it writes before failing.
			name: "a body without a challenge header is a handler failure",
			write: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("body without a status"))
			},
			wantStatus: fiber.StatusInternalServerError,
			wantBody:   "Internal Server Error",
		},
		{
			// The case both earlier status-based gates let through: a manager picking its own
			// 4xx before failing. Only the absent challenge header distinguishes it.
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
			// Asserted unconditionally: the bare-challenge row expects an empty body, which a
			// "" meaning "do not check" could not express.
			require.Equal(t, tc.wantBody, string(ctx.Response.Body()))
		})
	}
}

// TestRecorderDefaultsToOKOnABareWrite asserts the recorder's rule directly. End to
// end the missing header classifies it, so this is the only test that pins it.
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

// TestRecorderHeaderIsUsableBeforeAnyWrite pins the lazy map: gokrb5 calls
// Header().Set before writing, so a nil map would panic on the first challenge.
func TestRecorderHeaderIsUsableBeforeAnyWrite(t *testing.T) {
	var recorder responseRecorder

	recorder.Header().Set(fiber.HeaderWWWAuthenticate, spnegoBareChallenge)
	require.Equal(t, spnegoBareChallenge, recorder.Header().Get(fiber.HeaderWWWAuthenticate))
	require.Zero(t, recorder.status, "setting a header is not writing a response")
}

// TestRecorderFlushCommitsTheImplicitStatus pins Flush against the writer it stands
// in for. Nothing is sent — the response must be read whole before classifying.
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
// A resumed session records nothing, so Header() would allocate the map just to
// iterate nothing, per request. A nil map ranges zero times.
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

// TestRequestForSPNEGOToleratesANilContext covers the guard on WithContext, which
// panics. app.NewCtxFunc can supply a Ctx that yields nil, and Fiber has no recover.
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

// TestRecorderSatisfiesFlusher pins why Flush exists: gokrb5 hands a manager this
// recorder, and a single-value assertion would panic inside the middleware.
func TestRecorderSatisfiesFlusher(t *testing.T) {
	var recorder responseRecorder
	require.NotPanics(t, func() {
		_ = http.ResponseWriter(&recorder).(http.Flusher)
	})
}

// TestIsSPNEGOOutcome pins which values count as gokrb5's own. A manager holding
// the raw writer can set whatever it likes, so a lookalike must not pass.
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
		// The scheme alone is not enough: this is the value a scheme-matching test admitted,
		// and the shape a manager reaches for first.
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

// TestSessionManagerCannotForgeAnOutcomeHeader is the end-to-end consequence: a
// forged header would be replayed wholesale, cookie for a phantom session included.
//
// The bare "Negotiate" row matters most: gokrb5 writes it too, so only knowing the
// manager failed catches it. sessionManagerProbe is what does.
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

// TestSessionFailureCarriesWhatTheHandlerWroteNotWhy pins what an operator actually
// gets, which is less than the store's reason and documented as such.
//
// gokrb5 sends the manager's error to its own logger and writes only boilerplate to
// the response, so for a manager that writes nothing that is the whole of it.
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

// TestSessionFailureCannotForgeALogLine: the handler's body is quoted into the
// diagnostic, so a newline in client-supplied text cannot start a second line.
func TestSessionFailureCannotForgeALogLine(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	// The shape a manager reaches for when it reports what it could not find:
	// its own text with the client's cookie in it.
	forged := "no session for \nspnego: [Error] all clear, nothing to see here"
	manager := &failingSessionManager{
		onNew: func(w http.ResponseWriter) { http.Error(w, forged, http.StatusConflict) },
	}
	stubAuthenticate(t, func(w http.ResponseWriter, r *http.Request) goidentity.Identity {
		settings := service.NewSettings(nil, serviceSettings(Config{SessionManager: manager})...)
		if err := settings.SessionManager().New(w, r, "creds", []byte("marshalled")); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return nil
	})

	var hookErr error
	serveProtected(t, Config{
		KeytabLookup:   testKeytabLookup(t),
		SessionManager: manager,
		OnError:        func(_ fiber.Ctx, err error) { hookErr = err },
	})

	require.ErrorIs(t, hookErr, ErrSPNEGOHandlerFailed)
	for name, line := range map[string]string{"the log": logged.String(), "OnError": hookErr.Error()} {
		require.Contains(t, line, `\nspnego:`,
			"%s must carry the newline escaped, not as a line break", name)
		require.NotContains(t, line, "\nspnego: [Error] all clear",
			"%s must not let the handler's body start a line of its own", name)
	}
}

// TestQuoteForLog pins both jobs: escaping, so untrusted bytes cannot start a line,
// and truncation, so one write cannot carry a whole failed query.
func TestQuoteForLog(t *testing.T) {
	require.Equal(t, `""`, quoteForLog(nil))
	require.Equal(t, `""`, quoteForLog([]byte("  \n\t ")),
		"surrounding whitespace is the handler's formatting, not its message")
	require.Equal(t, `"a\nb"`, quoteForLog([]byte("a\nb")))
	require.Equal(t, `"session store unreachable"`,
		quoteForLog([]byte("\n session store unreachable \n")))

	// Exactly at the limit is not truncated; one byte past it is, and says by
	// how much rather than trailing off.
	atLimit := bytes.Repeat([]byte("x"), loggedBodyLimit)
	require.Equal(t, strconv.Quote(string(atLimit)), quoteForLog(atLimit))

	over := bytes.Repeat([]byte("x"), loggedBodyLimit+37)
	require.Equal(t, strconv.Quote(string(atLimit))+" (+37 bytes)", quoteForLog(over))

	// Truncation before quoting, so a body cannot pass the limit by inflating through
	// escaping. The anchors keep TrimSpace off the escape-heavy middle.
	//
	// The bound is on the input: a byte escapes to four characters, so the rendered
	// form runs to about four times the limit.
	inflating := append(append([]byte("a"), bytes.Repeat([]byte("\n"), loggedBodyLimit*4)...), 'b')
	rendered := quoteForLog(inflating)
	require.NotContains(t, rendered, "\n",
		"no raw newline may survive, however many there were")
	require.Contains(t, rendered, "bytes)", "a body this long must be truncated")
	require.LessOrEqual(t, len(rendered), 4*loggedBodyLimit+len(` (+9999 bytes)`),
		"the rendered line must stay within four characters per kept byte")

	// A rune straddling the cut is dropped rather than left as loose bytes, which Quote
	// renders as hex. 512 is not a multiple of 3, so this cut lands mid-rune.
	wide := bytes.Repeat([]byte("世"), loggedBodyLimit)
	rune3 := quoteForLog(wide)
	require.NotContains(t, rune3, `\x`,
		"the cut must fall on a rune boundary, not mid-character")
	// And the count reports what was actually kept, not where the cut was
	// aimed: backing up two bytes means two more are left over.
	require.Equal(t,
		strconv.Quote(string(wide[:loggedBodyLimit-2]))+
			fmt.Sprintf(" (+%d bytes)", len(wide)-(loggedBodyLimit-2)),
		rune3)

	// Binary that is not UTF-8 at all still gets through: the backup is capped
	// at the length of one rune, so it cannot eat back through a keytab.
	binary := bytes.Repeat([]byte{0x80}, loggedBodyLimit*2)
	require.Contains(t, quoteForLog(binary), `\x80`)
	require.Contains(t, quoteForLog(binary), "bytes)")
}

// TestSessionFailureReportsTheStatusTheHandlerWrote: the 401 fallback belongs to
// the challenge path, and borrowing it would name a responder that never ran.
func TestSessionFailureReportsTheStatusTheHandlerWrote(t *testing.T) {
	flog.SetOutput(io.Discard)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	stubAuthenticate(t, func(http.ResponseWriter, *http.Request) goidentity.Identity {
		// Nothing written at all: no header, no body, no status.
		return nil
	})

	var hookErr error
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		OnError:      func(_ fiber.Ctx, err error) { hookErr = err },
	})

	require.Equal(t, fiber.StatusInternalServerError, ctx.Response.StatusCode())
	require.ErrorIs(t, hookErr, ErrSPNEGOHandlerFailed)
	require.Contains(t, hookErr.Error(), "status 0",
		"a handler that wrote no status must not be reported as having written one")
	require.NotContains(t, hookErr.Error(), "status 401")
}

// TestUnparsableKeytabDoesNotLeakKeyMaterial is the reason gokrb5's parse error
// is dropped rather than quoted.
//
// gokrb5's message interpolates the bytes it was handed, and for a single-principal
// keytab that is the whole key — so quoting would bound the noise and ship the secret.
func TestUnparsableKeytabDoesNotLeakKeyMaterial(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	dir := t.TempDir()
	file := path.Join(dir, "sso.keytab")
	_, clean, err := utils.NewMockKeytab(
		utils.WithPrincipal("HTTP/sso.example.com"),
		utils.WithRealm("EXAMPLE.LOCAL"),
		utils.WithPairs(utils.EncryptTypePair{Version: 3, EncryptType: 18, CreateTime: time.Now()}),
		utils.WithFilename(file),
	)
	require.NoError(t, err)
	t.Cleanup(clean)

	good, err := os.ReadFile(file)
	require.NoError(t, err)

	// The needle is the entry's own key read out of the parsed keytab. The first version
	// cut it from the file's tail — from bytes `torn` does not have, so it never matched.
	parsed, err := keytab.Load(file)
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Entries)
	key := parsed.Entries[0].Key.KeyValue
	require.NotEmpty(t, key)

	// Header kept so gokrb5 reaches the length check, the branch that interpolates the
	// file. Everything before the cut is real, so a leak carries the real key.
	torn := good[:len(good)-1]
	require.NoError(t, os.WriteFile(file, torn, 0o600))
	require.Contains(t, string(torn), string(key),
		"the bytes handed to gokrb5 must contain the key, or nothing below can fail")

	now := time.Now()
	cache := &keytabFileCache{
		files:      []string{file},
		staleGrace: 30 * time.Second,
		retryEvery: time.Nanosecond,
		nowFn:      func() time.Time { return now },
	}
	// Nothing has parsed, so there is no cached keytab and the error returns as-is —
	// the shape a caller sees through OnError and its ErrorHandler.
	_, parseErr := cache.load()
	require.Error(t, parseErr)
	require.NotContains(t, parseErr.Error(), string(key),
		"the returned error must not carry the keytab's key material")
	require.Contains(t, parseErr.Error(), "keytab did not parse")
	require.Contains(t, parseErr.Error(), file, "the file is what identifies the fault")
	require.Contains(t, parseErr.Error(), fmt.Sprintf("(%d bytes read)", len(torn)),
		"the length is what is kept: a torn write is short, a corrupt file usually is not")

	// And now with a cached keytab behind it, which is the path that announces
	// a degraded episode — the same cause, going to the log this time.
	require.NoError(t, os.WriteFile(file, good, 0o600))
	_, err = cache.load()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, torn, 0o600))
	_, err = cache.load()
	require.NoError(t, err, "the cached keytab is served through the grace window")

	require.Contains(t, logged.String(), "serving the last keytab that parsed")
	require.Contains(t, logged.String(), "keytab did not parse")

	// The log needs a different needle: quoteForLog renders a key's bytes as \xNN, so
	// searching for the raw bytes could never match however badly it leaked.
	//
	// Both renderings are checked: Quote decodes runes, so a key spliced into a longer
	// string does not always escape to the same characters as it does alone.
	quoted := strconv.Quote(string(key))
	escaped := quoted[1 : len(quoted)-1]
	require.Contains(t, quoteForLog([]byte("keytab did not parse: "+string(key))), escaped,
		"the escaped key must be what a leak would look like, or the check below is empty")

	// The cause must fit inside the cap, or a leak could be truncated away — a second
	// keytab file or a longer temp directory would be enough to do it.
	require.Less(t, len(parseErr.Error()), loggedBodyLimit,
		"a cause past the cap would be truncated, so absence would prove nothing")

	require.NotContains(t, logged.String(), escaped,
		"the log must not carry the keytab's key material either")
	require.NotContains(t, logged.String(), string(key),
		"nor unescaped, if the quoting is ever removed")
}

// TestKeytabPathCannotForgeALogLine is what the episode quoting is for now gokrb5's
// message is dropped: Unix allows a newline in a path, and paths come from config.
func TestKeytabPathCannotForgeALogLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		// A newline is not a legal filename character there, so the vector this
		// covers does not exist and the file cannot be created to test it.
		t.Skip("filenames cannot contain a newline on Windows")
	}

	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	dir := t.TempDir()
	file := path.Join(dir, "sso\nspnego: [Error] keytab loads cleanly again\n.keytab")
	_, clean, err := utils.NewMockKeytab(
		utils.WithPrincipal("HTTP/sso.example.com"),
		utils.WithRealm("EXAMPLE.LOCAL"),
		utils.WithPairs(utils.EncryptTypePair{Version: 3, EncryptType: 18, CreateTime: time.Now()}),
		utils.WithFilename(file),
	)
	require.NoError(t, err)
	t.Cleanup(clean)

	now := time.Now()
	cache := &keytabFileCache{
		files:      []string{file},
		staleGrace: 30 * time.Second,
		retryEvery: time.Nanosecond,
		nowFn:      func() time.Time { return now },
	}
	_, err = cache.load()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(file, []byte("not a keytab"), 0o600))
	_, err = cache.load()
	require.NoError(t, err, "the cached keytab is served through the grace window")

	require.Contains(t, logged.String(), "serving the last keytab that parsed")
	require.NotContains(t, logged.String(), "\nspnego: [Error] keytab loads cleanly again",
		"the degraded line must not let the path start a line of its own")

	// And the expiry line, which carries the same cause.
	logged.Reset()
	now = now.Add(2 * cache.staleGrace)
	_, err = cache.load()
	require.Error(t, err)
	require.Contains(t, logged.String(), "keytab still unusable after")
	require.NotContains(t, logged.String(), "\nspnego: [Error] keytab loads cleanly again",
		"the expiry line must not either")
}

// TestKeytabFileLookupRejectsAnEmptyPath covers the shape an unset environment
// variable takes — accepting it defers a config mistake into a 500 per request.
func TestKeytabFileLookupRejectsAnEmptyPath(t *testing.T) {
	for name, files := range map[string][]string{
		"no files at all": {},
		"one empty path":  {""},
		"an empty path among others": {
			"/etc/krb5.keytab", "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			lookup, err := NewKeytabFileLookupFunc(files...)
			require.ErrorIs(t, err, ErrConfigInvalidOfAtLeastOneKeytabFileRequired)
			require.Nil(t, lookup)
		})
	}
}

// TestKeytabFailureCannotForgeALogLine is the lookup path's half: a caller's hook
// for databases and remote services can carry an upstream's text.
func TestKeytabFailureCannotForgeALogLine(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	forged := errors.New("vault: \nspnego: [Error] keytab loads cleanly again")
	var hookErr error
	ctx := serveProtected(t, Config{
		KeytabLookup: func() (*keytab.Keytab, error) { return nil, forged },
		OnError:      func(_ fiber.Ctx, err error) { hookErr = err },
	})

	require.Equal(t, fiber.StatusInternalServerError, ctx.Response.StatusCode())
	require.Contains(t, logged.String(), `\nspnego:`,
		"the newline must be escaped, not emitted")
	require.NotContains(t, logged.String(), "\nspnego: [Error] keytab loads cleanly again",
		"the lookup's text must not start a line of its own")

	// The error itself is untouched, so errors.Is and anything a caller reads
	// off it still see what the lookup actually returned.
	require.ErrorIs(t, hookErr, ErrLookupKeytabFailed)
	require.ErrorIs(t, hookErr, forged)
	require.Contains(t, hookErr.Error(), "vault: \nspnego:")
}

// failingSessionManager writes whatever onNew writes and then refuses, which is
// how a session store that cannot be reached behaves.
type failingSessionManager struct {
	onNew func(w http.ResponseWriter)
}

// errSessionStoreDown is the manager's own reason. gokrb5 sends it to its logger,
// never the response, which is why the logged body is boilerplate.
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

// TestSessionManagerProbeLeavesGetAlone pins the asymmetry: gokrb5 discards a failed
// Get, so recording it would turn a slow cache into a 500 on every request.
func TestSessionManagerProbeLeavesGetAlone(t *testing.T) {
	probe := &sessionManagerProbe{delegate: &failingSessionManager{}}
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
	ok := &sessionManagerProbe{delegate: &recordingSessionManager{}}
	require.NoError(t, ok.New(&clean, httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil))
	require.False(t, clean.sessionFailed)
}

// wrappedWriter hides another writer, the shape a future gokrb5 would take. Unwrap
// is the convention net/http follows in http.ResponseController.
type wrappedWriter struct {
	http.ResponseWriter
	inner http.ResponseWriter
}

func (w wrappedWriter) Unwrap() http.ResponseWriter { return w.inner }

// selfWrappingWriter unwraps to itself, which is what a cycle looks like from
// inside the walk.
type selfWrappingWriter struct{ http.ResponseWriter }

func (w *selfWrappingWriter) Unwrap() http.ResponseWriter { return w }

// TestSessionManagerProbeSeesThroughAWrappedWriter covers gokrb5 no longer passing
// the writer straight through: the recorder is still found, so the signal survives.
func TestSessionManagerProbeSeesThroughAWrappedWriter(t *testing.T) {
	var recorder responseRecorder
	writer := wrappedWriter{
		ResponseWriter: httptest.NewRecorder(),
		inner:          wrappedWriter{ResponseWriter: httptest.NewRecorder(), inner: &recorder},
	}

	probe := &sessionManagerProbe{delegate: &failingSessionManager{}}
	err := probe.New(writer, httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil)
	require.ErrorIs(t, err, errSessionStoreDown)
	require.True(t, recorder.sessionFailed, "the walk must reach the recorder through the wrappers")
}

// TestSessionManagerProbeSurvivesAForeignWriter covers both ways the walk ends
// without a recorder: a writer that never unwraps, and one whose Unwrap cycles.
func TestSessionManagerProbeSurvivesAForeignWriter(t *testing.T) {
	// Reported apart because they point elsewhere: a swapped writer is a dependency
	// change; an exhausted walk is a deeper chain or a loop, and must name both.
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

			probe := &sessionManagerProbe{delegate: &failingSessionManager{}}
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

// TestSessionManagerProbeUnwrapsExactlyAsManyTimesAsItSays pins the bound against
// its name: stopping a hop early loses the signal the constant promises to cover.
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
// The throttle is touched only when the recorder is not found, which no test through
// gokrb5 reaches — so a nil one would panic only on the day the signal is lost.
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

// TestSessionManagerProbeThrottlesItsDiagnostic covers the rate: the line fires per
// request reaching a failing store, which is the flood throttling exists to stop.
func TestSessionManagerProbeThrottlesItsDiagnostic(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	// The clock is installed before use and stepped through a variable: nowFn is read
	// without the mutex, so reassigning it mid-use races once a second goroutine drives it.
	now := time.Now()
	probe := &sessionManagerProbe{delegate: &failingSessionManager{}}
	probe.signalLost.nowFn = func() time.Time { return now }

	for range 5 {
		require.ErrorIs(t,
			probe.New(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil),
			errSessionStoreDown)
	}
	require.Equal(t, 1, strings.Count(logged.String(), "session failure could not be recorded"),
		"a repeating fault must not turn into one log line per request")

	// The window is claimed, not suppressed for good: winding past it lets the next
	// line through, so a lasting fault reappears.
	now = now.Add(2 * internalErrorLogEvery)
	require.ErrorIs(t,
		probe.New(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil),
		errSessionStoreDown)
	require.Equal(t, 2, strings.Count(logged.String(), "session failure could not be recorded"))
}

// TestSessionManagerProbeIsQuietWhenItFindsTheRecorder is the other half: emitting
// on the ordinary path would train readers to ignore the line.
func TestSessionManagerProbeIsQuietWhenItFindsTheRecorder(t *testing.T) {
	var logged bytes.Buffer
	flog.SetOutput(&logged)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	var recorder responseRecorder
	probe := &sessionManagerProbe{delegate: &failingSessionManager{}}
	require.ErrorIs(t,
		probe.New(&recorder, httptest.NewRequest(http.MethodGet, "/", nil), "creds", nil),
		errSessionStoreDown)

	require.True(t, recorder.sessionFailed)
	require.Empty(t, logged.String(),
		"the ordinary failure is reported through OnError and the throttled log, not from here")
}

// TestMergedKeytabIsSerialisable pins why readAll uses keytab.New: the zero value
// marshals with a version byte gokrb5's own loader rejects.
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

// TestLogThrottleWindowBoundary pins the inclusive/exclusive edge, which stepping
// well past the window leaves unspecified.
func TestLogThrottleWindowBoundary(t *testing.T) {
	now := time.Now()
	throttle := &logThrottle{nowFn: func() time.Time { return now }}
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

// TestUnderLockMatchClosesEpisode covers load's under-lock snapshot match: a loader
// finding the cached revision back on disk must close the episode, or every hit stats.
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
	// Waited for, not slept on: a loader that had not yet stat'd would close the episode
	// through the lock-free path, satisfying both assertions without the branch running.
	waitUntilBlockedOnMutex(t, "(*keytabFileCache).load(")
	require.NoError(t, os.WriteFile(filename, original, 0o600))
	require.NoError(t, os.Chtimes(filename, info.ModTime(), info.ModTime()))
	cache.mu.Unlock()
	<-done

	require.False(t, cache.degraded.Load(),
		"a revision matching the snapshot under the lock must close the episode")
	require.Contains(t, logged.String(), "loads cleanly again")
}

// TestFileStampTracksSizeAndModTime pins each half of the change signal — every
// other rotation moves both at once, so either could be dropped unnoticed.
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

// TestMalformedTokenCannotPanicTheRequest covers a defect in gokrb5 v8.4.4 that
// an unauthenticated caller can reach.
//
// AcceptSecContext evaluates MechTypes[0] before Verify rejects the mechanisms
// (spnego/spnego.go:78), so an empty sequence indexes an empty slice, uncredentialed.
func TestMalformedTokenCannotPanicTheRequest(t *testing.T) {
	flog.SetOutput(io.Discard)
	t.Cleanup(func() { flog.SetOutput(os.Stderr) })

	// A NegTokenInit with an empty MechTypes SEQUENCE, wrapped with the SPNEGO OID.
	// Built by hand, because no honest client emits one.
	type negTokenInit struct {
		MechTypes []asn1.ObjectIdentifier `asn1:"explicit,tag:0"`
	}
	body, err := asn1.Marshal(negTokenInit{MechTypes: []asn1.ObjectIdentifier{}})
	require.NoError(t, err)
	negToken, err := asn1.Marshal(asn1.RawValue{Tag: 0, Class: 2, IsCompound: true, Bytes: body})
	require.NoError(t, err)
	oid, err := asn1.Marshal(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 2})
	require.NoError(t, err)
	token, err := asn1.Marshal(asn1.RawValue{
		Class: 1, Tag: 0, IsCompound: true, Bytes: append(oid, negToken...),
	})
	require.NoError(t, err)

	var hookErr error
	ctx := serveProtected(t, Config{
		KeytabLookup: testKeytabLookup(t),
		OnError:      func(_ fiber.Ctx, err error) { hookErr = err },
	}, func(c *fasthttp.RequestCtx) {
		c.Request.Header.Set(fiber.HeaderAuthorization,
			"Negotiate "+base64.StdEncoding.EncodeToString(token))
	})

	require.Equal(t, fiber.StatusInternalServerError, ctx.Response.StatusCode(),
		"a token gokrb5 cannot parse safely must not take the request down")
	require.Equal(t, "Internal Server Error", string(ctx.Response.Body()),
		"and must not tell the caller what broke")
	require.ErrorIs(t, hookErr, ErrSPNEGOHandlerFailed)
	require.Contains(t, hookErr.Error(), "gokrb5 panicked")
	// Quoted like every other text this package did not write: a panic value is
	// arbitrary, and this one is reached from a client-supplied token.
	require.Contains(t, hookErr.Error(), `"runtime error: index out of range`)
}

// TestDownstreamPanicIsNotCaught pins what the recover must not do: the chain runs
// after gokrb5 returns, so an application's panic keeps its own value and stack.
func TestDownstreamPanicIsNotCaught(t *testing.T) {
	user := goidentity.NewUser("alice")
	stubAuthenticate(t, func(w http.ResponseWriter, _ *http.Request) goidentity.Identity {
		w.Header().Set(fiber.HeaderWWWAuthenticate, spnegoAccepted)
		return &user
	})

	handler, err := New(Config{
		KeytabLookup: testKeytabLookup(t),
		OnError: func(fiber.Ctx, error) {
			t.Error("a downstream panic is not this middleware's failure")
		},
	})
	require.NoError(t, err)

	app := fiber.New()
	app.Use(handler)
	app.Get("/*", func(fiber.Ctx) error { panic("from the application") })

	fasthttpCtx := &fasthttp.RequestCtx{}
	fasthttpCtx.Request.SetRequestURI("/protected")
	require.PanicsWithValue(t, "from the application", func() {
		app.Handler()(fasthttpCtx)
	}, "the panic must reach the caller unchanged, not become a 500")
}

// TestKeytabStatFailureSurfacesFromBothPaths covers the two places a failing os.Stat
// is reported from — the in-place compare and the slow path — with one sentinel.
func TestKeytabStatFailureSurfacesFromBothPaths(t *testing.T) {
	t.Run("after a successful load", func(t *testing.T) {
		dir := t.TempDir()
		filename := writeMockKeytab(t, dir, "sso.keytab", "HTTP/sso.example.com")
		cache := newKeytabFileCache([]string{filename})
		good, err := cache.load()
		require.NoError(t, err)
		require.NotNil(t, good)

		require.NoError(t, os.Remove(filename))

		_, err = cache.load()
		require.Error(t, err, "a deleted keytab must not be covered for by the cache")
		require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("with no snapshot to compare against", func(t *testing.T) {
		cache := newKeytabFileCache([]string{filepath.Join(t.TempDir(), "absent.keytab")})

		_, err := cache.load()
		require.Error(t, err, "a keytab that never loaded must report the failure")
		require.ErrorIs(t, err, ErrLoadKeytabFileFailed)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})
}
