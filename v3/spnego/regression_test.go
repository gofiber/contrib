package spnego

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/keytab"
	gospnego "github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TestUnauthorizedNotCalledOnContinueNeeded covers the "continue needed" leg of
// the handshake. It is a 401, but it tells the client to retry with KRB5 rather
// than reporting a failure, so handing it to Config.Unauthorized would let the
// status or body change and strand clients that only renegotiate on an
// untouched 401.
func TestUnauthorizedNotCalledOnContinueNeeded(t *testing.T) {
	filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")
	lookupFunc, err := NewKeytabFileLookupFunc(filename)
	require.NoError(t, err)

	var unauthorizedCalled bool
	cfg := Config{
		KeytabLookup: lookupFunc,
		Unauthorized: func(c fiber.Ctx) error {
			unauthorizedCalled = true
			return c.Status(fiber.StatusForbidden).SendString("denied")
		},
	}

	middleware, err := New(cfg)
	require.NoError(t, err)
	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})

	// An NTLMSSP token is a Negotiate header gokrb5 cannot parse as SPNEGO or
	// raw KRB5, which is exactly the path that asks the client to renegotiate.
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	ctx.Request.Header.Set(fiber.HeaderAuthorization,
		"Negotiate "+base64.StdEncoding.EncodeToString([]byte("NTLMSSP\x00\x01\x00\x00\x00")))
	app.Handler()(ctx)

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
	filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")
	lookupFunc, err := NewKeytabFileLookupFunc(filename)
	require.NoError(t, err)

	middleware, err := New(Config{
		KeytabLookup: lookupFunc,
		Unauthorized: func(c fiber.Ctx) error {
			return c.Status(fiber.StatusForbidden).SendString("denied")
		},
	})
	require.NoError(t, err)
	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	app.Handler()(ctx)

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Equal(t, "Negotiate", string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)),
		"the opening challenge must reach the client untouched")
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
	middleware, err := New(Config{
		KeytabLookup: func() (*keytab.Keytab, error) {
			return nil, fmt.Errorf("open %s: permission denied", secretPath)
		},
	})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	app.Handler()(ctx)

	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode())
	require.NotContains(t, string(ctx.Response.Body()), secretPath)
	require.NotContains(t, string(ctx.Response.Body()), "permission denied")
}

// TestNilKeytabFromLookup covers a caller-supplied lookup that reports success
// but returns no keytab. gokrb5 dereferences it unconditionally, so passing it
// through panics the process on an unauthenticated request.
func TestNilKeytabFromLookup(t *testing.T) {
	middleware, err := New(Config{
		KeytabLookup: func() (*keytab.Keytab, error) { return nil, nil },
	})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")

	require.NotPanics(t, func() { app.Handler()(ctx) })
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
	filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")
	lookupFunc, err := NewKeytabFileLookupFunc(filename)
	require.NoError(t, err)

	var called bool
	middleware, err := New(Config{
		KeytabLookup: lookupFunc,
		Unauthorized: func(c fiber.Ctx) error {
			called = true
			return c.Status(fiber.StatusForbidden).SendString("denied")
		},
	})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	ctx.Request.Header.Set(fiber.HeaderAuthorization, rejectedNegotiateHeader(t))
	app.Handler()(ctx)

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
	filename := writeMockKeytab(t, t.TempDir(), "sso.keytab", "HTTP/sso.example.com")
	lookupFunc, err := NewKeytabFileLookupFunc(filename)
	require.NoError(t, err)
	middleware, err := New(Config{KeytabLookup: lookupFunc})
	require.NoError(t, err)

	app := fiber.New()
	app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
		return c.SendString("authenticated")
	})
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/authenticate")
	ctx.Request.Header.Set(fiber.HeaderAuthorization, rejectedNegotiateHeader(t))
	app.Handler()(ctx)

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Equal(t, spnegoRejected, string(ctx.Response.Header.Peek(fiber.HeaderWWWAuthenticate)))
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

	now = now.Add(31 * time.Second)
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
	app.Get("/*", func(c fiber.Ctx) error {
		got = requestForSPNEGO(c)
		return nil
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/a%2Fb/c%20d?q=1&x=%41")
	ctx.Request.Header.Set(fiber.HeaderHost, "sso.example.com:8443")
	ctx.Request.Header.Set(fiber.HeaderAuthorization, "Negotiate abc")
	app.Handler()(ctx)

	require.NotNil(t, got)
	require.Equal(t, fiber.MethodGet, got.Method)
	require.Equal(t, "Negotiate abc", got.Header.Get(fiber.HeaderAuthorization))
	// net/http documents a server request's Host as "host or host:port", so the
	// port must survive.
	require.Equal(t, "sso.example.com:8443", got.Host)

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
	require.Regexp(t, `\[Warn\] spnego: .*serving the last keytab that parsed`, logged.String())
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

	// Now flip the file between broken and good while readers hammer the cache.
	stop := make(chan struct{})
	var flipper, readers sync.WaitGroup
	flipper.Add(1)
	go func() {
		defer flipper.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				_ = os.WriteFile(filename, []byte("12"), 0o600)
			} else {
				_ = os.WriteFile(filename, good, 0o600)
			}
		}
	}()
	for range 16 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 200 {
				kt, loadErr := cache.load()
				// Either a keytab or an error, never both and never neither.
				if loadErr == nil {
					require.NotNil(t, kt)
				} else {
					require.Nil(t, kt)
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	flipper.Wait()

	// Whatever the interleaving, the file ends good and the cache converges.
	require.NoError(t, os.WriteFile(filename, good, 0o600))
	kt, err := cache.load()
	require.NoError(t, err)
	require.NotNil(t, kt)
	require.False(t, cache.degraded.Load(), "the episode must close once the keytab is good")

	logMu.Lock()
	defer logMu.Unlock()
	require.NotEmpty(t, logged.String(), "a degraded episode must leave a trace")
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
