package pydanticai_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

func withoutTimestamps(events []agui.Event) []agui.Event {
	for i := range events {
		events[i].Timestamp = 0
	}
	return events
}

func Test_AGUI_StreamsRun(t *testing.T) {
	t.Parallel()

	app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel())))

	resp, body := post(t, app, "/agui", aguiBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, fiber.MIMETextEventStream, resp.Header.Get(fiber.HeaderContentType))
	require.Equal(t, "no-cache", resp.Header.Get(fiber.HeaderCacheControl))
	require.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))
	require.Empty(t, resp.Header.Get("x-vercel-ai-ui-message-stream"))

	events := aguiEvents(t, body)
	types := make([]agui.EventType, len(events))
	for i, event := range events {
		types[i] = event.Type
	}
	require.Equal(t, []agui.EventType{
		agui.EventRunStarted,
		agui.EventTextMessageStart,
		agui.EventTextMessageContent,
		agui.EventTextMessageEnd,
		agui.EventRunFinished,
	}, types)
	require.Equal(t, "thread-1", events[0].ThreadID)
	require.Equal(t, "run-1", events[0].RunID)
	require.Equal(t, "success (no tool calls)", pydanticaitest.AGUIText(events))
}

func Test_AGUI_MatchesNetHTTPHandler(t *testing.T) {
	t.Parallel()

	agent := weatherAgent()

	// The handler of pydantic-ai-go is the reference for the wire format.
	recorder := httptest.NewRecorder()
	request := newRequest(fiber.MethodPost, "/", fiber.MIMEApplicationJSON, aguiBody)
	agui.NewAdapter(agent, agui.Config{}).Handler(struct{}{}).ServeHTTP(recorder, request)
	want := withoutTimestamps(aguiEvents(t, recorder.Body.String()))
	require.Greater(t, len(want), 5)

	_, body := post(t, newApp("/agui", pydanticai.AGUI(agent)), "/agui", aguiBody)
	got := withoutTimestamps(aguiEvents(t, body))

	require.Equal(t, want, got)
	var tools int
	for _, event := range got {
		if event.Type == agui.EventToolCallResult {
			tools++
		}
	}
	require.Equal(t, 1, tools, "the tool call should have run")
}

func Test_Stream_Deps(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols[userDeps]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			app := newApp(protocol.path, protocol.handler(userAgent(), streamConfig[userDeps]{Deps: userFromQuery}))
			for _, user := range []string{"alice", "bob"} {
				_, body := post(t, app, protocol.path+"?user="+user, protocol.body)
				require.Equal(t, "user="+user, protocol.text(t, body))
			}
			for i, body := range concurrently(t, app, protocol.path, protocol.body) {
				require.Equal(t, fmt.Sprintf("user=user-%d", i), protocol.text(t, body))
			}

			t.Run("zero value without Deps", func(t *testing.T) {
				t.Parallel()
				app := newApp(protocol.path, protocol.handler(userAgent(), streamConfig[userDeps]{}))
				_, body := post(t, app, protocol.path, protocol.body)
				require.Equal(t, "user=", protocol.text(t, body))
			})
		})
	}
}

func Test_Stream_DepsError(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols[struct{}]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			model, called := unreachableModel()
			app := newApp(protocol.path, protocol.handler(ai.NewAgent[struct{}, string](model), streamConfig[struct{}]{
				Deps: func(fiber.Ctx) (struct{}, error) { return struct{}{}, fiber.ErrUnauthorized },
			}))

			resp, body := post(t, app, protocol.path, protocol.body)
			require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
			require.NotContains(t, resp.Header.Get(fiber.HeaderContentType), "event-stream")
			require.Empty(t, resp.Header.Get("x-vercel-ai-ui-message-stream"))
			require.Equal(t, "Unauthorized", body)
			require.False(t, called.Load(), "the agent must not run for a rejected request")

			// A caller that is turned away does not get its payload parsed.
			resp, _ = post(t, app, protocol.path, "{")
			require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
		})
	}
}

func Test_Stream_RunOptionsAndContext(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols[struct{}]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				c.SetContext(context.WithValue(c.Context(), ctxKey{}, "trace-1"))
				return c.Next()
			})
			app.Post(protocol.path, protocol.handler(ai.NewAgent[struct{}, string](echoModel()), streamConfig[struct{}]{
				RunOptions: func(c fiber.Ctx) ([]ai.RunOption, error) {
					if c.Query("deny") != "" {
						return nil, fiber.ErrForbidden
					}
					return []ai.RunOption{ai.WithRunInstructions("from-options")}, nil
				},
			}))

			_, body := post(t, app, protocol.path, protocol.body)
			require.Equal(t, "from-options|trace-1", protocol.text(t, body))

			resp, _ := post(t, app, protocol.path+"?deny=1", protocol.body)
			require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
		})
	}
}

