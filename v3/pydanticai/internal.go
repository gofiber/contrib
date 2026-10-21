package pydanticai

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"mime"
	"net"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"github.com/gofiber/utils/v2"
	"github.com/valyala/fasthttp"
)

const (
	// defaultMaxRequestBytes mirrors the default of the pydantic-ai-go adapters.
	defaultMaxRequestBytes int64 = 10 << 20

	// defaultHeartbeatInterval is how often an idle stream is written to. A
	// client that went away is only noticed by the second or third write after
	// it left, so a run that nobody waits for is canceled after a few intervals.
	defaultHeartbeatInterval = 5 * time.Second

	// releaseBodySize is the size above which the buffer of a request body is
	// released once the body is decoded.
	releaseBodySize = 64 << 10
)

var (
	// defaultContentTypes are the media types accepted unless configured otherwise.
	defaultContentTypes = []string{fiber.MIMEApplicationJSON}

	errBodyTooLarge = fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body too large")

	// errShutdown is why a run was canceled when the server started shutting
	// down. It wraps ai.ErrRunCancelled, which the agent treats as a request to
	// stop: the run ends with the messages and the usage it has, and the stream
	// ends like one that timed out instead of with an error.
	errShutdown = fmt.Errorf("pydanticai: the server is shutting down: %w", ai.ErrRunCancelled)

	// errStreamFailed cancels a run whose events can no longer be sent. It wraps
	// ai.ErrRunCancelled as well, so the agent stops at its next step.
	errStreamFailed = fmt.Errorf("pydanticai: the stream failed: %w", ai.ErrRunCancelled)

	// streamEndGrace is how long a write to the client may block once Timeout
	// elapsed or the server started shutting down. A client that takes nothing
	// for that long stopped reading, and its connection is closed.
	streamEndGrace = 5 * time.Second
)

// requestLimits holds the HTTP checks that the net/http handlers of
// pydantic-ai-go apply before they decode a request. Those handlers are not
// used here, so the same checks run in front of the Fiber handlers. The zero
// value applies the defaults of the adapters; newRequestLimits also prepares
// the media types for the checks.
type requestLimits struct {
	// contentTypes are the accepted media types. nil accepts application/json.
	contentTypes []string
	// plain are the contentTypes that are a bare media type: valid, without
	// parameters, and in the form that mime.ParseMediaType returns up to case.
	// A header that spells one of them is accepted without parsing it.
	plain []string
	// maxBytes bounds the body. Zero means defaultMaxRequestBytes.
	maxBytes int64
	// skipContentTypeCheck accepts any media type.
	skipContentTypeCheck bool
}

// newRequestLimits returns the limits for the accepted media types, where nil
// accepts application/json, the largest body, where zero means
// defaultMaxRequestBytes, and whether to accept any media type.
func newRequestLimits(contentTypes []string, maxBytes int64, skipContentTypeCheck bool) requestLimits {
	if contentTypes == nil {
		contentTypes = defaultContentTypes
	}
	limits := requestLimits{
		contentTypes:         make([]string, len(contentTypes)),
		maxBytes:             maxBytes,
		skipContentTypeCheck: skipContentTypeCheck,
	}
	for i, contentType := range contentTypes {
		contentType = strings.TrimSpace(contentType)
		limits.contentTypes[i] = contentType
		// Only a type that mime.ParseMediaType returns as it is, up to case, is
		// compared with the header as it is. The others, with a parameter or a
		// trailing semicolon, or that it rejects, are compared with the parsed
		// header, as they always were.
		if mediaType, _, err := mime.ParseMediaType(contentType); err == nil && mediaType == strings.ToLower(contentType) {
			limits.plain = append(limits.plain, contentType)
		}
	}
	return limits
}

