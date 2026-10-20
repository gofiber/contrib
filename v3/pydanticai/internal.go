package pydanticai

import (
	"context"
	"encoding/json"
	"mime"
	"slices"
	"strings"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
)

const (
	// defaultMaxRequestBytes mirrors the default of the pydantic-ai-go adapters.
	defaultMaxRequestBytes int64 = 10 << 20
	// defaultContentType is the only media type accepted unless configured otherwise.
	defaultContentType = "application/json"
)

// requestLimits holds the HTTP checks that the net/http handlers of
// pydantic-ai-go apply before they decode a request. Those handlers are not
// used here, so the same checks run in front of the Fiber handlers.
type requestLimits struct {
	contentTypes         []string
	maxBytes             int64
	skipContentTypeCheck bool
}

// newRequestLimits fills the defaults the adapters use for unset values.
func newRequestLimits(maxBytes int64, contentTypes []string, skipContentTypeCheck bool) requestLimits {
	if maxBytes == 0 {
		maxBytes = defaultMaxRequestBytes
	}
	if contentTypes == nil {
		contentTypes = []string{defaultContentType}
	}
	return requestLimits{
		contentTypes:         contentTypes,
		maxBytes:             maxBytes,
		skipContentTypeCheck: skipContentTypeCheck,
	}
}

// body checks the method, the content type and the size of the request and
// returns its body. These checks only look at headers and at the length, so a
// request that fails them costs next to nothing.
func (l requestLimits) body(c fiber.Ctx) ([]byte, error) {
	if c.Method() != fiber.MethodPost {
		c.Set(fiber.HeaderAllow, fiber.MethodPost)
		return nil, fiber.ErrMethodNotAllowed
	}
	if !l.skipContentTypeCheck {
		mediaType, _, err := mime.ParseMediaType(c.Get(fiber.HeaderContentType))
		if err != nil || !slices.ContainsFunc(l.contentTypes, func(allowed string) bool {
			return strings.EqualFold(strings.TrimSpace(allowed), mediaType)
		}) {
			return nil, fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported content type")
		}
	}
	body := c.Body()
	if int64(len(body)) > l.maxBytes {
		return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body too large")
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

// onClose binds the OnClose callback of a config to the dependencies of one
// request. It returns nil when the config has no callback.
func onClose[Deps any](callback func(Deps, error), deps Deps) func(error) {
	if callback == nil {
		return nil
	}
	return func(err error) { callback(deps, err) }
}

// streamOptions are the response settings shared by the streaming handlers.
type streamOptions struct {
	timeout          time.Duration
	heartbeat        time.Duration
	disableHeartbeat bool
}

// serveStream sends what run writes as a server-sent events response and calls
// onClose, which may be nil, once the stream has ended.
//
// The context passed to run is canceled when a write to the client fails, when
// a heartbeat detects that the client went away, and when the stream ends.
// fasthttp does not cancel the request context for a disconnected client, so
// this is the only signal that stops a run nobody is listening to anymore.
//
// run and onClose execute on the goroutine that fasthttp starts for the body
// of the response. When the client goes away fasthttp reuses the request while
// that goroutine is still winding down, so neither of them may use c.
func serveStream(
	c fiber.Ctx,
	opts streamOptions,
	onClose func(err error),
	run func(ctx context.Context, send func(data []byte) error) error,
) error {
	config := sse.Config{
		HeartbeatInterval: opts.heartbeat,
		DisableHeartbeat:  opts.disableHeartbeat,
		Handler: func(_ fiber.Ctx, stream *sse.Stream) error {
			ctx := stream.Context()
			if opts.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, opts.timeout)
				defer cancel()
			}
			return run(ctx, func(data []byte) error {
				return stream.Event(sse.Event{Data: data})
			})
		},
	}
	if onClose != nil {
		config.OnClose = func(_ fiber.Ctx, err error) { onClose(err) }
	}
	return sse.New(config)(c)
}