func Test_Stream_AgentError(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")
	agent := ai.NewAgent[struct{}, string](agenttest.Failing(secret))

	for _, protocol := range streamProtocols[struct{}]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			t.Run("tells the client the kind of failure, and OnClose the error", func(t *testing.T) {
				t.Parallel()
				closed := make(chan error, 1)
				handler := protocol.handler(agent, streamConfig[struct{}]{OnClose: func(_ struct{}, err error) { closed <- err }})

				resp, body := post(t, newApp(protocol.path, handler), protocol.path, protocol.body)
				require.Equal(t, fiber.StatusOK, resp.StatusCode, "the status is sent before the run starts")
				require.Equal(t, "Internal Server Error", protocol.failure(t, body))
				require.NotContains(t, body, "sk-secret", "the error of the provider must not reach the client")
				require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), secret)
			})

			t.Run("ErrorMessage replaces the message", func(t *testing.T) {
				t.Parallel()
				closed := make(chan error, 1)
				handler := protocol.handler(agent, streamConfig[struct{}]{
					ErrorMessage: func(error) string { return "the model failed" },
					OnClose:      func(_ struct{}, err error) { closed <- err },
				})

				_, body := post(t, newApp(protocol.path, handler), protocol.path, protocol.body)
				require.Equal(t, "the model failed", protocol.failure(t, body))
				require.NotContains(t, body, "sk-secret")
				require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), secret, "OnClose still receives the original error")
			})

			t.Run("ErrorMessage can send the error itself", func(t *testing.T) {
				t.Parallel()
				handler := protocol.handler(agent, streamConfig[struct{}]{ErrorMessage: tellError})

				_, body := post(t, newApp(protocol.path, handler), protocol.path, protocol.body)
				require.Contains(t, protocol.failure(t, body), "sk-secret")
			})
		})
	}
}

func Test_Stream_EventThatCannotBeEncoded(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols[struct{}]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			t.Run("ends the stream with an error", func(t *testing.T) {
				t.Parallel()
				closed := make(chan error, 1)
				handler := protocol.handler(progressAgent(), streamConfig[struct{}]{OnClose: func(_ struct{}, err error) { closed <- err }})

				resp, body := post(t, newApp(protocol.path, handler), protocol.path, protocol.body)
				require.Equal(t, fiber.StatusOK, resp.StatusCode)
				require.Equal(t, "Internal Server Error", protocol.failure(t, body))
				err := agenttest.Receive(t, closed, "OnClose")
				require.ErrorContains(t, err, protocol.encodeError)
				require.ErrorContains(t, err, "cannot encode the payload")
			})

			t.Run("ErrorMessage replaces the message", func(t *testing.T) {
				t.Parallel()
				handler := protocol.handler(progressAgent(), streamConfig[struct{}]{
					ErrorMessage: func(error) string { return "the run failed" },
				})

				_, body := post(t, newApp(protocol.path, handler), protocol.path, protocol.body)
				require.Equal(t, "the run failed", protocol.failure(t, body))
			})
		})
	}
}

func Test_Stream_OnCloseReceivesTheDeps(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols[userDeps]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			closed := make(chan closeEvent, 1)
			handler := protocol.handler(userAgent(), streamConfig[userDeps]{
				Deps:    userFromQuery,
				OnClose: func(deps userDeps, err error) { closed <- closeEvent{deps: deps, err: err} },
			})

			post(t, newApp(protocol.path, handler), protocol.path+"?user=alice", protocol.body)
			event := agenttest.Receive(t, closed, "OnClose")
			require.NoError(t, event.err)
			require.Equal(t, userDeps{User: "alice"}, event.deps)
		})
	}
}

func Test_Stream_Timeout(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols[struct{}]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			model, _, canceled := agenttest.Blocking()
			closed := make(chan error, 1)
			handler := protocol.handler(ai.NewAgent[struct{}, string](model), streamConfig[struct{}]{
				Timeout: 100 * time.Millisecond,
				OnClose: func(_ struct{}, err error) { closed <- err },
			})

			resp, body := post(t, newApp(protocol.path, handler), protocol.path, protocol.body)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
			require.Equal(t, protocol.timeoutReason, protocol.canceled(t, body), "the adapter tells a timeout from a cancellation")
			require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the model call"), context.DeadlineExceeded)
			require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), context.DeadlineExceeded)
		})
	}
}

