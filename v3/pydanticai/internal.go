package pydanticai

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/sse"
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

	// errRunPanicked is what a client is told when the run panics. The panic
	// itself goes to the log and to OnClose.
	errRunPanicked = errors.New("internal error")

	// errShutdown is why a run was canceled when the server started shutting
	// down. It wraps ai.ErrRunCancelled, which the agent treats as a request to
	// stop: the run ends with the messages and the usage it has, and the stream
	// ends like one that timed out instead of with an error.
	errShutdown = fmt.Errorf("pydanticai: the server is shutting down: %w", ai.ErrRunCancelled)
)

// requestLimits holds the HTTP checks that the net/http handlers of
// pydantic-ai-go apply before they decode a request. Those handlers are not
// used here, so the same checks run in front of the Fiber handlers. The zero
// value applies the defaults of the adapters.
type requestLimits struct {
	// contentTypes are the accepted media types. nil accepts application/json.
	contentTypes []string
	// maxBytes bounds the body. Zero means defaultMaxRequestBytes.
	maxBytes int64
	// skipContentTypeCheck accepts any media type.
	skipContentTypeCheck bool
}

// check validates the method, the media type and the encoding of a request. It
// only looks at the headers, so a request that fails it costs next to nothing.
func (l requestLimits) check(c fiber.Ctx) error {
	if c.Method() != fiber.MethodPost {
		c.Set(fiber.HeaderAllow, fiber.MethodPost)
		return fiber.ErrMethodNotAllowed
	}
	if !l.skipContentTypeCheck {
		allowed := l.contentTypes
		if allowed == nil {
			allowed = defaultContentTypes
		}
		mediaType, _, err := mime.ParseMediaType(c.Get(fiber.HeaderContentType))
		if err != nil || !slices.ContainsFunc(allowed, func(candidate string) bool {
			return strings.EqualFold(strings.TrimSpace(candidate), mediaType)
		}) {
			return fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported content type")
		}
	}
	// The handlers decode the bytes as they arrive and never inflate them. A
	// compressed body would fail the JSON decoder with a 400 that blames the
	// content, so say what is wrong with the request instead.
	if encoding := c.Get(fiber.HeaderContentEncoding); encoding != "" && !strings.EqualFold(encoding, fiber.StrIdentity) {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported content encoding")
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
		var err error
		if body, err = io.ReadAll(io.LimitReader(stream, maxBytes+1)); err != nil {
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

// errorText is the message that a client receives for err.
func errorText(message func(error) string, err error) string {
	if message != nil {
		return message(err)
	}
	return err.Error()
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

// runTracker records the ai.RunInfo of a run for OnRunEnd. Only capabilities
// are given the RunInfo, so the tracker is one that starts with the run.
type runTracker struct{ info atomic.Pointer[ai.RunInfo] }

// trackRun adds the run option that records the run to options when wanted.
// The returned tracker is nil otherwise, and nothing asks it for the run.
func trackRun(options []ai.RunOption, wanted bool) ([]ai.RunOption, *runTracker) {
	if !wanted {
		return options, nil
	}
	tracker := new(runTracker)
	record := ai.BeforeRunFunc(func(_ context.Context, run *ai.RunInfo) error {
		tracker.info.Store(run)
		return nil
	})
	// A new slice, so that the slice that RunOptions returned is not written to.
	return append([]ai.RunOption{ai.WithRunCapabilities(record)}, options...), tracker
}

// run returns the recorded run, or nil when the agent did not start.
func (t *runTracker) run() *ai.RunInfo { return t.info.Load() }

// safely calls fn, which runs on the goroutine of the connection, where no
// recover middleware guards it. A panic is logged instead of ending the process.
func safely(name string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Errorf("pydanticai: panic in %s: %v\n%s", name, recovered, debug.Stack())
		}
	}()
	fn()
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
	tracker *runTracker,
	fail func(send func(data []byte) error, err error),
	run func(ctx context.Context, send func(data []byte) error) error,
) error {
	// The run outlives the handler, but a middleware can scope the context of
	// the request to the handler and cancel it when the handler returns, as the
	// timeout middleware of Fiber does. That must not cancel the run, which has
	// only just started. The values of the context are kept.
	runCtx, abort := context.WithCancelCause(context.WithoutCancel(c.Context()))

	// fasthttp closes this channel when the server starts shutting down, which
	// is before it waits for the connections to finish.
	if shutdown := c.RequestCtx().Done(); opts.cancelOnShutdown && shutdown != nil {
		go func() {
			select {
			case <-shutdown:
				abort(errShutdown)
			case <-runCtx.Done():
			}
		}()
	}

	// ended receives how the stream ended, once the run has returned.
	ended := make(chan error, 1)
	hold := &requestHold{
		Ctx: c,
		ctx: runCtx,
		release: sync.OnceFunc(func() {
			abort(nil)
			err := <-ended
			if opts.onRunEnd != nil {
				safely("OnRunEnd", func() { opts.onRunEnd(deps, tracker.run(), err) })
			}
			if opts.onClose != nil {
				safely("OnClose", func() { opts.onClose(deps, err) })
			}
		}),
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
			}
			send := func(data []byte) error {
				return stream.Event(sse.Event{Data: data})
			}
			// A panic in a tool or in a callback would end the stream without a
			// word, as sse recovers it for OnClose only.
			defer func() {
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("pydanticai: panic in the run: %v", recovered)
					log.Errorf("%v\n%s", err, debug.Stack())
					fail(send, errRunPanicked)
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
}

// Context is what sse derives the context of the stream from. It carries the
// values of the request, and is canceled by release rather than by a
// middleware that scoped the context of the request to the handler.
func (h *requestHold) Context() context.Context { return h.ctx }

// SendStreamWriter sets the body of the response like fiber.Ctx does, with a
// body stream that holds the request.
func (h *requestHold) SendStreamWriter(write func(*bufio.Writer)) error {
	return h.SendStream(&holdingBody{ReadCloser: fasthttp.NewStreamReader(write), release: h.release})
}

// holdingBody is the body stream of a response that holds its request.
type holdingBody struct {
	io.ReadCloser
	release func()
}

// Close ends the response. It does not return before the run has ended.
func (b *holdingBody) Close() error {
	err := b.ReadCloser.Close()
	// fasthttp calls Close once the response is complete, and when it cannot
	// write the response any more, which means that the client went away.
	b.release()
	return err
}
