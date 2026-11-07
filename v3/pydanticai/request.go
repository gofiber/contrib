package pydanticai

import (
	"cmp"
	"encoding/json"
	"io"
	"math"
	"mime"
	"slices"
	"strings"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	"github.com/valyala/fasthttp"
)

const (
	// defaultMaxRequestBytes mirrors the default of the pydantic-ai-go adapters.
	defaultMaxRequestBytes int64 = 10 << 20

	releaseBodySize = 64 << 10
)

var (
	defaultContentTypes = []string{fiber.MIMEApplicationJSON}

	errBodyTooLarge = fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body too large")
)

// requestLimits repeats the checks of the net/http handlers of pydantic-ai-go.
type requestLimits struct {
	contentTypes []string
	// plain are the bare media types: a header that spells one is accepted unparsed.
	plain                []string
	maxBytes             int64
	skipContentTypeCheck bool
}

func newRequestLimits(contentTypes []string, maxBytes int64, skipContentTypeCheck bool) requestLimits {
	if contentTypes == nil {
		contentTypes = defaultContentTypes
	}
	limits := requestLimits{
		contentTypes:         make([]string, len(contentTypes)),
		maxBytes:             cmp.Or(maxBytes, defaultMaxRequestBytes),
		skipContentTypeCheck: skipContentTypeCheck,
	}
	for i, contentType := range contentTypes {
		contentType = strings.TrimSpace(contentType)
		limits.contentTypes[i] = contentType
		if mediaType, _, err := mime.ParseMediaType(contentType); err == nil && mediaType == strings.ToLower(contentType) {
			limits.plain = append(limits.plain, contentType)
		}
	}
	return limits
}

func (l requestLimits) allowsContentType(contentType string) bool {
	// Fast path: the type alone or with the charset, without the map that
	// mime.ParseMediaType builds. The other headers are parsed.
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
	return slices.ContainsFunc(l.contentTypes, func(candidate string) bool {
		return strings.EqualFold(candidate, mediaType)
	})
}

func (l requestLimits) check(c fiber.Ctx) error {
	if c.Method() != fiber.MethodPost {
		c.Set(fiber.HeaderAllow, fiber.MethodPost)
		return fiber.ErrMethodNotAllowed
	}
	if !l.skipContentTypeCheck && !l.allowsContentType(c.Get(fiber.HeaderContentType)) {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported content type")
	}
	// A compressed body would fail as a 400 about its content: say what is wrong.
	// Every header line counts.
	for _, encoding := range c.Request().Header.PeekAll(fiber.HeaderContentEncoding) {
		if len(encoding) > 0 && !utils.EqualFold(utils.UnsafeString(encoding), fiber.StrIdentity) {
			return fiber.NewError(fiber.StatusUnsupportedMediaType, "unsupported content encoding")
		}
	}
	return nil
}

func (l requestLimits) read(c fiber.Ctx) ([]byte, error) {
	var body []byte
	if stream := c.BodyStream(); stream != nil {
		// Like the adapters, read at most one byte beyond the limit, to keep a huge
		// body out of memory.
		if int64(c.ContentLength()) > l.maxBytes {
			return nil, errBodyTooLarge
		}
		// No +1 on math.MaxInt64: it would wrap around and read nothing.
		var err error
		if body, err = io.ReadAll(io.LimitReader(stream, min(l.maxBytes, math.MaxInt64-1)+1)); err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, "read request body")
		}
	} else {
		body = c.BodyRaw()
	}
	if int64(len(body)) > l.maxBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

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

// serverDone returns the shutdown channel of fasthttp, or nil for a RequestCtx
// without a server, where Done panics.
func serverDone(ctx *fasthttp.RequestCtx) (done <-chan struct{}) {
	defer func() {
		if recover() != nil {
			done = nil
		}
	}()
	return ctx.Done()
}

// shutdownSignal is serverDone if cancelOnShutdown, else nil.
func shutdownSignal(c fiber.Ctx, cancelOnShutdown bool) <-chan struct{} {
	if !cancelOnShutdown {
		return nil
	}
	return serverDone(c.RequestCtx())
}

type admission[Deps, In any] struct {
	input   In
	deps    Deps
	options []ai.RunOption
}

// admit checks the headers, resolves the dependencies and only then reads the
// body, so a rejection is cheap.
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
	// encoding/json, not the app's decoder: the protocol types are specified for it.
	if err := json.Unmarshal(body, &request.input); err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid "+protocol+" request")
	}
	if len(body) > releaseBodySize {
		// Fiber keeps the request while the response is written, which can take minutes:
		// release a large buffer, as the input holds all the run needs.
		c.Request().ReleaseBody(0)
	}
	return request, nil
}