// allowsContentType reports whether contentType, the Content-Type header of a
// request, names an accepted media type.
func (l requestLimits) allowsContentType(contentType string) bool {
	// Nearly every request sends an accepted media type, alone or with the
	// charset. Those are told apart by comparing, which mime.ParseMediaType
	// does not allow: it builds the parameters as a map first. Every other
	// header is parsed, so the verdict is that of mime.ParseMediaType.
	for _, plain := range l.plain {
		if !utils.HasPrefixFold(contentType, plain) {
			continue
		}
		rest := contentType[len(plain):]
		if rest == "" || utils.EqualFold(rest, "; charset=utf-8") || utils.EqualFold(rest, ";charset=utf-8") {
			return true
		}
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	allowed := l.contentTypes
	if allowed == nil {
		allowed = defaultContentTypes
	}
	return slices.ContainsFunc(allowed, func(candidate string) bool {
		return strings.EqualFold(candidate, mediaType)
	})
}

// check validates the method, the media type and the encoding of a request. It
// only looks at the headers, so a request that fails it costs next to nothing.
func (l requestLimits) check(c fiber.Ctx) error {
	if c.Method() != fiber.MethodPost {
		c.Set(fiber.HeaderAllow, fiber.MethodPost)
		return fiber.ErrMethodNotAllowed
	}
	if !l.skipContentTypeCheck && !l.allowsContentType(c.Get(fiber.HeaderContentType)) {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported content type")
	}
	// The handlers decode the bytes as they arrive and never inflate them. A
	// compressed body would fail the JSON decoder with a 400 that blames the
	// content, so say what is wrong with the request instead. Every line of the
	// header counts, as the encodings of a body can be listed over several.
	for _, encoding := range c.Request().Header.PeekAll(fiber.HeaderContentEncoding) {
		if len(encoding) > 0 && !utils.EqualFold(utils.UnsafeString(encoding), fiber.StrIdentity) {
			return fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported content encoding")
		}
	}
	return nil
}

// read returns the body of a request that passed check, or fails when it is
// larger than the limit.
func (l requestLimits) read(c fiber.Ctx) ([]byte, error) {
	maxBytes := cmp.Or(l.maxBytes, defaultMaxRequestBytes)
	var body []byte
	if stream := c.BodyStream(); stream != nil {
		// With StreamRequestBody fasthttp leaves a body above its limit unread.
		// The adapters never read more than one byte beyond theirs, so neither
		// do these handlers, which keeps a huge body out of memory.
		if int64(c.ContentLength()) > maxBytes {
			return nil, errBodyTooLarge
		}
		// The byte beyond the limit is not added to a limit of math.MaxInt64,
		// which would wrap around and read nothing.
		var err error
		if body, err = io.ReadAll(io.LimitReader(stream, min(maxBytes, math.MaxInt64-1)+1)); err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, "read request body")
		}
	} else {
		body = c.BodyRaw()
	}
	if int64(len(body)) > maxBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// resolve runs the per-request callbacks of a handler. Both are optional: the
// dependencies default to the zero value of Deps and there are no extra options.
func resolve[Deps any](
	c fiber.Ctx,
	deps func(fiber.Ctx) (Deps, error),
	options func(fiber.Ctx) ([]ai.RunOption, error),
) (value Deps, extra []ai.RunOption, err error) {
	if deps != nil {
		if value, err = deps(c); err != nil {
			return value, nil, err
		}
	}
	if options != nil {
		if extra, err = options(c); err != nil {
			return value, nil, err
		}
	}
	return value, extra, nil
}

// admission is a request that a handler let through.
type admission[Deps, In any] struct {
	input   In
	deps    Deps
	options []ai.RunOption
}

// admit lets a request through to a handler, in the order that keeps a rejected
// request cheap. The headers are checked first. The dependencies are resolved
// next, which is where a caller is turned away. Only then is the body read and
// decoded, so a caller that is turned away does not get its payload touched.
func admit[Deps, In any](
	c fiber.Ctx,
	limits requestLimits,
	protocol string,
	deps func(fiber.Ctx) (Deps, error),
	options func(fiber.Ctx) ([]ai.RunOption, error),
) (*admission[Deps, In], error) {
	if err := limits.check(c); err != nil {
		return nil, err
	}
	request := new(admission[Deps, In])
	var err error
	if request.deps, request.options, err = resolve(c, deps, options); err != nil {
		return nil, err
	}
	body, err := limits.read(c)
	if err != nil {
		return nil, err
	}
	// encoding/json, not the JSONDecoder of the app: the protocol types are
	// specified against the standard library.
	if err := json.Unmarshal(body, &request.input); err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid "+protocol+" request")
	}
	if len(body) > releaseBodySize {
		// Fiber keeps the request for as long as the response is written, and a
		// run can take minutes. The decoded input holds everything the run
		// needs, so the buffer of a large body, which would otherwise stay
		// pinned for each open stream, can go.
		c.Request().ReleaseBody(0)
	}
	return request, nil
}

