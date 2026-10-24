package pydanticai_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// withoutTimestamps clears the only field of an event that differs between runs.
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

func Test_AGUI_Deps(t *testing.T) {
	t.Parallel()

	handler := pydanticai.AGUI(userAgent(), pydanticai.AGUIConfig[userDeps]{
		Deps: userFromQuery,
	})
	app := newApp("/agui", handler)

	for _, user := range []string{"alice", "bob"} {
		_, body := post(t, app, "/agui?user="+user, aguiBody)
		require.Equal(t, "user="+user, pydanticaitest.AGUIText(aguiEvents(t, body)))
	}

	t.Run("zero value without Deps", func(t *testing.T) {
		t.Parallel()
		_, body := post(t, newApp("/agui", pydanticai.AGUI(userAgent())), "/agui", aguiBody)
		require.Equal(t, "user=", pydanticaitest.AGUIText(aguiEvents(t, body)))
	})
}

func Test_AGUI_DepsAreNotShared(t *testing.T) {
	t.Parallel()

	handler := pydanticai.AGUI(userAgent(), pydanticai.AGUIConfig[userDeps]{
		Deps: userFromQuery,
	})

	for i, body := range concurrently(t, newApp("/agui", handler), "/agui", aguiBody) {
		require.Equal(t, fmt.Sprintf("user=user-%d", i), pydanticaitest.AGUIText(aguiEvents(t, body)))
	}
}

func Test_AGUI_DepsError(t *testing.T) {
	t.Parallel()

	model, called := unreachableModel()
	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		Deps: func(fiber.Ctx) (struct{}, error) { return struct{}{}, fiber.ErrUnauthorized },
	})
	app := newApp("/agui", handler)

	resp, body := post(t, app, "/agui", aguiBody)
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	require.NotContains(t, resp.Header.Get(fiber.HeaderContentType), "event-stream")
	require.Equal(t, "Unauthorized", body)
	require.False(t, called.Load(), "the agent must not run for a rejected request")

	// A caller that is turned away does not get its payload parsed.
	resp, _ = post(t, app, "/agui", "{")
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func Test_AGUI_RunOptions(t *testing.T) {
	t.Parallel()

	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](echoModel()), pydanticai.AGUIConfig[struct{}]{
		RunOptions: func(c fiber.Ctx) ([]ai.RunOption, error) {
			if c.Query("deny") != "" {
				return nil, fiber.ErrForbidden
			}
			return []ai.RunOption{ai.WithRunInstructions("from-options")}, nil
		},
	})
	app := newApp("/agui", handler)

	_, body := post(t, app, "/agui", aguiBody)
	require.Equal(t, "from-options", pydanticaitest.AGUIText(aguiEvents(t, body)))

	resp, _ := post(t, app, "/agui?deny=1", aguiBody)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func Test_AGUI_PropagatesRequestContext(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxKey{}, "trace-1"))
		return c.Next()
	})
	app.Post("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](echoModel())))

	_, body := post(t, app, "/agui", aguiBody)
	require.Equal(t, "|trace-1", pydanticaitest.AGUIText(aguiEvents(t, body)))
}

func Test_AGUI_AgentError(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")

	t.Run("reports the error to the client and OnClose", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](agenttest.Failing(secret)), pydanticai.AGUIConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode, "the status is sent before the run starts")
		require.Contains(t, aguiFailure(t, body), "sk-secret")
		require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), secret)
	})

	t.Run("ErrorMessage replaces the message", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](agenttest.Failing(secret)), pydanticai.AGUIConfig[struct{}]{
			ErrorMessage: func(error) string { return "the model failed" },
			OnClose:      func(_ struct{}, err error) { closed <- err },
		})

		_, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, "the model failed", aguiFailure(t, body))
		require.NotContains(t, body, "sk-secret")
		require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), secret, "OnClose still receives the original error")
	})
}

func Test_AGUI_EventThatCannotBeEncoded(t *testing.T) {
	t.Parallel()

	t.Run("ends the stream with RUN_ERROR", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.AGUI(progressAgent(), pydanticai.AGUIConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		require.Contains(t, aguiFailure(t, body), "encode AG-UI event")
		require.ErrorContains(t, agenttest.Receive(t, closed, "OnClose"), "cannot encode the payload")
	})

	t.Run("ErrorMessage replaces the message", func(t *testing.T) {
		t.Parallel()

		handler := pydanticai.AGUI(progressAgent(), pydanticai.AGUIConfig[struct{}]{
			ErrorMessage: func(error) string { return "the run failed" },
		})

		_, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, "the run failed", aguiFailure(t, body))
	})
}

