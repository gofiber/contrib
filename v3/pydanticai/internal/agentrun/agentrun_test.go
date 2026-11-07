package agentrun_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/stretchr/testify/require"
)

// end is one call of OnRunEnd.
type end[Deps any] struct {
	deps Deps
	run  *ai.RunInfo
	err  error
}

// ends records what OnRunEnd receives.
type ends[Deps any] struct {
	mu    sync.Mutex
	calls []end[Deps]
}

func (e *ends[Deps]) onRunEnd(deps Deps, run *ai.RunInfo, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, end[Deps]{deps: deps, run: run, err: err})
}

// only returns the one call of OnRunEnd, and fails the test unless there was one.
func (e *ends[Deps]) only(t *testing.T) (deps Deps, run *ai.RunInfo, err error) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Len(t, e.calls, 1, "OnRunEnd is called once")
	return e.calls[0].deps, e.calls[0].run, e.calls[0].err
}

// newRun starts to supervise a run that ends with the test.
func newRun(t *testing.T, cfg agentrun.Config[struct{}]) *agentrun.Run[struct{}] {
	t.Helper()
	run, _ := agentrun.New(context.Background(), cfg, nil)
	t.Cleanup(run.Cancel)
	return run
}

// execute runs agent as a handler that answers with one response does.
func execute[Deps any](agent *ai.Agent[Deps, string], cfg agentrun.Config[Deps]) (*ai.RunResult[string], error) {
	return agentrun.Execute(context.Background(), agent, "Hi", nil, cfg)
}

// expiry returns a channel that is closed when run expires.
func expiry[Deps any](run *agentrun.Run[Deps]) <-chan struct{} {
	expired := make(chan struct{})
	run.AfterExpiry(func() { close(expired) })
	return expired
}