// heartbeatInterval returns the interval to use for the configured one.
func heartbeatInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return defaultHeartbeatInterval
	}
	return interval
}

// streamOptions are the response settings shared by the streaming handlers.
type streamOptions[Deps any] struct {
	onClose          func(deps Deps, err error)
	onRunEnd         func(deps Deps, run *ai.RunInfo, err error)
	timeout          time.Duration
	heartbeat        time.Duration
	disableHeartbeat bool
	cancelOnShutdown bool
}

// streamGuard keeps the events of a run flowing after its consumer stopped
// taking them, as the adapters do after an event that they cannot convert.
// pydantic-ai-go goes on emitting the events of tool calls that run
// concurrently, and an iterator that is resumed after its consumer stopped
// panics on the goroutine of the tool, where nothing recovers. The guard
// cancels the run instead, and drops the rest of its events.
type streamGuard struct{ cancel context.CancelCauseFunc }

// Setup implements ai.Capability.
func (streamGuard) Setup(*ai.CapabilityRegistry) error { return nil }

// WrapRunEventStream implements ai.RunEventStreamWrapper.
func (g streamGuard) WrapRunEventStream(_ context.Context, _ *ai.RunInfo, stream ai.EventStream) ai.EventStream {
	return func(yield func(ai.StreamEvent, error) bool) {
		stopped := false
		for event, err := range stream {
			if stopped {
				continue
			}
			if !yield(event, err) {
				stopped = true
				g.cancel(errStreamFailed)
			}
		}
	}
}

// protocol describes the events of a streaming protocol for relay.
type protocol[E any] struct {
	// what names an event of the protocol in errors.
	what string
	// encode returns the data of the server-sent event that carries event.
	encode func(event E) ([]byte, error)
	// failed reports whether event is the event that reports a failed run, and
	// withMessage returns it with message as the text for the client. Only the
	// message changes: the adapter can have set other fields of the event.
	failed      func(event E) bool
	withMessage func(event E, message string) E
	// completed reports whether event ends a run that completed. A run that was
	// canceled ends with an event of its own.
	completed func(event E) bool
}

// relay sends the events of a run to the client, and returns the error that
// ended the stream: nil when the run completed, and otherwise the error of the
// run, the error of the stream or why the run was canceled. run starts the run
// with the options it is given.
//
// relay does not stop taking events before the run ends. pydantic-ai-go emits
// the events of tool calls that run concurrently from their goroutines, and an
// iterator that is resumed after its loop stopped panics there, where nothing
// recovers. When the stream fails, relay cancels the run and drops the events
// that follow.
func relay[E any](
	ctx context.Context,
	p protocol[E],
	run func(ctx context.Context, options ...ai.RunOption) iter.Seq2[E, error],
	options []ai.RunOption,
	errorMessage func(error) string,
	send func(data []byte) error,
	fail func(send func(data []byte) error, err error),
) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	options, tools := agentrun.GuardTools(options)
	options = append([]ai.RunOption{ai.WithRunCapabilities(streamGuard{cancel: cancel})}, options...)

	var (
		failed    error // why the stream ended before the run
		completed bool  // whether the run completed
	)
	for event, runErr := range run(ctx, options...) {
		if failed != nil {
			continue
		}
		if runErr != nil {
			if panicErr := tools.Err(); panicErr != nil {
				// A tool panicked. The error of the run carries the value of the
				// panic, or the error of a tool that the panic canceled.
				failed = panicErr
				fail(send, agentrun.ErrPanicked)
				continue
			}
			if errorMessage != nil && p.failed(event) {
				event = p.withMessage(event, agentrun.Text(errorMessage, runErr))
			}
		}
		if encoded, err := p.encode(event); err != nil {
			failed = fmt.Errorf("pydanticai: encode %s: %w", p.what, err)
			fail(send, failed)
		} else if err := send(encoded); err != nil {
			failed = err
		} else if runErr != nil {
			// The adapters end the stream after the event of an error.
			failed = runErr
		} else if p.completed(event) {
			completed = true
		}
		if failed != nil {
			cancel(errStreamFailed)
		}
	}
	switch {
	case failed != nil:
		return failed
	case completed:
		// A deadline or a shutdown that comes once the run has completed does
		// not fail it.
		return nil
	default:
		// A canceled run ends the stream without an error, so report why it was
		// canceled.
		return context.Cause(ctx)
	}
}

