package pydanticai_test

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"runtime"
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
	"github.com/gofiber/fiber/v3/middleware/compress"
	recoverer "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/gofiber/fiber/v3/middleware/timeout"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func Test_AGUI_StringsFromTheRequestSurviveAClientThatLeft(t *testing.T) {
	// Not parallel: with a single P, fasthttp hands the request of the client
	// that left to the next client, which is what makes a violation of the
	// lifetime of the request visible.
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })

	type observed struct{ user, requestID, tenant string }
	started := make(chan struct{}, 1)
	canceled := make(chan struct{})
	release := make(chan struct{})
	late := make(chan observed, 1)
	var request fiber.Ctx // kept by the dependencies, as a tool may keep it

	agent := ai.NewAgent[userDeps, string](fakes.NewTestModel())
	ai.AddTool(agent, "slow", func(ctx context.Context, rc *ai.RunContext[userDeps], _ struct{}) (string, error) {
		started <- struct{}{}
		go func() { // like many tools, this one does not stop when its context is canceled
			<-ctx.Done()
			close(canceled)
		}()
		<-release
		tenant, _ := request.Locals("tenant").(string)
		late <- observed{user: rc.Deps.User, requestID: requestid.FromContext(ctx), tenant: tenant}
		return "done", nil
	})

	app := fiber.New(fiber.Config{PassLocalsToContext: true})
	app.Use(requestid.New())
	app.Use(func(c fiber.Ctx) error {
		c.Locals("tenant", "tenant-A")
		return c.Next()
	})
	app.Post("/agui", pydanticai.AGUI(agent, pydanticai.AGUIConfig[userDeps]{
		HeartbeatInterval: 20 * time.Millisecond,
		Deps: func(c fiber.Ctx) (userDeps, error) {
			request = c
			// The string points into the request, and is not copied.
			return userDeps{User: c.Query("user")}, nil
		},
	}))
	app.Get("/other", func(c fiber.Ctx) error { return c.SendString(c.Query("user")) })
	base := listen(t, app)

	// Client A leaves while its tool works.
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	require.NoError(t, err)
	_, err = fmt.Fprintf(conn, "POST /agui?user=AAAAAAAAAAAA HTTP/1.1\r\nHost: x\r\n"+
		"Content-Type: application/json\r\nX-Request-ID: request-A-0000000001\r\nContent-Length: %d\r\n\r\n%s",
		len(aguiBody), aguiBody)
	require.NoError(t, err)
	_, ok := pydanticaitest.NewReader(conn).NextData()
	require.True(t, ok)
	agenttest.Receive(t, started, "the tool call")
	require.NoError(t, conn.Close())
	agenttest.Receive(t, canceled, "the cancellation of the run") // the heartbeats fail and fasthttp gives up on the response

	// Other clients make fasthttp reuse what it can.
	for range 8 {
		request, err := http.NewRequest(http.MethodGet, base+"/other?user=BBBBBBBBBBBB", http.NoBody)
		require.NoError(t, err)
		request.Header.Set("X-Request-ID", "request-B-0000000001")
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, response.Body)
		require.NoError(t, response.Body.Close())
	}

	close(release)
	got := agenttest.Receive(t, late, "the tool")
	require.Equal(t, "AAAAAAAAAAAA", got.user, "the dependencies of the run")
	require.Equal(t, "request-A-0000000001", got.requestID, "the value of the middleware in the context of the run")
	require.Equal(t, "tenant-A", got.tenant, "the Locals of the request")
}

func Test_AGUI_APanicInOnCloseIsRecovered(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	called := make(chan struct{}, 2)
	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.AGUIConfig[struct{}]{
		OnClose: func(struct{}, error) {
			called <- struct{}{}
			panic("bug in OnClose")
		},
	})
	app := newApp("/agui", handler)

	// Nothing guards the goroutine that calls OnClose, so the process would be gone.
	for range 2 {
		resp, _ := post(t, app, "/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		agenttest.Receive(t, called, "OnClose")
	}
	require.Eventually(t, func() bool {
		return strings.Count(logs.String(), "panic in OnClose: bug in OnClose") == 2
	}, observeTimeout, 10*time.Millisecond, logs.String())
}

