package pydanticai_test

import (
	"bufio"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// requestCase is a request to a streaming handler and the status that the
// net/http handlers of pydantic-ai-go answer it with. The Fiber handlers have
// to answer the same.
type requestCase struct {
	name                    string
	method                  string
	contentType             string
	body                    string
	maxBytes                int64
	allowedContentTypes     []string
	disableContentTypeCheck bool
	status                  int
}

// request builds the request of the case.
func (c requestCase) request() *http.Request {
	return newRequest(c.method, "/", c.contentType, c.body)
}

// requestCases are the requests that the checks in front of a streaming handler
// tell apart. valid is a complete request of the protocol.
func requestCases(valid string) []requestCase {
	const (
		post = fiber.MethodPost
		json = fiber.MIMEApplicationJSON
	)
	return []requestCase{
		{name: "valid", method: post, contentType: json, body: valid, status: fiber.StatusOK},
		{name: "charset parameter", method: post, contentType: json + "; charset=utf-8", body: valid, status: fiber.StatusOK},
		{name: "upper case media type", method: post, contentType: "APPLICATION/JSON", body: valid, status: fiber.StatusOK},
		{name: "plain text", method: post, contentType: fiber.MIMETextPlain, body: valid, status: fiber.StatusUnsupportedMediaType},
		{name: "missing content type", method: post, body: valid, status: fiber.StatusUnsupportedMediaType},
		{name: "longer media type", method: post, contentType: "application/jsonp", body: valid, status: fiber.StatusUnsupportedMediaType},
		{name: "json patch", method: post, contentType: "application/json-patch+json", body: valid, status: fiber.StatusUnsupportedMediaType},
		{name: "form", method: post, contentType: fiber.MIMEApplicationForm, body: valid, status: fiber.StatusUnsupportedMediaType},
		{name: "malformed media type", method: post, contentType: "application/json;;", body: valid, status: fiber.StatusUnsupportedMediaType},
		{name: "get", method: fiber.MethodGet, contentType: json, body: valid, status: fiber.StatusMethodNotAllowed},
		{name: "put", method: fiber.MethodPut, contentType: json, body: valid, status: fiber.StatusMethodNotAllowed},
		{name: "delete", method: fiber.MethodDelete, contentType: json, body: valid, status: fiber.StatusMethodNotAllowed},
		{name: "patch", method: fiber.MethodPatch, contentType: json, body: valid, status: fiber.StatusMethodNotAllowed},
		{name: "invalid json", method: post, contentType: json, body: "{", status: fiber.StatusBadRequest},
		{name: "empty body", method: post, contentType: json, status: fiber.StatusBadRequest},
		{name: "array", method: post, contentType: json, body: "[]", status: fiber.StatusBadRequest},
		{name: "string", method: post, contentType: json, body: `"text"`, status: fiber.StatusBadRequest},
		{
			name: "body at the limit", method: post, contentType: json,
			body: valid, maxBytes: int64(len(valid)), status: fiber.StatusOK,
		},
		{
			name: "body above the limit", method: post, contentType: json,
			body: valid + " ", maxBytes: int64(len(valid)), status: fiber.StatusRequestEntityTooLarge,
		},
		{
			name: "allowed content type", method: post, contentType: "application/x-json",
			body: valid, allowedContentTypes: []string{"application/x-json"}, status: fiber.StatusOK,
		},
		{
			name: "json missing from the allowed content types", method: post, contentType: json,
			body: valid, allowedContentTypes: []string{"application/x-json"}, status: fiber.StatusUnsupportedMediaType,
		},
		{
			name: "one of several allowed content types", method: post, contentType: "text/plain; charset=utf-8",
			body: valid, allowedContentTypes: []string{"application/x-json", "text/plain"}, status: fiber.StatusOK,
		},
		{
			name: "no content type is allowed", method: post, contentType: json,
			body: valid, allowedContentTypes: []string{}, status: fiber.StatusUnsupportedMediaType,
		},
		{
			name: "content type check disabled", method: post, contentType: fiber.MIMETextPlain,
			body: valid, disableContentTypeCheck: true, status: fiber.StatusOK,
		},
		{
			name: "content type check disabled without a content type", method: post,
			body: valid, disableContentTypeCheck: true, status: fiber.StatusOK,
		},
		{
			name: "content type check disabled keeps the method check", method: fiber.MethodGet,
			body: valid, disableContentTypeCheck: true, status: fiber.StatusMethodNotAllowed,
		},
	}
}

// checkRequests sends the requestCases to the net/http handler of pydantic-ai-go
// and to the Fiber handler, and expects the same status from both.
func checkRequests(
	t *testing.T, valid string, reference func(requestCase) http.Handler, handler func(requestCase) fiber.Handler,
) {
	t.Helper()
	for _, test := range requestCases(valid) {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			reference(test).ServeHTTP(recorder, test.request())
			require.Equal(t, test.status, recorder.Code, "the net/http handler of pydantic-ai-go")

			app := fiber.New()
			app.All("/", handler(test))
			resp, _ := send(t, app, test.request())
			require.Equal(t, test.status, resp.StatusCode, "the Fiber handler")

			if test.status == fiber.StatusMethodNotAllowed {
				require.Equal(t, fiber.MethodPost, recorder.Header().Get(fiber.HeaderAllow))
				require.Equal(t, fiber.MethodPost, resp.Header.Get(fiber.HeaderAllow))
			}
		})
	}
}

