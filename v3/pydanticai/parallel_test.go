package pydanticai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	recoverer "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/stretchr/testify/require"
)

// pydantic-ai-go runs the tool calls of one model response concurrently, each
// on a goroutine of its own that nothing guards. The tests in this file make
// sure that what happens on those goroutines cannot end the process.

// concurrentCalls is a model that asks for one call of each named tool in its
// first response, so that the calls run concurrently, and answers "done" once
// it has their results.
func concurrentCalls(names ...string) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(
		_ context.Context, messages []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		for _, message := range messages {
			if _, ok := message.(ai.ModelResponse); ok {
				return textResponse("done"), nil
			}
		}
		parts := make([]ai.ResponsePart, len(names))
		for i, name := range names {
			parts[i] = ai.ToolCallPart{ToolName: name, Args: json.RawMessage(`{}`), ToolCallID: fmt.Sprintf("call-%d", i)}
		}
		return &ai.ModelResponse{Parts: parts}, nil
	})
}

// concurrentAgent calls the named tools concurrently. "explode" panics,
// "waiting" waits for its context, "fine" returns at once.
func concurrentAgent(names ...string) *ai.Agent[struct{}, string] {
	agent := ai.NewAgent[struct{}, string](concurrentCalls(names...))
	ai.AddSimpleTool(agent, "explode", func(context.Context, struct{}) (string, error) {
		time.Sleep(20 * time.Millisecond) // lets "waiting" start first
		panic("kaboom")
	})
	ai.AddSimpleTool(agent, "waiting", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	ai.AddSimpleTool(agent, "fine", func(context.Context, struct{}) (string, error) {
		return "ok", nil
	})
	return agent
}

func Test_APanicInAConcurrentToolCallEndsTheRunWithAnError(t *testing.T) {
	logs := captureLog(t) // the panic and its stack go to the log

	// "waiting" comes first: the panic cancels it, and its error is the first
	// error of the calls, which the run reports.
	for _, calls := range [][]string{{"explode", "fine"}, {"fine", "explode"}, {"waiting", "explode"}} {
		agent := concurrentAgent(calls...)
		name := strings.Join(calls, ", ")

		t.Run("AG-UI: "+name, func(t *testing.T) {
			closed := make(chan error, 1)
			ends := make(chan runEnd, 1)
			handler := pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
				OnClose:  func(_ struct{}, err error) { closed <- err },
				OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) { ends <- runEnd{run: run, err: err} },
			})

			resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
			require.Equal(t, "internal error", aguiFailure(t, body))
			require.NotContains(t, body, "kaboom", "the value of the panic stays on the server")
			require.ErrorContains(t, receive(t, closed, "OnClose"), "panic in the run: kaboom")
			end := receive(t, ends, "OnRunEnd")
			require.ErrorContains(t, end.err, "panic in the run: kaboom")
			require.NotNil(t, end.run)
		})

		t.Run("Vercel: "+name, func(t *testing.T) {
			closed := make(chan error, 1)
			var seen string
			handler := pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{
				ErrorMessage: func(err error) string {
					seen = err.Error()
					return "the run failed"
				},
				OnClose: func(_ struct{}, err error) { closed <- err },
			})

			resp, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
			_, done := vercelChunks(t, body)
			require.True(t, done, "the stream still ends with [DONE]")
			require.Equal(t, "the run failed", vercelFailure(t, body))
			require.ErrorContains(t, receive(t, closed, "OnClose"), "panic in the run: kaboom")
			require.Equal(t, "internal error", seen, "ErrorMessage is not given the value of the panic")
		})

		t.Run("Run: "+name, func(t *testing.T) {
			ends := make(chan runEnd, 1)
			app := fiber.New()
			app.Use(recoverer.New())
			app.Post("/ask", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{
				OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) { ends <- runEnd{run: run, err: err} },
			}))

			resp, body := post(t, app, "/ask", runBody)
			require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
			require.NotContains(t, body, "kaboom", "the value of the panic stays on the server")
			end := receive(t, ends, "OnRunEnd")
			require.ErrorContains(t, end.err, "panic in the run: kaboom")
			require.NotNil(t, end.run)
		})
	}
	require.Contains(t, logs.String(), "panic in the run: kaboom")
	require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")
}