func Test_ARequestWithoutCallbacksLogsNothing(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	// A callback that is not configured is not called. The handlers recover the
	// panic of a nil one and log it, so the log is where a call would show.
	agent := ai.NewAgent[struct{}, string](agenttest.Answering("hello"))
	for _, test := range []struct {
		name    string
		handler fiber.Handler
		body    string
	}{
		{"AGUI", pydanticai.AGUI(agent), aguiBody},
		{"Vercel", pydanticai.Vercel(agent), vercelBody},
		{"Run", pydanticai.Run(agent), runBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp, _ := post(t, newApp("/", test.handler), "/", test.body)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
		})
	}
	require.Empty(t, logs.String())
}

func Test_AGUI_APanicInTheRunEndsTheStreamWithAnError(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it. Neither are the
	// subtests, as the log is read after them.
	logs := agenttest.CaptureLog(t)

	t.Run("the client is told", func(t *testing.T) {
		closed := make(chan error, 1)
		handler := pydanticai.AGUI(panicAgent(), pydanticai.AGUIConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		require.Equal(t, "internal error", aguiFailure(t, body))
		require.NotContains(t, body, "kaboom", "the value of the panic stays on the server")
		require.ErrorContains(t, agenttest.Receive(t, closed, "OnClose"), "panic in the run: kaboom")
	})

	t.Run("ErrorMessage applies", func(t *testing.T) {
		var seen atomic.Value
		handler := pydanticai.AGUI(panicAgent(), pydanticai.AGUIConfig[struct{}]{
			ErrorMessage: func(err error) string {
				seen.Store(err.Error())
				return "the run failed"
			},
		})

		_, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, "the run failed", aguiFailure(t, body))
		require.Equal(t, "internal error", seen.Load())
	})

	// The panic and its stack are in the log, where nothing else would show them.
	require.Contains(t, logs.String(), "panic in the run: kaboom")
	require.Contains(t, logs.String(), "goroutine ")
}

func Test_Vercel_APanicInTheRunEndsTheStreamWithAnError(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	closed := make(chan error, 1)
	handler := pydanticai.Vercel(panicAgent(), pydanticai.VercelConfig[struct{}]{
		OnClose: func(_ struct{}, err error) { closed <- err },
	})

	resp, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	_, done := vercelChunks(t, body)
	require.True(t, done, "the stream still ends with [DONE]")
	require.Equal(t, "internal error", vercelFailure(t, body))
	require.NotContains(t, body, "kaboom", "the value of the panic stays on the server")
	require.ErrorContains(t, agenttest.Receive(t, closed, "OnClose"), "panic in the run: kaboom")
	require.Contains(t, logs.String(), "panic in the run: kaboom")
}

// The model is called on the goroutine of the stream, outside of any tool call,
// so the handler recovers its panic, and not the run.
func Test_APanicInTheModelEndsTheStreamWithAnError(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it. Neither are the
	// subtests, as the log is read after them.
	logs := agenttest.CaptureLog(t)

	agent := ai.NewAgent[struct{}, string](agenttest.Panicking("model kaboom"))

	t.Run("AG-UI", func(t *testing.T) {
		closed := make(chan error, 1)
		handler := pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		require.Equal(t, "internal error", aguiFailure(t, body))
		require.NotContains(t, body, "kaboom", "the value of the panic stays on the server")
		require.ErrorContains(t, agenttest.Receive(t, closed, "OnClose"), "panic in the run: model kaboom")
	})

	t.Run("Vercel", func(t *testing.T) {
		closed := make(chan error, 1)
		handler := pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		_, done := vercelChunks(t, body)
		require.True(t, done, "the stream still ends with [DONE]")
		require.Equal(t, "internal error", vercelFailure(t, body))
		require.NotContains(t, body, "kaboom", "the value of the panic stays on the server")
		require.ErrorContains(t, agenttest.Receive(t, closed, "OnClose"), "panic in the run: model kaboom")
	})

	require.Contains(t, logs.String(), "panic in the run: model kaboom")
	require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")
}

