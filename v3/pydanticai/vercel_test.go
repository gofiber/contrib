package pydanticai_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

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
	require.Equal(t, "success (no tool calls)", vercelText(chunks))
	require.True(t, strings.HasSuffix(body, "data: [DONE]\n\n"))
}

func Test_Vercel_MatchesNetHTTPHandler(t *testing.T) {
	t.Parallel()

	agent := weatherAgent()
	config := vercel.Config{ServerMessageID: "message-1", SDKVersion: 6}

	// The handler of pydantic-ai-go is the reference for the wire format.
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

func Test_Vercel_Deps(t *testing.T) {
	t.Parallel()

	handler := pydanticai.Vercel(userAgent(), pydanticai.VercelConfig[userDeps]{
		Deps: userFromQuery,
	})
	app := newApp("/chat", handler)

	for _, user := range []string{"alice", "bob"} {
		_, body := post(t, app, "/chat?user="+user, vercelBody)
		chunks, _ := vercelChunks(t, body)
		require.Equal(t, "user="+user, vercelText(chunks))
	}

	for i, body := range concurrently(t, app, "/chat", vercelBody) {
		chunks, _ := vercelChunks(t, body)
		require.Equal(t, fmt.Sprintf("user=user-%d", i), vercelText(chunks))
	}

	t.Run("zero value without Deps", func(t *testing.T) {
		t.Parallel()
		_, body := post(t, newApp("/chat", pydanticai.Vercel(userAgent())), "/chat", vercelBody)
		chunks, _ := vercelChunks(t, body)
		require.Equal(t, "user=", vercelText(chunks))
	})
}

func Test_Vercel_DepsError(t *testing.T) {
	t.Parallel()

	model, called := unreachableModel()
	handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](model), pydanticai.VercelConfig[struct{}]{
		Deps: func(fiber.Ctx) (struct{}, error) { return struct{}{}, fiber.ErrUnauthorized },
	})
	app := newApp("/chat", handler)

	resp, body := post(t, app, "/chat", vercelBody)
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	require.Empty(t, resp.Header.Get("x-vercel-ai-ui-message-stream"))
	require.Equal(t, "Unauthorized", body)
	require.False(t, called.Load(), "the agent must not run for a rejected request")

	// A caller that is turned away does not get its payload parsed.
	resp, _ = post(t, app, "/chat", "{")
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func Test_Vercel_RunOptionsAndContext(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxKey{}, "trace-1"))
		return c.Next()
	})
	app.Post("/chat", pydanticai.Vercel(ai.NewAgent[struct{}, string](echoModel()), pydanticai.VercelConfig[struct{}]{
		RunOptions: func(c fiber.Ctx) ([]ai.RunOption, error) {
			if c.Query("deny") != "" {
				return nil, fiber.ErrForbidden
			}
			return []ai.RunOption{ai.WithRunInstructions("from-options")}, nil
		},
	}))

	_, body := post(t, app, "/chat", vercelBody)
	chunks, _ := vercelChunks(t, body)
	require.Equal(t, "from-options|trace-1", vercelText(chunks))

	resp, _ := post(t, app, "/chat?deny=1", vercelBody)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func Test_Vercel_RequiresAChatID(t *testing.T) {
	t.Parallel()

	// The adapter takes the id as the conversation ID, which panics when it is empty.
	model, called := unreachableModel()
	app := newApp("/chat", pydanticai.Vercel(ai.NewAgent[struct{}, string](model)))

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
	require.False(t, called.Load())
}

func Test_Vercel_AgentError(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")

	t.Run("reports the error to the client and OnClose", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](failingModel(secret)), pydanticai.VercelConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode, "the status is sent before the run starts")
		require.Contains(t, vercelFailure(t, body), "sk-secret")
		require.ErrorIs(t, receive(t, closed, "OnClose"), secret)
	})

	t.Run("ErrorMessage replaces the message", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](failingModel(secret)), pydanticai.VercelConfig[struct{}]{
			ErrorMessage: func(error) string { return "the model failed" },
			OnClose:      func(_ struct{}, err error) { closed <- err },
		})

		_, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
		require.Equal(t, "the model failed", vercelFailure(t, body))
		require.NotContains(t, body, "sk-secret")
		require.ErrorIs(t, receive(t, closed, "OnClose"), secret, "OnClose still receives the original error")
	})
}