// slowTools adds two tools that ignore their context and return 300 and 600
// milliseconds after they started, which started reports.
func slowTools(agent *ai.Agent[struct{}, string], started chan<- struct{}) {
	for name, duration := range map[string]time.Duration{"first": 300 * time.Millisecond, "second": 600 * time.Millisecond} {
		ai.AddSimpleTool(agent, name, func(context.Context, struct{}) (string, error) {
			started <- struct{}{}
			time.Sleep(duration)
			return "ok", nil
		})
	}
}

func Test_AClientThatLeavesWhileToolsRunConcurrently(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		body   string
		handle func(agent *ai.Agent[struct{}, string], closed func(struct{}, error)) fiber.Handler
	}{
		{
			name: "AG-UI", path: "/agui", body: aguiBody,
			handle: func(agent *ai.Agent[struct{}, string], closed func(struct{}, error)) fiber.Handler {
				return pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{HeartbeatInterval: 20 * time.Millisecond, OnClose: closed})
			},
		},
		{
			name: "Vercel", path: "/chat", body: vercelBody,
			handle: func(agent *ai.Agent[struct{}, string], closed func(struct{}, error)) fiber.Handler {
				return pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{HeartbeatInterval: 20 * time.Millisecond, OnClose: closed})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			agent := ai.NewAgent[struct{}, string](concurrentCalls("first", "second"))
			started := make(chan struct{}, 2)
			slowTools(agent, started)
			closed := make(chan error, 1)
			app := newApp(test.path, test.handle(agent, func(_ struct{}, err error) { closed <- err }))

			response, closeConnection := openStream(t, listen(t, app)+test.path, test.body)
			_, ok := pydanticaitest.NewReader(response.Body).NextData()
			require.True(t, ok)
			receive(t, started, "the first tool call")
			receive(t, started, "the second tool call")
			closeConnection()

			// The tools emit their results after the stream failed, which ended
			// the process when the handler stopped taking the events of the run.
			require.Error(t, receive(t, closed, "OnClose"))
		})
	}
}

// unencodableToolResult is a tool result that JSON cannot encode.
type unencodableToolResult struct {
	Value float64 `json:"value"`
}

func Test_AnEventThatCannotBeSentWhileToolsRunConcurrently(t *testing.T) {
	t.Parallel()

	slow := func(agent *ai.Agent[struct{}, string]) {
		ai.AddSimpleTool(agent, "slow", func(context.Context, struct{}) (string, error) {
			time.Sleep(300 * time.Millisecond)
			return "ok", nil
		})
	}
	// progress emits an event that no protocol can encode.
	progress := ai.NewAgent[struct{}, string](concurrentCalls("progress", "slow"))
	ai.AddTool(progress, "progress", func(_ context.Context, rc *ai.RunContext[struct{}], _ struct{}) (string, error) {
		return "ok", rc.Emit(ai.NewCustomEvent("progress", unencodable{}))
	})
	slow(progress)
	// nan returns a result that the AG-UI adapter cannot convert and the Vercel
	// handler cannot encode.
	nan := ai.NewAgent[struct{}, string](concurrentCalls("nan", "slow"))
	ai.AddSimpleTool(nan, "nan", func(context.Context, struct{}) (unencodableToolResult, error) {
		return unencodableToolResult{Value: math.NaN()}, nil
	})
	slow(nan)

	for name, agent := range map[string]*ai.Agent[struct{}, string]{"an unencodable event": progress, "an unencodable tool result": nan} {
		t.Run("AG-UI: "+name, func(t *testing.T) {
			t.Parallel()
			closed := make(chan error, 1)
			handler := pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
				OnClose: func(_ struct{}, err error) { closed <- err },
			})

			resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
			require.Contains(t, aguiFailure(t, body), "encode")
			require.Equal(t, 1, strings.Count(body, `"RUN_ERROR"`), body)
			require.Error(t, receive(t, closed, "OnClose"))
		})
		t.Run("Vercel: "+name, func(t *testing.T) {
			t.Parallel()
			closed := make(chan error, 1)
			handler := pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{
				OnClose: func(_ struct{}, err error) { closed <- err },
			})

			resp, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
			require.Contains(t, vercelFailure(t, body), "encode")
			require.Error(t, receive(t, closed, "OnClose"))
		})
	}
}