// serverDone returns the channel that fasthttp closes when the server of ctx
// starts shutting down, or nil when ctx has no server, as a RequestCtx that a
// test or a benchmark builds itself: Done of fasthttp dereferences the server.
func serverDone(ctx *fasthttp.RequestCtx) (done <-chan struct{}) {
	defer func() {
		if recover() != nil {
			done = nil
		}
	}()
	return ctx.Done()
}

// serveStream sends what run writes as a server-sent events response.
//
// The context passed to run is canceled when a write to the client fails, when
// a heartbeat detects that the client went away, when fasthttp gives up on the
// response and when the stream ends. fasthttp does not cancel the request
// context for a disconnected client, so these are the signals that stop a run
// nobody is listening to anymore. The cancellation of the context of the
// request is not among them, see requestHold.
//
// fail ends the stream with the error event of the protocol. It is called when
// run panics.
//
// onRunEnd and then onClose are called after the response is over and the run
// has ended. tracker is the one that recorded the run, if onRunEnd is set.
func serveStream[Deps any](
	c fiber.Ctx,
	opts streamOptions[Deps],
	deps Deps,
	tracker *agentrun.Tracker,
	fail func(send func(data []byte) error, err error),
	run func(ctx context.Context, send func(data []byte) error) error,
) error {
	// The context of the request is only done before the handler returns when
	// a middleware gave up on the request, as the timeout middleware of Fiber
	// does when its deadline passes while Deps or RunOptions run. It answered
	// for the handler, and fasthttp never writes the response that the stream
	// would be sent with, so the run would never end.
	if err := context.Cause(c.Context()); err != nil {
		return err
	}

	// The run outlives the handler, but a middleware can scope the context of
	// the request to the handler and cancel it when the handler returns, as the
	// timeout middleware of Fiber does. That must not cancel the run, which has
	// only just started. The values of the context are kept.
	runCtx, abort := context.WithCancelCause(context.WithoutCancel(c.Context()))

	// ended receives how the stream ended, once the run has returned.
	ended := make(chan error, 1)
	hold := &requestHold{
		Ctx:   c,
		ctx:   runCtx,
		conn:  c.RequestCtx().Conn(),
		grace: streamEndGrace,
		release: sync.OnceFunc(func() {
			abort(nil)
			err := <-ended
			if opts.onRunEnd != nil {
				agentrun.Safely("OnRunEnd", func() { opts.onRunEnd(deps, tracker.Run(), err) })
			}
			if opts.onClose != nil {
				agentrun.Safely("OnClose", func() { opts.onClose(deps, err) })
			}
		}),
	}

	// fasthttp closes this channel when the server starts shutting down, which
	// is before it waits for the connections to finish.
	if opts.cancelOnShutdown {
		if shutdown := serverDone(c.RequestCtx()); shutdown != nil {
			go func() {
				select {
				case <-shutdown:
					abort(errShutdown)
					hold.expire()
				case <-runCtx.Done():
				}
			}()
		}
	}

	config := sse.Config{
		HeartbeatInterval: heartbeatInterval(opts.heartbeat),
		DisableHeartbeat:  opts.disableHeartbeat,
		Handler: func(_ fiber.Ctx, stream *sse.Stream) (err error) {
			ctx := stream.Context()
			if opts.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, opts.timeout)
				defer cancel()
				// The run ends when its deadline passes, unless it is stuck in a
				// write to a client that stopped reading.
				defer context.AfterFunc(ctx, func() {
					if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
						hold.expire()
					}
				})()
			}
			send := func(data []byte) error {
				// sse copies a []byte into a string before it writes it, and
				// writes a string as it is. The event is written when Event
				// returns, and nothing changes data until then.
				return stream.Event(sse.Event{Data: utils.UnsafeString(data)})
			}
			// A panic in a tool or in a callback would end the stream without a
			// word, as sse recovers it for OnClose only.
			defer func() {
				if recovered := recover(); recovered != nil {
					err = agentrun.PanicError(recovered)
					log.Errorf("%v\n%s", err, debug.Stack())
					fail(send, agentrun.ErrPanicked)
				}
			}()
			return run(ctx, send)
		},
		OnClose: func(_ fiber.Ctx, err error) { ended <- err },
	}
	return sse.New(config)(hold)
}

