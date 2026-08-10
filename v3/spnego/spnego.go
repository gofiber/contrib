package spnego

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/spnego"
)

// The WWW-Authenticate values gokrb5 v8.4.4 sets, mirroring unexported
// constants in its spnego/http.go. Re-check these when upgrading gokrb5.
const (
	spnegoContinueNeeded = "Negotiate oRQwEqADCgEBoQsGCSqGSIb3EgECAg=="
	spnegoRejected       = "Negotiate oQcwBaADCgEC"
	spnegoBareChallenge  = "Negotiate"
	spnegoAccepted       = "Negotiate oRQwEqADCgEAoQsGCSqGSIb3EgECAg=="
)

// Matched in requestForSPNEGO so it stores the constant rather than a view into
// fasthttp's pooled buffer, without allocating a copy.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"

	protocolHTTP11 = "HTTP/1.1"
	protocolHTTP10 = "HTTP/1.0"
)

// isSPNEGOOutcome reports whether gokrb5 wrote this WWW-Authenticate value.
// Exact, not by scheme: a session manager holds the same writer.
func isSPNEGOOutcome(value string) bool {
	switch value {
	case spnegoBareChallenge, spnegoContinueNeeded, spnegoRejected, spnegoAccepted:
		return true
	default:
		return false
	}
}

// sessionManagerProbe learns a failed New from the error it returned, rather
// than from a response header the manager itself could have written.
type sessionManagerProbe struct {
	delegate   service.SessionMgr
	signalLost logThrottle
}

func (p *sessionManagerProbe) New(w http.ResponseWriter, r *http.Request, k string, v []byte) error {
	err := p.delegate.New(w, r, k, v)
	if err != nil {
		if reason := recordSessionFailure(w); reason != "" {
			// Not fatal, but it is the hole this probe closed. Throttled: it fires
			// per request for as long as the session store stays down.
			p.signalLost.do(func() {
				flog.Errorf("spnego: session failure could not be recorded: %s (writer %T); "+
					"a session manager that writes a Negotiate header before failing may now "+
					"be misread as authenticated", reason, w)
			})
		}
	}
	return err //nolint:wrapcheck // gokrb5 inspects only whether this is nil
}

// recordSessionFailure marks the recorder, reporting why it could not. Unwraps
// along the way, as http.ResponseController does.
func recordSessionFailure(w http.ResponseWriter) string {
	// Bounded: a writer whose Unwrap returns itself would hang the request.
	for unwraps := 0; ; unwraps++ {
		if recorder, ok := w.(*responseRecorder); ok {
			recorder.sessionFailed = true
			return ""
		}
		if unwraps == maxResponseWriterUnwraps {
			return "the response writer chain either nests deeper than the unwrap limit or loops"
		}
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return "gokrb5 replaced the response writer"
		}
		w = wrapper.Unwrap()
	}
}

const maxResponseWriterUnwraps = 8

func (p *sessionManagerProbe) Get(r *http.Request, k string) ([]byte, error) {
	return p.delegate.Get(r, k) //nolint:wrapcheck // passed through to gokrb5 untouched
}

// loggedBodyLimit bounds text this package did not write. Not a redaction —
// readAll drops gokrb5's parse errors rather than capping them.
const loggedBodyLimit = 512

// quoteForLog renders untrusted bytes as one quoted token, truncated first so
// escaping cannot inflate past the limit. Quoting stops a newline forging a line.
func quoteForLog(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) <= loggedBodyLimit {
		return strconv.Quote(string(trimmed))
	}
	// Backed up to a rune boundary, or a split rune renders as hex escapes.
	// Bounded at UTFMax-1: the keytab case is not UTF-8 at all.
	kept := trimmed[:loggedBodyLimit]
	for range utf8.UTFMax - 1 {
		// size, not just r: an empty slice reports RuneError at width zero.
		if r, size := utf8.DecodeLastRune(kept); r != utf8.RuneError || size != 1 {
			break
		}
		kept = kept[:len(kept)-1]
	}
	return strconv.Quote(string(kept)) +
		fmt.Sprintf(" (+%d bytes)", len(trimmed)-len(kept))
}

// serveSPNEGO runs gokrb5's handler, turning a panic out of it into an error.
// Only gokrb5's frames: the chain runs later, so its panics still propagate.
func serveSPNEGO(handler http.Handler, recorder *responseRecorder, req *http.Request) (failure error) {
	defer func() {
		if p := recover(); p != nil {
			failure = fmt.Errorf("%w: gokrb5 panicked: %s",
				ErrSPNEGOHandlerFailed, quoteForLog(fmt.Appendf(nil, "%v", p)))
		}
	}()
	handler.ServeHTTP(recorder, req)
	return nil
}

