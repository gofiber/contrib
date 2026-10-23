package agentrun_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/stretchr/testify/require"
)

// shortTimeout is a timeout that elapses while a test waits for it.
const shortTimeout = 20 * time.Millisecond

// ends records what OnRunEnd receives.
type ends[Deps any] struct {
	mu    sync.Mutex
	deps  []Deps
	runs  []*ai.RunInfo
	errs  []error
	calls int
}

func (e *ends[Deps]) onRunEnd(deps Deps, run *ai.RunInfo, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deps, e.runs, e.errs = append(e.deps, deps), append(e.runs, run), append(e.errs, err)
	e.calls++
}

// only returns the one call of OnRunEnd, and fails the test unless there was one.
func (e *ends[Deps]) only(t *testing.T) (deps Deps, run *ai.RunInfo, err error) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Equal(t, 1, e.calls, "OnRunEnd is called once")
	return e.deps[0], e.runs[0], e.errs[0]
}

// blocking is a model that tells that it started, and waits for its context.
func blocking() (model *fakes.FunctionModel, started <-chan struct{}) {
	signal := make(chan struct{})
	var once sync.Once
	model = fakes.NewFunctionModel(func(ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams) (*ai.ModelResponse, error) {
		once.Do(func() { close(signal) })
		<-ctx.Done()
		return nil, ctx.Err()
	})
	return model, signal
}

// failing is a model that fails with err.
func failing(err error) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		return nil, err
	})
}

// panicking is a model that panics with value.
func panicking(value any) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		panic(value)
	})
}

func Test_OneShotTimeout(t *testing.T) {
	t.Parallel()

	// The default is part of the documentation of the handlers: five minutes.
	require.Equal(t, 5*time.Minute, agentrun.OneShotTimeout(0))
	require.Zero(t, agentrun.OneShotTimeout(-1), "a negative value turns the bound off")
	require.Zero(t, agentrun.OneShotTimeout(-time.Hour))
	require.Equal(t, time.Nanosecond, agentrun.OneShotTimeout(time.Nanosecond))
	require.Equal(t, 2*time.Hour, agentrun.OneShotTimeout(2*time.Hour), "a longer timeout is used as it is")
}

func Test_ErrShutdown(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, agentrun.ErrShutdown, ai.ErrRunCancelled, "the agent treats it as a request to stop")
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

func Test_Recover(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	t.Run("a panic", func(t *testing.T) {
		var reported error
		require.NotPanics(t, func() {
			defer agentrun.Recover(func(err error) { reported = err })
			panic("kaboom")
		})
		require.EqualError(t, reported, "pydanticai: panic in the run: kaboom")
		require.Contains(t, logs.String(), "panic in the run: kaboom")
		require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")

		// A panic with an error does not make the error the cause: it would turn
		// the panic into a retry or a failure of a tool.
		cause := errors.New("cause")
		require.NotPanics(t, func() {
			defer agentrun.Recover(func(err error) { reported = err })
			panic(cause)
		})
		require.NotErrorIs(t, reported, cause)
	})

	t.Run("no panic", func(t *testing.T) {
		called := false
		func() {
			defer agentrun.Recover(func(error) { called = true })
		}()
		require.False(t, called)
	})
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

	t.Run("the error of a run that panicked", func(t *testing.T) {
		var panicErr error
		func() {
			defer agentrun.Recover(func(err error) { panicErr = err })
			panic("secret value of the panic")
		}()
		require.ErrorContains(t, panicErr, "secret value of the panic", "the logs and OnRunEnd have it")

		require.Equal(t, agentrun.ErrPanicked.Error(), agentrun.Text(nil, panicErr))
		require.Equal(t, agentrun.ErrPanicked.Error(), agentrun.Text(nil, errors.Join(failure, panicErr)),
			"also when something else wrapped it")
		var told error
		require.Equal(t, "told", agentrun.Text(func(err error) string { told = err; return "told" }, panicErr))
		require.Same(t, agentrun.ErrPanicked, told, "ErrorMessage is given ErrPanicked, and not the panic")
	})
}