func Test_Stream_Heartbeat(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols[struct{}]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			config := streamConfig[struct{}]{HeartbeatInterval: 30 * time.Millisecond}

			_, body := post(t, newApp(protocol.path, protocol.handler(slowAgent(), config)), protocol.path, protocol.body)
			data, heartbeats := parseSSE(body)
			require.Positive(t, heartbeats, "an idle stream should carry heartbeat comments")
			require.Equal(t, "done", protocol.text(t, body))
			require.Len(t, data, protocol.records)

			config.DisableHeartbeat = true
			_, body = post(t, newApp(protocol.path, protocol.handler(slowAgent(), config)), protocol.path, protocol.body)
			_, heartbeats = parseSSE(body)
			require.Zero(t, heartbeats)
		})
	}
}

func Test_Stream_Panics(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	for _, protocol := range streamProtocols[struct{}]() {
		require.Panics(t, func() { protocol.handler(nil, streamConfig[struct{}]{}) }, protocol.name)
		require.Panics(t, func() {
			protocol.handler(agent, streamConfig[struct{}]{Adapter: adapterLimits{MaxRequestBytes: -1}})
		}, protocol.name)
	}
	require.Panics(t, func() {
		pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{Adapter: agui.Config{Version: "not-a-version"}})
	})
	require.Panics(t, func() {
		pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{Adapter: vercel.Config{SDKVersion: 4}})
	})
}

func Test_AGUI_ToolsSeeTheRequestContextAndItsCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	seen := make(chan any, 1)
	canceled := make(chan error, 1)
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "slow", func(ctx context.Context, _ struct{}) (string, error) {
		seen <- ctx.Value(ctxKey{})
		close(started)
		<-ctx.Done()
		canceled <- context.Cause(ctx)
		return "", ctx.Err()
	})

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxKey{}, "trace-1"))
		return c.Next()
	})
	app.Post("/agui", pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond,
	}))

	response, closeConnection := openStream(t, listen(t, app)+"/agui", aguiBody)
	_, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	agenttest.Receive(t, started, "the tool call")
	closeConnection()

	require.Equal(t, "trace-1", agenttest.Receive(t, seen, "the context value in the tool"))
	require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the tool call"), context.Canceled)
}

func Test_AGUI_UsesTheAppErrorHandler(t *testing.T) {
	t.Parallel()

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
				return c.Status(fiberErr.Code).JSON(fiber.Map{"error": fiberErr.Message})
			}
			return c.SendStatus(fiber.StatusInternalServerError)
		},
	})
	app.Post("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel())))

	resp, body := do(t, app, fiber.MethodPost, "/agui", fiber.MIMETextPlain, aguiBody)
	require.Equal(t, fiber.StatusUnsupportedMediaType, resp.StatusCode)
	require.JSONEq(t, `{"error":"unsupported content type"}`, body)
}

func chunkTypes(chunks []vercel.Chunk) []vercel.ChunkType {
	types := make([]vercel.ChunkType, len(chunks))
	for i, chunk := range chunks {
		types[i] = chunk.Type
	}
	return types
}

func Test_Vercel_StreamsRun(t *testing.T) {
	t.Parallel()

	app := newApp("/chat", pydanticai.Vercel(ai.NewAgent[struct{}, string](fakes.NewTestModel())))

	resp, body := post(t, app, "/chat", vercelBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, fiber.MIMETextEventStream, resp.Header.Get(fiber.HeaderContentType))
	require.Equal(t, "no-cache", resp.Header.Get(fiber.HeaderCacheControl))
	require.Equal(t, "v1", resp.Header.Get("x-vercel-ai-ui-message-stream"))

	chunks, done := vercelChunks(t, body)
	require.True(t, done, "the stream must end with [DONE]")
	require.Equal(t, []vercel.ChunkType{
		vercel.ChunkStart,
		vercel.ChunkStartStep,
		vercel.ChunkTextStart,
		vercel.ChunkTextDelta,
		vercel.ChunkTextEnd,
		vercel.ChunkFinishStep,
		vercel.ChunkFinish,
	}, chunkTypes(chunks))
	require.Equal(t, "success (no tool calls)", pydanticaitest.VercelText(chunks))
	require.True(t, strings.HasSuffix(body, "data: [DONE]\n\n"))
}