func Test_AGUI_RunOutlivesAMiddlewareThatCancelsTheRequestContext(t *testing.T) {
	t.Parallel()

	tests := map[string]func(app *fiber.App, handler fiber.Handler){
		"cancel when the handler returns": func(app *fiber.App, handler fiber.Handler) {
			app.Use(func(c fiber.Ctx) error {
				ctx, cancel := context.WithTimeout(context.WithValue(c.Context(), ctxKey{}, "trace-1"), time.Minute)
				defer cancel()
				c.SetContext(ctx)
				return c.Next()
			})
			app.Post("/agui", handler)
		},
		"timeout middleware": func(app *fiber.App, handler fiber.Handler) {
			app.Post("/agui", timeout.New(handler, timeout.Config{Timeout: time.Minute}))
		},
	}
	for name, register := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// The model answers once the handler and its middleware have returned.
			returned := make(chan struct{})
			agent := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
				ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
			) (*ai.ModelResponse, error) {
				select {
				case <-returned:
				case <-ctx.Done():
				}
				return textResponse("done"), nil
			}))
			closed := make(chan error, 1)
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				defer close(returned)
				return c.Next()
			})
			register(app, pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
				OnClose: func(_ struct{}, err error) { closed <- err },
			}))

			resp, body := do(t, app, fiber.MethodPost, "/agui", fiber.MIMEApplicationJSON, aguiBody, fiber.TestConfig{Timeout: 3 * time.Second, FailOnTimeout: true})
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
			events := aguiEvents(t, body)
			last := events[len(events)-1]
			require.Equal(t, agui.EventRunFinished, last.Type, body)
			require.NotNil(t, last.Outcome)
			require.Equal(t, "success", last.Outcome.Type)
			require.Equal(t, "done", pydanticaitest.AGUIText(events))
			require.NoError(t, agenttest.Receive(t, closed, "OnClose"))
		})
	}
}

func Test_ARequestThatATimeoutMiddlewareGaveUpOnStartsNoRun(t *testing.T) {
	t.Parallel()

	// slowDeps outlasts the timeout middleware.
	slowDeps := func(c fiber.Ctx) (struct{}, error) {
		<-c.Context().Done()
		return struct{}{}, nil
	}
	tests := []struct {
		name   string
		path   string
		body   string
		handle func(agent *ai.Agent[struct{}, string]) fiber.Handler
	}{
		{
			name: "AG-UI", path: "/agui", body: aguiBody,
			handle: func(agent *ai.Agent[struct{}, string]) fiber.Handler {
				return pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{Deps: slowDeps})
			},
		},
		{
			name: "Vercel", path: "/chat", body: vercelBody,
			handle: func(agent *ai.Agent[struct{}, string]) fiber.Handler {
				return pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{Deps: slowDeps})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model, called := unreachableModel()
			handler := test.handle(ai.NewAgent[struct{}, string](model))
			returned := make(chan error, 1)
			app := newApp(test.path, timeout.New(func(c fiber.Ctx) error {
				err := handler(c)
				returned <- err
				return err
			}, timeout.Config{Timeout: 10 * time.Millisecond}))

			// The middleware answers for the handler, and fasthttp never writes
			// the response of the handler, so a run would never end.
			resp, _ := post(t, app, test.path, test.body)
			require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
			require.ErrorIs(t, agenttest.Receive(t, returned, "the handler"), context.DeadlineExceeded)
			require.False(t, called.Load(), "the agent ran for a request nobody waits for")
		})
	}
}