// fired reports whether the channel is closed.
func fired(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

// recovered returns the error that Recover reports for a panic with value.
func recovered(value any) (err error) {
	defer agentrun.Recover(func(panicErr error) { err = panicErr })
	panic(value)
}

// describedValue is a panic value whose description changes.
type describedValue struct{ text string }

func (d *describedValue) String() string { return d.text }

// shortCircuit is a capability of the caller that answers for the run without
// starting it.
type shortCircuit struct{}

// Setup implements ai.Capability.
func (shortCircuit) Setup(*ai.CapabilityRegistry) error { return nil }

// WrapRun implements ai.RunWrapper.
func (shortCircuit) WrapRun(context.Context, *ai.RunInfo, ai.RunFunc) (ai.RunOutcome, error) {
	return ai.CompletedRunOutcome("answered"), nil
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
		require.EqualError(t, recovered("kaboom"), "pydanticai: panic in the run: kaboom")
		require.Contains(t, logs.String(), "panic in the run: kaboom")
		require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")

		// A panic with an error does not make the error the cause: it would turn
		// the panic into a retry or a failure of a tool.
		cause := errors.New("cause")
		require.NotErrorIs(t, recovered(cause), cause)
	})

	t.Run("the message is made when the panic is recovered", func(t *testing.T) {
		value := &describedValue{text: "before"}
		reported := recovered(value)
		value.text = "after"
		require.EqualError(t, reported, "pydanticai: panic in the run: before",
			"the error keeps what was recovered, and not the value, which may describe itself differently by now")
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
		panicErr := recovered("secret value of the panic")
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
		t.Cleanup(run.Cancel)

		result, err := agent.Run(run.Context(), "Hi", "alice", options...)
		require.NoError(t, err)
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
		run := newRun(t, agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd})

		rejected := errors.New("rejected")
		run.End(rejected)
		_, info, err := recorded.only(t)
		require.Nil(t, info)
		require.Same(t, rejected, err)
	})

	t.Run("without a callback", func(t *testing.T) {
		t.Parallel()

		run := newRun(t, agentrun.Config[struct{}]{})
		require.NotPanics(t, func() { run.End(nil) })
	})

	t.Run("the options of the caller are not written to", func(t *testing.T) {
		t.Parallel()

		options := make([]ai.RunOption, 1, 8) // room to spare, which an append would write into
		options[0] = ai.WithRunMetadata(map[string]any{"k": "v"})
		run, supervised := agentrun.New(context.Background(), agentrun.Config[struct{}]{}, options)
		t.Cleanup(run.Cancel)

		require.Len(t, supervised, 2, "the options of the caller, and the one that supervises the run")
		require.Nil(t, options[:2][1], "the slice of the caller is not written to")
	})

	t.Run("the supervision is ahead of the capabilities of the caller", func(t *testing.T) {
		t.Parallel()

		// A wrapper of the caller that answers for the run does not start it. The
		// supervision would not see the run if it came after the wrapper, and
		// OnRunEnd would not be told what the run consumed.
		var recorded ends[struct{}]
		agent := ai.NewAgent[struct{}, string](agenttest.Answering("not asked"))
		run, options := agentrun.New(context.Background(), agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd},
			[]ai.RunOption{ai.WithRunCapabilities(shortCircuit{})})
		t.Cleanup(run.Cancel)

		result, err := agent.Run(run.Context(), "Hi", struct{}{}, options...)
		require.NoError(t, err)
		require.Equal(t, "answered", result.Output, "the wrapper of the caller answered")

		run.End(nil)
		_, info, _ := recorded.only(t)
		require.NotNil(t, info, "and the supervision saw the run")
	})

	t.Run("a failure that no tool caused", func(t *testing.T) {
		t.Parallel()

		run := newRun(t, agentrun.Config[struct{}]{})
		failure := errors.New("failed")
		final, panicked := run.Failure(failure)
		require.Same(t, failure, final)
		require.False(t, panicked)
		final, panicked = run.Failure(nil)
		require.NoError(t, final)
		require.False(t, panicked)
	})

	t.Run("a panic in a tool", func(t *testing.T) {
		// Not parallel: the log is global, and CaptureLog replaces it.
		logs := agenttest.CaptureLog(t)

		agent := agenttest.ConcurrentAgent("explode", "waiting")
		run, options := agentrun.New(context.Background(), agentrun.Config[struct{}]{}, nil)
		t.Cleanup(run.Cancel)

		// The two tools run on goroutines of their own: without the supervision
		// the panic would end the process.
		_, err := agent.Run(run.Context(), "Hello", struct{}{}, options...)
		require.Error(t, err)
		require.Contains(t, logs.String(), "panic in the run: kaboom")

		// The error of the run carries the value of the panic, or the error of a
		// tool that the panic canceled: what is reported is the panic.
		final, panicked := run.Failure(err)
		require.True(t, panicked)
		require.ErrorContains(t, final, "panic in the run: kaboom")
		final, panicked = run.Failure(nil)
		require.NoError(t, final, "a run that did not fail has no failure to explain")
		require.False(t, panicked)
	})

	t.Run("the context of the run", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			type key struct{}
			parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), key{}, "value"))
			run, _ := agentrun.New(parent, agentrun.Config[struct{}]{Timeout: time.Hour}, nil)
			t.Cleanup(run.Cancel)
			expired := expiry(run)

			require.Equal(t, "value", run.Context().Value(key{}), "the values of the parent are the run's")
			require.NoError(t, run.Context().Err())

			cancelParent()
			synctest.Wait()
			require.Error(t, run.Context().Err(), "the run ends with its parent")
			require.False(t, fired(expired), "its owner canceled it, and the server did not")
		})
	})

	t.Run("a run that its owner canceled", func(t *testing.T) {
		t.Parallel()

		for _, test := range []struct {
			name     string
			timeout  time.Duration
			shutdown bool
		}{
			{"nothing to watch", 0, false},
			{"a timeout", time.Hour, false},
			{"a shutdown", 0, true},
			{"a timeout and a shutdown", time.Hour, true},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()

				synctest.Test(t, func(t *testing.T) {
					shutdown := make(chan struct{})
					cfg := agentrun.Config[struct{}]{Timeout: test.timeout}
					if test.shutdown {
						cfg.Shutdown = shutdown
					}
					run := newRun(t, cfg)
					expired := expiry(run)
					run.Cancel()
					run.Cancel() // more than once is fine
					synctest.Wait()
					require.Error(t, run.Context().Err())
					require.False(t, fired(expired), "its owner canceled it")

					close(shutdown)
					synctest.Wait()
					require.False(t, fired(expired), "a shutdown that comes later is not why the run ended")
					require.NotErrorIs(t, context.Cause(run.Context()), agentrun.ErrShutdown)
				})
			})
		}
	})

	t.Run("a run that timed out", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			run := newRun(t, agentrun.Config[struct{}]{Timeout: time.Minute})
			<-expiry(run) // the clock of the test advances to the deadline
			require.ErrorIs(t, run.Context().Err(), context.DeadlineExceeded)
		})
	})

	t.Run("a run that the shutdown canceled", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			shutdown := make(chan struct{})
			run := newRun(t, agentrun.Config[struct{}]{Timeout: time.Hour, Shutdown: shutdown})
			expired := expiry(run)
			synctest.Wait()
			require.False(t, fired(expired), "the server is not shutting down")

			close(shutdown)
			<-expired
			require.ErrorIs(t, context.Cause(run.Context()), agentrun.ErrShutdown, "with a timeout as well")
		})
	})

	t.Run("a server that is already shutting down", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			shutdown := make(chan struct{})
			close(shutdown)
			run := newRun(t, agentrun.Config[struct{}]{Shutdown: shutdown})
			<-expiry(run)
		})
	})

	t.Run("only a run with a timeout or a shutdown to watch can expire", func(t *testing.T) {
		t.Parallel()

		plain := newRun(t, agentrun.Config[struct{}]{})
		require.False(t, plain.AfterExpiry(func() {})(), "nothing is waiting for the run to expire")

		watching := newRun(t, agentrun.Config[struct{}]{Shutdown: make(chan struct{})})
		require.True(t, watching.AfterExpiry(func() {})(), "a run that watches the shutdown can expire")

		// A call that was stopped is not made, whatever happens to the run.
		synctest.Test(t, func(t *testing.T) {
			bounded := newRun(t, agentrun.Config[struct{}]{Timeout: time.Hour})
			called := make(chan struct{})
			require.True(t, bounded.AfterExpiry(func() { close(called) })(), "the call was waiting, and it is stopped")
			time.Sleep(2 * time.Hour) // the deadline passes
			synctest.Wait()
			require.Error(t, bounded.Context().Err())
			require.False(t, fired(called))
		})
	})

	t.Run("the goroutine that waits for the shutdown ends with the run", func(t *testing.T) {
		t.Parallel()

		// synctest.Test fails the test when a goroutine of the bubble is still
		// blocked once the function returned, which is what a goroutine that does
		// not end with its run is: it keeps waiting for a shutdown that may never
		// come, one for each request.
		synctest.Test(t, func(t *testing.T) {
			shutdown := make(chan struct{})
			for range 100 {
				run, _ := agentrun.New(context.Background(), agentrun.Config[struct{}]{Shutdown: shutdown}, nil)
				run.Cancel()
			}
		})
	})
}

