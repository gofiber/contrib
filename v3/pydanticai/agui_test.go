package pydanticai_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/gofiber/contrib/v3/pydanticai"
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
	require.Equal(t, "success (no tool calls)", aguiText(events))
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
		require.Equal(t, "user="+user, aguiText(aguiEvents(t, body)))
	}

	t.Run("zero value without Deps", func(t *testing.T) {
		t.Parallel()
		_, body := post(t, newApp("/agui", pydanticai.AGUI(userAgent())), "/agui", aguiBody)
		require.Equal(t, "user=", aguiText(aguiEvents(t, body)))
	})
}

func Test_AGUI_DepsAreNotShared(t *testing.T) {
	t.Parallel()

	handler := pydanticai.AGUI(userAgent(), pydanticai.AGUIConfig[userDeps]{
		Deps: userFromQuery,
	})

	for i, body := range concurrently(t, newApp("/agui", handler), "/agui", aguiBody) {
		require.Equal(t, fmt.Sprintf("user=user-%d", i), aguiText(aguiEvents(t, body)))
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
	require.Equal(t, "from-options", aguiText(aguiEvents(t, body)))

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
	require.Equal(t, "|trace-1", aguiText(aguiEvents(t, body)))
}

func Test_AGUI_AgentError(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")

	t.Run("reports the error to the client and OnClose", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](failingModel(secret)), pydanticai.AGUIConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode, "the status is sent before the run starts")
		require.Contains(t, aguiFailure(t, body), "sk-secret")
		require.ErrorIs(t, receive(t, closed, "OnClose"), secret)
	})

	t.Run("ErrorMessage replaces the message", func(t *testing.T) {
		t.Parallel()

		closed := make(chan error, 1)
		handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](failingModel(secret)), pydanticai.AGUIConfig[struct{}]{
			ErrorMessage: func(error) string { return "the model failed" },
			OnClose:      func(_ struct{}, err error) { closed <- err },
		})

		_, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, "the model failed", aguiFailure(t, body))
		require.NotContains(t, body, "sk-secret")
		require.ErrorIs(t, receive(t, closed, "OnClose"), secret, "OnClose still receives the original error")
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
		require.ErrorContains(t, receive(t, closed, "OnClose"), "cannot encode the payload")
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
	event := receive(t, closed, "OnClose")
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
	_, ok := nextData(newScanner(response.Body))
	require.True(t, ok)
	receive(t, started, "the tool call")
	closeConnection()

	require.Equal(t, "trace-1", receive(t, seen, "the context value in the tool"))
	require.ErrorIs(t, receive(t, canceled, "the cancellation of the tool call"), context.Canceled)
}

func Test_AGUI_Timeout(t *testing.T) {
	t.Parallel()

	model, _, canceled := blockingModel()
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
	require.ErrorIs(t, receive(t, canceled, "the cancellation of the model call"), context.DeadlineExceeded)
	require.ErrorIs(t, receive(t, closed, "OnClose"), context.DeadlineExceeded)
}

func Test_AGUI_Heartbeat(t *testing.T) {
	t.Parallel()

	// The model needs longer than the heartbeat interval to answer.
	config := pydanticai.AGUIConfig[struct{}]{HeartbeatInterval: 30 * time.Millisecond}

	_, body := post(t, newApp("/agui", pydanticai.AGUI(slowAgent(), config)), "/agui", aguiBody)
	data, heartbeats := parseSSE(body)
	require.Positive(t, heartbeats, "an idle stream should carry heartbeat comments")
	require.Equal(t, "done", aguiText(aguiEvents(t, body)))
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