func Test_APanicInErrorMessageEndsTheStreamWithAnError(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it. Neither are the
	// subtests, as the log is read after them.
	logs := agenttest.CaptureLog(t)

	secret := errors.New("provider rejected the key sk-secret")
	errorMessage := func(error) string { panic("bug in ErrorMessage") }
	// Each way in which a stream ends with an error, and what OnClose receives for it.
	tests := map[string]struct {
		agent  func() *ai.Agent[struct{}, string]
		closed func(t *testing.T, err error)
	}{
		"a failed run": {
			agent:  func() *ai.Agent[struct{}, string] { return ai.NewAgent[struct{}, string](agenttest.Failing(secret)) },
			closed: func(t *testing.T, err error) { require.ErrorIs(t, err, secret) },
		},
		"a panic in the run": {
			agent:  panicAgent,
			closed: func(t *testing.T, err error) { require.ErrorContains(t, err, "panic in the run: kaboom") },
		},
		"an event that cannot be encoded": {
			agent:  progressAgent,
			closed: func(t *testing.T, err error) { require.ErrorContains(t, err, "cannot encode the payload") },
		},
	}
	for name, test := range tests {
		t.Run("AG-UI: "+name, func(t *testing.T) {
			closed := make(chan error, 1)
			handler := pydanticai.AGUI(test.agent(), pydanticai.AGUIConfig[struct{}]{
				ErrorMessage: errorMessage,
				OnClose:      func(_ struct{}, err error) { closed <- err },
			})

			_, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
			require.Equal(t, "internal error", aguiFailure(t, body))
			require.NotContains(t, body, "sk-secret", "the client is not told what ErrorMessage hides")
			test.closed(t, agenttest.Receive(t, closed, "OnClose"))
		})
		t.Run("Vercel: "+name, func(t *testing.T) {
			closed := make(chan error, 1)
			handler := pydanticai.Vercel(test.agent(), pydanticai.VercelConfig[struct{}]{
				ErrorMessage: errorMessage,
				OnClose:      func(_ struct{}, err error) { closed <- err },
			})

			_, body := post(t, newApp("/chat", handler), "/chat", vercelBody)
			require.Equal(t, "internal error", vercelFailure(t, body))
			require.NotContains(t, body, "sk-secret", "the client is not told what ErrorMessage hides")
			test.closed(t, agenttest.Receive(t, closed, "OnClose"))
		})
	}
	require.Contains(t, logs.String(), "panic in ErrorMessage: bug in ErrorMessage")
}

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
				return agenttest.Receive(t, ends, "OnRunEnd")
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
				end := run(t, ai.NewAgent[userDeps, string](agenttest.Failing(secret)), protocol.body)
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
				require.Equal(t, "OnRunEnd", agenttest.Receive(t, order, "the first callback"))
				require.Equal(t, "OnClose", agenttest.Receive(t, order, "the second callback"))
			})
		})
	}
}

func Test_OnRunEnd_ARunWhoseClientLeft(t *testing.T) {
	t.Parallel()

	for _, protocol := range streamProtocols() {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			model, started, _ := agenttest.Blocking()
			ends := make(chan runEnd, 1)
			// The heartbeat notices that the client left.
			handler := protocol.build(ai.NewAgent[userDeps, string](model), func(deps userDeps, run *ai.RunInfo, err error) {
				ends <- runEnd{deps: deps, run: run, err: err}
			}, nil, 20*time.Millisecond)
			url := listen(t, newApp(protocol.path, handler)) + protocol.path + "?user=alice"

			response, closeConnection := openStream(t, url, protocol.body)
			_, ok := pydanticaitest.NewReader(response.Body).NextData()
			require.True(t, ok)
			agenttest.Receive(t, started, "the model call")
			closeConnection()

			// The run was canceled, and what it used so far is still reported.
			end := agenttest.Receive(t, ends, "OnRunEnd")
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
		end := agenttest.Receive(t, ends, "OnRunEnd")
		require.NoError(t, end.err)
		require.Equal(t, userDeps{User: "alice"}, end.deps)
		require.NotNil(t, end.run)
		require.Positive(t, end.run.Usage().Requests)
	})

	t.Run("a failed run", func(t *testing.T) {
		t.Parallel()

		ends := make(chan runEnd, 1)
		app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](agenttest.Failing(secret)), pydanticai.RunConfig[struct{}, string]{
			OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) { ends <- runEnd{run: run, err: err} },
		}))

		resp, body := post(t, app, "/ask", runBody)
		require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
		require.NotContains(t, body, "sk-secret")
		end := agenttest.Receive(t, ends, "OnRunEnd")
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
		app.Post("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](agenttest.Panicking("model kaboom")), pydanticai.RunConfig[struct{}, string]{
			OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) { ends <- runEnd{run: run, err: err} },
		}))

		resp, _ := post(t, app, "/ask", runBody)
		require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
		require.Equal(t, "model kaboom", agenttest.Receive(t, recovered, "the recover middleware"), "the panic goes on to the recover middleware")
		end := agenttest.Receive(t, ends, "OnRunEnd")
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

		end := agenttest.Receive(t, ends, protocol.name+" OnRunEnd")
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
	require.NoError(t, agenttest.Receive(t, closed, "OnClose"), "the panic in OnRunEnd does not take OnClose with it")
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

			end := agenttest.Receive(t, ends, "OnRunEnd")
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

		end := agenttest.Receive(t, ends, "OnRunEnd")
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
			require.NoError(t, agenttest.Receive(t, ends, "OnRunEnd"))
			require.NoError(t, agenttest.Receive(t, closed, "OnClose"))
		})
	}
}