// isRejection reports a refusal rather than a leg of an ongoing negotiation.
// Only a refusal reaches Config.Unauthorized.
func isRejection(headers http.Header) bool {
	return headers.Get(fiber.HeaderWWWAuthenticate) == spnegoRejected
}

// nowOr reports fn's time, or the wall clock when fn is nil (a test's seam).
func nowOr(fn func() time.Time) time.Time {
	if fn != nil {
		return fn()
	}
	return time.Now()
}

// internalErrorLogEvery throttles a repeating internal failure, whose rate
// unauthenticated callers would otherwise control.
const internalErrorLogEvery = 30 * time.Second

// logThrottle runs at most one write per internalErrorLogEvery, outside the
// lock so a slow sink is not latency on the request path. Zero value is ready.
type logThrottle struct {
	// Test seam, read without the mutex. Set before the throttle is shared.
	nowFn func() time.Time

	mu   sync.Mutex
	last time.Time
}

// do runs write if the window elapsed, consuming it. Taking the write rather
// than reporting a verdict keeps the consumption at the call site.
func (t *logThrottle) do(write func()) {
	// nowFn is caller-supplied, so it is read outside the lock, like write.
	if t.claimWindow(nowOr(t.nowFn)) {
		write()
	}
}

// claimWindow reports whether the window elapsed and claims it — named for the
// mutation, since claiming without writing swallows a line.
func (t *logThrottle) claimWindow(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.last) < internalErrorLogEvery {
		return false
	}
	t.last = now
	return true
}

// clientSafeError keeps the cause matchable by errors.Is while showing a fixed
// message. Is and not Unwrap: an exposed chain lets a *fiber.Error set status.
type clientSafeError struct{ cause error }

func (e *clientSafeError) Error() string { return "Internal Server Error" }

func (e *clientSafeError) Is(target error) bool { return errors.Is(e.cause, target) }