func Test_Vercel_ChunkThatCannotBeEncoded(t *testing.T) {
	t.Parallel()

	t.Run("ends the stream with an error chunk", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.Vercel(progressAgent(), pydanticai.VercelConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		_, done := vercelChunks(t, body)
		require.True(t, done, "the stream still ends with [DONE]")
		require.Contains(t, vercelFailure(t, body), "encode Vercel AI chunk")
		require.ErrorContains(t, receive(t, closed, "OnClose"), "cannot encode the payload")
	})

	t.Run("ErrorMessage replaces the message", func(t *testing.T) {
		t.Parallel()

		handler := pydanticai.Vercel(progressAgent(), pydanticai.VercelConfig[struct{}]{
			ErrorMessage: func(error) string { return "the run failed" },
		})

		_, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
		require.Equal(t, "the run failed", vercelFailure(t, body))
	})
}

func Test_Vercel_OnCloseReceivesTheDeps(t *testing.T) {
	t.Parallel()

	closed := make(chan closeEvent, 1)
	handler := pydanticai.Vercel(userAgent(), pydanticai.VercelConfig[userDeps]{
		Deps:    userFromQuery,
		OnClose: func(deps userDeps, err error) { closed <- closeEvent{deps: deps, err: err} },
	})

	post(t, newApp("/chat", handler), "/chat?user=alice", vercelBody)
	event := receive(t, closed, "OnClose")
	require.NoError(t, event.err)
	require.Equal(t, userDeps{User: "alice"}, event.deps)
}

func Test_Vercel_Timeout(t *testing.T) {
	t.Parallel()

	model, _, canceled := blockingModel()
	closed := make(chan error, 1)
	handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](model), pydanticai.VercelConfig[struct{}]{
		Timeout: 100 * time.Millisecond,
		OnClose: func(_ struct{}, err error) { closed <- err },
	})

	resp, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	chunks, done := vercelChunks(t, body)
	require.True(t, done, "a canceled run still ends with [DONE]")
	require.Equal(t, vercel.ChunkAbort, chunks[len(chunks)-1].Type)
	require.ErrorIs(t, receive(t, canceled, "the cancellation of the model call"), context.DeadlineExceeded)
	require.ErrorIs(t, receive(t, closed, "OnClose"), context.DeadlineExceeded)
}

func Test_Vercel_Heartbeat(t *testing.T) {
	t.Parallel()

	config := pydanticai.VercelConfig[struct{}]{HeartbeatInterval: 30 * time.Millisecond}

	_, body := post(t, newApp("/chat", pydanticai.Vercel(slowAgent(), config)), "/chat", vercelBody)
	_, heartbeats := parseSSE(body)
	require.Positive(t, heartbeats, "an idle stream should carry heartbeat comments")
	chunks, done := vercelChunks(t, body)
	require.True(t, done)
	require.Equal(t, "done", vercelText(chunks))

	config.DisableHeartbeat = true
	_, body = post(t, newApp("/chat", pydanticai.Vercel(slowAgent(), config)), "/chat", vercelBody)
	_, heartbeats = parseSSE(body)
	require.Zero(t, heartbeats)
}

func Test_Vercel_Panics(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() { pydanticai.Vercel[struct{}, string](nil) })
	require.Panics(t, func() {
		pydanticai.Vercel(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.VercelConfig[struct{}]{
			Adapter: vercel.Config{MaxRequestBytes: -1},
		})
	})
	require.Panics(t, func() {
		pydanticai.Vercel(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.VercelConfig[struct{}]{
			Adapter: vercel.Config{SDKVersion: 4},
		})
	})
}
