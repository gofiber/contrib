package spnego

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/spnego"
)

// gokrb5 answers 401 in several situations and distinguishes them only by the
// WWW-Authenticate value it sets:
//
//   - a bare "Negotiate" is the opening challenge, and clients begin SPNEGO
//     only when they receive it on an untouched 401;
//   - spnegoContinueNeeded asks the client to retry with KRB5;
//   - spnegoRejected means the ticket was refused.
//
// Only the last is treated as a failure. Note that spnegoContinueNeeded is not
// exclusively a handshake step: gokrb5 sends it from three places — a genuine
// GSS-API continue, a Negotiate header that fails base64 decoding, and a token
// that unmarshals as neither SPNEGO nor raw KRB5 (spnego/http.go:284, :320 and
// :330 in v8.4.4). A malformed or NTLM-only token is therefore indistinguishable
// from a real continuation, and such failures cannot reach Config.Unauthorized.
// Treating them as rejections instead would break genuine continuations, which
// is the more damaging error, so they are passed through.
//
// These values mirror unexported constants in gokrb5/v8/spnego/http.go, pinned
// at v8.4.4. Re-check them against the source when upgrading gokrb5: a change
// there would silently reclassify responses. The tests that would catch a drift
// are the ones that drive live gokrb5 and compare what it emitted —
// TestUnauthorizedNotCalledOnContinueNeeded, TestUnauthorizedCalledOnRejection
// and TestRejectionWithoutHandlerPassesThrough. TestIsRejection is not one of
// them: it builds its table from these same constants, so it holds whatever
// they say.
const (
	spnegoContinueNeeded = "Negotiate oRQwEqADCgEBoQsGCSqGSIb3EgECAg=="
	spnegoRejected       = "Negotiate oQcwBaADCgEC"
)

// isRejection reports whether the response SPNEGO produced is a refusal rather
// than a leg of an ongoing negotiation. Only a refusal is handed to
// Config.Unauthorized; rewriting either challenge would stop clients from ever
// completing the handshake.
func isRejection(headers http.Header) bool {
	return headers.Get(fiber.HeaderWWWAuthenticate) == spnegoRejected
}

// nowOr reports fn's time, or the wall clock when fn is nil. The nil case is
// production; a non-nil fn is a test driving the clock.
func nowOr(fn func() time.Time) time.Time {
	if fn != nil {
		return fn()
	}
	return time.Now()
}

// internalErrorLogEvery throttles the log line for a repeating internal
// failure. Without it, a keytab the process cannot read turns every request —
// at a rate unauthenticated callers control — into a log line.
const internalErrorLogEvery = 30 * time.Second

// logThrottle runs at most one write per window. The write happens outside the
// lock, so a slow sink cannot become latency on the request path this sits on.
type logThrottle struct {
	every time.Duration
	// nowFn is a seam for tests; the wall clock is used when it is nil.
	nowFn func() time.Time

	mu   sync.Mutex
	last time.Time
}

// window reports the throttle interval, defaulting rather than trusting a
// non-positive value, which would let every event through — the flood the type
// exists to prevent.
func (t *logThrottle) window() time.Duration {
	if t.every <= 0 {
		return internalErrorLogEvery
	}
	return t.every
}

// do runs write if the window has elapsed, and consumes the window when it
// does. Taking the write rather than reporting a verdict keeps the consumption
// visible at the call site: a predicate would invite a second call that
// silently swallows the line it was added for.
//
// Sub against the zero time saturates, so the first event always runs.
func (t *logThrottle) do(write func()) {
	if t.claimWindow(nowOr(t.nowFn)) {
		write()
	}
}

// claimWindow reports whether the window has elapsed, and claims it when it
// has. Named for the mutation rather than the question so a second call site is
// obviously wrong: claiming without writing swallows the line for a full
// window. Split out from do only so the lock can be defer-guarded, since do
// must call write with the lock released.
func (t *logThrottle) claimWindow(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.last) < t.window() {
		return false
	}
	t.last = now
	return true
}

// clientSafeError keeps an internal cause matchable with errors.Is while
// showing the client nothing but a generic message. Fiber's DefaultErrorHandler
// writes err.Error() straight into the response body, and these causes name
// keytab paths and OS errors.
//
// It deliberately implements Is rather than Unwrap. errors.As walks the Unwrap
// chain, and Fiber derives the response status from any *fiber.Error it finds
// there, so exposing the chain would let a caller's KeytabLookupFunc returning,
// say, fiber.ErrUnauthorized turn an infrastructure fault into a 401 with no
// WWW-Authenticate header. Stopping the walk here pins the status at 500 while
// leaving the package's sentinels matchable.
type clientSafeError struct{ cause error }

