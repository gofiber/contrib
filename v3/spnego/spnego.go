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
// constants in its spnego/http.go. Only spnegoRejected is a failure; both
// challenges are handshake legs. Re-check these when upgrading gokrb5.
const (
	spnegoContinueNeeded = "Negotiate oRQwEqADCgEBoQsGCSqGSIb3EgECAg=="
	spnegoRejected       = "Negotiate oQcwBaADCgEC"
	spnegoBareChallenge  = "Negotiate"
	spnegoAccepted       = "Negotiate oRQwEqADCgEAoQsGCSqGSIb3EgECAg=="
)

// Matched against in requestForSPNEGO so it can store the constant rather than
// a view into fasthttp's pooled request buffer, without allocating a copy.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"

	protocolHTTP11 = "HTTP/1.1"
	protocolHTTP10 = "HTTP/1.0"
)

// spnegoOutcomes is every WWW-Authenticate value gokrb5 sets, and so the set
// that marks a response as an authentication outcome rather than a failure.
var spnegoOutcomes = map[string]struct{}{
	spnegoBareChallenge:  {},
	spnegoContinueNeeded: {},
	spnegoRejected:       {},
	spnegoAccepted:       {},
}

// isSPNEGOOutcome reports whether a WWW-Authenticate value is one gokrb5 wrote.
// Compared exactly, not by scheme: a session manager holds the same writer, so
// a looser test would let it pass its own failure off as an outcome.
func isSPNEGOOutcome(value string) bool {
	_, ok := spnegoOutcomes[value]
	return ok
}

// sessionManagerProbe wraps the caller's session manager so a New that refused
// to persist is known from the error it returned, rather than inferred from a
// response header the manager itself could have written. Get passes through.
type sessionManagerProbe struct {
	delegate service.SessionMgr
	// Pointer because the probe is copied by value into gokrb5's settings, and
	// a throttle copied with it would let every copy through its own first line.
	signalLost *logThrottle
}

// newSessionManagerProbe builds the probe with its throttle. Constructing the
// struct directly leaves that nil, which panics on the path it reports on.
func newSessionManagerProbe(delegate service.SessionMgr) sessionManagerProbe {
	return sessionManagerProbe{
		delegate:   delegate,
		signalLost: &logThrottle{every: internalErrorLogEvery},
	}
}

func (p sessionManagerProbe) New(w http.ResponseWriter, r *http.Request, k string, v []byte) error {
	err := p.delegate.New(w, r, k, v)
	if err != nil {
		if reason := recordSessionFailure(w); reason != "" {
			// Not fatal — isSPNEGOOutcome still classifies most of these — but it
			// is the hole this probe closed. Throttled because it fires once per
			// request for as long as the session store stays down.
			p.signalLost.do(func() {
				flog.Errorf("spnego: session failure could not be recorded: %s (writer %T); "+
					"a session manager that writes a Negotiate header before failing may now "+
					"be misread as authenticated", reason, w)
			})
		}
	}
	return err //nolint:wrapcheck // gokrb5 inspects only whether this is nil
}

// recordSessionFailure marks the middleware's recorder, returning why it could
// not when it could not. The writer is unwrapped along the way, as
// http.ResponseController does, so a wrapper gokrb5 added is still seen through.
func recordSessionFailure(w http.ResponseWriter) string {
	// Bounded: a writer whose Unwrap returns itself would otherwise hang the
	// request. Counted in unwraps, so the writer handed in is inspected first.
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

func (p sessionManagerProbe) Get(r *http.Request, k string) ([]byte, error) {
	return p.delegate.Get(r, k) //nolint:wrapcheck // passed through to gokrb5 untouched
}

// loggedBodyLimit bounds text this package did not write: a session manager's
// body and a KeytabLookupFunc's error, neither bounded at source. Not a
// redaction — readAll drops gokrb5's parse errors instead of capping them.
const loggedBodyLimit = 512

// quoteForLog renders untrusted bytes as one quoted token, truncated first so
// the escaping cannot inflate past the limit. Quoting is what stops a newline
// in the text from forging a log line under this package's prefix.
func quoteForLog(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) <= loggedBodyLimit {
		return strconv.Quote(string(trimmed))
	}
	// Backed up to a rune boundary, or a split rune renders as hex escapes.
	// Bounded at UTFMax-1 because the keytab case is not UTF-8 at all.
	kept := trimmed[:loggedBodyLimit]
	for range utf8.UTFMax - 1 {
		// Stops on a decoded rune, a genuine U+FFFD (full width), and an empty
		// slice (RuneError at width zero) — hence testing size, not just r.
		if r, size := utf8.DecodeLastRune(kept); r != utf8.RuneError || size != 1 {
			break
		}
		kept = kept[:len(kept)-1]
	}
	return strconv.Quote(string(kept)) +
		fmt.Sprintf(" (+%d bytes)", len(trimmed)-len(kept))
}