func Test_Run(t *testing.T) {
	// Not parallel: the subtests that capture the log are not, and they have to
	// run apart from the ones that are.
	t.Run("the end of the run is reported once", func(t *testing.T) {
		t.Parallel()

		var recorded ends[string]
		agent := ai.NewAgent[string, string](agenttest.Answering("hello"))
		run, options := agentrun.New(context.Background(), agentrun.Config[string]{
			Deps: "alice", OnRunEnd: recorded.onRunEnd,
		}, nil)
		defer run.Cancel()

		result, err := agent.Run(run.Context(), "Hi", "alice", options...)
		require.NoError(t, err)
		require.NoError(t, run.Panic(), "nothing has panicked")
		failure := errors.New("failed afterwards")
		run.End(failure)
		run.End(nil)

		deps, info, got := recorded.only(t)
		require.Equal(t, "alice", deps)
		require.Same(t, failure, got, "the first end counts")
		require.NotNil(t, info)
		require.Equal(t, result.RunID(), info.RunID, "the run that the agent started")
	})

	t.Run("a run that did not start", func(t *testing.T) {
		t.Parallel()

		var recorded ends[struct{}]
		run, _ := agentrun.New(context.Background(), agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd}, nil)
		defer run.Cancel()

		rejected := errors.New("rejected")
		run.End(rejected)
		_, info, err := recorded.only(t)
		require.Nil(t, info)
		require.Same(t, rejected, err)
	})

	t.Run("without a callback", func(t *testing.T) {
		t.Parallel()

		run, _ := agentrun.New(context.Background(), agentrun.Config[struct{}]{}, nil)
		defer run.Cancel()
		require.NotPanics(t, func() { run.End(nil) })
	})

	t.Run("the options of the caller are not written to", func(t *testing.T) {
		t.Parallel()

		options := make([]ai.RunOption, 1, 8) // room to spare, which an append would write into
		options[0] = ai.WithRunMetadata(map[string]any{"k": "v"})
		run, guarded := agentrun.New(context.Background(), agentrun.Config[struct{}]{}, options)
		defer run.Cancel()

		require.Len(t, guarded, 2, "the options of the caller, and the one that supervises the run")
		require.Nil(t, options[:2][1], "the slice of the caller is not written to")
	})

	t.Run("a panic in a tool", func(t *testing.T) {
		// Not parallel: the log is global, and CaptureLog replaces it.
		logs := agenttest.CaptureLog(t)

		agent := agenttest.ConcurrentAgent("explode", "waiting")
		run, options := agentrun.New(context.Background(), agentrun.Config[struct{}]{}, nil)
		defer run.Cancel()

		// The two tools run on goroutines of their own: without the supervision
		// the panic would end the process.
		_, err := agent.Run(run.Context(), "Hello", struct{}{}, options...)
		require.Error(t, err)
		require.ErrorContains(t, run.Panic(), "panic in the run: kaboom")
		require.Contains(t, logs.String(), "panic in the run: kaboom")
	})

	t.Run("the context of the run", func(t *testing.T) {
		t.Parallel()

		type key struct{}
		parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), key{}, "value"))
		run, _ := agentrun.New(parent, agentrun.Config[struct{}]{}, nil)
		defer run.Cancel()

		require.Equal(t, "value", run.Context().Value(key{}), "the values of the parent are the run's")
		require.NoError(t, run.Context().Err())
		require.False(t, run.Expired())

		cancelParent()
		<-run.Context().Done()
		require.False(t, run.Expired(), "its owner canceled the run, and the server did not")
	})

	t.Run("a run that its owner canceled", func(t *testing.T) {
		t.Parallel()

		shutdown := make(chan struct{})
		run, _ := agentrun.New(context.Background(), agentrun.Config[struct{}]{Timeout: time.Hour, Shutdown: shutdown}, nil)
		run.Cancel()
		run.Cancel() // more than once is fine
		<-run.Context().Done()
		require.False(t, run.Expired())

		close(shutdown)
		require.False(t, run.Expired(), "a shutdown that comes later is not why the run ended")
		require.NotErrorIs(t, context.Cause(run.Context()), agentrun.ErrShutdown)
	})

	t.Run("a run that timed out", func(t *testing.T) {
		t.Parallel()

		run, _ := agentrun.New(context.Background(), agentrun.Config[struct{}]{Timeout: shortTimeout}, nil)
		defer run.Cancel()

		<-run.Context().Done()
		require.ErrorIs(t, run.Context().Err(), context.DeadlineExceeded)
		require.True(t, run.Expired())
	})

	t.Run("a run that the shutdown canceled", func(t *testing.T) {
		t.Parallel()

		shutdown := make(chan struct{})
		run, _ := agentrun.New(context.Background(), agentrun.Config[struct{}]{Timeout: time.Hour, Shutdown: shutdown}, nil)
		defer run.Cancel()
		require.False(t, run.Expired())

		close(shutdown)
		<-run.Context().Done()
		require.True(t, run.Expired())
		require.ErrorIs(t, context.Cause(run.Context()), agentrun.ErrShutdown, "with a timeout as well")
	})

	t.Run("a server that is already shutting down", func(t *testing.T) {
		t.Parallel()

		shutdown := make(chan struct{})
		close(shutdown)
		run, _ := agentrun.New(context.Background(), agentrun.Config[struct{}]{Shutdown: shutdown}, nil)
		defer run.Cancel()

		<-run.Context().Done()
		require.True(t, run.Expired())
	})
}