// pydantic-ai-go runs the tool calls of one model response concurrently, each
// on a goroutine of its own that nothing guards. The tests in this file make
// sure that what happens on those goroutines cannot end the process.

func Test_APanicInAConcurrentToolCallEndsTheRunWithAnError(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it. Neither are the
	// subtests, as the log is read after them.
	logs := agenttest.CaptureLog(t) // the panic and its stack go to the log

	// "waiting" comes first: the panic cancels it, and its error is the first
	// error of the calls, which the run reports.
	for _, calls := range [][]string{{"explode", "fine"}, {"fine", "explode"}, {"waiting", "explode"}} {
		agent := agenttest.ConcurrentAgent(calls...)
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
			require.ErrorContains(t, agenttest.Receive(t, closed, "OnClose"), "panic in the run: kaboom")
			end := agenttest.Receive(t, ends, "OnRunEnd")
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
			require.ErrorContains(t, agenttest.Receive(t, closed, "OnClose"), "panic in the run: kaboom")
			require.Equal(t, "internal error", seen, "ErrorMessage is not given the value of the panic")
		})

		t.Run("Run: "+name, func(t *testing.T) {
			ends, recovered := make(chan runEnd, 1), make(chan any, 1)
			app := fiber.New()
			app.Use(recoverer.New(recoverer.Config{PanicHandler: func(_ fiber.Ctx, r any) error {
				recovered <- r
				return fiber.ErrInternalServerError
			}}))
			app.Post("/ask", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{
				OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) { ends <- runEnd{run: run, err: err} },
			}))

			resp, body := post(t, app, "/ask", runBody)
			require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
			require.NotContains(t, body, "kaboom", "the value of the panic stays on the server")
			end := agenttest.Receive(t, ends, "OnRunEnd")
			require.ErrorContains(t, end.err, "panic in the run: kaboom")
			require.NotNil(t, end.run)
			// The tool ran on a goroutine that no middleware reaches: Run recovered
			// the panic and failed the run, so the recover middleware saw nothing.
			require.Empty(t, recovered)
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

			agent := ai.NewAgent[struct{}, string](agenttest.ToolCalls("first", "second"))
			started := make(chan struct{}, 2)
			slowTools(agent, started)
			closed := make(chan error, 1)
			app := newApp(test.path, test.handle(agent, func(_ struct{}, err error) { closed <- err }))

			response, closeConnection := openStream(t, listen(t, app)+test.path, test.body)
			_, ok := pydanticaitest.NewReader(response.Body).NextData()
			require.True(t, ok)
			agenttest.Receive(t, started, "the first tool call")
			agenttest.Receive(t, started, "the second tool call")
			closeConnection()

			// The tools emit their results after the stream failed, which ended
			// the process when the handler stopped taking the events of the run.
			require.Error(t, agenttest.Receive(t, closed, "OnClose"))
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
	progress := ai.NewAgent[struct{}, string](agenttest.ToolCalls("progress", "slow"))
	ai.AddTool(progress, "progress", func(_ context.Context, rc *ai.RunContext[struct{}], _ struct{}) (string, error) {
		return "ok", rc.Emit(ai.NewCustomEvent("progress", unencodable{}))
	})
	slow(progress)
	// nan returns a result that the AG-UI adapter cannot convert and the Vercel
	// handler cannot encode.
	nan := ai.NewAgent[struct{}, string](agenttest.ToolCalls("nan", "slow"))
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
			require.Error(t, agenttest.Receive(t, closed, "OnClose"))
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
			require.Error(t, agenttest.Receive(t, closed, "OnClose"))
		})
	}
}