// serveSPNEGO runs gokrb5's handler and turns a panic out of it into an error.
// Only gokrb5's frames are covered: the inner handler records and returns
// without running the application's chain, so downstream panics still propagate.
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

// isRejection reports whether the response is a refusal rather than a leg of an
// ongoing negotiation. Only a refusal reaches Config.Unauthorized; rewriting a
// challenge would stop clients from ever completing the handshake.
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

// logThrottle runs at most one write per window. The write happens outside the
// lock, so a slow sink cannot become latency on the request path.
type logThrottle struct {
	every time.Duration
	// nowFn is a test seam read without the mutex, which guards only last. Set
	// it before the throttle is shared and leave it alone afterwards.
	nowFn func() time.Time

	mu   sync.Mutex
	last time.Time
}

// window reports the throttle interval, defaulting rather than trusting a
// non-positive value, which would let every event through.
func (t *logThrottle) window() time.Duration {
	if t.every <= 0 {
		return internalErrorLogEvery
	}
	return t.every
}

// do runs write if the window has elapsed, and consumes the window when it
// does. Taking the write rather than reporting a verdict keeps the consumption
// at the call site. Sub against the zero time saturates, so the first runs.
func (t *logThrottle) do(write func()) {
	// nowFn is caller-supplied, so it is read outside the lock for the same
	// reason write is called outside it: no arbitrary code in the critical
	// section of a type every request contends on.
	if t.claimWindow(nowOr(t.nowFn)) {
		write()
	}
}

// claimWindow reports whether the window has elapsed, and claims it when it
// has — named for the mutation, since claiming without writing swallows a line.
// Split from do only so the lock can be defer-guarded.
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
// showing the client a fixed message. It implements Is and not Unwrap on
// purpose: an exposed chain would let a caller's *fiber.Error set the status.
type clientSafeError struct{ cause error }

func (e *clientSafeError) Error() string { return "Internal Server Error" }

func (e *clientSafeError) Is(target error) bool { return errors.Is(e.cause, target) }

