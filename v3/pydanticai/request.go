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

	// releaseBodySize is the size above which the buffer of a request body is
	// released once the body is decoded.
	releaseBodySize = 64 << 10
)

var (
	// defaultContentTypes are the media types accepted unless configured otherwise.
	defaultContentTypes = []string{fiber.MIMEApplicationJSON}

	errBodyTooLarge = fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body too large")
)

// requestLimits holds the HTTP checks that the net/http handlers of
// pydantic-ai-go apply before they decode a request. Those handlers are not
// used here, so the same checks run in front of the Fiber handlers.
// newRequestLimits makes them, with the defaults of the adapters and the media
// types prepared for the checks.
type requestLimits struct {
	// contentTypes are the accepted media types.
	contentTypes []string
	// plain are the contentTypes that are a bare media type: valid, without
	// parameters, and in the form that mime.ParseMediaType returns up to case.
	// A header that spells one of them is accepted without parsing it.
	plain []string
	// maxBytes bounds the body.
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
		maxBytes:             cmp.Or(maxBytes, defaultMaxRequestBytes),
		skipContentTypeCheck: skipContentTypeCheck,
	}
	for i, contentType := range contentTypes {
		contentType = strings.TrimSpace(contentType)
		limits.contentTypes[i] = contentType
		// Only a type that mime.ParseMediaType returns as it is, up to case, is
		// compared with the header as it is. The others, with a parameter or a
		// trailing semicolon, or that it rejects, are compared with the parsed
		// header.
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
	return slices.ContainsFunc(l.contentTypes, func(candidate string) bool {
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
	var body []byte
	if stream := c.BodyStream(); stream != nil {
		// With StreamRequestBody fasthttp leaves a body above its limit unread.
		// The adapters never read more than one byte beyond theirs, so neither
		// do these handlers, which keeps a huge body out of memory.
		if int64(c.ContentLength()) > l.maxBytes {
			return nil, errBodyTooLarge
		}
		// The byte beyond the limit is not added to a limit of math.MaxInt64,
		// which would wrap around and read nothing.
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

// resolve runs the per-request callbacks of a handler, Deps and then
// RunOptions. Both are optional: the dependencies default to the zero value of
// Deps and there are no extra options.
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

// serverDone returns the channel that fasthttp closes when the server of ctx
// starts shutting down, which is before it waits for the connections to finish,
// or nil when ctx has no server, as a RequestCtx that a test or a benchmark
// builds itself: Done of fasthttp dereferences the server.
func serverDone(ctx *fasthttp.RequestCtx) (done <-chan struct{}) {
	defer func() {
		if recover() != nil {
			done = nil
		}
	}()
	return ctx.Done()
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