func (e *clientSafeError) Error() string { return "Internal Server Error" }

func (e *clientSafeError) Is(target error) bool { return errors.Is(e.cause, target) }

// responseRecorder captures what the SPNEGO handler writes so the middleware
// can inspect the outcome before replaying it onto the Fiber response.
type responseRecorder struct {
	headers http.Header
	status  int
	body    bytes.Buffer
}

func (r *responseRecorder) Header() http.Header {
	if r.headers == nil {
		r.headers = make(http.Header)
	}
	return r.headers
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

// copyHeadersTo replays the recorded headers onto the Fiber response. SPNEGO
// sets WWW-Authenticate on success as well as on failure, and the client needs
// it in both cases to complete mutual authentication.
func (r *responseRecorder) copyHeadersTo(ctx fiber.Ctx) {
	for key, values := range r.Header() {
		for _, value := range values {
			ctx.Response().Header.Add(key, value)
		}
	}
}

// resolveLogger picks the logger handed to gokrb5, or nil to leave its logging
// off. fiberLogger is Fiber's default logger for *log.Logger, which is a nil
// interface whenever the registered logger is backed by something else — a zap
// or zerolog adapter, say — so it must be checked before Logger is called on
// it.
func resolveLogger(cfg Config, fiberLogger flog.AllLogger[*log.Logger]) *log.Logger {
	if cfg.Log != nil {
		return cfg.Log
	}
	if !cfg.UseFiberLogger || fiberLogger == nil {
		return nil
	}
	return fiberLogger.Logger()
}

// requestForSPNEGO builds the net/http request gokrb5 inspects. Assembling it
// directly avoids adaptor.ConvertRequest, which parses the client-supplied URI
// and copies every header on each authenticated request.
//
// gokrb5 v8.4.4's server path reads the Authorization header and the remote
// address, and reads cookies only behind a session-manager check — which is why
// withCookies mirrors whether Config.SessionManager is set. Re-check this when
// upgrading gokrb5.
func requestForSPNEGO(ctx fiber.Ctx, withCookies bool) *http.Request {
	fasthttpCtx := ctx.RequestCtx()
	header := make(http.Header, 2)
	if auth := ctx.Get(fiber.HeaderAuthorization); auth != "" {
		header.Set(fiber.HeaderAuthorization, auth)
	}
	// Forwarded only for a session manager, which reads the session out of the
	// cookies. Sending them unconditionally would hand gokrb5 every cookie the
	// site sets on requests that have no use for them.
	//
	// Rebuilt from the parsed cookies rather than copied from the header,
	// because fasthttp's Peek returns only the first Cookie line until
	// something in the chain has read a cookie and made it collect the rest.
	// Copying it would forward a subset that varies with whatever unrelated
	// middleware ran first, and a session cookie on the second line would go
	// missing — which reads as "no session" and mints a fresh one per request.
	if withCookies {
		var cookies strings.Builder
		for key, value := range fasthttpCtx.Request.Header.Cookies() {
			if cookies.Len() > 0 {
				cookies.WriteString("; ")
			}
			// fasthttp parses a valueless "Cookie: flag" as key "" and value
			// "flag", and re-serialises it without the "=". Emitting one
			// anyway would produce "=flag", which net/http then drops for an
			// invalid name — so the manager would see a different cookie set
			// than the client sent.
			if len(key) > 0 {
				cookies.Write(key)
				cookies.WriteByte('=')
			}
			cookies.Write(value)
		}
		if cookies.Len() > 0 {
			header.Set(fiber.HeaderCookie, cookies.String())
		}
	}
	// Scheme and TLS exist only for a session manager, which is the sole reader
	// of either — gokrb5 itself looks at neither. They are gated on the same
	// flag as the cookies so an ordinary request pays for neither: reading the
	// TLS state copies and heap-allocates a tls.ConnectionState, and ctx.Scheme
	// walks every header once TrustProxy is on.
	//
	// Their guarantee is narrower than it looks. A store deciding Secure from
	// r.TLS != nil is right only where Fiber terminates TLS itself. Behind a
	// TLS-terminating proxy the connection into Fiber is plain, so TLS is nil
	// while Scheme reports https from X-Forwarded-Proto — the two disagree, and
	// only Scheme reflects what the client actually used.
	requestURL := &url.URL{}
	var tlsState *tls.ConnectionState
	if withCookies {
		requestURL.Scheme = ctx.Scheme()
		tlsState = fasthttpCtx.TLSConnectionState()
	}
	return &http.Request{
		Method: ctx.Method(),
		// Scheme at most. gokrb5 never reads the URL, and a faithful path is
		// not reconstructible from here: Fiber's is percent-encoded or not
		// depending on Config.UnescapePath, and net/url cannot represent a
		// malformed escape at all. The scheme, unlike the path, can be stated
		// truthfully.
		URL: requestURL,
		// net/http documents a server request's Host as "host or host:port", so
		// it is Host rather than Hostname, which drops the port. gokrb5 does
		// not read it; it is set because it can be set faithfully, unlike the
		// rest of the URL.
		Host:       ctx.Host(),
		Header:     header,
		TLS:        tlsState,
		RemoteAddr: fasthttpCtx.RemoteAddr().String(),
	}
}

// serviceSettings translates Config into the options gokrb5 accepts. Only the
// fields the caller actually set are passed on, so gokrb5's own defaults stand
// everywhere else — notably MaxClockSkew, which it resolves to five minutes
// when left at zero, and PAC decoding, which it performs unless told not to.
func serviceSettings(cfg Config) []func(*service.Settings) {
	// gokrb5 logs unconditionally, once per request and without a level, so it
	// is wired up only when the caller asks for it. Config.Log takes priority;
	// otherwise Fiber's logger is used, but only when it is backed by a
	// *log.Logger. DefaultLogger returns a nil interface when it is not, so the
	// result has to be checked before calling Logger on it.
	opts := make([]func(*service.Settings), 0, 6)
	if l := resolveLogger(cfg, flog.DefaultLogger[*log.Logger]()); l != nil {
		opts = append(opts, service.Logger(l))
	}
	// service.SName is deliberately not wired up. gokrb5 v8.4.4 reads it only
	// in KRB5BasicAuthenticator; the SPNEGO accept path runs VerifyAPREQ, which
	// never looks at it, so exposing it would promise a restriction that does
	// nothing. KeytabPrincipal is the control that actually pins the principal.
	if cfg.KeytabPrincipal != "" {
		opts = append(opts, service.KeytabPrincipal(cfg.KeytabPrincipal))
	}
	// Not observable today: gokrb5 maps an explicitly-set zero to the same five
	// minutes it uses for unset. The guard is against that changing — a version
	// that read zero as "no skew tolerated" would reject every ticket on any
	// host whose clock is not exact, which is all of them.
	if cfg.MaxClockSkew > 0 {
		opts = append(opts, service.MaxClockSkew(cfg.MaxClockSkew))
	}
	if cfg.DisablePACDecoding {
		opts = append(opts, service.DecodePAC(false))
	}
	if cfg.RequireHostAddress {
		opts = append(opts, service.RequireHostAddr(true))
	}
	if cfg.SessionManager != nil {
		opts = append(opts, service.SessionManager(cfg.SessionManager))
	}
	return opts
}

// authenticate wraps a handler in SPNEGO acceptance. It is a variable so tests
// can substitute a stub: reaching the authenticated branch for real needs a
// live KDC, which would leave identity propagation, the accept-completed header
// replay and downstream error handling permanently unexercised.
//
// New captures it once, so the request path reads a local rather than this
// package-level variable. A test therefore has to substitute before
// constructing its middleware, and cannot race a request already in flight.
var authenticate = spnego.SPNEGOKRB5Authenticate

// New creates a new SPNEGO authentication middleware.
// It takes a Config struct and returns a Fiber handler or an error.
// The middleware handles Kerberos authentication for incoming requests using the
// SPNEGO protocol, verifying client credentials against the configured keytab.
func New(cfg Config) (fiber.Handler, error) {
	// Validate configuration
	if cfg.MaxClockSkew < 0 {
		return nil, ErrConfigInvalidOfNegativeMaxClockSkew
	}
	if strings.Contains(cfg.KeytabPrincipal, "@") {
		return nil, ErrConfigInvalidOfKeytabPrincipalRealm
	}
	keytabLookup := cfg.KeytabLookup
	if keytabLookup == nil {
		if !cfg.FallbackToSystemKeytab {
			return nil, ErrConfigInvalidOfKeytabLookupFunctionRequired
		}
		var err error
		if keytabLookup, err = NewSystemKeytabLookupFunc(); err != nil {
			return nil, err
		}
		// Nothing named the keytab explicitly, so confirm the resolved system
		// keytab actually loads. Otherwise a misconfigured host starts clean
		// and fails every request instead of failing at startup.
		if _, err = keytabLookup(); err != nil {
			return nil, err
		}
	}
	opts := serviceSettings(cfg)
	// Captured once: gokrb5 looks at cookies only when a session manager is
	// configured, so nothing else has to pay for copying them.
	forwardCookies := cfg.SessionManager != nil
	// Captured at construction; see the note on authenticate.
	acceptSPNEGO := authenticate

	// Internal failures are logged here rather than returned to the client, and
	// throttled so a persistent fault cannot become a log flood.
	//
	// There are two kinds, and each gets its own throttle: a keytab lookup that
	// fails, and a 5xx raised inside the SPNEGO handler — which today means a
	// session manager that could not store or fetch a session. Sharing one
	// throttle would let either suppress the other, so an operator chasing a
	// broken session store could be looking at a keytab line instead. Keying on
	// the cause is what to avoid: that would let a caller defeat the throttle
	// by varying client-controlled text inside it.
	lookupFailures := &logThrottle{every: internalErrorLogEvery}
	handlerFailures := &logThrottle{every: internalErrorLogEvery}
	logInternal := func(throttle *logThrottle, failure error) {
		throttle.do(func() {
			flog.Errorf("spnego: %v", failure)
		})
	}
	// Return the middleware handler
	return func(ctx fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(ctx) {
			return ctx.Next()
		}
		// Look up the keytab
		kt, err := keytabLookup()
		if err == nil && kt == nil {
			// A lookup is caller-supplied and may return no keytab and no
			// error. gokrb5 dereferences the keytab while decrypting a ticket,
			// so letting nil through panics the process on the first request
			// that carries a well-formed AP-REQ.
			err = errNilKeytab
		}
		if err != nil {
			failure := fmt.Errorf("%w: %w", ErrLookupKeytabFailed, err)
			logInternal(lookupFailures, failure)
			if cfg.OnError != nil {
				// Given the wrapped cause rather than the client-safe wrapper:
				// the hook is internal diagnostics, and a caller that wanted the
				// sanitised form would have nothing to inspect.
				cfg.OnError(ctx, failure)
			}
			return &clientSafeError{cause: failure}
		}
		req := requestForSPNEGO(ctx, forwardCookies)

		var (
			authenticated bool
			nextErr       error
		)
		recorder := &responseRecorder{}
		// The inner handler runs synchronously, on this goroutine, only once
		// SPNEGO has accepted the ticket. Keeping it on this goroutine means a
		// panic from a downstream handler propagates with its original value
		// and stack.
		inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			authenticated = true
			// SPNEGO has already written its accept-completed header at this
			// point; the client needs it to authenticate the server.
			recorder.copyHeadersTo(ctx)
			// Set the authenticated identity in the Fiber context
			identity := goidentity.FromHTTPRequestContext(r)
			SetAuthenticatedIdentityToContext(ctx, identity)
			// Before ctx.Next, so a hook counting authentications is not ordered
			// behind however long the rest of the chain takes.
			if cfg.OnSuccess != nil {
				cfg.OnSuccess(ctx, identity)
			}
			// Call the next handler in the chain
			nextErr = ctx.Next()
		})
		acceptSPNEGO(inner, kt, opts...).ServeHTTP(recorder, req)
		if authenticated {
			return nextErr
		}

		// Authentication did not complete.
		status := recorder.status
		if status == 0 {
			status = fiber.StatusUnauthorized
		}
		// A 5xx from inside the SPNEGO handler is not an authentication
		// outcome, it is this service failing. gokrb5 raises one when a session
		// manager cannot persist a session, and passing it through as an
		// ordinary response would leave a broken store 500ing every
		// authenticated request with nothing logged and no hook fired.
		//
		// Neither the body nor the headers are replayed here, unlike the
		// challenge path below. gokrb5 hands the session manager the raw
		// ResponseWriter, so a manager that reports its own failure — a DSN, a
		// host, a driver error — would have that captured by the recorder and
		// echoed to an unauthenticated caller. Returning the error instead
		// routes it through the application's ErrorHandler with the same
		// sanitised body a keytab failure gets, and drops any Set-Cookie the
		// manager wrote before failing, which would otherwise hand out a
		// session that was never stored.
		if status >= fiber.StatusInternalServerError {
			failure := fmt.Errorf("%w: status %d", ErrSPNEGOHandlerFailed, status)
			logInternal(handlerFailures, failure)
			if cfg.OnError != nil {
				cfg.OnError(ctx, failure)
			}
			return &clientSafeError{cause: failure}
		}

		// Replay SPNEGO's own challenge.
		recorder.copyHeadersTo(ctx)
		ctx.Status(status)
		// Only a refusal reaches the caller's handler. The opening challenge
		// and the continuation token are legs of a handshake that can still
		// succeed, and clients renegotiate only when they arrive untouched.
		if cfg.Unauthorized != nil && isRejection(recorder.Header()) {
			return cfg.Unauthorized(ctx)
		}
		return ctx.Send(recorder.body.Bytes()) //nolint:wrapcheck // Fiber's own error
	}, nil
}
