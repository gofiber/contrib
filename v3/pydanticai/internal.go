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
	// The bytes are decoded as they come, like the adapters do. Fiber would
	// inflate a compressed body, up to its BodyLimit, before anything else
	// could turn the caller away, and a small request could expand to a large
	// allocation.
	if encoding := c.Get(fiber.HeaderContentEncoding); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported content encoding")
	}
	return nil
}

// read returns the body of a request that passed check, or fails when it is
// larger than the limit.
func (l requestLimits) read(c fiber.Ctx) ([]byte, error) {
	maxBytes := cmp.Or(l.maxBytes, defaultMaxRequestBytes)
	request := c.Request()
	if stream := request.BodyStream(); stream != nil {
		// With StreamRequestBody fasthttp leaves a body above its limit unread.
		// The adapters never read more than one byte beyond theirs, so neither
		// do these handlers, which keeps a huge body out of memory.
		if int64(request.Header.ContentLength()) > maxBytes {
			return nil, errBodyTooLarge
		}
		body, err := io.ReadAll(io.LimitReader(stream, maxBytes+1))
		if err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, "read request body")
		}
		if int64(len(body)) > maxBytes {
			return nil, errBodyTooLarge
		}
		return body, nil
	}
	body := c.BodyRaw()
	if int64(len(body)) > maxBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// decodeJSON decodes the body of a request into target. It uses encoding/json
// instead of the JSONDecoder of the app because the protocol types are
// specified against the standard library.
func decodeJSON(body []byte, target any, protocol string) error {
	if err := json.Unmarshal(body, target); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid "+protocol+" request")
	}
	return nil
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
) (admission[Deps, In], error) {
	var request admission[Deps, In]
	if err := limits.check(c); err != nil {
		return request, err
	}
	var err error
	if request.deps, request.options, err = resolve(c, deps, options); err != nil {
		return request, err
	}
	body, err := limits.read(c)
	if err != nil {
		return request, err
	}
	if err := decodeJSON(body, &request.input, protocol); err != nil {
		return request, err
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
	timeout          time.Duration
	heartbeat        time.Duration
	disableHeartbeat bool
}

// serveStream sends what run writes as a server-sent events response.
//
// The context passed to run is canceled when a write to the client fails, when
// a heartbeat detects that the client went away, when fasthttp gives up on the
// response and when the stream ends. fasthttp does not cancel the request
// context for a disconnected client, so these are the signals that stop a run
// nobody is listening to anymore. The cancellation of the context of the
// request is not among them, see below.
//
// fail ends the stream with the error event of the protocol. It is called when
// run panics.
//
// onClose is called after the response is over and the run has ended.
func serveStream[Deps any](
	c fiber.Ctx,
	opts streamOptions[Deps],
	deps Deps,
	fail func(send func(data []byte) error, err error),
	run func(ctx context.Context, send func(data []byte) error) error,
) error {
	gone, abort := context.WithCancel(context.Background())
	hold := &requestHold{
		Ctx:      c,
		abort:    abort,
		finished: make(chan struct{}),
		closed: func(err error) {
			if opts.onClose == nil {
				return
			}
			// OnClose runs on the goroutine of the connection, which no
			// recover middleware guards.
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Errorf("pydanticai: panic in OnClose: %v\n%s", recovered, debug.Stack())
				}
			}()
			opts.onClose(deps, err)
		},
	}

	config := sse.Config{
		HeartbeatInterval: heartbeatInterval(opts.heartbeat),
		DisableHeartbeat:  opts.disableHeartbeat,
		Handler: func(_ fiber.Ctx, stream *sse.Stream) (err error) {
			ctx, cancel := context.WithCancel(stream.Context())
			defer cancel()
			defer context.AfterFunc(gone, cancel)()
			if opts.timeout > 0 {
				var cancelTimeout context.CancelFunc
				ctx, cancelTimeout = context.WithTimeout(ctx, opts.timeout)
				defer cancelTimeout()
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
		OnClose: func(_ fiber.Ctx, err error) {
			hold.err = err
			close(hold.finished)
		},
	}

	// The run outlives the handler, but a middleware can scope the context of
	// the request to the handler and cancel it when the handler returns, as the
	// timeout middleware of Fiber does. That must not cancel the run, which has
	// only just started. The values of the context are kept.
	parent := c.Context()
	c.SetContext(context.WithoutCancel(parent))
	defer c.SetContext(parent)
	return sse.New(config)(hold)
}

// requestHold is the fiber.Ctx that sse writes its response to. It ties the
// lifetime of the request to the end of the run.
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

	// abort cancels the run, once fasthttp has given up on the response.
	abort context.CancelFunc
	// finished is closed when the stream function has returned, after err is set.
	finished chan struct{}
	// err is how the stream ended.
	err error
	// closed reports err, once, to the OnClose callback.
	closed func(err error)
	once   sync.Once
}

// SendStreamWriter sets the body of the response like fiber.Ctx does, with a
// body stream that holds the request.
func (h *requestHold) SendStreamWriter(write func(*bufio.Writer)) error {
	return h.SendStream(&holdingBody{ReadCloser: fasthttp.NewStreamReader(write), hold: h}, -1)
}

// holdingBody is the body stream of a response that holds its request.
type holdingBody struct {
	io.ReadCloser
	hold *requestHold
}

// Close ends the response. It does not return before the run has ended.
func (b *holdingBody) Close() error {
	err := b.ReadCloser.Close()
	// fasthttp calls Close once the response is complete, and when it cannot
	// write the response any more, which means that the client went away.
	b.hold.abort()
	<-b.hold.finished
	b.hold.once.Do(func() { b.hold.closed(b.hold.err) })
	return err
}
