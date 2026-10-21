package pydanticai_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/gofiber/fiber/v3/middleware/timeout"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a log destination that can be read while it is written.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the log of Fiber to the returned buffer until the test ends.
// The logger is global, so the test must not run in parallel with others.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	logs := new(syncBuffer)
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return logs
}

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
	_, ok := nextData(newScanner(conn))
	require.True(t, ok)
	receive(t, started, "the tool call")
	require.NoError(t, conn.Close())
	receive(t, canceled, "the cancellation of the run") // the heartbeats fail and fasthttp gives up on the response

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
	got := receive(t, late, "the tool")
	require.Equal(t, "AAAAAAAAAAAA", got.user, "the dependencies of the run")
	require.Equal(t, "request-A-0000000001", got.requestID, "the value of the middleware in the context of the run")
	require.Equal(t, "tenant-A", got.tenant, "the Locals of the request")
}

func Test_AGUI_APanicInOnCloseIsRecovered(t *testing.T) {
	logs := captureLog(t)

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
		receive(t, called, "OnClose")
	}
	require.Eventually(t, func() bool {
		return strings.Count(logs.String(), "panic in OnClose: bug in OnClose") == 2
	}, observeTimeout, 10*time.Millisecond, logs.String())
}

func Test_AGUI_APanicInTheRunEndsTheStreamWithAnError(t *testing.T) {
	logs := captureLog(t)

	t.Run("the client is told", func(t *testing.T) {
		closed := make(chan error, 1)
		handler := pydanticai.AGUI(panicAgent(), pydanticai.AGUIConfig[struct{}]{
			OnClose: func(_ struct{}, err error) { closed <- err },
		})

		resp, body := post(t, newApp("/agui", handler), "/agui", aguiBody)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		require.Equal(t, "internal error", aguiFailure(t, body))
		require.NotContains(t, body, "kaboom", "the value of the panic stays on the server")
		require.ErrorContains(t, receive(t, closed, "OnClose"), "panic in the run: kaboom")
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
	logs := captureLog(t)

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
	require.ErrorContains(t, receive(t, closed, "OnClose"), "panic in the run: kaboom")
	require.Contains(t, logs.String(), "panic in the run: kaboom")
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
			require.Equal(t, "done", aguiText(events))
			require.NoError(t, receive(t, closed, "OnClose"))
		})
	}
}