func Test_AGUI_OnCloseReceivesTheDeps(t *testing.T) {
	t.Parallel()

	closed := make(chan closeEvent, 1)
	handler := pydanticai.AGUI(userAgent(), pydanticai.AGUIConfig[userDeps]{
		Deps:    userFromQuery,
		OnClose: func(deps userDeps, err error) { closed <- closeEvent{deps: deps, err: err} },
	})

	post(t, newApp("/agui", handler), "/agui?user=alice", aguiBody)
	event := agenttest.Receive(t, closed, "OnClose")
	require.NoError(t, event.err)
	require.Equal(t, userDeps{User: "alice"}, event.deps)
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

	// The client leaves while the tool is the one that is busy.
	response, closeConnection := openStream(t, listen(t, app)+"/agui", aguiBody)
	_, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	agenttest.Receive(t, started, "the tool call")
	closeConnection()

	require.Equal(t, "trace-1", agenttest.Receive(t, seen, "the context value in the tool"))
	require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the tool call"), context.Canceled)
}

func Test_AGUI_Timeout(t *testing.T) {
	t.Parallel()

	model, _, canceled := agenttest.Blocking()
	closed := make(chan error, 1)
	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		Timeout: 100 * time.Millisecond,
		OnClose: func(_ struct{}, err error) { closed <- err },
	})

	resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	events := aguiEvents(t, body)
	last := events[len(events)-1]
	require.Equal(t, agui.EventRunFinished, last.Type)
	require.Nil(t, last.Outcome, "a canceled run has no outcome")
	require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the model call"), context.DeadlineExceeded)
	require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), context.DeadlineExceeded)
}

func Test_AGUI_Heartbeat(t *testing.T) {
	t.Parallel()

	// The model needs longer than the heartbeat interval to answer.
	config := pydanticai.AGUIConfig[struct{}]{HeartbeatInterval: 30 * time.Millisecond}

	_, body := post(t, newApp("/agui", pydanticai.AGUI(slowAgent(), config)), "/agui", aguiBody)
	data, heartbeats := parseSSE(body)
	require.Positive(t, heartbeats, "an idle stream should carry heartbeat comments")
	require.Equal(t, "done", pydanticaitest.AGUIText(aguiEvents(t, body)))
	require.Len(t, data, 5)

	config.DisableHeartbeat = true
	_, body = post(t, newApp("/agui", pydanticai.AGUI(slowAgent(), config)), "/agui", aguiBody)
	_, heartbeats = parseSSE(body)
	require.Zero(t, heartbeats)
}

func Test_AGUI_Panics(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() { pydanticai.AGUI[struct{}, string](nil) })
	require.Panics(t, func() {
		pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
			Adapter: agui.Config{MaxRequestBytes: -1},
		})
	})
	require.Panics(t, func() {
		pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
			Adapter: agui.Config{Version: "not-a-version"},
		})
	})
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
		require.Equal(t, "user="+user, pydanticaitest.VercelText(chunks))
	}

	for i, body := range concurrently(t, app, "/chat", vercelBody) {
		chunks, _ := vercelChunks(t, body)
		require.Equal(t, fmt.Sprintf("user=user-%d", i), pydanticaitest.VercelText(chunks))
	}

	t.Run("zero value without Deps", func(t *testing.T) {
		t.Parallel()
		_, body := post(t, newApp("/chat", pydanticai.Vercel(userAgent())), "/chat", vercelBody)
		chunks, _ := vercelChunks(t, body)
		require.Equal(t, "user=", pydanticaitest.VercelText(chunks))
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
	require.Equal(t, "from-options|trace-1", pydanticaitest.VercelText(chunks))

	resp, _ := post(t, app, "/chat?deny=1", vercelBody)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
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

func Test_Vercel_AgentError(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")

	t.Run("reports the error to the client and OnClose", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](agenttest.Failing(secret)), pydanticai.VercelConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode, "the status is sent before the run starts")
		require.Contains(t, vercelFailure(t, body), "sk-secret")
		require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), secret)
	})

	t.Run("ErrorMessage replaces the message", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](agenttest.Failing(secret)), pydanticai.VercelConfig[struct{}]{
			ErrorMessage: func(error) string { return "the model failed" },
			OnClose:      func(_ struct{}, err error) { closed <- err },
		})

		_, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
		require.Equal(t, "the model failed", vercelFailure(t, body))
		require.NotContains(t, body, "sk-secret")
		require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), secret, "OnClose still receives the original error")
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
		require.ErrorContains(t, agenttest.Receive(t, closed, "OnClose"), "cannot encode the payload")
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
	event := agenttest.Receive(t, closed, "OnClose")
	require.NoError(t, event.err)
	require.Equal(t, userDeps{User: "alice"}, event.deps)
}

func Test_Vercel_Timeout(t *testing.T) {
	t.Parallel()

	model, _, canceled := agenttest.Blocking()
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
	require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the model call"), context.DeadlineExceeded)
	require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), context.DeadlineExceeded)
}

func Test_Vercel_Heartbeat(t *testing.T) {
	t.Parallel()

	config := pydanticai.VercelConfig[struct{}]{HeartbeatInterval: 30 * time.Millisecond}

	_, body := post(t, newApp("/chat", pydanticai.Vercel(slowAgent(), config)), "/chat", vercelBody)
	_, heartbeats := parseSSE(body)
	require.Positive(t, heartbeats, "an idle stream should carry heartbeat comments")
	chunks, done := vercelChunks(t, body)
	require.True(t, done)
	require.Equal(t, "done", pydanticaitest.VercelText(chunks))

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