// The AG-UI adapter stops by itself when it cannot convert a tool result, while
// a tool that ignores its context goes on and emits an event afterwards. The
// stream guard keeps that event from reaching the loop that the adapter left.
func Test_AnAdapterThatStopsEarlyWhileAToolEmitsLater(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](agenttest.ToolCalls("nan", "late"))
	ai.AddSimpleTool(agent, "nan", func(context.Context, struct{}) (unencodableToolResult, error) {
		return unencodableToolResult{Value: math.NaN()}, nil // the adapter cannot convert it
	})
	ai.AddTool(agent, "late", func(_ context.Context, rc *ai.RunContext[struct{}], _ struct{}) (string, error) {
		time.Sleep(300 * time.Millisecond)
		return "ok", rc.Emit(ai.NewCustomEvent("late", "x"))
	})
	closed := make(chan error, 1)
	handler := pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
		OnClose: func(_ struct{}, err error) { closed <- err },
	})

	_, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
	require.Contains(t, aguiFailure(t, body), "encode tool result", "the failure of the adapter, not a panic")
	require.NotContains(t, agenttest.Receive(t, closed, "OnClose").Error(), "panic")
}

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

			model, started, canceled := agenttest.Blocking()
			closed := make(chan error, 1)
			app := newApp(test.path, test.handle(ai.NewAgent[struct{}, string](model), func(_ struct{}, err error) { closed <- err }))
			response, _ := openStream(t, listen(t, app)+test.path, test.body)
			scanner := pydanticaitest.NewReader(response.Body)
			_, ok := scanner.NextData()
			require.True(t, ok)
			agenttest.Receive(t, started, "the model call")

			// The run does not end on its own, so a shutdown that waits for it takes the whole timeout.
			begin := time.Now()
			require.NoError(t, app.ShutdownWithTimeout(10*time.Second))
			require.Less(t, time.Since(begin), 5*time.Second, "the shutdown waited for the run")

			require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the model call"), ai.ErrRunCancelled)
			require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), ai.ErrRunCancelled)

			// The client gets the end of the stream, not a broken connection.
			records := drain(scanner)
			require.NotEmpty(t, records)
			require.Contains(t, records[len(records)-1], test.ends)
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
	require.NoError(t, agenttest.Receive(t, closed, "OnClose"))
}

func Test_ShutdownWaitsForARunByDefault(t *testing.T) {
	t.Parallel()

	model, started, canceled := agenttest.Blocking()
	app := newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond, // notices the client that leaves at the end
	}))
	response, closeConnection := openStream(t, listen(t, app)+"/agui", aguiBody)
	_, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	agenttest.Receive(t, started, "the model call")

	require.ErrorIs(t, app.ShutdownWithTimeout(200*time.Millisecond), context.DeadlineExceeded)
	select {
	case <-canceled:
		t.Fatal("the shutdown canceled the run without CancelOnShutdown")
	default:
	}

	// The run ends when its client leaves.
	closeConnection()
	require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the model call"), context.Canceled)
}

func Test_CancelOnShutdownDoesNotWaitForAClientThatStoppedReading(t *testing.T) {
	t.Parallel()

	closed := make(chan error, 1)
	app := fiber.New()
	app.Post("/agui", pydanticai.AGUI(talkativeAgent(), pydanticai.WithStreamEndGrace(pydanticai.AGUIConfig[struct{}]{
		CancelOnShutdown: true,
		OnClose:          func(_ struct{}, err error) { closed <- err },
	}, 100*time.Millisecond)))

	postAndStopReading(t, app, "/agui", aguiBody)
	time.Sleep(300 * time.Millisecond) // the writes to the client block

	begin := time.Now()
	require.NoError(t, app.ShutdownWithTimeout(10*time.Second))
	require.Less(t, time.Since(begin), 5*time.Second, "the shutdown waited for the client")
	require.Error(t, agenttest.Receive(t, closed, "OnClose"))
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
	agenttest.Receive(t, model.closing, "the close of the model")
	require.NoError(t, app.ShutdownWithTimeout(observeTimeout))

	events := pydanticaitest.AGUIEvents(t, response.Body)
	last := events[len(events)-1]
	require.Equal(t, agui.EventRunFinished, last.Type)
	require.NotNil(t, last.Outcome, "the client was told that the run completed")
	require.NoError(t, agenttest.Receive(t, closed, "OnClose"))
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
			require.NoError(t, agenttest.Receive(t, closed, "OnClose"))
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

// The tests in this file talk to a server on a real socket. app.Test hands the
// whole body over once the handler is done, so it cannot show that events are
// flushed as they are written or that a closed socket stops the run.

// listen serves app on a free local port and returns its base URL.
func listen(t *testing.T, app *fiber.App) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() {
		served <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() {
		require.NoError(t, app.ShutdownWithTimeout(observeTimeout))
		require.NoError(t, <-served)
	})
	return "http://" + ln.Addr().String()
}

