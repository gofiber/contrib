package spnego

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
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
// TestUnauthorizedNotCalledOnContinueNeeded, TestUnauthorizedCalledOnRejection,
// TestRejectionWithoutHandlerPassesThrough and TestChallengeIsNotAnInternalFailure.
// TestIsRejection and TestIsSPNEGOOutcome are not among them: they build their
// tables from these same constants, so they hold whatever the constants say.
//
// One premise here has no live test and cannot get one: that
// spnegoInternalServerError sets no WWW-Authenticate, which is what lets
// isSPNEGOOutcome tell a failure from an outcome. Reaching it for real needs a
// ticket, and so a KDC. Check it by reading the source on every upgrade.
//
// A gokrb5 release that started setting a Negotiate header on that path would
// make this middleware read an internal failure as an authentication outcome.
// The consequence is bounded but not nil: sessionManagerProbe covers the case
// that occurs in practice, a session manager refusing to persist, and would
// still classify it correctly. What would be misread is the other one, gokrb5
// failing to marshal the credentials — a response with no session cookie to
// leak, and a body gokrb5 wrote itself.
const (
	spnegoContinueNeeded = "Negotiate oRQwEqADCgEBoQsGCSqGSIb3EgECAg=="
	spnegoRejected       = "Negotiate oQcwBaADCgEC"
	// spnegoBareChallenge is what gokrb5 sets when the request carried no
	// usable Authorization header, and spnegoAccepted when authentication
	// completed. Neither has its own named test the way the two challenge
	// tokens above do: the bare challenge is compared live in
	// TestChallengeIsNotAnInternalFailure, and the accepted value is only ever
	// seen on a path that returns before the check that reads these.
	spnegoBareChallenge = "Negotiate"
	spnegoAccepted      = "Negotiate oRQwEqADCgEAoQsGCSqGSIb3EgECAg=="

	// The values Fiber and fasthttp answer with when nothing has to be read out
	// of the request buffer. Matching them lets requestForSPNEGO assign a
	// constant instead of a copy; see the switches there for why that matters.
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// protocolHTTP11 and protocolHTTP10 are the two request protocols fasthttp
// serves, as bytes, so the comparison does not convert.
var (
	protocolHTTP11 = []byte("HTTP/1.1")
	protocolHTTP10 = []byte("HTTP/1.0")
)

// spnegoOutcomes is every WWW-Authenticate value gokrb5 v8.4.4 sets, and so the
// set that marks a response as an authentication outcome rather than the
// handler failing.
var spnegoOutcomes = map[string]struct{}{
	spnegoBareChallenge:  {},
	spnegoContinueNeeded: {},
	spnegoRejected:       {},
	spnegoAccepted:       {},
}

// isSPNEGOOutcome reports whether a WWW-Authenticate value is one gokrb5 wrote.
//
// The values are compared exactly rather than by scheme. gokrb5 hands a
// Config.SessionManager the raw ResponseWriter, so the header is not gokrb5's
// alone, and a manager that set a Negotiate header of its own before failing
// would otherwise have the failure read as an outcome and replayed — its
// status, its message, and the Set-Cookie for a session it never stored.
//
// This narrows that to a manager writing one of these four values verbatim, and
// is deliberately not the only defence: a manager whose New refuses is recorded
// as a fact by sessionManagerProbe, which no header can talk the gate out of.
// What is left to this check is the one internal failure that happens before
// the manager is reached, gokrb5 failing to marshal the credentials.
func isSPNEGOOutcome(value string) bool {
	_, ok := spnegoOutcomes[value]
	return ok
}

// sessionManagerProbe wraps the caller's session manager so a New that refused
// to persist is known rather than inferred. gokrb5 reports that refusal through
// spnegoInternalServerError, which writes no WWW-Authenticate — but it writes to
// a ResponseWriter the manager itself was just holding, so reading the absence
// of a header back as "the manager failed" trusts the manager not to have left
// one. This does not: the error the manager returned is the signal.
//
// Get is passed through untouched. gokrb5 discards its error and falls through
// to full ticket validation, so a failed Get is not a failed request and must
// not be recorded as one.
type sessionManagerProbe struct {
	delegate service.SessionMgr
	// signalLost throttles the one log line this type emits, which fires per
	// request for as long as the condition lasts. A pointer because the probe
	// is copied into gokrb5's settings by value, and a throttle that copied
	// with it would let every copy through its own first line.
	signalLost *logThrottle
}

// newSessionManagerProbe builds the probe with its throttle. Constructing the
// struct directly leaves that nil, which panics on the path it exists to report
// — inside the authentication middleware, on a request that was already failing.
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
			// Losing the signal is not fatal — isSPNEGOOutcome still classifies
			// most of these correctly — but it is exactly the hole this probe
			// closed, so it must not go unnoticed.
			//
			// Throttled like every other internal failure in this package, and
			// for the same reason: this fires once per request that reaches a
			// failing session store, which is a rate the caller does not
			// control but the store's outage duration does.
			p.signalLost.do(func() {
				flog.Errorf("spnego: session failure could not be recorded: %s (writer %T); "+
					"a session manager that writes a Negotiate header before failing may now "+
					"be misread as authenticated", reason, w)
			})
		}
	}
	return err //nolint:wrapcheck // gokrb5 inspects only whether this is nil
}