func Test_AGUI_RequestChecksMatchTheNetHTTPHandler(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	config := func(c requestCase) agui.Config {
		return agui.Config{
			MaxRequestBytes: c.maxBytes, AllowedContentTypes: c.allowedContentTypes,
			DisableContentTypeCheck: c.disableContentTypeCheck,
		}
	}
	checkRequests(t, aguiBody,
		func(c requestCase) http.Handler {
			return agui.NewAdapter(agent, config(c)).Handler(struct{}{})
		},
		func(c requestCase) fiber.Handler {
			return pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{Adapter: config(c)})
		},
	)
}

func Test_Vercel_RequestChecksMatchTheNetHTTPHandler(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	config := func(c requestCase) vercel.Config {
		return vercel.Config{
			MaxRequestBytes: c.maxBytes, AllowedContentTypes: c.allowedContentTypes,
			DisableContentTypeCheck: c.disableContentTypeCheck,
		}
	}
	checkRequests(t, vercelBody,
		func(c requestCase) http.Handler {
			return vercel.NewAdapter(agent, config(c)).Handler(struct{}{})
		},
		func(c requestCase) fiber.Handler {
			return pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{Adapter: config(c)})
		},
	)
}

// checkEncodedBodies sends bodies in encodings the handler does not decode and
// expects them to be turned away before the dependencies are resolved.
func checkEncodedBodies(t *testing.T, app *fiber.App, path, body string, resolved *atomic.Int32) {
	t.Helper()

	status := func(encoding string) int {
		request := newRequest(fiber.MethodPost, path, fiber.MIMEApplicationJSON, body)
		request.Header.Set(fiber.HeaderContentEncoding, encoding)
		resp, _ := send(t, app, request)
		return resp.StatusCode
	}
	for _, encoding := range []string{"gzip", "br", "deflate", "zstd", "unknown", "identity, gzip", "identitz", "identit"} {
		require.Equal(t, fiber.StatusUnsupportedMediaType, status(encoding), encoding)
	}
	// The encodings can be listed over several lines of the header.
	request := newRequest(fiber.MethodPost, path, fiber.MIMEApplicationJSON, body)
	request.Header[fiber.HeaderContentEncoding] = []string{fiber.StrIdentity, "gzip"}
	resp, _ := send(t, app, request)
	require.Equal(t, fiber.StatusUnsupportedMediaType, resp.StatusCode, "identity and gzip on two lines")
	require.Zero(t, resolved.Load(), "Deps must not run for a body that is not read")

	// The identity encoding is no encoding.
	for _, encoding := range []string{"identity", "Identity", "IDENTITY", "idENTITy"} {
		require.Equal(t, fiber.StatusOK, status(encoding), encoding)
	}
	require.EqualValues(t, 4, resolved.Load())
}