func Test_Vercel_MatchesNetHTTPHandler(t *testing.T) {
	t.Parallel()

	agent := weatherAgent()
	config := vercel.Config{ServerMessageID: "message-1", SDKVersion: 6}

	recorder := httptest.NewRecorder()
	request := newRequest(fiber.MethodPost, "/", fiber.MIMEApplicationJSON, vercelBody)
	vercel.NewAdapter(agent, config).Handler(struct{}{}).ServeHTTP(recorder, request)
	want, wantDone := vercelChunks(t, recorder.Body.String())
	require.True(t, wantDone)
	require.Greater(t, len(want), 7)

	app := newApp("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{Adapter: config}))
	_, body := post(t, app, "/chat", vercelBody)
	got, gotDone := vercelChunks(t, body)

	require.True(t, gotDone)
	require.Equal(t, want, got)
	require.Contains(t, chunkTypes(got), vercel.ChunkToolOutputAvailable, "the tool call should have run")
}

func Test_Vercel_RequiresAChatID(t *testing.T) {
	t.Parallel()

	// The adapter takes the id as the conversation ID, which panics when it is empty.
	model, called := unreachableModel()
	app := newApp("/chat", pydanticai.Vercel(ai.NewAgent[struct{}, string](model)))
	// A cleanup runs once the parallel subtests are done, unlike the rest of this function.
	t.Cleanup(func() { require.False(t, called.Load(), "the agent must not run for a rejected request") })

	const message = `[{"id":"user-1","role":"user","parts":[{"type":"text","text":"Hello"}]}]`
	for name, body := range map[string]string{
		"missing":  `{"trigger":"submit-message","messages":` + message + `}`,
		"empty":    `{"trigger":"submit-message","id":"","messages":` + message + `}`,
		"not text": `{"trigger":"submit-message","id":7,"messages":` + message + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resp, _ := post(t, app, "/chat", body)
			require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
			require.Empty(t, resp.Header.Get("x-vercel-ai-ui-message-stream"), "no stream was started")
		})
	}
}

// requestCase is a request, and the status that both the net/http and the Fiber handlers answer.
type requestCase struct {
	name        string
	method      string
	contentType string
	body        string
	limits      adapterLimits
	status      int
}

func (c requestCase) request() *http.Request {
	return newRequest(c.method, "/", c.contentType, c.body)
}

// requestCases are the requests that the checks tell apart; valid is a complete request.
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
			body: valid, limits: adapterLimits{MaxRequestBytes: int64(len(valid))}, status: fiber.StatusOK,
		},
		{
			name: "body above the limit", method: post, contentType: json,
			body: valid + " ", limits: adapterLimits{MaxRequestBytes: int64(len(valid))}, status: fiber.StatusRequestEntityTooLarge,
		},
		{
			name: "allowed content type", method: post, contentType: "application/x-json",
			body: valid, limits: adapterLimits{AllowedContentTypes: []string{"application/x-json"}}, status: fiber.StatusOK,
		},
		{
			name: "json missing from the allowed content types", method: post, contentType: json,
			body: valid, limits: adapterLimits{AllowedContentTypes: []string{"application/x-json"}}, status: fiber.StatusUnsupportedMediaType,
		},
		{
			name: "one of several allowed content types", method: post, contentType: "text/plain; charset=utf-8",
			body: valid, limits: adapterLimits{AllowedContentTypes: []string{"application/x-json", "text/plain"}}, status: fiber.StatusOK,
		},
		{
			name: "no content type is allowed", method: post, contentType: json,
			body: valid, limits: adapterLimits{AllowedContentTypes: []string{}}, status: fiber.StatusUnsupportedMediaType,
		},
		{
			name: "content type check disabled", method: post, contentType: fiber.MIMETextPlain,
			body: valid, limits: adapterLimits{DisableContentTypeCheck: true}, status: fiber.StatusOK,
		},
		{
			name: "content type check disabled without a content type", method: post,
			body: valid, limits: adapterLimits{DisableContentTypeCheck: true}, status: fiber.StatusOK,
		},
		{
			name: "content type check disabled keeps the method check", method: fiber.MethodGet,
			body: valid, limits: adapterLimits{DisableContentTypeCheck: true}, status: fiber.StatusMethodNotAllowed,
		},
	}
}

