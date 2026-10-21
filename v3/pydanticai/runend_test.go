package pydanticai_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// runEnd is what an OnRunEnd callback received.
type runEnd struct {
	deps userDeps
	run  *ai.RunInfo
	err  error
}

// streamProtocol builds the streaming handler of one protocol around a user agent.
type streamProtocol struct {
	name string
	path string
	body string
	// emptyBody is a request that the adapter rejects before the agent runs.
	emptyBody string
	build     func(agent *ai.Agent[userDeps, string], end func(userDeps, *ai.RunInfo, error), closed func(userDeps, error), heartbeat time.Duration) fiber.Handler
}

func streamProtocols() []streamProtocol {
	return []streamProtocol{
		{
			name: "AG-UI", path: "/agui", body: aguiBody,
			emptyBody: `{"threadId":"thread-1","runId":"run-1","messages":[]}`,
			build: func(agent *ai.Agent[userDeps, string], end func(userDeps, *ai.RunInfo, error), closed func(userDeps, error), heartbeat time.Duration) fiber.Handler {
				return pydanticai.AGUI(agent, pydanticai.AGUIConfig[userDeps]{
					Deps: userFromQuery, OnRunEnd: end, OnClose: closed, HeartbeatInterval: heartbeat,
				})
			},
		},
		{
			name: "Vercel", path: "/chat", body: vercelBody,
			emptyBody: `{"trigger":"submit-message","id":"chat-1","messages":[]}`,
			build: func(agent *ai.Agent[userDeps, string], end func(userDeps, *ai.RunInfo, error), closed func(userDeps, error), heartbeat time.Duration) fiber.Handler {
				return pydanticai.Vercel(agent, pydanticai.VercelConfig[userDeps]{
					Deps: userFromQuery, OnRunEnd: end, OnClose: closed, HeartbeatInterval: heartbeat,
				})
			},
		},
	}
}

func Test_OnRunEnd_StreamingHandlers(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")

	for _, protocol := range streamProtocols() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			// run posts body and returns what OnRunEnd received.
			run := func(t *testing.T, agent *ai.Agent[userDeps, string], body string) runEnd {
				t.Helper()
				ends := make(chan runEnd, 1)
				handler := protocol.build(agent, func(deps userDeps, run *ai.RunInfo, err error) {
					ends <- runEnd{deps: deps, run: run, err: err}
				}, nil, 0)
				post(t, newApp(protocol.path, handler), protocol.path+"?user=alice", body)
				return receive(t, ends, "OnRunEnd")
			}

			t.Run("a completed run", func(t *testing.T) {
				t.Parallel()
				end := run(t, ai.NewAgent[userDeps, string](fakes.NewTestModel()), protocol.body)
				require.NoError(t, end.err)
				require.Equal(t, userDeps{User: "alice"}, end.deps)
				require.NotNil(t, end.run)
				require.NotEmpty(t, end.run.RunID)
				require.Positive(t, end.run.Usage().Requests, "the usage of the run")
				require.NotEmpty(t, end.run.Messages(), "the conversation of the run")
			})

			t.Run("a failed run", func(t *testing.T) {
				t.Parallel()
				end := run(t, ai.NewAgent[userDeps, string](failingModel(secret)), protocol.body)
				require.ErrorIs(t, end.err, secret)
				require.NotNil(t, end.run, "a run that failed was still started")
			})

			t.Run("a request that the agent never saw", func(t *testing.T) {
				t.Parallel()
				end := run(t, ai.NewAgent[userDeps, string](fakes.NewTestModel()), protocol.emptyBody)
				require.Error(t, end.err)
				require.Nil(t, end.run, "there is no run to report")
			})

			t.Run("before OnClose", func(t *testing.T) {
				t.Parallel()
				order := make(chan string, 2)
				handler := protocol.build(ai.NewAgent[userDeps, string](fakes.NewTestModel()),
					func(userDeps, *ai.RunInfo, error) { order <- "OnRunEnd" },
					func(userDeps, error) { order <- "OnClose" }, 0)
				post(t, newApp(protocol.path, handler), protocol.path, protocol.body)
				require.Equal(t, "OnRunEnd", receive(t, order, "the first callback"))
				require.Equal(t, "OnClose", receive(t, order, "the second callback"))
			})
		})
	}
}