func Test_AGUI_RejectsEncodedBodiesBeforeResolvingDeps(t *testing.T) {
	t.Parallel()

	var resolved atomic.Int32
	app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
		Deps: func(fiber.Ctx) (struct{}, error) {
			resolved.Add(1)
			return struct{}{}, nil
		},
	}))
	checkEncodedBodies(t, app, "/agui", aguiBody, &resolved)
}

func Test_Vercel_RejectsEncodedBodiesBeforeResolvingDeps(t *testing.T) {
	t.Parallel()

	var resolved atomic.Int32
	app := newApp("/chat", pydanticai.Vercel(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.VercelConfig[struct{}]{
		Deps: func(fiber.Ctx) (struct{}, error) {
			resolved.Add(1)
			return struct{}{}, nil
		},
	}))
	checkEncodedBodies(t, app, "/chat", vercelBody, &resolved)
}

func Test_Next(t *testing.T) {
	t.Parallel()

	skip := func(c fiber.Ctx) bool { return c.Query("skip") == "1" }
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	tests := []struct {
		name    string
		handler fiber.Handler
		body    string
		served  string // what the handler writes when it does not skip
	}{
		{"AGUI", pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{Next: skip}), aguiBody, "RUN_FINISHED"},
		{"Vercel", pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{Next: skip}), vercelBody, "[DONE]"},
		{"Run", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{Next: skip}), runBody, `"output"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := fiber.New()
			app.Post("/", test.handler, func(c fiber.Ctx) error { return c.SendString("skipped") })

			_, body := post(t, app, "/?skip=1", test.body)
			require.Equal(t, "skipped", body)

			_, body = post(t, app, "/", test.body)
			require.Contains(t, body, test.served)
		})
	}
}

// statusLine sends request to addr without closing the connection and returns
// the status line of the answer, which has to arrive although the request is
// not complete.
func statusLine(t *testing.T, addr, request string) string {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(observeTimeout)))
	_, err = io.WriteString(conn, request)
	require.NoError(t, err)
	line, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err, "the handler did not answer before the body was complete")
	return strings.TrimSpace(line)
}

// partialBody is how much of an announced body the tests send. fasthttp runs the
// handler of a streamed body once it has received more than a buffer of it.
const partialBody = 8000

// streamingApp serves handler with StreamRequestBody, which leaves a body above
// the BodyLimit unread when the handler runs.
func streamingApp(handler fiber.Handler) *fiber.App {
	return newApp("/agui", handler, fiber.Config{StreamRequestBody: true, BodyLimit: 1 << 10})
}

func Test_AGUI_StreamedBodiesAreReadUpToTheLimitOnly(t *testing.T) {
	t.Parallel()

	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
		Adapter: agui.Config{MaxRequestBytes: 1 << 10},
	})
	addr := strings.TrimPrefix(listen(t, streamingApp(handler)), "http://")
	const headers = "POST /agui HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n"

	t.Run("declared length above the limit", func(t *testing.T) {
		t.Parallel()
		// Most of the body is announced and never sent: the answer must not wait for it.
		line := statusLine(t, addr, headers+"Content-Length: 1000000\r\n\r\n"+strings.Repeat("a", partialBody))
		require.Contains(t, line, "413")
	})

	t.Run("chunked above the limit", func(t *testing.T) {
		t.Parallel()
		// One chunk above the limit, and no end of the body.
		chunk := strings.Repeat("a", 4096)
		line := statusLine(t, addr, headers+"Transfer-Encoding: chunked\r\n\r\n"+strconv.FormatInt(int64(len(chunk)), 16)+"\r\n"+chunk+"\r\n")
		require.Contains(t, line, "413")
	})

	t.Run("a complete body above the limit", func(t *testing.T) {
		t.Parallel()
		// Larger than the limit of the handler, and than the BodyLimit of the app, so fasthttp streams it.
		padding := strings.Repeat(" ", 2<<10-len(aguiBody))
		body := aguiBody[:len(aguiBody)-1] + padding + "}"
		response, _ := openStream(t, "http://"+addr+"/agui", body)
		require.Equal(t, fiber.StatusRequestEntityTooLarge, response.StatusCode)
	})

	t.Run("a streamed body that fits", func(t *testing.T) {
		t.Parallel()
		app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel())),
			fiber.Config{StreamRequestBody: true, BodyLimit: 64})
		response, _ := openStream(t, listen(t, app)+"/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, response.StatusCode)
		record, ok := pydanticaitest.NewReader(response.Body).NextData()
		require.True(t, ok)
		require.Contains(t, record, `"type":"RUN_STARTED"`)
	})
}

func Test_AGUI_StreamedBodiesWaitForDeps(t *testing.T) {
	t.Parallel()

	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
		Deps: func(fiber.Ctx) (struct{}, error) { return struct{}{}, fiber.ErrUnauthorized },
	})
	addr := strings.TrimPrefix(listen(t, streamingApp(handler)), "http://")

	// The body is too large as well, but the caller is turned away first.
	line := statusLine(t, addr,
		"POST /agui HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 1000000\r\n\r\n"+
			strings.Repeat("a", partialBody))
	require.Contains(t, line, "401")
}

func Test_AGUI_AStreamedBodyThatDoesNotArriveIsABadRequest(t *testing.T) {
	t.Parallel()

	failed := make(chan error, 1)
	app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel())), fiber.Config{
		StreamRequestBody: true,
		BodyLimit:         1 << 10,
		ReadTimeout:       300 * time.Millisecond,
		ErrorHandler: func(c fiber.Ctx, err error) error {
			failed <- err
			return fiber.DefaultErrorHandler(c, err)
		},
	})
	addr := strings.TrimPrefix(listen(t, app), "http://")
	// The client announces more of the body than it sends.
	const request = "POST /agui HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n"

	tests := []struct {
		name    string
		leave   bool
		message string
	}{
		{"the client leaves", true, "invalid AG-UI request"},
		{"the client stalls", false, "read request body"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			_, err = io.WriteString(conn, request+strings.Repeat("a", partialBody))
			require.NoError(t, err)
			if test.leave {
				require.NoError(t, conn.Close())
			}

			var failure *fiber.Error
			require.ErrorAs(t, receive(t, failed, "the error handler"), &failure)
			require.Equal(t, fiber.StatusBadRequest, failure.Code)
			require.Equal(t, test.message, failure.Message)
		})
	}
}

func Test_AGUI_ReleasesTheBufferOfLargeBodies(t *testing.T) {
	t.Parallel()

	bodyLength := func(body string) int {
		var length atomic.Int64
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			err := c.Next()
			length.Store(int64(len(c.Body())))
			return err
		})
		app.Post("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel())))
		resp, _ := post(t, app, "/agui", body)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		return int(length.Load())
	}

	// A small body stays where it is for the middleware that runs after the handler.
	require.Equal(t, len(aguiBody), bodyLength(aguiBody))

	// A large one is not kept for as long as the run takes.
	large := aguiBody[:len(aguiBody)-len(`"Hello"}]}`)] + `"` + strings.Repeat("x", 100<<10) + `"}]}`
	require.Greater(t, len(large), 100<<10)
	require.Zero(t, bodyLength(large))
}

// The limit is checked against the body plus one byte, which must not wrap
// around for the largest limit.
func Test_MaxRequestBytesOfMaxInt64(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	for _, streamed := range []bool{false, true} {
		config := fiber.Config{StreamRequestBody: streamed}

		app := newApp("/agui", pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
			Adapter: agui.Config{MaxRequestBytes: math.MaxInt64},
		}), config)
		resp, _ := post(t, app, "/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode, "AGUI, StreamRequestBody: %v", streamed)

		app = newApp("/ask", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{
			MaxRequestBytes: math.MaxInt64,
		}), config)
		resp, _ = post(t, app, "/ask", runBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode, "Run, StreamRequestBody: %v", streamed)
	}
}