func Test_Execute(t *testing.T) {
	// Not parallel: the subtests that capture the log are not, and they have to
	// run apart from the ones that are.
	t.Run("a run that completes", func(t *testing.T) {
		t.Parallel()

		var recorded ends[string]
		agent := ai.NewAgent[string, string](agenttest.Answering("hello"))
		result, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[string]{
			Deps: "alice", OnRunEnd: recorded.onRunEnd,
		})
		require.NoError(t, err)
		require.Equal(t, "hello", strings.TrimSpace(result.Output))

		deps, info, got := recorded.only(t)
		require.Equal(t, "alice", deps)
		require.NoError(t, got)
		require.NotNil(t, info)
		require.Equal(t, result.RunID(), info.RunID)
	})

	t.Run("without a callback", func(t *testing.T) {
		t.Parallel()

		agent := ai.NewAgent[struct{}, string](agenttest.Answering("hello"))
		result, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{})
		require.NoError(t, err)
		require.Equal(t, "hello", strings.TrimSpace(result.Output))
	})

	t.Run("a run that fails", func(t *testing.T) {
		t.Parallel()

		var recorded ends[struct{}]
		rejected := errors.New("the provider rejected the key")
		agent := ai.NewAgent[struct{}, string](failing(rejected))
		result, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{
			OnRunEnd: recorded.onRunEnd,
		})
		require.ErrorIs(t, err, rejected)
		require.Nil(t, result)

		_, _, got := recorded.only(t)
		require.Equal(t, err, got, "the callback is told what the caller is told")
	})

	t.Run("a panic in a tool outranks the error that it caused", func(t *testing.T) {
		// Not parallel: the log is global, and CaptureLog replaces it.
		agenttest.CaptureLog(t)

		var recorded ends[struct{}]
		agent := agenttest.ConcurrentAgent("explode", "waiting")
		_, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{
			OnRunEnd: recorded.onRunEnd,
		})
		require.ErrorContains(t, err, "panic in the run: kaboom")
		require.Equal(t, agentrun.ErrPanicked.Error(), agentrun.Text(nil, err), "a client is not told what panicked")

		_, _, got := recorded.only(t)
		require.Equal(t, err, got)
	})

	t.Run("a panic in the model goes on after it was reported", func(t *testing.T) {
		t.Parallel()

		var recorded ends[struct{}]
		agent := ai.NewAgent[struct{}, string](panicking("model bug"))
		require.PanicsWithValue(t, "model bug", func() {
			_, _ = agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{
				OnRunEnd: recorded.onRunEnd,
			})
		})

		_, info, err := recorded.only(t)
		require.EqualError(t, err, "pydanticai: panic in the run: model bug")
		require.NotNil(t, info, "the run had started: it consumed what it consumed")
	})

	t.Run("a panic in the model without a callback goes on", func(t *testing.T) {
		t.Parallel()

		agent := ai.NewAgent[struct{}, string](panicking("model bug"))
		require.PanicsWithValue(t, "model bug", func() {
			_, _ = agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{})
		})
	})

	t.Run("a panic in OnRunEnd is recovered", func(t *testing.T) {
		// Not parallel: the log is global, and CaptureLog replaces it.
		logs := agenttest.CaptureLog(t)

		agent := ai.NewAgent[struct{}, string](agenttest.Answering("hello"))
		result, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{
			OnRunEnd: func(struct{}, *ai.RunInfo, error) { panic("bug in OnRunEnd") },
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Contains(t, logs.String(), "panic in OnRunEnd: bug in OnRunEnd")
	})

	t.Run("the timeout", func(t *testing.T) {
		t.Parallel()

		var recorded ends[struct{}]
		model, _ := blocking()
		agent := ai.NewAgent[struct{}, string](model)
		_, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{
			OnRunEnd: recorded.onRunEnd, Timeout: shortTimeout,
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)

		_, _, got := recorded.only(t)
		require.Equal(t, err, got)
	})

	t.Run("the deadline of the run", func(t *testing.T) {
		t.Parallel()

		for _, test := range []struct {
			name    string
			timeout time.Duration
		}{
			{"none by default", 0},
			{"the timeout", 2 * time.Hour},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()

				model, deadlines := agenttest.DeadlineProbe()
				agent := ai.NewAgent[struct{}, string](model)
				_, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{Timeout: test.timeout})
				require.NoError(t, err)
				agenttest.Receive(t, deadlines, "the model call").Require(t, test.timeout)
			})
		}
	})

	t.Run("the shutdown says why it canceled the run", func(t *testing.T) {
		t.Parallel()

		for _, timeout := range []time.Duration{0, time.Hour} {
			t.Run(timeout.String(), func(t *testing.T) {
				t.Parallel()

				var recorded ends[struct{}]
				model, started := blocking()
				agent := ai.NewAgent[struct{}, string](model)
				shutdown := make(chan struct{})
				go func() {
					<-started
					close(shutdown)
				}()
				_, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{
					OnRunEnd: recorded.onRunEnd, Timeout: timeout, Shutdown: shutdown,
				})
				require.Same(t, agentrun.ErrShutdown, err, "and not that the context was canceled")

				_, _, got := recorded.only(t)
				require.Same(t, agentrun.ErrShutdown, got)
			})
		}
	})

	t.Run("a context that was canceled by someone else", func(t *testing.T) {
		t.Parallel()

		model, started := blocking()
		agent := ai.NewAgent[struct{}, string](model)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-started
			cancel()
		}()
		_, err := agentrun.Execute(ctx, agent, "Hi", nil, agentrun.Config[struct{}]{Shutdown: make(chan struct{})})
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, agentrun.ErrShutdown, "the server did not cancel it")
	})

	t.Run("a shutdown after the run completed", func(t *testing.T) {
		t.Parallel()

		shutdown := make(chan struct{})
		agent := ai.NewAgent[struct{}, string](agenttest.Answering("hello"))
		_, err := agentrun.Execute(context.Background(), agent, "Hi", nil, agentrun.Config[struct{}]{Shutdown: shutdown})
		require.NoError(t, err)
		close(shutdown)
	})

	t.Run("deferred tool calls", func(t *testing.T) {
		t.Parallel()

		agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
		ai.AddSimpleTool(agent, "delete", func(context.Context, struct{}) (string, error) {
			return "deleted", nil
		}, ai.WithApprovalRequired())

		t.Run("a run that ends deferred completes", func(t *testing.T) {
			t.Parallel()

			var recorded ends[struct{}]
			result, err := agentrun.Execute(context.Background(), agent, "Delete it", nil, agentrun.Config[struct{}]{
				OnRunEnd: recorded.onRunEnd,
			})
			require.NoError(t, err)
			require.NotNil(t, result.Deferred(), "the handler decides what that means")

			_, _, got := recorded.only(t)
			require.NoError(t, got)
		})

		t.Run("or fails with the error of the handler", func(t *testing.T) {
			t.Parallel()

			var recorded ends[struct{}]
			cannotResume := errors.New("cannot resume")
			_, err := agentrun.Execute(context.Background(), agent, "Delete it", nil, agentrun.Config[struct{}]{
				OnRunEnd: recorded.onRunEnd, Deferred: cannotResume,
			})
			require.Same(t, cannotResume, err)

			_, _, got := recorded.only(t)
			require.Same(t, cannotResume, got)
		})
	})

	t.Run("the options of the caller", func(t *testing.T) {
		t.Parallel()

		var recorded ends[struct{}]
		agent := ai.NewAgent[struct{}, string](agenttest.Answering("hello"))
		_, err := agentrun.Execute(context.Background(), agent, "Hi",
			[]ai.RunOption{ai.WithRunMetadata(map[string]any{"k": "v"})},
			agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd})
		require.NoError(t, err)

		_, info, _ := recorded.only(t)
		require.NotNil(t, info)
		require.Equal(t, "v", info.Metadata()["k"])
	})
}