func Test_OnRunEnd_ARunWhoseClientLeft(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			model, started, _ := blockingModel()
			ends := make(chan runEnd, 1)
			// The heartbeat notices that the client left.
			handler := protocol.build(ai.NewAgent[userDeps, string](model), func(deps userDeps, run *ai.RunInfo, err error) {
				ends <- runEnd{deps: deps, run: run, err: err}
			}, nil, 20*time.Millisecond)
			url := listen(t, newApp(protocol.path, handler)) + protocol.path + "?user=alice"

			response, closeConnection := openStream(t, url, protocol.body)
			_, ok := pydanticaitest.NewReader(response.Body).NextData()
			require.True(t, ok)
			receive(t, started, "the model call")
			closeConnection()

			// The run was canceled, and what it used so far is still reported.
			end := receive(t, ends, "OnRunEnd")
			require.Error(t, end.err)
			require.Equal(t, userDeps{User: "alice"}, end.deps)
			require.NotNil(t, end.run)
			require.NotEmpty(t, end.run.Messages(), "the prompt of the run")
		})
	}
}

func Test_Run_OnRunEnd(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")

	t.Run("a completed run", func(t *testing.T) {
		t.Parallel()

		ends := make(chan runEnd, 1)
		app := newApp("/ask", pydanticai.Run(ai.NewAgent[userDeps, string](fakes.NewTestModel()), pydanticai.RunConfig[userDeps, string]{
			Deps:     userFromQuery,
			OnRunEnd: func(deps userDeps, run *ai.RunInfo, err error) { ends <- runEnd{deps: deps, run: run, err: err} },
		}))

		resp, _ := post(t, app, "/ask?user=alice", runBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		end := receive(t, ends, "OnRunEnd")
		require.NoError(t, end.err)
		require.Equal(t, userDeps{User: "alice"}, end.deps)
		require.NotNil(t, end.run)
		require.Positive(t, end.run.Usage().Requests)
	})

	t.Run("a failed run", func(t *testing.T) {
		t.Parallel()

		ends := make(chan runEnd, 1)
		app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](failingModel(secret)), pydanticai.RunConfig[struct{}, string]{
			OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) { ends <- runEnd{run: run, err: err} },
		}))

		resp, body := post(t, app, "/ask", runBody)
		require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
		require.NotContains(t, body, "sk-secret")
		end := receive(t, ends, "OnRunEnd")
		require.ErrorIs(t, end.err, secret)
		require.NotNil(t, end.run)
	})

	t.Run("a request that never reached the agent", func(t *testing.T) {
		t.Parallel()

		var called bool
		app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.RunConfig[struct{}, string]{
			OnRunEnd: func(struct{}, *ai.RunInfo, error) { called = true },
		}))

		resp, _ := post(t, app, "/ask", `{}`)
		require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
		require.False(t, called)
	})
}

func Test_OnRunEnd_ARunThatPanics(t *testing.T) {
	captureLog(t) // the stack of the panic goes to the log

	panicking := ai.NewAgent[userDeps, string](fakes.NewTestModel())
	ai.AddSimpleTool(panicking, "explode", func(context.Context, struct{}) (string, error) { panic("kaboom") })

	for _, protocol := range streamProtocols() {
		ends := make(chan runEnd, 1)
		handler := protocol.build(panicking, func(deps userDeps, run *ai.RunInfo, err error) {
			ends <- runEnd{deps: deps, run: run, err: err}
		}, nil, 0)
		post(t, newApp(protocol.path, handler), protocol.path, protocol.body)

		end := receive(t, ends, protocol.name+" OnRunEnd")
		require.ErrorContains(t, end.err, "panic in the run: kaboom", protocol.name)
		require.NotNil(t, end.run, protocol.name)
	}
}

func Test_OnRunEnd_APanicIsRecovered(t *testing.T) {
	logs := captureLog(t)

	closed := make(chan error, 1)
	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
		OnRunEnd: func(struct{}, *ai.RunInfo, error) { panic("bug in OnRunEnd") },
		OnClose:  func(_ struct{}, err error) { closed <- err },
	})

	resp, _ := post(t, newApp("/agui", handler), "/agui", aguiBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.NoError(t, receive(t, closed, "OnClose"), "the panic in OnRunEnd does not take OnClose with it")
	require.Contains(t, logs.String(), "panic in OnRunEnd: bug in OnRunEnd")

	// The same for the handler that answers on the goroutine of the request.
	app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.RunConfig[struct{}, string]{
		OnRunEnd: func(struct{}, *ai.RunInfo, error) { panic("bug in OnRunEnd of Run") },
	}))
	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.True(t, strings.Contains(body, `"output"`))
	require.Contains(t, logs.String(), "panic in OnRunEnd: bug in OnRunEnd of Run")
}