// openStream posts body to url and returns the response, whose body can be read
// while the server is still working, and a function that closes the connection.
// prepare changes the request before it is sent.
func openStream(t *testing.T, url, body string, prepare ...func(*http.Request)) (*http.Response, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*observeTimeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	for _, change := range prepare {
		change(request)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		_ = response.Body.Close()
	})
	return response, cancel
}

func Test_AGUI_FlushesEventsWhileTheRunIsStillWorking(t *testing.T) {
	t.Parallel()

	model, release := gatedModel("late answer")
	url := listen(t, newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](model)))) + "/agui"

	response, _ := openStream(t, url, aguiBody)
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	require.Contains(t, response.TransferEncoding, "chunked")
	scanner := pydanticaitest.NewReader(response.Body)

	// The start of the run reaches the client while the model has not answered.
	record, ok := scanner.NextData()
	require.True(t, ok)
	require.Contains(t, record, `"type":"RUN_STARTED"`)

	close(release)
	rest := strings.Join(drain(scanner), "")
	require.Contains(t, rest, "late answer")
	require.Contains(t, rest, `"type":"RUN_FINISHED"`)
}

func Test_AGUI_StreamsThroughCompress(t *testing.T) {
	t.Parallel()

	model, release := gatedModel("late answer")
	closed := make(chan error, 1)
	app := fiber.New()
	app.Use(compress.New())
	app.Post("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		OnClose: func(_ struct{}, err error) { closed <- err },
	}))

	response, _ := openStream(t, listen(t, app)+"/agui", aguiBody, func(request *http.Request) {
		request.Header.Set(fiber.HeaderAcceptEncoding, "gzip") // set by hand, so net/http leaves the body encoded
	})
	require.Equal(t, "gzip", response.Header.Get(fiber.HeaderContentEncoding))
	reader, err := gzip.NewReader(response.Body)
	require.NoError(t, err)
	scanner := pydanticaitest.NewReader(reader)

	// The start of the run gets through the compression while the model has not answered.
	record, ok := scanner.NextData()
	require.True(t, ok)
	require.Contains(t, record, `"type":"RUN_STARTED"`)

	close(release)
	require.Contains(t, strings.Join(drain(scanner), ""), `"type":"RUN_FINISHED"`)
	// Closing the compressed body closes the body of the handler, which ends the hold.
	require.NoError(t, agenttest.Receive(t, closed, "OnClose"))
}

func Test_AGUI_ClosedSocketCancelsRun(t *testing.T) {
	t.Parallel()

	model, started, canceled := agenttest.Blocking()
	closed := make(chan error, 1)
	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond,
		OnClose:           func(_ struct{}, err error) { closed <- err },
	})
	url := listen(t, newApp("/agui", handler)) + "/agui"

	response, closeConnection := openStream(t, url, aguiBody)
	record, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	require.Contains(t, record, `"type":"RUN_STARTED"`)
	agenttest.Receive(t, started, "the model call")

	closeConnection()

	require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the model call"), context.Canceled)
	require.Error(t, agenttest.Receive(t, closed, "OnClose"))
}

func Test_Vercel_ClosedSocketCancelsRun(t *testing.T) {
	t.Parallel()

	model, started, canceled := agenttest.Blocking()
	closed := make(chan error, 1)
	handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](model), pydanticai.VercelConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond,
		OnClose:           func(_ struct{}, err error) { closed <- err },
	})
	url := listen(t, newApp("/chat", handler)) + "/chat"

	response, closeConnection := openStream(t, url, vercelBody)
	require.Equal(t, "v1", response.Header.Get("x-vercel-ai-ui-message-stream"))
	record, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	require.Contains(t, record, `"type":"start"`)
	agenttest.Receive(t, started, "the model call")

	closeConnection()

	require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the model call"), context.Canceled)
	require.Error(t, agenttest.Receive(t, closed, "OnClose"))
}

