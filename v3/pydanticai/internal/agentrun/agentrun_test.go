package agentrun_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

func Test_PanicError(t *testing.T) {
	t.Parallel()

	err := agentrun.PanicError("kaboom")
	require.EqualError(t, err, "pydanticai: panic in the run: kaboom")

	// A panic with an error does not make the error the cause: it would turn
	// the panic into a retry or a failure of a tool.
	cause := errors.New("cause")
	require.NotErrorIs(t, agentrun.PanicError(cause), cause)
}

func Test_Timeout(t *testing.T) {
	t.Parallel()

	// The default is part of the documentation of the handlers: five minutes.
	require.Equal(t, 5*time.Minute, agentrun.Timeout(0))
	require.Equal(t, 5*time.Minute, agentrun.DefaultTimeout)
	require.Zero(t, agentrun.Timeout(-1), "a negative value turns the bound off")
	require.Zero(t, agentrun.Timeout(-time.Hour))
	require.Equal(t, time.Nanosecond, agentrun.Timeout(time.Nanosecond))
	require.Equal(t, 2*time.Hour, agentrun.Timeout(2*time.Hour), "a longer timeout is used as it is")
}

func Test_Safely(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	called := false
	agentrun.Safely("Callback", func() { called = true })
	require.True(t, called)
	require.Empty(t, logs.String())

	require.NotPanics(t, func() { agentrun.Safely("Callback", func() { panic("bug in Callback") }) })
	require.Contains(t, logs.String(), "panic in Callback: bug in Callback")
	require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")
}

func Test_Text(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	failure := errors.New("provider rejected the key sk-secret")
	require.Equal(t, failure.Error(), agentrun.Text(nil, failure))
	require.Equal(t, "the run failed", agentrun.Text(func(error) string { return "the run failed" }, failure))

	// The client is told what it is told for a panic, not what message hides.
	text := agentrun.Text(func(error) string { panic("bug in ErrorMessage") }, failure)
	require.Equal(t, agentrun.ErrPanicked.Error(), text)
	require.Contains(t, logs.String(), "panic in ErrorMessage: bug in ErrorMessage")
}

// resolved runs Resolve in a handler of an app, which is where it gets its Ctx.
func resolved[Deps any](
	t *testing.T,
	deps func(fiber.Ctx) (Deps, error),
	options func(fiber.Ctx) ([]ai.RunOption, error),
) (value Deps, extra []ai.RunOption, err error) {
	t.Helper()

	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		value, extra, err = agentrun.Resolve(c, deps, options)
		return nil
	})
	resp, testErr := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	require.NoError(t, testErr)
	require.NoError(t, resp.Body.Close())
	return value, extra, err
}

func Test_Resolve(t *testing.T) {
	t.Parallel()

	failure := errors.New("rejected")
	user := func(fiber.Ctx) (string, error) { return "alice", nil }
	metadata := func(fiber.Ctx) ([]ai.RunOption, error) {
		return []ai.RunOption{ai.WithRunMetadata(map[string]any{"k": "v"})}, nil
	}

	t.Run("without callbacks", func(t *testing.T) {
		t.Parallel()

		deps, options, err := resolved[string](t, nil, nil)
		require.NoError(t, err)
		require.Empty(t, deps, "the zero value of Deps")
		require.Nil(t, options)
	})

	t.Run("both callbacks", func(t *testing.T) {
		t.Parallel()

		deps, options, err := resolved(t, user, metadata)
		require.NoError(t, err)
		require.Equal(t, "alice", deps)
		require.Len(t, options, 1)
	})

	t.Run("Deps fails", func(t *testing.T) {
		t.Parallel()

		asked := false
		_, options, err := resolved(t,
			func(fiber.Ctx) (string, error) { return "", failure },
			func(fiber.Ctx) ([]ai.RunOption, error) { asked = true; return nil, nil })
		require.ErrorIs(t, err, failure)
		require.Nil(t, options)
		require.False(t, asked, "an error ends the resolution")
	})

	t.Run("RunOptions fails", func(t *testing.T) {
		t.Parallel()

		_, options, err := resolved(t, user, func(fiber.Ctx) ([]ai.RunOption, error) {
			return []ai.RunOption{ai.WithRunMetadata(nil)}, failure
		})
		require.ErrorIs(t, err, failure)
		require.Nil(t, options, "the options of a failed call are dropped")
	})
}