// responseRecorder captures what the SPNEGO handler writes so the middleware
// can inspect the outcome before replaying it onto the Fiber response.
type responseRecorder struct {
	headers http.Header
	status  int
	body    bytes.Buffer
	// Set by sessionManagerProbe: the one internal failure the middleware
	// learns as a fact rather than reading back out of the response.
	sessionFailed bool
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
// Nothing is sent — the response must be seen whole before it can be classified
// — and it is the only optional interface a buffer can honour at all.
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

// copyHeadersTo replays the recorded headers, which SPNEGO sets on success as
// well as failure. Ranged directly so a resumed session, which records nothing,
// does not allocate the map; Add because a manager may write a header twice.
func (r *responseRecorder) copyHeadersTo(ctx fiber.Ctx) {
	for key, values := range r.headers {
		for _, value := range values {
			ctx.Response().Header.Add(key, value)
		}
	}
}

// discardLogger is what gokrb5 gets when the caller asked for no logging. Not
// nil: Ticket.GetPACType calls Printf on it unchecked when a ticket's
// authorization data fails to unmarshal, and a nil *log.Logger panics there.
var discardLogger = log.New(io.Discard, "", 0)

// resolveLogger picks the logger handed to gokrb5, falling back to one that
// discards. fiberLogger is a nil interface whenever Fiber's registered logger
// is backed by something other than *log.Logger, so it is checked first.
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
// adaptor.ConvertRequest. gokrb5 v8.4.4 reads only the Authorization header and
// the remote address; the rest is for a session manager. Recheck on upgrade.
//
// Strings out of fasthttp's pooled buffer are copied only when a manager is
// there to outlive it: a Kerberos header runs to kilobytes, and unauthenticated
// callers control how often it arrives.
func requestForSPNEGO(ctx fiber.Ctx, forSessionManager bool) *http.Request {
	fasthttpCtx := ctx.RequestCtx()
	header := make(http.Header, 2)
	if auth := ctx.Get(fiber.HeaderAuthorization); auth != "" {
		if forSessionManager {
			auth = strings.Clone(auth)
		}
		header.Set(fiber.HeaderAuthorization, auth)
	}
	// Rebuilt from the parsed cookies, not copied from the header: fasthttp's
	// Peek returns only the first Cookie line until something has read a cookie,
	// so a session cookie on a later line would go missing. Owned for free.
	if forSessionManager {
		var cookies strings.Builder
		for key, value := range fasthttpCtx.Request.Header.Cookies() {
			// fasthttp parses "Cookie: flag" and "Cookie: =sneaky" alike as an
			// empty key. Neither can be looked up by name, which is all a manager
			// does, so forwarding either can only mislead.
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
	req := &http.Request{
		// Not cloned: Fiber answers this from its own table of constants.
		Method: ctx.Method(),
		// Scheme at most, filled in below. gokrb5 never reads the URL, and the
		// path is not faithfully reconstructible from here.
		URL:    &url.URL{},
		Header: header,
		// Not copied: net.Addr.String builds a new string every call.
		RemoteAddr: fasthttpCtx.RemoteAddr().String(),
		// net/http guarantees a server request's Body is non-nil, and a session
		// manager may well hold the usual "defer r.Body.Close()". There is no
		// body to offer, so it is the empty one rather than nil.
		Body: http.NoBody,
	}
	// Host, protocol, scheme, TLS and the context exist only for a session
	// manager — gokrb5 reads none of them — and each costs a copy, a header walk
	// or an allocation, so an ordinary request pays for none of it.
	if forSessionManager {
		// Carries a deadline or tracing span the application set. Skipped when
		// there is nothing to carry, since Context() installs Background as a
		// side effect that every later Locals lookup then scans past.
		if requestCtx := ctx.Context(); requestCtx != nil && requestCtx != req.Context() {
			req = req.WithContext(requestCtx)
		}
		// Host rather than Hostname: net/http documents a server request's Host
		// as "host or host:port".
		req.Host = strings.Clone(ctx.Host())
		req.TLS = fasthttpCtx.TLSConnectionState()
		// Each arm assigns its own constant, not the matched value: what Fiber
		// returns may be a view into the request buffer even when its contents
		// are one of these two, and matching does not move a string's bytes.
		switch scheme := ctx.Scheme(); scheme {
		case schemeHTTP:
			req.URL.Scheme = schemeHTTP
		case schemeHTTPS:
			req.URL.Scheme = schemeHTTPS
		default:
			req.URL.Scheme = strings.Clone(scheme)
		}
		// Same shape and the same reason. The numbers are stated alongside the
		// string so a manager comparing ProtoAtLeast against Proto is not told
		// two different things; an unparsable version leaves all three unset.
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

// serviceSettings translates Config into the options gokrb5 accepts. Only
// fields the caller set are passed on, so gokrb5's defaults stand elsewhere —
// notably MaxClockSkew's five minutes and PAC decoding.
func serviceSettings(cfg Config) []func(*service.Settings) {
	// Never nil — see discardLogger for the panic that would invite.
	opts := make([]func(*service.Settings), 0, 6)
	opts = append(opts, service.Logger(resolveLogger(cfg, flog.DefaultLogger[*log.Logger]())))
	// service.SName is deliberately not wired up: gokrb5 reads it only in
	// KRB5BasicAuthenticator, never on the SPNEGO accept path, so exposing it
	// would promise a restriction that does nothing. KeytabPrincipal is the one.
	if cfg.KeytabPrincipal != "" {
		opts = append(opts, service.KeytabPrincipal(cfg.KeytabPrincipal))
	}
	// Not observable today — gokrb5 maps an explicit zero to its own default —
	// but a version reading zero as "no skew" would reject every ticket.
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
		opts = append(opts, service.SessionManager(newSessionManagerProbe(cfg.SessionManager)))
	}
	// Clipped because every concurrent request shares this backing array. A
	// gokrb5 that appended to it rather than to a fresh slice would be writing
	// into it from several goroutines; Clip turns that into a copy.
	return slices.Clip(opts)
}

// authenticate wraps a handler in SPNEGO acceptance. A variable so tests can
// substitute a stub — reaching the authenticated branch for real needs a live
// KDC. New captures it once, so a test cannot race a request in flight.
var authenticate = spnego.SPNEGOKRB5Authenticate

// New builds the SPNEGO middleware from a Config, returning a Fiber handler
// that authenticates incoming requests against the configured keytab, or an
// error if the configuration is invalid.
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
		// Nothing named the keytab explicitly, so confirm the resolved one loads.
		// Otherwise a misconfigured host starts clean and fails every request.
		if _, err = keytabLookup(); err != nil {
			return nil, err
		}
	}
	opts := serviceSettings(cfg)
	// A session manager is the only reader of most of what requestForSPNEGO can
	// put on the request, so without one none of it is assembled.
	forSessionManager := cfg.SessionManager != nil
	// Captured at construction; see the note on authenticate.
	acceptSPNEGO := authenticate

	// Internal failures are logged rather than returned to the client, and
	// throttled separately: sharing one throttle would let a keytab line
	// suppress a session-store line, and vice versa. A third lives on the probe.
	lookupFailures := &logThrottle{every: internalErrorLogEvery}
	handlerFailures := &logThrottle{every: internalErrorLogEvery}
	// Return the middleware handler
	return func(ctx fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(ctx) {
			return ctx.Next()
		}
		// Look up the keytab
		kt, err := keytabLookup()
		if err == nil && kt == nil {
			// A caller's lookup may return no keytab and no error. gokrb5
			// dereferences it while decrypting, so nil would panic the process
			// on the first request carrying a well-formed AP-REQ.
			err = errNilKeytab
		}
		if err != nil {
			// Built once: this is the returned error's cause as well as the log
			// line, so a caller matching on it needs the value either way.
			failure := fmt.Errorf("%w: %w", ErrLookupKeytabFailed, err)
			// Quoted because a lookup is caller-supplied and may carry an
			// upstream's text. Only the log line is; OnError and the
			// ErrorHandler get the error unchanged.
			lookupFailures.do(func() {
				flog.Errorf("spnego: %s: %s", ErrLookupKeytabFailed, quoteForLog([]byte(err.Error())))
			})
			if cfg.OnError != nil {
				// The wrapped cause, not the client-safe wrapper: the hook is
				// diagnostics, and the sanitised form has nothing to inspect.
				cfg.OnError(ctx, failure)
			}
			return &clientSafeError{cause: failure}
		}
		req := requestForSPNEGO(ctx, forSessionManager)

		var (
			authenticated bool
			identity      goidentity.Identity
		)
		recorder := &responseRecorder{}
		// Records what SPNEGO decided and returns, deliberately not running the
		// rest of the chain: gokrb5's stack is under the recover below, which must
		// not swallow an application panic. gokrb5 does nothing after this returns.
		inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			authenticated = true
			identity = goidentity.FromHTTPRequestContext(r)
		})

		// gokrb5 evaluates MechTypes[0] before Verify can reject the mechanisms
		// (spnego/spnego.go:78), so an empty-but-present sequence panics — from
		// a request needing no credentials to send.
		//
		// Recovered rather than pre-validated: the same unchecked-index pattern
		// recurs throughout that package. Reported as an internal failure, since
		// nothing got far enough to judge the ticket.
		if failure := serveSPNEGO(acceptSPNEGO(inner, kt, opts...), recorder, req); failure != nil {
			handlerFailures.do(func() { flog.Errorf("spnego: %v", failure) })
			if cfg.OnError != nil {
				cfg.OnError(ctx, failure)
			}
			return &clientSafeError{cause: ErrSPNEGOHandlerFailed}
		}
		if authenticated {
			// SPNEGO wrote its accept-completed header before calling the inner
			// handler; the client needs it to authenticate the server.
			recorder.copyHeadersTo(ctx)
			SetAuthenticatedIdentityToContext(ctx, identity)
			// Before ctx.Next, so a hook counting authentications is not ordered
			// behind however long the rest of the chain takes.
			if cfg.OnSuccess != nil {
				cfg.OnSuccess(ctx, identity)
			}
			return ctx.Next()
		}
		// The request did not authenticate. sessionFailed decides it where set;
		// otherwise the header does, since gokrb5 sets one of four values on
		// every outcome and none on its internal error.
		//
		// The status decides nothing: the recorder keeps the first one written,
		// so a manager reporting its own trouble pins it at whatever it chose.
		//
		// Nothing is replayed, unlike the challenge path: returning the error
		// routes it through the ErrorHandler with a sanitised body and drops any
		// cookie for a session never stored. What was written goes to the log.
		if recorder.sessionFailed || !isSPNEGOOutcome(recorder.headers.Get(fiber.HeaderWWWAuthenticate)) {
			// On demand and at most once: this path runs on every request during
			// a session-store outage, while the throttle silences all but one
			// per window and OnError is usually nil.
			//
			// The status is the one the handler wrote, zero included — borrowing
			// the 401 substituted below would describe a handler that wrote
			// nothing as having answered.
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

		// Replay SPNEGO's own challenge. gokrb5 always writes a status alongside
		// the header; 401 is the fallback if one ever stops, since it is the
		// only answer that leaves a negotiation open.
		status := recorder.status
		if status == 0 {
			status = fiber.StatusUnauthorized
		}
		recorder.copyHeadersTo(ctx)
		ctx.Status(status)
		// Only a refusal reaches the caller's handler. The opening challenge and
		// the continuation token are legs of a handshake that can still succeed,
		// and clients renegotiate only when they arrive untouched.
		if cfg.Unauthorized != nil && isRejection(recorder.headers) {
			return cfg.Unauthorized(ctx)
		}
		return ctx.Send(recorder.body.Bytes()) //nolint:wrapcheck // Fiber's own error
	}, nil
}