// smallBuffers is a listener whose connections have small send buffers, so that
// a client that does not read blocks the writes of the server soon.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(4 << 10)
	}
	return conn, err
}

// postAndStopReading serves app with small socket buffers, and posts body to path
// on a connection that never reads the response.
func postAndStopReading(t *testing.T, app *fiber.App, path, body string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(smallBuffers{ln}, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = app.ShutdownWithTimeout(observeTimeout) })

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(4<<10))
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		path, len(body), body)
	require.NoError(t, err)
}

// talkativeAgent answers with far more than the socket buffers of a client that
// does not read can hold.
func talkativeAgent() *ai.Agent[struct{}, string] {
	parts := make([]ai.ResponsePart, 400)
	for i := range parts {
		parts[i] = ai.TextPart{Content: strings.Repeat("y", 2000)}
	}
	return ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		context.Context, []ai.ModelMessage, ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		return &ai.ModelResponse{Parts: parts}, nil
	}))
}

func Test_TimeoutEndsAStreamWhoseClientStoppedReading(t *testing.T) {
	t.Parallel()

	ends, closed := make(chan error, 1), make(chan error, 1)
	app := fiber.New()
	app.Post("/agui", pydanticai.AGUI(talkativeAgent(), pydanticai.WithStreamEndGrace(pydanticai.AGUIConfig[struct{}]{
		Timeout:  200 * time.Millisecond,
		OnRunEnd: func(_ struct{}, _ *ai.RunInfo, err error) { ends <- err },
		OnClose:  func(_ struct{}, err error) { closed <- err },
	}, 100*time.Millisecond)))

	// The run is stuck in a write to the client, where its context does not
	// reach it. Its connection is closed once the write blocked for the grace.
	postAndStopReading(t, app, "/agui", aguiBody)
	require.Error(t, agenttest.Receive(t, ends, "OnRunEnd"))
	require.Error(t, agenttest.Receive(t, closed, "OnClose"))
}

func Test_TimeoutLeavesTheClientOfARunThatIsSlowToEnd(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		context.Context, []ai.ModelMessage, ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		time.Sleep(time.Second) // ignores its context
		return textResponse("late"), nil
	}))
	closed := make(chan error, 1)
	app := newApp("/agui", pydanticai.AGUI(agent, pydanticai.WithStreamEndGrace(pydanticai.AGUIConfig[struct{}]{
		Timeout: 200 * time.Millisecond,
		OnClose: func(_ struct{}, err error) { closed <- err },
	}, 100*time.Millisecond)))

	// The client reads, so nothing blocks: the stream is not cut.
	response, _ := openStream(t, listen(t, app)+"/agui", aguiBody)
	events := pydanticaitest.AGUIEvents(t, response.Body)
	last := events[len(events)-1]
	require.Equal(t, agui.EventRunFinished, last.Type)
	require.Nil(t, last.Outcome, "the run timed out")
	require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), context.DeadlineExceeded)
}

func Test_AConnectionServesTheNextRequestAfterAStreamThatTimedOut(t *testing.T) {
	t.Parallel()

	app := newApp("/agui", pydanticai.AGUI(slowAgent(), pydanticai.WithStreamEndGrace(
		pydanticai.AGUIConfig[struct{}]{Timeout: 50 * time.Millisecond}, 100*time.Millisecond)))
	url := listen(t, app) + "/agui"
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	// stream posts a request and returns whether it reused a connection.
	stream := func() bool {
		var reused bool
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(context.Background(), trace), 2*observeTimeout)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(aguiBody))
		require.NoError(t, err)
		request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		response, err := client.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		events := pydanticaitest.AGUIEvents(t, response.Body)
		require.Equal(t, agui.EventRunFinished, events[len(events)-1].Type)
		return reused
	}

	stream()
	time.Sleep(300 * time.Millisecond) // past the grace
	require.True(t, stream(), "the connection was closed")
}