func Test_Recovered(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	var err error
	func() {
		defer func() { err = agentrun.Recovered(recover()) }()
		panic("kaboom")
	}()
	require.EqualError(t, err, "pydanticai: panic in the run: kaboom")
	require.Contains(t, logs.String(), "panic in the run: kaboom")
	require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")
}

func Test_Guard(t *testing.T) {
	t.Run("a panic in a tool", func(t *testing.T) {
		// Not parallel: the log is global, and CaptureLog replaces it.
		logs := agenttest.CaptureLog(t)

		agent := agenttest.ConcurrentAgent("explode", "waiting")
		options := make([]ai.RunOption, 1, 8) // room to spare, which an append would write into
		options[0] = ai.WithRunMetadata(map[string]any{"k": "v"})
		var (
			mu   sync.Mutex
			ends []error
			runs []*ai.RunInfo
		)
		guard, guarded := agentrun.NewGuard(options, struct{}{}, func(_ struct{}, run *ai.RunInfo, err error) {
			mu.Lock()
			defer mu.Unlock()
			ends, runs = append(ends, err), append(runs, run)
		})
		require.Len(t, guarded, 2)
		require.Nil(t, options[:2][1], "the slice of the caller is not written to")
		require.NoError(t, guard.Err(), "nothing has panicked")

		// The two tools run on goroutines of their own: without the guard the
		// panic would end the process.
		_, err := agent.Run(context.Background(), "Hello", struct{}{}, guarded...)
		require.Error(t, err)
		require.ErrorContains(t, guard.Err(), "panic in the run: kaboom")
		require.Contains(t, logs.String(), "panic in the run: kaboom")

		// The end of the run is reported once, with the run that the guard recorded.
		guard.End(err)
		guard.End(nil)
		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, []error{err}, ends)
		require.Len(t, runs, 1)
		require.NotNil(t, runs[0])
	})

	t.Run("a run that completes", func(t *testing.T) {
		t.Parallel()

		agent := agenttest.ConcurrentAgent("fine")
		var run *ai.RunInfo
		guard, guarded := agentrun.NewGuard(nil, struct{}{}, func(_ struct{}, info *ai.RunInfo, _ error) { run = info })
		require.Nil(t, run, "no run has started")
		result, err := agent.Run(context.Background(), "Hello", struct{}{}, guarded...)
		require.NoError(t, err)
		require.Equal(t, "done", strings.TrimSpace(result.Output))
		require.NoError(t, guard.Err())

		guard.End(nil)
		require.NotNil(t, run)
		require.Equal(t, result.RunID(), run.RunID)
	})

	t.Run("a run that did not start", func(t *testing.T) {
		t.Parallel()

		var (
			called bool
			run    *ai.RunInfo
		)
		guard, _ := agentrun.NewGuard(nil, struct{}{}, func(_ struct{}, info *ai.RunInfo, _ error) { called, run = true, info })
		guard.End(errors.New("rejected"))
		require.True(t, called)
		require.Nil(t, run)
	})

	t.Run("without a callback", func(t *testing.T) {
		t.Parallel()

		guard, _ := agentrun.NewGuard[struct{}](nil, struct{}{}, nil)
		require.NotPanics(t, func() { guard.End(nil) })
	})
}

func Test_ServerDone(t *testing.T) {
	t.Parallel()

	t.Run("a RequestCtx that no server serves", func(t *testing.T) {
		t.Parallel()

		require.NotPanics(t, func() { require.Nil(t, agentrun.ServerDone(new(fasthttp.RequestCtx))) })
	})

	t.Run("closed when the server starts to shut down", func(t *testing.T) {
		t.Parallel()

		done := make(chan (<-chan struct{}), 1)
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { done <- agentrun.ServerDone(ctx) }}
		listener := fasthttputil.NewInmemoryListener()
		served := make(chan error, 1)
		go func() { served <- server.Serve(listener) }()

		conn, err := listener.Dial()
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
		require.NoError(t, err)

		var shutdown <-chan struct{}
		select {
		case shutdown = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the handler was not called")
		}
		require.NotNil(t, shutdown)
		select {
		case <-shutdown:
			t.Fatal("the server is not shutting down")
		default:
		}

		go func() { _ = server.Shutdown() }()
		select {
		case <-shutdown:
		case <-time.After(5 * time.Second):
			t.Fatal("the channel was not closed")
		}
		require.NoError(t, <-served)
	})

	t.Run("the error of a run canceled by the shutdown", func(t *testing.T) {
		t.Parallel()

		require.ErrorIs(t, agentrun.ErrShutdown, ai.ErrRunCancelled)
	})
}