// recordSessionFailure marks the middleware's recorder, and reports why it could
// not when it could not — the empty string meaning it did.
//
// The writer is unwrapped along the way, following the convention net/http
// itself uses in http.ResponseController, so a gokrb5 that wrapped the writer to
// add a capability would still be seen through.
//
// The two failures are told apart because they point somewhere different. A
// writer that does not unwrap is one gokrb5 substituted, so the thing to look at
// is what changed in gokrb5. A walk that runs out has a chain it could not get
// to the end of, which is either deeper than the bound or a cycle — the message
// says both, because the two are not distinguishable from here without tracking
// every writer seen, and only one of them is fixed by raising the bound.
func recordSessionFailure(w http.ResponseWriter) string {
	// Bounded because a writer whose Unwrap returns itself, or a pair that
	// returns each other, would otherwise hang the request. Written as an
	// unwrap count rather than an inspection count so it means what its name
	// says: the writer handed in is inspected before any unwrap happens, and
	// then once more after each one.
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

// maxResponseWriterUnwraps bounds the walk in recordSessionFailure.
const maxResponseWriterUnwraps = 8

func (p sessionManagerProbe) Get(r *http.Request, k string) ([]byte, error) {
	return p.delegate.Get(r, k) //nolint:wrapcheck // passed through to gokrb5 untouched
}

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
	// nowFn is a seam for tests; the wall clock is used when it is nil. Set it
	// before the throttle is shared and leave it alone afterwards: it is read
	// without the mutex, which guards only last.
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
	// Read before the lock, not under it. nowFn is caller-supplied — a test's
	// clock — and holding the mutex across it would put arbitrary code inside
	// the critical section of a type every request handler contends on, which
	// is the same reason write is called with the lock released.
	//
	// That leaves nowFn itself unguarded, and it has to stay that way: taking
	// the lock to read it would not help, because nothing takes the lock to
	// write it. It is a construction-time seam, set before the throttle is
	// shared and not written again — a locked read would only look safe.
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
	// sessionFailed is set by sessionManagerProbe when the caller's session
	// manager refused to persist a session. It is the one internal failure the
	// middleware learns as a fact rather than reading back out of the response.
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

// Flush exists so a session manager written against net/http conventions —
// gokrb5 hands it this recorder as the raw ResponseWriter — does not panic on a
// w.(http.Flusher) assertion.
//
// It is the only optional interface implemented, and the only one worth
// implementing: Hijacker and CloseNotifier both need a connection, and there is
// none behind a buffer. A manager that asserts either without comma-ok panics,
// which is documented on Config.SessionManager rather than papered over with a
// method that could not honour the contract anyway.
//
// Nothing is sent. The middleware has to see the whole response before it can
// tell an authentication outcome from a failure, so everything stays buffered
// until it decides, and a manager cannot stream through this. Implementing the
// method makes a comma-ok probe answer yes to a writer that cannot honour it,
// which is the lesser of the two: the alternative is a panic that drops the
// connection, and a manager that streams could not work here either way.
//
// Committing the implicit status is what a real ResponseWriter does on flush,
// and matches httptest.ResponseRecorder. It costs nothing here because the
// status is no longer what classifies the response.
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

// copyHeadersTo replays the recorded headers onto the Fiber response. SPNEGO
// sets WWW-Authenticate on success as well as on failure, and the client needs
// it in both cases to complete mutual authentication.
//
// Not on every success, though. A request served from an established session
// goes straight to the inner handler with nothing recorded at all: gokrb5 sends
// no accept-completed token, because the token proves an exchange and a resumed
// session had none, and the session manager cannot have written anything either
// — the resume path calls only SessionMgr.Get, which is handed the request and
// not the writer. So this replays an empty set on every resumed request, which
// is why it copies what is there rather than expecting a particular header.
//
// r.headers is ranged directly rather than through Header(), which would
// allocate the map on exactly that path to iterate nothing. A nil map ranges
// zero times.
//
// Add rather than Set: a manager may write a header more than once — during
// New, the one path where it does hold the writer — and Set would keep only the
// last. Set-Cookie would survive either way, since fasthttp routes it through a
// path where Set appends, but nothing else would.
func (r *responseRecorder) copyHeadersTo(ctx fiber.Ctx) {
	for key, values := range r.headers {
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
// forSessionManager mirrors whether Config.SessionManager is set. Re-check this when
// upgrading gokrb5.
//
// Strings that come out of the request buffer are copied, but only when a
// session manager is configured. Fiber returns those as views into fasthttp's
// storage unless Config.Immutable is set, and fasthttp pools its request
// contexts across connections — so a string kept past the handler reads back
// whichever client came next, not merely more of the same one. Nothing else
// keeps them: gokrb5 splits and base64-decodes the Authorization header while
// the handler runs and reads none of the other fields, so only a
// Config.SessionManager can outlive the buffer, and that is ordinary enough for
// one recording which host it issued a session for.
//
// Gating them matters because the copy is not free on the request that does not
// need it: a Kerberos Authorization header runs to kilobytes, and an
// unauthenticated caller controls how often it arrives. A session record naming
// the wrong client, on the other hand, cannot be undone.
//
// Three fields need no copy either way, and say so where they are set: Method
// comes from Fiber's table of constants, RemoteAddr from net.Addr.String, and
// the Cookie header is rebuilt through a strings.Builder.
func requestForSPNEGO(ctx fiber.Ctx, forSessionManager bool) *http.Request {
	fasthttpCtx := ctx.RequestCtx()
	header := make(http.Header, 2)
	if auth := ctx.Get(fiber.HeaderAuthorization); auth != "" {
		if forSessionManager {
			auth = strings.Clone(auth)
		}
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
	//
	// Rebuilding is also what makes this one owned for free: the Builder
	// accumulates its own bytes, so unlike Host and the Authorization value —
	// which have to be copied explicitly a few lines either side of here —
	// there is nothing to do.
	if forSessionManager {
		var cookies strings.Builder
		for key, value := range fasthttpCtx.Request.Header.Cookies() {
			// A cookie with no name is dropped rather than rendered. fasthttp
			// parses both "Cookie: flag" and "Cookie: =sneaky" as an empty key,
			// so the two are indistinguishable here: emitting "=flag" produces
			// something net/http discards for an invalid name, while emitting
			// "flag" would turn "=sneaky" into a cookie actually named
			// "sneaky" that the client never sent. Neither form can be looked
			// up by name, which is all a session manager does, so forwarding
			// either can only mislead.
			if len(key) == 0 {
				continue
			}
			// Written after the skip, or a dropped cookie would leave an empty
			// element behind for the manager to parse.
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
		// Not cloned, unlike Host and the header values: Fiber answers this
		// from its own table of method constants rather than from the request
		// buffer, so there is nothing here to be invalidated.
		Method: ctx.Method(),
		// Scheme at most, filled in below. gokrb5 never reads the URL, and a
		// faithful path is not reconstructible from here: Fiber's is
		// percent-encoded or not depending on Config.UnescapePath, and net/url
		// cannot represent a malformed escape at all. The scheme, unlike the
		// path, can be stated truthfully.
		URL:    &url.URL{},
		Header: header,
		// Not copied: net.Addr.String builds a new string every call, so this
		// never pointed into the request buffer to begin with.
		RemoteAddr: fasthttpCtx.RemoteAddr().String(),
		// net/http guarantees a server request's Body is never nil, and a
		// Config.SessionManager is ordinary net/http code that may well hold
		// the usual "defer r.Body.Close()". Leaving it nil would turn that into
		// a panic inside the authentication middleware on every request; Fiber
		// installs no recover by default, so the connection would simply drop.
		// There is no body to offer — gokrb5 reads only the header, and
		// fasthttp's is not an io.ReadCloser — so it is the empty one.
		Body: http.NoBody,
	}
	// Host, protocol, scheme and TLS exist only for a session manager, which is
	// the sole reader of any of them — gokrb5 looks at none — so they sit behind
	// the same gate as the cookies, along with the context. An ordinary request
	// then pays for none of it: reading the TLS state copies and heap-allocates
	// a tls.ConnectionState, ctx.Scheme and ctx.Host each walk the headers once
	// TrustProxy is on, and the protocol has to be copied out of the request
	// buffer and parsed. The context is dearer than it looks — see below.
	//
	// Neither TLS nor Scheme is a dependable signal on its own. TLS is non-nil
	// only where Fiber terminates TLS itself, and Scheme reports https from
	// X-Forwarded-Proto only when Fiber is configured to trust the proxy —
	// without Config.TrustProxy it answers http regardless of the header. So
	// behind a TLS-terminating proxy both can say "not secure" for a connection
	// the client made over HTTPS.
	if forSessionManager {
		// A session manager is ordinary net/http code, so its store call is
		// likely to be a QueryContext or an outbound request taking
		// r.Context(). Left unset that is context.Background(), detached from
		// whatever the application is carrying; with this, a deadline or a
		// tracing span set by middleware upstream reaches the store.
		//
		// Do not read more into it than that. Fiber's own context is
		// context.Background() unless something calls SetContext, and Fiber
		// never cancels it when a client disconnects — Deadline, Done and Err
		// on its default Ctx are all documented as no-ops. So this carries what
		// the application put there and adds nothing of its own: a hung session
		// store is still bounded by its driver, or by fasthttp's read and write
		// timeouts, and not by anything visible here.
		//
		// Gated, and not only for the shallow copy WithContext makes: on a
		// request where nothing has called SetContext, Fiber's Context()
		// installs Background as a side effect, which writes a user value that
		// every later Locals lookup then scans past and that release has to
		// clear.
		//
		// Skipped entirely when there is nothing to carry, which is the common
		// case: an application that never calls SetContext gets Background from
		// Fiber, and a request with no context of its own already answers
		// Background, so the copy would produce an identical request for the
		// price of one. Comparing contexts cannot panic here — the dynamic
		// types differ unless both are Background, and that one is comparable.
		//
		// gokrb5 does read this, unlike the other four here: goidentity's
		// AddToHTTPRequestContext derives the identity's context from it, and
		// the inner handler reads the identity back out of that request. What
		// makes the base harmless is that AddToHTTPRequestContext only wraps
		// whatever it is handed with a WithValue, so nothing in the base can
		// shadow or displace the identity.
		//
		// The nil check is net/http's own: WithContext panics rather than
		// returning an error, and this takes the fiber.Ctx interface, which an
		// application may implement itself through app.NewCtxFunc. It is not a
		// general defence against such a Ctx — a nil RequestCtx would already
		// have brought the request down several lines above — only the one
		// place where a nil costs a panic instead of a zero value.
		if requestCtx := ctx.Context(); requestCtx != nil && requestCtx != req.Context() {
			req = req.WithContext(requestCtx)
		}
		// net/http documents a server request's Host as "host or host:port", so
		// it is Host rather than Hostname, which drops the port.
		req.Host = strings.Clone(ctx.Host())
		req.TLS = fasthttpCtx.TLSConnectionState()
		// Matched against the two schemes Fiber can answer with before falling
		// back to a copy. Without Config.TrustProxy — the default — Fiber
		// returns its own constants here, which cannot alias the request buffer
		// and so need no copy; assigning one anyway would allocate on every
		// request, since this escapes into the URL. Only the trusted-proxy path
		// can produce anything else, and that one does come out of the buffer.
		// Each arm assigns its own constant rather than the matched value: what
		// comes back from Fiber may be a view into the request buffer even when
		// its contents are one of these two, and matching a constant does not
		// change where a string's bytes live.
		switch scheme := ctx.Scheme(); scheme {
		case schemeHTTP:
			req.URL.Scheme = schemeHTTP
		case schemeHTTPS:
			req.URL.Scheme = schemeHTTPS
		default:
			req.URL.Scheme = strings.Clone(scheme)
		}
		// Same shape, and for the same reason: req.Proto escapes, so converting
		// fasthttp's bytes would allocate on every request. fasthttp serves
		// these two and nothing else, but the parse is kept for the default arm
		// rather than assuming that.
		//
		// The numbers are stated alongside the string so a manager comparing
		// ProtoAtLeast against Proto is not told two different things.
		// RequestURI stays empty along with the rest of the path, which cannot
		// be reconstructed faithfully.
		switch proto := fasthttpCtx.Request.Header.Protocol(); {
		case bytes.Equal(proto, protocolHTTP11):
			req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/1.1", 1, 1
		case bytes.Equal(proto, protocolHTTP10):
			req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/1.0", 1, 0
		case len(proto) > 0:
			parsed := string(proto)
			if major, minor, ok := http.ParseHTTPVersion(parsed); ok {
				req.Proto, req.ProtoMajor, req.ProtoMinor = parsed, major, minor
			}
		}
	}
	return req
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
		opts = append(opts, service.SessionManager(newSessionManagerProbe(cfg.SessionManager)))
	}
	// Clipped because this slice is passed variadically to gokrb5 and closed
	// over by the handler, so every concurrent request shares its backing
	// array. gokrb5 v8.4.4 only ranges over it and appends onto a fresh slice
	// of its own, but a version that appended to this one would be writing into
	// that shared array from several goroutines at once. Removing the spare
	// capacity turns that into a copy rather than a race, and costs nothing
	// here: Clip only narrows the header.
	return slices.Clip(opts)
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
	// Captured once. A session manager is the only reader of most of what
	// requestForSPNEGO can put on the request — cookies, host, scheme, TLS,
	// protocol and the context — so without one none of it is assembled.
	forSessionManager := cfg.SessionManager != nil
	// Captured at construction; see the note on authenticate.
	acceptSPNEGO := authenticate

	// Internal failures are logged here rather than returned to the client, and
	// throttled so a persistent fault cannot become a log flood.
	//
	// Two kinds are throttled here, each on its own: a keytab lookup that fails,
	// and a SPNEGO handler that answered something other than an authentication
	// outcome — which today means a session manager that could not persist a
	// session. Sharing one throttle would let either suppress the other, so an
	// operator chasing a broken session store could be looking at a keytab line
	// instead. Keying on the cause is what to avoid: that would let a caller
	// defeat the throttle by varying client-controlled text inside it.
	//
	// A third lives on sessionManagerProbe, and is separate for the same
	// reason even though it reports on the same fault as the second. It fires
	// only when the probe cannot record what it saw, which is a defect in this
	// middleware's grip on gokrb5 rather than a fault in the caller's session
	// store — different audience, different fix, so it must not be suppressed
	// by the line about the store being down.
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
		req := requestForSPNEGO(ctx, forSessionManager)

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
		// Reaching here means the request did not authenticate. Whether that is
		// an authentication outcome or the handler failing is decided by two
		// things, neither of them the status.
		//
		// sessionFailed is the reliable one, and covers the only internal
		// failure that occurs in practice: sessionManagerProbe saw the caller's
		// session manager refuse to persist, which nothing written to the
		// response can contradict.
		//
		// The header covers the rest. gokrb5 v8.4.4 sets one of four
		// WWW-Authenticate values on every outcome it produces, and
		// spnegoInternalServerError — its only other responder, reached when it
		// cannot marshal the credentials — sets none. Those four are compared
		// exactly rather than by scheme: the manager held this same
		// ResponseWriter a moment ago, so a looser test would let it pass its
		// own failure off as an outcome. Exact comparison does not make that
		// impossible, only deliberate.
		//
		// The status cannot be used for this at all. The recorder keeps the
		// first status written, so a manager that reports its own trouble
		// before failing pins it at whatever it chose — 200 from a bare Write,
		// or any 4xx it liked — and gokrb5's own 500 is dropped. Both earlier
		// attempts here tested the status, and both let the manager through
		// with its message and a Set-Cookie for a session never stored.
		//
		// Neither body nor headers are replayed, unlike the challenge path
		// below. Returning the error routes it through the application's
		// ErrorHandler with the same sanitised body a keytab failure gets, and
		// drops the cookie rather than advertising that session. Whatever the
		// handler wrote goes to the log instead — where it belongs, and where
		// it is the only description of what broke unless Config.Log is set.
		if recorder.sessionFailed || !isSPNEGOOutcome(recorder.headers.Get(fiber.HeaderWWWAuthenticate)) {
			failure := fmt.Errorf("%w: status %d: %s",
				ErrSPNEGOHandlerFailed, status, strings.TrimSpace(recorder.body.String()))
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
		if cfg.Unauthorized != nil && isRejection(recorder.headers) {
			return cfg.Unauthorized(ctx)
		}
		return ctx.Send(recorder.body.Bytes()) //nolint:wrapcheck // Fiber's own error
	}, nil
}
