package pydanticai

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// benchContext returns a request context of app for a POST with the headers.
func benchContext(b testing.TB, app *fiber.App, body string, headers map[string]string) fiber.Ctx {
	b.Helper()

	ctx := new(fasthttp.RequestCtx)
	ctx.Request.Header.SetMethod(fiber.MethodPost)
	for name, value := range headers {
		ctx.Request.Header.Set(name, value)
	}
	ctx.Request.SetBodyString(body)
	c := app.AcquireCtx(ctx)
	b.Cleanup(func() { app.ReleaseCtx(c) })
	return c
}

func Benchmark_requestLimits_check(b *testing.B) {
	app := fiber.New()
	tests := []struct {
		name    string
		limits  requestLimits
		headers map[string]string
	}{
		{"json", newRequestLimits(nil, 0, false), map[string]string{fiber.HeaderContentType: "application/json"}},
		{"json with charset", newRequestLimits(nil, 0, false), map[string]string{fiber.HeaderContentType: "application/json; charset=utf-8"}},
		{"json in mixed case", newRequestLimits(nil, 0, false), map[string]string{fiber.HeaderContentType: "Application/JSON; Charset=UTF-8"}},
		{"identity encoding", newRequestLimits(nil, 0, false), map[string]string{
			fiber.HeaderContentType: "application/json", fiber.HeaderContentEncoding: "Identity",
		}},
		{"one of several types", newRequestLimits([]string{"application/json", "text/plain"}, 0, false),
			map[string]string{fiber.HeaderContentType: "text/plain"}},
		{"rejected", newRequestLimits(nil, 0, false), map[string]string{fiber.HeaderContentType: "text/plain"}},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			c := benchContext(b, app, "", test.headers)
			b.ReportAllocs()
			for b.Loop() {
				_ = test.limits.check(c)
			}
		})
	}
}

