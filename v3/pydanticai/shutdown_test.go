package pydanticai_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func Test_CancelOnShutdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		body   string
		ends   string // the record that ends a canceled stream
		handle func(agent *ai.Agent[struct{}, string], closed func(struct{}, error)) fiber.Handler
	}{
		{
			name: "AG-UI", path: "/agui", body: aguiBody, ends: `"type":"RUN_FINISHED"`,
			handle: func(agent *ai.Agent[struct{}, string], closed func(struct{}, error)) fiber.Handler {
				return pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{CancelOnShutdown: true, OnClose: closed})
			},
		},
		{
			name: "Vercel", path: "/chat", body: vercelBody, ends: "[DONE]",
			handle: func(agent *ai.Agent[struct{}, string], closed func(struct{}, error)) fiber.Handler {
				return pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{CancelOnShutdown: true, OnClose: closed})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model, started, canceled := blockingModel()
			closed := make(chan error, 1)
			app := newApp(test.path, test.handle(ai.NewAgent[struct{}, string](model), func(_ struct{}, err error) { closed <- err }))
			response, _ := openStream(t, listen(t, app)+test.path, test.body)
			scanner := pydanticaitest.NewReader(response.Body)
			_, ok := scanner.NextData()
			require.True(t, ok)
			receive(t, started, "the model call")

			// The run does not end on its own, so a shutdown that waits for it takes the whole timeout.
			begin := time.Now()
			require.NoError(t, app.ShutdownWithTimeout(10*time.Second))
			require.Less(t, time.Since(begin), 5*time.Second, "the shutdown waited for the run")

			require.ErrorIs(t, receive(t, canceled, "the cancellation of the model call"), ai.ErrRunCancelled)
			require.ErrorIs(t, receive(t, closed, "OnClose"), ai.ErrRunCancelled)

			// The client gets the end of the stream, not a broken connection.
			var last string
			for {
				record, ok := scanner.NextData()
				if !ok {
					break
				}
				last = record
			}
			require.Contains(t, last, test.ends)
		})
	}
}

func Test_CancelOnShutdown_ARunThatEndsFirst(t *testing.T) {
	t.Parallel()

	closed := make(chan error, 1)
	app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
		CancelOnShutdown: true,
		OnClose:          func(_ struct{}, err error) { closed <- err },
	}))

	// A server that is not shutting down leaves the run alone.
	response, _ := openStream(t, listen(t, app)+"/agui", aguiBody)
	events := pydanticaitest.AGUIEvents(t, response.Body)
	require.Equal(t, agui.EventRunFinished, events[len(events)-1].Type)
	require.NoError(t, receive(t, closed, "OnClose"))
}

func Test_ShutdownWaitsForARunByDefault(t *testing.T) {
	t.Parallel()

	model, started, canceled := blockingModel()
	app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond, // notices the client that leaves at the end
	}))
	response, closeConnection := openStream(t, listen(t, app)+"/agui", aguiBody)
	_, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	receive(t, started, "the model call")

	require.ErrorIs(t, app.ShutdownWithTimeout(200*time.Millisecond), context.DeadlineExceeded)
	select {
	case <-canceled:
		t.Fatal("the shutdown canceled the run without CancelOnShutdown")
	default:
	}

	// The run ends when its client leaves.
	closeConnection()
	require.ErrorIs(t, receive(t, canceled, "the cancellation of the model call"), context.Canceled)
}

func Test_CancelOnShutdownDoesNotWaitForAClientThatStoppedReading(t *testing.T) {
	pydanticai.SetStreamEndGrace(t, 100*time.Millisecond) // not parallel: the grace is global

	closed := make(chan error, 1)
	app := fiber.New()
	app.Post("/agui", pydanticai.AGUI(talkativeAgent(), pydanticai.AGUIConfig[struct{}]{
		CancelOnShutdown: true,
		OnClose:          func(_ struct{}, err error) { closed <- err },
	}))

	postAndStopReading(t, app, "/agui", aguiBody)
	time.Sleep(300 * time.Millisecond) // the writes to the client block

	begin := time.Now()
	require.NoError(t, app.ShutdownWithTimeout(10*time.Second))
	require.Less(t, time.Since(begin), 5*time.Second, "the shutdown waited for the client")
	require.Error(t, receive(t, closed, "OnClose"))
}

func Test_AShutdownAfterTheRunCompletedDoesNotFailIt(t *testing.T) {
	t.Parallel()

	model := newLingeringModel()
	closed := make(chan error, 1)
	app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		CancelOnShutdown: true,
		OnClose:          func(_ struct{}, err error) { closed <- err },
	}))
	response, _ := openStream(t, listen(t, app)+"/agui", aguiBody)
	receive(t, model.closing, "the close of the model")
	require.NoError(t, app.ShutdownWithTimeout(observeTimeout))

	events := pydanticaitest.AGUIEvents(t, response.Body)
	last := events[len(events)-1]
	require.Equal(t, agui.EventRunFinished, last.Type)
	require.NotNil(t, last.Outcome, "the client was told that the run completed")
	require.NoError(t, receive(t, closed, "OnClose"))
}

// A RequestCtx that no fasthttp.Server serves, as tests and benchmarks build
// it, gets the whole stream, with and without CancelOnShutdown.
func Test_ARequestCtxWithoutAServer(t *testing.T) {
	t.Parallel()

	for _, cancelOnShutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("AG-UI, CancelOnShutdown: %v", cancelOnShutdown), func(t *testing.T) {
			t.Parallel()

			closed := make(chan error, 1)
			app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
				CancelOnShutdown: cancelOnShutdown,
				OnClose:          func(_ struct{}, err error) { closed <- err },
			}))
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(fiber.MethodPost)
			ctx.Request.SetRequestURI("/agui")
			ctx.Request.Header.SetContentType(fiber.MIMEApplicationJSON)
			ctx.Request.SetBodyString(aguiBody)

			require.NotPanics(t, func() { app.Handler()(ctx) })
			require.Equal(t, fiber.StatusOK, ctx.Response.StatusCode())
			events := aguiEvents(t, string(ctx.Response.Body())) // reads the stream to its end
			require.Equal(t, agui.EventRunFinished, events[len(events)-1].Type)
			require.NoError(t, receive(t, closed, "OnClose"))
		})

		t.Run(fmt.Sprintf("Vercel, CancelOnShutdown: %v", cancelOnShutdown), func(t *testing.T) {
			t.Parallel()

			handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.VercelConfig[struct{}]{
				CancelOnShutdown: cancelOnShutdown,
			})
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(fiber.MethodPost)
			ctx.Request.Header.SetContentType(fiber.MIMEApplicationJSON)
			ctx.Request.SetBodyString(vercelBody)
			c := fiber.New().AcquireCtx(ctx) // the handler called without the router

			require.NotPanics(t, func() { require.NoError(t, handler(c)) })
			_, done := vercelChunks(t, string(ctx.Response.Body()))
			require.True(t, done)
		})
	}
}