func Test_Execute(t *testing.T) {
	// Not parallel: the subtests that capture the log are not, and they have to
	// run apart from the ones that are.
	t.Run("a run that completes", func(t *testing.T) {
		t.Parallel()

		var recorded ends[string]
		agent := ai.NewAgent[string, string](agenttest.Answering("hello"))
		result, err := execute(agent, agentrun.Config[string]{Deps: "alice", OnRunEnd: recorded.onRunEnd})
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
		result, err := execute(agent, agentrun.Config[struct{}]{})
		require.NoError(t, err)
		require.Equal(t, "hello", strings.TrimSpace(result.Output))
	})

	t.Run("a run that fails", func(t *testing.T) {
		t.Parallel()

		var recorded ends[struct{}]
		rejected := errors.New("the provider rejected the key")
		agent := ai.NewAgent[struct{}, string](agenttest.Failing(rejected))
		result, err := execute(agent, agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd})
		require.ErrorIs(t, err, rejected)
		require.Nil(t, result)

		_, _, got := recorded.only(t)
		require.Equal(t, err, got, "the callback is told what the caller is told")
	})

	t.Run("a panic in a tool outranks the error that it caused", func(t *testing.T) {
		// Not parallel: the log is global, and CaptureLog replaces it.
		agenttest.CaptureLog(t)

		var recorded ends[struct{}]
		_, err := execute(agenttest.ConcurrentAgent("explode", "waiting"), agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd})
		require.ErrorContains(t, err, "panic in the run: kaboom")
		require.Equal(t, agentrun.ErrPanicked.Error(), agentrun.Text(nil, err), "a client is not told what panicked")

		_, _, got := recorded.only(t)
		require.Equal(t, err, got)
	})

	t.Run("the first panic in a tool is the one that is reported", func(t *testing.T) {
		// Not parallel: the log is global, and CaptureLog replaces it.
		logs := agenttest.CaptureLog(t)

		// The failure of the first tool cancels the run, and the second tool panics
		// when it notices.
		_, err := execute(agenttest.ConcurrentAgent("explode", "aftershock"), agentrun.Config[struct{}]{})
		require.ErrorContains(t, err, "panic in the run: kaboom in explode")
		require.NotContains(t, err.Error(), "aftershock")
		require.Contains(t, logs.String(), "kaboom in aftershock", "the second panic is logged all the same")
	})

	t.Run("a panic in the model goes on after it was reported", func(t *testing.T) {
		t.Parallel()

		var recorded ends[struct{}]
		agent := ai.NewAgent[struct{}, string](agenttest.Panicking("model bug"))
		require.PanicsWithValue(t, "model bug", func() {
			_, _ = execute(agent, agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd})
		})

		_, info, err := recorded.only(t)
		require.EqualError(t, err, "pydanticai: panic in the run: model bug")
		require.NotNil(t, info, "the run had started: it consumed what it consumed")
	})

	t.Run("a panic in the model without a callback goes on", func(t *testing.T) {
		t.Parallel()

		agent := ai.NewAgent[struct{}, string](agenttest.Panicking("model bug"))
		require.PanicsWithValue(t, "model bug", func() { _, _ = execute(agent, agentrun.Config[struct{}]{}) })
	})

	t.Run("a panic in OnRunEnd is recovered", func(t *testing.T) {
		// Not parallel: the log is global, and CaptureLog replaces it.
		logs := agenttest.CaptureLog(t)

		agent := ai.NewAgent[struct{}, string](agenttest.Answering("hello"))
		result, err := execute(agent, agentrun.Config[struct{}]{
			OnRunEnd: func(struct{}, *ai.RunInfo, error) { panic("bug in OnRunEnd") },
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Contains(t, logs.String(), "panic in OnRunEnd: bug in OnRunEnd")
	})

	t.Run("the timeout", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			var recorded ends[struct{}]
			model, _, _ := agenttest.Blocking()
			agent := ai.NewAgent[struct{}, string](model)
			_, err := execute(agent, agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd, Timeout: time.Hour})
			require.ErrorIs(t, err, context.DeadlineExceeded)

			_, _, got := recorded.only(t)
			require.Equal(t, err, got)
		})
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
				_, err := execute(ai.NewAgent[struct{}, string](model), agentrun.Config[struct{}]{Timeout: test.timeout})
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
				model, started, _ := agenttest.Blocking()
				shutdown := make(chan struct{})
				go func() {
					<-started
					close(shutdown)
				}()
				_, err := execute(ai.NewAgent[struct{}, string](model), agentrun.Config[struct{}]{
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

		model, started, _ := agenttest.Blocking()
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

	t.Run("what supervises the run ends with it", func(t *testing.T) {
		t.Parallel()

		// As for New: synctest.Test fails the test when the goroutine that waits for
		// the shutdown is still blocked once the run is over.
		synctest.Test(t, func(t *testing.T) {
			agent := ai.NewAgent[struct{}, string](agenttest.Answering("hello"))
			_, err := execute(agent, agentrun.Config[struct{}]{Shutdown: make(chan struct{})})
			require.NoError(t, err)
		})
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
			result, err := execute(agent, agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd})
			require.NoError(t, err)
			require.NotNil(t, result.Deferred(), "the handler decides what that means")

			_, _, got := recorded.only(t)
			require.NoError(t, got)
		})

		t.Run("or fails with the error of the handler", func(t *testing.T) {
			t.Parallel()

			var recorded ends[struct{}]
			cannotResume := errors.New("cannot resume")
			_, err := execute(agent, agentrun.Config[struct{}]{OnRunEnd: recorded.onRunEnd, Deferred: cannotResume})
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