// Benchmark_admit shows the share of check in the admission of a request.
func Benchmark_admit(b *testing.B) {
	const body = `{"threadId":"t","runId":"r","messages":[{"id":"u","role":"user","content":"` +
		`Hello, how is the weather in Paris today?"}],"tools":[],"context":[],"state":null,"forwardedProps":null}`
	app := fiber.New()
	for _, contentType := range []string{"application/json", "application/json; charset=utf-8"} {
		b.Run(strings.ReplaceAll(contentType, " ", ""), func(b *testing.B) {
			c := benchContext(b, app, body, map[string]string{fiber.HeaderContentType: contentType})
			limits := newRequestLimits(nil, 0, false) // once per handler, not per request
			b.ReportAllocs()
			for b.Loop() {
				if _, err := admit[struct{}, agui.RunAgentInput](c, limits, "AG-UI", nil, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// parsedContentTypeAllows is the reference verdict that allowsContentType has to
// agree with: the header is parsed, and the media type must be one of the
// configured ones, as the net/http handlers of pydantic-ai-go check it.
func parsedContentTypeAllows(contentTypes []string, header string) bool {
	if contentTypes == nil {
		contentTypes = defaultContentTypes
	}
	mediaType, _, err := mime.ParseMediaType(header)
	return err == nil && slices.ContainsFunc(contentTypes, func(candidate string) bool {
		return strings.EqualFold(strings.TrimSpace(candidate), mediaType)
	})
}

// contentTypeLists are the accepted media types that the tests configure.
var contentTypeLists = [][]string{
	nil,
	{},
	{"application/json"},
	{" Application/JSON\t"},
	{"application/json", "text/plain"},
	{"APPLICATION/X-NDJSON", "text/event-stream"},
	{"application/*"},
	{"json"},
	{""},
	{"application/json;"},
	{"application/json; charset=utf-8"},
	{"application/json+x", "application/json"},
	// Not valid, and equal to a valid one only for a Unicode-aware comparison: ſ is a case of s, and the Kelvin sign of k.
	{"application/jſon", "text/\u212Aplain"},
}

// contentTypeHeaders returns the Content-Type headers that the tests send: the
// media types that the lists accept or nearly accept, followed by every kind of
// parameters, so that most of them are one of the usual forms but off by a byte.
func contentTypeHeaders() []string {
	bases := []string{
		"", "application/json", "Application/JSON", "APPLICATION/JSON", "application/jsoN", "application/js0n",
		" application/json", "application/json ", "\tapplication/json\t", "application/json\x00", "\x00application/json",
		"application/jsonx", "application/jso", "application/json+x", "application/*", "json", "text/plain", "TEXT/PLAIN",
		"application/x-ndjson", "text/event-stream", "application/json;", "application/json; charset=utf-8",
		// Bytes whose lower or upper case is a letter only for a Unicode-aware comparison, or that
		// differ from a letter by the bit that an ASCII case fold clears: ';' is ESC with it,
		// '/' is SI, '-' is CR, '=' is GS and '8' is CAN.
		"application/json\x1b", "application/\x6ason", "application/jſon", "applicatioN/json", "application\x0fjson",
		"text/\u212Aplain", "text/kplain", "text/plain\x0d", "appl\u0131cation/json", "application/JSON\u00a0",
	}
	params := []string{
		"", ";", "; ", ";\t", ";;", "; charset=utf-8", ";charset=utf-8", "; Charset=UTF-8", ";CHARSET=utf-8", "; charset=utf-8 ",
		" ; charset=utf-8", "\t;charset=utf-8", ";  charset=utf-8", "; charset=utf-8;", "; charset=utf-8; ", "; charset=utf-8;;",
		"; charset=UTF-8", "; charset=utf8", "; charset=utf-16", "; charset=", "; charset", "; =utf-8", "; charset = utf-8",
		"; charset=\"utf-8\"", "; charset='utf-8'", "; charset=utf-8 x", "; charset=utf-8; boundary=x", "; boundary=x",
		"; charset=utf-8; charset=utf-8", "; charset=utf-8; charset=utf-16", "; charset*=utf-8", "; charset*0=utf",
		";\x1bcharset=utf-8", "; charset\x1dutf-8", "; charset=utf-\x18", "\x1b charset=utf-8", "; charset=utf-8\x00",
		"; charſet=utf-8", "; charset=utf-8\n", "; charset=utf-8\r\n", "; q=0.5",
	}
	headers := make([]string, 0, len(bases)*len(params))
	for _, base := range bases {
		for _, param := range params {
			headers = append(headers, base+param)
		}
	}
	return headers
}

func Test_requestLimits_allowsContentType(t *testing.T) {
	t.Parallel()

	headers := contentTypeHeaders()
	for _, list := range contentTypeLists {
		limits := newRequestLimits(list, 0, false)
		accepted := 0
		for _, header := range headers {
			want := parsedContentTypeAllows(list, header)
			if want {
				accepted++
			}
			require.Equal(t, want, limits.allowsContentType(header), "%q accepted by %q", header, list)
		}
		// The comparison is only worth something with headers on both sides of it.
		// A type with a parameter, which a header never spells, is the exception.
		if slices.ContainsFunc(list, func(contentType string) bool {
			return strings.TrimSpace(contentType) != "" && !strings.Contains(contentType, ";")
		}) {
			require.NotZero(t, accepted, "%q", list)
		}
		require.Less(t, accepted, len(headers), "%q", list)
	}
}

func Test_newRequestLimits_plain(t *testing.T) {
	t.Parallel()

	limits := newRequestLimits([]string{
		" application/json ", "Text/Plain", "application/*", "json", "", "application/json;",
		"application/json; charset=utf-8", "text/event-stream\t",
	}, 0, false)
	// What mime.ParseMediaType takes as it is written is compared, including a wildcard
	// and a single token. A type with parameters or an empty one is not: those are parsed.
	require.Equal(t, []string{"application/json", "Text/Plain", "application/*", "json", "text/event-stream"}, limits.plain)
	require.Equal(t, []string{
		"application/json", "Text/Plain", "application/*", "json", "", "application/json;",
		"application/json; charset=utf-8", "text/event-stream",
	}, limits.contentTypes, "the types are trimmed once, and kept in order")

	require.Equal(t, defaultContentTypes, newRequestLimits(nil, 0, false).plain)
	require.Empty(t, newRequestLimits([]string{}, 0, false).plain)
	require.NotNil(t, newRequestLimits([]string{}, 0, false).contentTypes, "an empty list accepts nothing, unlike nil")
}

// The usual headers are told apart without parsing them, which allocates.
//
// Not parallel: testing.AllocsPerRun panics when it is called by a parallel test.
func Test_requestLimits_allowsContentType_DoesNotAllocate(t *testing.T) {
	limits := newRequestLimits(nil, 0, false)
	for _, header := range []string{
		"application/json", "Application/JSON", "application/json; charset=utf-8", "application/json;charset=UTF-8",
	} {
		require.True(t, limits.allowsContentType(header), header)
		require.Zero(t, testing.AllocsPerRun(100, func() { _ = limits.allowsContentType(header) }), header)
	}
}

func Fuzz_requestLimits_allowsContentType(f *testing.F) {
	for _, header := range contentTypeHeaders() {
		f.Add(header)
	}
	limits := make([]requestLimits, len(contentTypeLists))
	for i, list := range contentTypeLists {
		limits[i] = newRequestLimits(list, 0, false)
	}
	f.Fuzz(func(t *testing.T, header string) {
		for i, list := range contentTypeLists {
			if want := parsedContentTypeAllows(list, header); want != limits[i].allowsContentType(header) {
				t.Fatalf("%q with %q: parsing says %v", header, list, want)
			}
		}
	})
}

// Benchmark_contentTypeCheck compares the check of the media type that parses
// the header with the one that compares it, on the same headers.
func Benchmark_contentTypeCheck(b *testing.B) {
	limits := newRequestLimits(nil, 0, false)
	for _, header := range []string{
		"application/json", "application/json; charset=utf-8", "Application/JSON; Charset=UTF-8",
		"application/json; charset=utf-8; boundary=x", "text/plain",
	} {
		b.Run("parse/"+header, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = parsedContentTypeAllows(nil, header)
			}
		})
		b.Run("compare/"+header, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = limits.allowsContentType(header)
			}
		})
	}
}

// resolved runs resolve with the context of a request.
func resolved[Deps any](
	t *testing.T,
	deps func(fiber.Ctx) (Deps, error),
	options func(fiber.Ctx) ([]ai.RunOption, error),
) (Deps, []ai.RunOption, error) {
	t.Helper()
	return resolve(benchContext(t, fiber.New(), "", nil), deps, options)
}

func Test_resolve(t *testing.T) {
	t.Parallel()

	failure := errors.New("rejected")
	user := func(fiber.Ctx) (string, error) { return "alice", nil }
	metadata := func(fiber.Ctx) ([]ai.RunOption, error) {
		return []ai.RunOption{ai.WithRunMetadata(map[string]any{"k": "v"})}, nil
	}

	t.Run("without callbacks", func(t *testing.T) {
		t.Parallel()

		deps, options, err := resolved[string](t, nil, nil)
		require.NoError(t, err)
		require.Empty(t, deps, "the zero value of Deps")
		require.Nil(t, options)
	})

	t.Run("both callbacks", func(t *testing.T) {
		t.Parallel()

		deps, options, err := resolved(t, user, metadata)
		require.NoError(t, err)
		require.Equal(t, "alice", deps)
		require.Len(t, options, 1)
	})

	t.Run("Deps fails", func(t *testing.T) {
		t.Parallel()

		asked := false
		_, options, err := resolved(t,
			func(fiber.Ctx) (string, error) { return "", failure },
			func(fiber.Ctx) ([]ai.RunOption, error) { asked = true; return nil, nil })
		require.ErrorIs(t, err, failure)
		require.Nil(t, options)
		require.False(t, asked, "an error ends the resolution")
	})

	t.Run("RunOptions fails", func(t *testing.T) {
		t.Parallel()

		_, options, err := resolved(t, user, func(fiber.Ctx) ([]ai.RunOption, error) {
			return []ai.RunOption{ai.WithRunMetadata(nil)}, failure
		})
		require.ErrorIs(t, err, failure)
		require.Nil(t, options, "the options of a failed call are dropped")
	})
}

func Test_serverDone(t *testing.T) {
	t.Parallel()

	t.Run("a RequestCtx that no server serves", func(t *testing.T) {
		t.Parallel()

		require.NotPanics(t, func() { require.Nil(t, serverDone(new(fasthttp.RequestCtx))) })
	})

	t.Run("closed when the server starts to shut down", func(t *testing.T) {
		t.Parallel()

		done := make(chan (<-chan struct{}), 1)
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { done <- serverDone(ctx) }}
		listener := fasthttputil.NewInmemoryListener()
		served := make(chan error, 1)
		go func() { served <- server.Serve(listener) }()

		conn, err := listener.Dial()
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
		require.NoError(t, err)

		shutdown := agenttest.Receive(t, done, "the handler")
		require.NotNil(t, shutdown)
		select {
		case <-shutdown:
			t.Fatal("the server is not shutting down")
		default:
		}

		go func() { _ = server.Shutdown() }()
		agenttest.Receive(t, shutdown, "the channel to be closed")
		require.NoError(t, agenttest.Receive(t, served, "the server to stop"))
	})
}

func Test_heartbeatInterval(t *testing.T) {
	t.Parallel()

	require.Equal(t, 5*time.Second, defaultHeartbeatInterval)
	require.Equal(t, defaultHeartbeatInterval, heartbeatInterval(0))
	require.Equal(t, defaultHeartbeatInterval, heartbeatInterval(-time.Second))
	require.Equal(t, 2*time.Second, heartbeatInterval(2*time.Second))
}

// closeRecorder is a connection that records whether it was closed.
type closeRecorder struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return nil
}