// requestHold is the fiber.Ctx that sse writes its response to. It ties the
// lifetime of the request to the end of the run, and gives the run a context
// of its own.
//
// fasthttp starts the goroutine that writes the response as soon as the handler
// sets it, and reuses the request when the connection is done with it, which
// for a client that went away is while the run is still winding down. A string
// that a tool took from the request would then read another request. Fiber
// keeps its own ctx, not the request of fasthttp.
//
// The body stream of the response that requestHold installs is closed by
// fasthttp before it reuses the request, after the response was written or has
// failed to be. Its Close waits for the run.
type requestHold struct {
	fiber.Ctx

	// ctx is the context of the run, see Context.
	ctx context.Context
	// release cancels the run, waits for it and reports the end of the stream
	// to OnRunEnd and OnClose. It does that once, however often it is called.
	release func()

	// conn is the connection of the request, taken while the handler runs.
	conn net.Conn
	// writing is when fasthttp started the write to the client that it is in,
	// in Unix nanoseconds, or 0 while it waits for the run.
	writing atomic.Int64

	// grace is how long a write may block once the run was told to end, see
	// streamEndGrace. It is taken when the stream starts.
	grace time.Duration

	mu sync.Mutex
	// done is set once fasthttp is done with the body: from then on the
	// connection can serve another request, and is left alone.
	done bool
	// check looks for a write that blocks, once the run was told to end.
	check *time.Timer
}

// expire starts to look for a client that stopped reading, once Timeout elapsed
// or the server started shutting down. The run, OnRunEnd, OnClose and the
// shutdown would otherwise wait for a write to the client that only fails when
// TCP gives up on the connection, which a client that keeps its receive window
// closed can delay forever.
func (h *requestHold) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done || h.conn == nil || h.check != nil {
		return
	}
	h.check = time.AfterFunc(h.grace, h.cut)
}

// cut closes the connection when a write to it has been blocked for the grace.
// The write then fails, which ends the response and the run.
func (h *requestHold) cut() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return
	}
	next := h.grace
	if since := h.writing.Load(); since != 0 {
		blocked := time.Since(time.Unix(0, since))
		if blocked >= h.grace {
			_ = h.conn.Close()
			return
		}
		next -= blocked
	}
	h.check.Reset(next)
}

// finish records that fasthttp is done with the body.
func (h *requestHold) finish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done = true
	if h.check != nil {
		h.check.Stop()
	}
}

// Context is what sse derives the context of the stream from. It carries the
// values of the request, and is canceled by release rather than by a
// middleware that scoped the context of the request to the handler.
func (h *requestHold) Context() context.Context { return h.ctx }

// SendStreamWriter sets the body of the response like fiber.Ctx does, with a
// body stream that holds the request.
func (h *requestHold) SendStreamWriter(write func(*bufio.Writer)) error {
	return h.SendStream(&holdingBody{ReadCloser: fasthttp.NewStreamReader(write), hold: h})
}

// holdingBody is the body stream of a response that holds its request.
type holdingBody struct {
	io.ReadCloser
	hold *requestHold
}

// Read is called by fasthttp between its writes to the client.
func (b *holdingBody) Read(p []byte) (int, error) {
	b.hold.writing.Store(0)
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.hold.writing.Store(time.Now().UnixNano())
	}
	return n, err
}

// Close ends the response. It does not return before the run has ended.
func (b *holdingBody) Close() error {
	b.hold.finish()
	err := b.ReadCloser.Close()
	// fasthttp calls Close once the response is complete, and when it cannot
	// write the response any more, which means that the client went away.
	b.hold.release()
	return err
}