// checkRequests expects the same status from the net/http handler of pydantic-ai-go and the Fiber one.
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

func Test_Stream_RequestChecksMatchTheNetHTTPHandler(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	for _, protocol := range streamProtocols[struct{}]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()
			checkRequests(t, protocol.body,
				func(c requestCase) http.Handler { return protocol.reference(agent, c.limits) },
				func(c requestCase) fiber.Handler {
					return protocol.handler(agent, streamConfig[struct{}]{Adapter: c.limits})
				},
			)
		})
	}
}

// checkEncodedBodies expects encoded bodies to be turned away before Deps runs.
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

	for _, encoding := range []string{"identity", "Identity", "IDENTITY", "idENTITy"} {
		require.Equal(t, fiber.StatusOK, status(encoding), encoding)
	}
	require.EqualValues(t, 4, resolved.Load())
}

func Test_Stream_RejectsEncodedBodiesBeforeResolvingDeps(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols[struct{}]() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			var resolved atomic.Int32
			app := newApp(protocol.path, protocol.handler(ai.NewAgent[struct{}, string](fakes.NewTestModel()), streamConfig[struct{}]{
				Deps: func(fiber.Ctx) (struct{}, error) {
					resolved.Add(1)
					return struct{}{}, nil
				},
			}))
			checkEncodedBodies(t, app, protocol.path, protocol.body, &resolved)
		})
	}
}

// A peer can write a header name in lower case, as HTTP/2 does, and an app can keep it.
func Test_RejectsEncodedBodiesWhateverTheSpellingOfTheHeader(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	tests := []struct {
		name    string
		path    string
		body    string
		handler fiber.Handler
	}{
		{"AG-UI", "/agui", aguiBody, pydanticai.AGUI(agent)},
		{"Vercel", "/chat", vercelBody, pydanticai.Vercel(agent)},
		{"Run", "/ask", runBody, pydanticai.Run(agent)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			app := newApp(test.path, test.handler, fiber.Config{DisableHeaderNormalizing: true})
			status := func(header http.Header) int {
				request := newRequest(fiber.MethodPost, test.path, fiber.MIMEApplicationJSON, test.body)
				maps.Copy(request.Header, header) // not Set, which writes the canonical spelling
				resp, _ := send(t, app, request)
				return resp.StatusCode
			}

			for _, name := range []string{"Content-Encoding", "content-encoding", "CONTENT-ENCODING"} {
				require.Equal(t, fiber.StatusUnsupportedMediaType, status(http.Header{name: {"gzip"}}), name)
			}
			require.Equal(t, fiber.StatusUnsupportedMediaType,
				status(http.Header{"Content-Encoding": {"identity"}, "content-encoding": {"gzip"}}))
			require.Equal(t, fiber.StatusUnsupportedMediaType,
				status(http.Header{"content-encoding": {"identity"}, "Content-Encoding": {"gzip"}}))

			require.Equal(t, fiber.StatusOK, status(http.Header{"content-encoding": {"identity"}}))
			require.Equal(t, fiber.StatusOK, status(http.Header{"content-encoding": {""}}))
		})
	}
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
			t.Parallel()

			app := fiber.New()
			app.Post("/", test.handler, func(c fiber.Ctx) error { return c.SendString("skipped") })

			_, body := post(t, app, "/?skip=1", test.body)
			require.Equal(t, "skipped", body)

			_, body = post(t, app, "/", test.body)
			require.Contains(t, body, test.served)
		})
	}
}

// statusLine sends an incomplete request to addr and returns the status line of the answer.
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

// partialBody is more than the buffer after which fasthttp runs the handler of a streamed body.
const partialBody = 8000

// streamingApp serves handler with StreamRequestBody: a body above the BodyLimit is unread when it runs.
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

	t.Run("declared length above a limit that the body has not reached", func(t *testing.T) {
		t.Parallel()
		// Only the announced length is above the limit: the answer must not wait for the rest.
		large := pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
			Adapter: agui.Config{MaxRequestBytes: 1 << 20},
		})
		line := statusLine(t, strings.TrimPrefix(listen(t, streamingApp(large)), "http://"),
			headers+"Content-Length: 5000000\r\n\r\n"+strings.Repeat("a", partialBody))
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
		// Not parallel: both requests report to the same error handler.
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
			require.ErrorAs(t, agenttest.Receive(t, failed, "the error handler"), &failure)
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

// The limit is checked against the body plus one byte, which must not overflow.
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
