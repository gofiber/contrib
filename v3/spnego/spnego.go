package spnego

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
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
// there would silently reclassify responses. TestIsRejection and
// TestUnauthorizedNotCalledOnContinueNeeded assert them against gokrb5's real
// output, so a drift fails the suite.
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

// internalErrorKind enumerates the internal failures the middleware logs. It is
// a closed set so the throttle's state is bounded by the compiler rather than
// by whatever error value a caller happens to produce.
type internalErrorKind int

const (
	internalErrorKeytabLookup internalErrorKind = iota
	numInternalErrorKinds
)

// internalErrorLogEvery throttles the log line for a repeating internal
// failure. Without it, a keytab the process cannot read turns every request —
// at a rate unauthenticated callers control — into a log line.
const internalErrorLogEvery = 30 * time.Second

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
// gokrb5 v8.4.4's server path reads only the Authorization header and the
// remote address; it consults cookies solely behind a session-manager check,
// and this middleware configures no session manager. URL and Host are populated
// anyway, both because net/http documents a server request as always having a
// URL and so that a future gokrb5 that reads them does not nil-panic. Re-check
// this when upgrading gokrb5 or if a session manager is ever exposed through
// Config.
func requestForSPNEGO(ctx fiber.Ctx) *http.Request {
	header := make(http.Header, 1)
	if auth := ctx.Get(fiber.HeaderAuthorization); auth != "" {
		header.Set(fiber.HeaderAuthorization, auth)
	}
	// url.URL.Path holds the decoded path; RawPath carries the original bytes
	// only when they differ, which is what url.URL.EscapedPath expects. Getting
	// this wrong would be worse than leaving URL nil, since a consumer
	// reconstructing the URI would silently see a double-encoded one.
	escaped := ctx.Path()
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		decoded = escaped
	}
	requestURL := &url.URL{Path: decoded, RawQuery: string(ctx.RequestCtx().URI().QueryString())}
	if decoded != escaped {
		requestURL.RawPath = escaped
	}
	return &http.Request{
		Method: ctx.Method(),
		URL:    requestURL,
		// Host, not Hostname: net/http documents a server request's Host as
		// "host or host:port", and a service on a non-default port needs the
		// port to derive its own principal.
		Host:       ctx.Host(),
		Header:     header,
		RemoteAddr: ctx.RequestCtx().RemoteAddr().String(),
	}
}

// New creates a new SPNEGO authentication middleware.
// It takes a Config struct and returns a Fiber handler or an error.
// The middleware handles Kerberos authentication for incoming requests using the
// SPNEGO protocol, verifying client credentials against the configured keytab.
func New(cfg Config) (fiber.Handler, error) {
	// Validate configuration
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
	// gokrb5 logs unconditionally, once per request and without a level, so it
	// is wired up only when the caller asks for it. Config.Log takes priority;
	// otherwise Fiber's logger is used, but only when it is backed by a
	// *log.Logger. DefaultLogger returns a nil interface when it is not, so the
	// result has to be checked before calling Logger on it.
	var opts = make([]func(settings *service.Settings), 0, 1)
	if l := resolveLogger(cfg, flog.DefaultLogger[*log.Logger]()); l != nil {
		opts = append(opts, service.Logger(l))
	}
	// Internal failures are logged here rather than returned to the client, and
	// throttled so a persistent fault cannot become a log flood.
	var (
		logMu      sync.Mutex
		lastLogged [numInternalErrorKinds]time.Time
	)
	logInternal := func(kind internalErrorKind, sentinel, cause error) {
		// Keyed by kind rather than by message: a cause can embed
		// client-controlled text, and keying on that would let a caller defeat
		// the throttle by varying it. A single shared slot would not do either,
		// since two alternating kinds would reset each other's window on every
		// request. The key is a closed type, so the state cannot grow.
		logMu.Lock()
		now := time.Now()
		throttled := now.Sub(lastLogged[kind]) < internalErrorLogEvery
		if !throttled {
			lastLogged[kind] = now
		}
		logMu.Unlock()
		if throttled {
			return
		}
		// Written outside the lock so the log sink is not a contention point.
		flog.Errorf("spnego: %v: %v", sentinel, cause)
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
			logInternal(internalErrorKeytabLookup, ErrLookupKeytabFailed, err)
			return &clientSafeError{cause: fmt.Errorf("%w: %w", ErrLookupKeytabFailed, err)}
		}
		req := requestForSPNEGO(ctx)

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
			SetAuthenticatedIdentityToContext(ctx, goidentity.FromHTTPRequestContext(r))
			// Call the next handler in the chain
			nextErr = ctx.Next()
		})
		spnego.SPNEGOKRB5Authenticate(inner, kt, opts...).ServeHTTP(recorder, req)
		if authenticated {
			return nextErr
		}

		// Authentication did not complete. Replay SPNEGO's own challenge.
		recorder.copyHeadersTo(ctx)
		status := recorder.status
		if status == 0 {
			status = fiber.StatusUnauthorized
		}
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