// responseRecorder captures what the SPNEGO handler writes, so the outcome can
// be inspected before anything is replayed onto the Fiber response.
type responseRecorder struct {
	headers http.Header
	status  int
	body    bytes.Buffer
	// Set by sessionManagerProbe: the one failure known as a fact.
	sessionFailed bool
	// Set by the inner handler. Fields, not captured locals, which would force
	// both to the heap on every request.
	authenticated bool
	identity      goidentity.Identity
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

// Flush exists so a session manager asserting w.(http.Flusher) does not panic.
// Nothing is sent: the response must be seen whole before it is classified.
func (r *responseRecorder) Flush() {
	if r.status == 0 {
		r.status = http.StatusOK
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

// copyHeadersTo replays the recorded headers, which SPNEGO sets on success too.
// Ranged directly to skip allocating the map; Add, since a header may repeat.
func (r *responseRecorder) copyHeadersTo(ctx fiber.Ctx) {
	for key, values := range r.headers {
		for _, value := range values {
			ctx.Response().Header.Add(key, value)
		}
	}
}

// discardLogger is what gokrb5 gets when logging is off. Not nil: GetPACType
// calls Printf unchecked, and a nil *log.Logger panics there.
var discardLogger = log.New(io.Discard, "", 0)

// resolveLogger picks gokrb5's logger, falling back to one that discards.
// fiberLogger is nil unless Fiber's registered logger is a *log.Logger.
func resolveLogger(cfg Config, fiberLogger flog.AllLogger[*log.Logger]) *log.Logger {
	if cfg.Log != nil {
		return cfg.Log
	}
	if !cfg.UseFiberLogger || fiberLogger == nil {
		return discardLogger
	}
	return fiberLogger.Logger()
}

// requestForSPNEGO builds the net/http request gokrb5 inspects, avoiding
// adaptor.ConvertRequest. v8.4.4 reads only Authorization and RemoteAddr.
//
// Strings out of fasthttp's pooled buffer are copied only when a manager can
// outlive it — a Kerberos header runs to kilobytes.
func requestForSPNEGO(ctx fiber.Ctx, forSessionManager bool) *http.Request {
	fasthttpCtx := ctx.RequestCtx()
	header := make(http.Header, 2)
	if auth := ctx.Get(fiber.HeaderAuthorization); auth != "" {
		if forSessionManager {
			auth = strings.Clone(auth)
		}
		header.Set(fiber.HeaderAuthorization, auth)
	}
	// Rebuilt from the parsed cookies: fasthttp's Peek returns only the first
	// Cookie line, so a session cookie on a later one would go missing.
	if forSessionManager {
		var cookies strings.Builder
		for key, value := range fasthttpCtx.Request.Header.Cookies() {
			// fasthttp parses "Cookie: flag" and "Cookie: =sneaky" alike as an empty
			// key. Neither is lookupable by name, so forwarding either misleads.
			if len(key) == 0 {
				continue
			}
			// After the skip, or a dropped cookie leaves an empty element behind.
			if cookies.Len() > 0 {
				cookies.WriteString("; ")
			}
			cookies.Write(key)
			cookies.WriteByte('=')
			cookies.Write(value)
		}
		if cookies.Len() > 0 {
			header.Set(fiber.HeaderCookie, cookies.String())
		}
	}
	// One allocation, not two: the URL stays per-request and writable.
	var buf struct {
		req http.Request
		url url.URL
	}
	buf.req = http.Request{
		// Not cloned: Fiber answers this from its own table of constants.
		Method: ctx.Method(),
		// Scheme at most. gokrb5 never reads the URL, and the path here is not
		// faithfully reconstructible.
		URL:    &buf.url,
		Header: header,
		// Not copied: net.Addr.String builds a new string every call.
		RemoteAddr: fasthttpCtx.RemoteAddr().String(),
		// net/http guarantees a non-nil Body, and a manager may well hold the
		// usual "defer r.Body.Close()". Nothing to offer, so the empty one.
		Body: http.NoBody,
	}
	req := &buf.req
	// Host, protocol, scheme, TLS and the context are read only by a session
	// manager, and each costs a copy, a header walk or an allocation.
	if forSessionManager {
		// Skipped when there is nothing to carry: Context() installs Background
		// as a side effect that every later Locals lookup then scans past.
		if requestCtx := ctx.Context(); requestCtx != nil && requestCtx != req.Context() {
			req = req.WithContext(requestCtx)
		}
		// Host, not Hostname: net/http documents this as "host or host:port".
		req.Host = strings.Clone(ctx.Host())
		req.TLS = fasthttpCtx.TLSConnectionState()
		// Each arm assigns its own constant, not the matched value: matching does
		// not move a string's bytes out of the request buffer.
		switch scheme := ctx.Scheme(); scheme {
		case schemeHTTP:
			req.URL.Scheme = schemeHTTP
		case schemeHTTPS:
			req.URL.Scheme = schemeHTTPS
		default:
			req.URL.Scheme = strings.Clone(scheme)
		}
		// Same shape and reason. Numbers stated alongside the string, so
		// ProtoAtLeast and Proto never disagree; an unparsable version sets none.
		switch proto := fasthttpCtx.Request.Header.Protocol(); string(proto) {
		case protocolHTTP11:
			req.Proto, req.ProtoMajor, req.ProtoMinor = protocolHTTP11, 1, 1
		case protocolHTTP10:
			req.Proto, req.ProtoMajor, req.ProtoMinor = protocolHTTP10, 1, 0
		default:
			parsed := string(proto)
			if major, minor, ok := http.ParseHTTPVersion(parsed); ok {
				req.Proto, req.ProtoMajor, req.ProtoMinor = parsed, major, minor
			}
		}
	}
	return req
}

// serviceSettings translates Config into gokrb5's options. Only fields the
// caller set are passed on, so gokrb5's own defaults stand elsewhere.
func serviceSettings(cfg Config) []func(*service.Settings) {
	// Never nil — see discardLogger for the panic that would invite.
	opts := make([]func(*service.Settings), 0, 6)
	opts = append(opts, service.Logger(resolveLogger(cfg, flog.DefaultLogger[*log.Logger]())))
	// service.SName is deliberately not wired up: the SPNEGO accept path never
	// reads it, so it would promise a restriction that does nothing.
	if cfg.KeytabPrincipal != "" {
		opts = append(opts, service.KeytabPrincipal(cfg.KeytabPrincipal))
	}
	// Not observable today, but a gokrb5 reading zero as "no skew" would reject
	// every ticket.
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
		opts = append(opts, service.SessionManager(&sessionManagerProbe{delegate: cfg.SessionManager}))
	}
	// Every concurrent request shares this array; Clip turns an append by a
	// future gokrb5 into a copy rather than a race.
	return slices.Clip(opts)
}

// authenticate wraps a handler in SPNEGO acceptance. A variable so tests can
// stub it; New captures it once, so a test cannot race a request in flight.
var authenticate = spnego.SPNEGOKRB5Authenticate

// New builds the SPNEGO middleware from a Config, returning a Fiber handler or
// an error if the configuration is invalid.
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
		// Nothing named the keytab, so confirm the resolved one loads — otherwise
		// a misconfigured host starts clean and fails every request.
		if _, err = keytabLookup(); err != nil {
			return nil, err
		}
	}
	opts := serviceSettings(cfg)
	// Without a manager, most of what requestForSPNEGO can build is unread.
	forSessionManager := cfg.SessionManager != nil
	// Captured at construction; see the note on authenticate.
	acceptSPNEGO := authenticate

	// Throttled separately: one shared throttle would let a keytab line suppress
	// a session-store line, and vice versa. A third lives on the probe.
	lookupFailures := &logThrottle{}
	handlerFailures := &logThrottle{}
	// Return the middleware handler
	return func(ctx fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(ctx) {
			return ctx.Next()
		}
		// Look up the keytab
		kt, err := keytabLookup()
		if err == nil && kt == nil {
			// gokrb5 dereferences the keytab while decrypting, so nil would panic
			// on the first request carrying a well-formed AP-REQ.
			err = errNilKeytab
		}
		if err != nil {
			// Built once: it is the returned error's cause as well as the log line.
			failure := fmt.Errorf("%w: %w", ErrLookupKeytabFailed, err)
			// Quoted: a caller's lookup may carry an upstream's text. Only the log
			// line — OnError and the ErrorHandler get the error unchanged.
			lookupFailures.do(func() {
				flog.Errorf("spnego: %s: %s", ErrLookupKeytabFailed, quoteForLog([]byte(err.Error())))
			})
			if cfg.OnError != nil {
				// The cause, not the wrapper: the hook is diagnostics.
				cfg.OnError(ctx, failure)
			}
			return &clientSafeError{cause: failure}
		}
		req := requestForSPNEGO(ctx, forSessionManager)

		recorder := &responseRecorder{}
		// Records and returns without running the chain: gokrb5's stack is under
		// the recover below, which must not swallow an application panic.
		inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			recorder.authenticated = true
			recorder.identity = goidentity.FromHTTPRequestContext(r)
		})

		// gokrb5 evaluates MechTypes[0] before Verify rejects the mechanisms
		// (spnego/spnego.go:78), so an empty sequence panics — with no credentials.
		//
		// Recovered, not pre-validated: that unchecked-index pattern recurs there.
		// An internal failure, since nothing got far enough to judge the ticket.
		if failure := serveSPNEGO(acceptSPNEGO(inner, kt, opts...), recorder, req); failure != nil {
			handlerFailures.do(func() { flog.Errorf("spnego: %v", failure) })
			if cfg.OnError != nil {
				cfg.OnError(ctx, failure)
			}
			return &clientSafeError{cause: ErrSPNEGOHandlerFailed}
		}
		if recorder.authenticated {
			// The client needs the accept-completed header to authenticate us.
			recorder.copyHeadersTo(ctx)
			SetAuthenticatedIdentityToContext(ctx, recorder.identity)
			// Before ctx.Next, so the hook is not ordered behind the whole chain.
			if cfg.OnSuccess != nil {
				cfg.OnSuccess(ctx, recorder.identity)
			}
			return ctx.Next()
		}
		// Did not authenticate. sessionFailed decides where set; otherwise the
		// header, since gokrb5 sets one of four on every outcome and none on error.
		//
		// The status decides nothing: the recorder keeps the first write, which a
		// manager may own. Nothing is replayed — a phantom session cookie included.
		if recorder.sessionFailed || !isSPNEGOOutcome(recorder.headers.Get(fiber.HeaderWWWAuthenticate)) {
			// On demand and at most once: this runs per request during an outage,
			// while the throttle silences all but one and OnError is usually nil.
			//
			// The status is what the handler wrote, zero included — the 401 below
			// would describe a handler that wrote nothing as having answered.
			describe := sync.OnceValue(func() error {
				return fmt.Errorf("%w: status %d: %s", ErrSPNEGOHandlerFailed,
					recorder.status, quoteForLog(recorder.body.Bytes()))
			})
			handlerFailures.do(func() { flog.Errorf("spnego: %v", describe()) })
			if cfg.OnError != nil {
				cfg.OnError(ctx, describe())
			}
			return &clientSafeError{cause: ErrSPNEGOHandlerFailed}
		}

		// Replay SPNEGO's challenge. gokrb5 always writes a status alongside the
		// header; 401 is the fallback, the only answer leaving negotiation open.
		status := recorder.status
		if status == 0 {
			status = fiber.StatusUnauthorized
		}
		recorder.copyHeadersTo(ctx)
		ctx.Status(status)
		// Only a refusal reaches the caller's handler: clients renegotiate only
		// when the challenge legs arrive untouched.
		if cfg.Unauthorized != nil && isRejection(recorder.headers) {
			return cfg.Unauthorized(ctx)
		}
		return ctx.Send(recorder.body.Bytes()) //nolint:wrapcheck // Fiber's own error
	}, nil
}