// fakeRun is a supervised run that records what the hold does to it. The test is
// the server: expire ends the run, as the server does when its timeout elapses or
// it starts shutting down.
type fakeRun struct {
	ctx     context.Context
	cancel  context.CancelFunc
	stopped atomic.Bool

	mu     sync.Mutex
	calls  []string
	expiry func() // what the hold asked to be called when the server ends the run
}

func newFakeRun() *fakeRun {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeRun{ctx: ctx, cancel: cancel}
}

func (f *fakeRun) Context() context.Context { return f.ctx }

func (f *fakeRun) AfterExpiry(fn func()) (stop func() bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expiry = fn
	return func() bool { return !f.stopped.Swap(true) }
}

func (f *fakeRun) Cancel() {
	f.record("cancel")
	f.cancel()
}

func (f *fakeRun) End(err error) { f.record("end " + text(err)) }

func (f *fakeRun) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeRun) recorded() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ", ")
}

// expire is the server ending the run, which calls what the hold asked for.
func (f *fakeRun) expire(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	expiry := f.expiry
	f.mu.Unlock()
	require.NotNil(t, expiry, "the hold asked to be told when the server ends the run")
	expiry()
}

func text(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// newTestHold returns the hold of a fake run, on a connection that records that
// it was closed. OnClose is recorded as a call of the run.
func newTestHold(grace time.Duration) (*requestHold, *fakeRun, *closeRecorder) {
	run, conn := newFakeRun(), new(closeRecorder)
	hold := newRequestHold(nil, conn, run, grace, func(err error) { run.record("close " + text(err)) })
	return hold, run, conn
}

func Test_requestHold_expire(t *testing.T) {
	t.Parallel()

	const grace = time.Hour // no check fires during the test

	t.Run("a check is armed", func(t *testing.T) {
		t.Parallel()

		h, _, _ := newTestHold(grace)
		t.Cleanup(h.finish)
		h.expire()
		require.NotNil(t, h.check)
	})

	t.Run("no check without a connection", func(t *testing.T) {
		t.Parallel()

		// A RequestCtx that no server serves has none.
		h := newRequestHold(nil, nil, newFakeRun(), grace, func(error) {})
		h.expire()
		require.Nil(t, h.check)
	})

	t.Run("no check once fasthttp is done with the body", func(t *testing.T) {
		t.Parallel()

		h, _, _ := newTestHold(grace)
		h.finish()
		h.expire()
		require.Nil(t, h.check)
	})
}

func Test_requestHold_cut(t *testing.T) {
	t.Parallel()

	// armed returns a hold whose check is armed, and whose write has been
	// blocked for blocked, or that waits for the run when blocked is 0.
	armed := func(t *testing.T, grace, blocked time.Duration) (*requestHold, *closeRecorder) {
		t.Helper()
		h, _, conn := newTestHold(grace)
		if blocked > 0 {
			h.writing.Store(time.Now().Add(-blocked).UnixNano())
		}
		h.expire()
		t.Cleanup(h.finish)
		return h, conn
	}

	t.Run("a write that is blocked for the grace", func(t *testing.T) {
		t.Parallel()

		h, conn := armed(t, time.Hour, time.Hour)
		h.cut()
		require.True(t, conn.closed.Load())
	})

	t.Run("no write", func(t *testing.T) {
		t.Parallel()

		h, conn := armed(t, time.Hour, 0)
		h.cut()
		require.False(t, conn.closed.Load(), "the client reads, and the run has nothing to send")
	})

	t.Run("a write that is blocked for part of the grace", func(t *testing.T) {
		t.Parallel()

		const grace = time.Second
		h, conn := armed(t, grace, grace-100*time.Millisecond)
		h.cut()
		require.False(t, conn.closed.Load())
		// The next check comes when the write has been blocked for the grace,
		// not a whole grace later.
		require.Eventually(t, conn.closed.Load, grace/2, time.Millisecond)
	})

	t.Run("a body that fasthttp is done with", func(t *testing.T) {
		t.Parallel()

		h, conn := armed(t, time.Hour, time.Hour)
		h.finish()
		require.False(t, h.check.Stop(), "finish stopped the check")
		h.cut() // a check that fired while finish stopped it
		require.False(t, conn.closed.Load(), "the connection serves another request")
	})
}

func Test_requestHold_release(t *testing.T) {
	t.Parallel()

	failure := errors.New("the client went away")

	t.Run("reports how the stream ended, once", func(t *testing.T) {
		t.Parallel()

		hold, run, _ := newTestHold(time.Hour)
		hold.streamEnded(failure)

		var wg sync.WaitGroup
		for range 3 {
			wg.Go(hold.release)
		}
		wg.Wait()
		hold.release()
		require.Equal(t, "cancel, end the client went away, close the client went away", run.recorded(),
			"the run is canceled, ended, and then OnClose is told, each once")
		require.Error(t, run.Context().Err(), "the run was canceled")
	})

	t.Run("waits for the run to return", func(t *testing.T) {
		t.Parallel()

		hold, run, _ := newTestHold(time.Hour)
		released := make(chan struct{})
		go func() {
			hold.release()
			close(released)
		}()

		agenttest.Receive(t, run.Context().Done(), "the run to be canceled") // first, so that it can return
		select {
		case <-released:
			t.Fatal("the hold let go of the request while the run was still running")
		case <-time.After(50 * time.Millisecond):
		}

		hold.streamEnded(nil)
		agenttest.Receive(t, released, "the release to return")
		require.Equal(t, "cancel, end ok, close ok", run.recorded())
	})

	t.Run("the context of the stream is the context of the run", func(t *testing.T) {
		t.Parallel()

		hold, run, _ := newTestHold(time.Hour)
		require.Equal(t, run.Context(), hold.Context())
	})
}

func Test_requestHold_expiry(t *testing.T) {
	t.Parallel()

	hold, run, conn := newTestHold(20 * time.Millisecond)
	t.Cleanup(hold.finish)
	hold.writing.Store(time.Now().Add(-time.Hour).UnixNano()) // a write to a client that stopped reading

	run.expire(t) // the server ended the run
	require.Eventually(t, conn.closed.Load, agenttest.ObserveTimeout, time.Millisecond)
}

func Test_holdingBody_Close(t *testing.T) {
	t.Parallel()

	const grace = 20 * time.Millisecond
	hold, run, conn := newTestHold(grace)
	hold.streamEnded(nil)
	body := &holdingBody{ReadCloser: io.NopCloser(strings.NewReader("events")), hold: hold}

	// The server ended the run while a write to the client is blocked: the check
	// that closes the connection is armed.
	hold.writing.Store(time.Now().Add(-time.Hour).UnixNano())
	hold.expire()

	// fasthttp is done with the body, and the connection can serve another request.
	require.NoError(t, body.Close())
	time.Sleep(4 * grace) // a check that Close did not stop would have fired by now
	require.False(t, conn.closed.Load(), "the connection of the next request is left alone")
	require.True(t, run.stopped.Load(), "the server can end the run, and nothing is left to look for")
	require.Equal(t, "cancel, end ok, close ok", run.recorded(), "Close releases the run")
}

// WithStreamEndGrace returns cfg with the time that a write to a client that
// stopped reading may block, once the run was told to end, set to grace.
func WithStreamEndGrace[Deps, A any](cfg StreamConfig[Deps, A], grace time.Duration) StreamConfig[Deps, A] {
	cfg.grace = grace
	return cfg
}
