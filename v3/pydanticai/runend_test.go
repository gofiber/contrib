package pydanticai_test

import (
	"context"
	"errors"
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
	recoverer "github.com/gofiber/fiber/v3/middleware/recover"
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

	t.Run("a run that panics", func(t *testing.T) {
		t.Parallel()

		ends, recovered := make(chan runEnd, 1), make(chan any, 1)
		app := fiber.New()
		app.Use(recoverer.New(recoverer.Config{PanicHandler: func(_ fiber.Ctx, r any) error {
			recovered <- r
			return fiber.ErrInternalServerError
		}}))
		app.Post("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
			context.Context, []ai.ModelMessage, ai.ModelRequestParams,
		) (*ai.ModelResponse, error) {
			panic("model kaboom")
		})), pydanticai.RunConfig[struct{}, string]{
			OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) { ends <- runEnd{run: run, err: err} },
		}))

		resp, _ := post(t, app, "/ask", runBody)
		require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
		require.Equal(t, "model kaboom", receive(t, recovered, "the recover middleware"), "the panic goes on to the recover middleware")
		end := receive(t, ends, "OnRunEnd")
		require.ErrorContains(t, end.err, "panic in the run: model kaboom")
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
	// Not parallel: the log is global, and CaptureLog replaces it.
	agenttest.CaptureLog(t) // the stack of the panic goes to the log

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
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

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

// The agent is the one that stops these runs, before its model is called. They
// were started all the same, and have a RunID.
func Test_OnRunEnd_ARunThatACapabilityOfTheAgentFailed(t *testing.T) {
	t.Parallel()

	blocked := errors.New("blocked by a capability")
	// guarded returns an agent whose capability fails every run, and stores its RunID in seen.
	guarded := func(seen *atomic.Value) *ai.Agent[userDeps, string] {
		return ai.NewAgent[userDeps, string](fakes.NewTestModel(), ai.WithCapabilities(ai.BeforeRunFunc(
			func(_ context.Context, run *ai.RunInfo) error {
				seen.Store(run.RunID)
				return blocked
			},
		)))
	}

	for _, protocol := range streamProtocols() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			var seen atomic.Value
			ends := make(chan runEnd, 1)
			handler := protocol.build(guarded(&seen), func(deps userDeps, run *ai.RunInfo, err error) {
				ends <- runEnd{deps: deps, run: run, err: err}
			}, nil, 0)
			post(t, newApp(protocol.path, handler), protocol.path, protocol.body)

			end := receive(t, ends, "OnRunEnd")
			require.ErrorIs(t, end.err, blocked)
			require.NotNil(t, end.run, "the run was started")
			require.Equal(t, seen.Load(), end.run.RunID)
		})
	}

	t.Run("Run", func(t *testing.T) {
		t.Parallel()

		var seen atomic.Value
		ends := make(chan runEnd, 1)
		app := newApp("/ask", pydanticai.Run(guarded(&seen), pydanticai.RunConfig[userDeps, string]{
			OnRunEnd: func(deps userDeps, run *ai.RunInfo, err error) { ends <- runEnd{deps: deps, run: run, err: err} },
		}))
		post(t, app, "/ask", runBody)

		end := receive(t, ends, "OnRunEnd")
		require.ErrorIs(t, end.err, blocked)
		require.NotNil(t, end.run, "the run was started")
		require.Equal(t, seen.Load(), end.run.RunID)
	})
}

// lingeringModel answers at once, and releases what it holds for the run only
// once the context of the run is done. While the run closes, nothing but
// Timeout or a shutdown can end that context, so they come after the result of
// the run is final.
type lingeringModel struct {
	*fakes.FunctionModel
	closing chan struct{}
}

func newLingeringModel() lingeringModel {
	return lingeringModel{FunctionModel: agenttest.Answering("done"), closing: make(chan struct{}, 1)}
}

// OpenModel implements ai.ModelOpener.
func (m lingeringModel) OpenModel(ctx context.Context) (ai.ModelCloseFunc, error) {
	return func(context.Context) error {
		select {
		case m.closing <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil
	}, nil
}

func Test_ATimeoutAfterTheRunCompletedDoesNotFailIt(t *testing.T) {
	t.Parallel()

	for _, protocol := range []string{"AG-UI", "Vercel"} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()

			agent := ai.NewAgent[struct{}, string](newLingeringModel())
			ends, closed := make(chan error, 1), make(chan error, 1)
			onRunEnd := func(_ struct{}, _ *ai.RunInfo, err error) { ends <- err }
			onClose := func(_ struct{}, err error) { closed <- err }
			if protocol == "AG-UI" {
				handler := pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
					Timeout: 100 * time.Millisecond, OnRunEnd: onRunEnd, OnClose: onClose,
				})
				_, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
				events := aguiEvents(t, body)
				last := events[len(events)-1]
				require.Equal(t, agui.EventRunFinished, last.Type)
				require.NotNil(t, last.Outcome, "the client was told that the run completed")
			} else {
				handler := pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{
					Timeout: 100 * time.Millisecond, OnRunEnd: onRunEnd, OnClose: onClose,
				})
				_, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
				chunks, done := vercelChunks(t, body)
				require.True(t, done)
				require.Equal(t, vercel.ChunkFinish, chunks[len(chunks)-1].Type, "the client was told that the run completed")
			}
			require.NoError(t, receive(t, ends, "OnRunEnd"))
			require.NoError(t, receive(t, closed, "OnClose"))
		})
	}
}
