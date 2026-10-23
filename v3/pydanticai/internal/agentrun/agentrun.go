// Package agentrun supervises one run of an agent for the handlers of
// pydanticai. It bounds the run, cancels it when the server shuts down, keeps a
// panic in a tool or in a callback of the application from taking the process
// down or reaching a client, and reports the end of the run once.
//
// A handler that answers with one response gives its run to Execute. A handler
// that streams keeps the run beyond the handler, so it takes the pieces: New
// makes the Run, and the handler cancels it and reports its end when the
// response is over.
package agentrun

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3/log"
)

// ErrPanicked is what a client is told when the run panics. The panic itself
// goes to the log and to the callbacks of the application.
var ErrPanicked = errors.New("internal error")

// ErrShutdown is why a run was canceled when the server started shutting down.
// It wraps ai.ErrRunCancelled, which the agent treats as a request to stop: a
// streamed run ends with the messages and the usage it has, and a run that is
// not streamed ends with an error that matches ai.ErrRunCancelled.
var ErrShutdown = fmt.Errorf("pydanticai: the server is shutting down: %w", ai.ErrRunCancelled)

// oneShotTimeout bounds the run of a handler that cannot notice that its client
// left, unless it is configured otherwise.
const oneShotTimeout = 5 * time.Minute

// OneShotTimeout returns how long a run may last for the configured Timeout of a
// handler that cannot notice that its client left, as Run and the MCP handler:
// zero is five minutes, and a negative value turns the bound off, which is a
// returned zero. A handler that streams notices a client that left through its
// heartbeat, and leaves a run unbounded unless it is configured.
func OneShotTimeout(configured time.Duration) time.Duration {
	switch {
	case configured == 0:
		return oneShotTimeout
	case configured < 0:
		return 0
	default:
		return configured
	}
}

// Config configures the supervision of one run.
type Config[Deps any] struct {
	// Deps are the dependencies of the run, which OnRunEnd receives.
	Deps Deps

	// OnRunEnd is called once with the end of the run, see Run.End. It is
	// optional.
	OnRunEnd func(deps Deps, run *ai.RunInfo, err error)

	// Timeout bounds the run: it is canceled with the context error when the
	// timeout elapses. Zero leaves it unbounded, see OneShotTimeout.
	Timeout time.Duration

	// Shutdown cancels the run with ErrShutdown when it is closed. It is the seam
	// of the server: a handler gives the channel that fasthttp closes when the
	// server starts shutting down, and a test any channel. Nil leaves the run
	// alone.
	Shutdown <-chan struct{}

	// Deferred is the error that Execute fails a run with when the run ends with
	// deferred tool calls, which the handler cannot resume. Nil lets such a run
	// complete.
	Deferred error
}

// Run is the supervision of one run of an agent. It records the run for
// OnRunEnd, remembers a panic in a tool, owns the context of the run and reports
// its end once.
//
// pydantic-ai-go runs the tool calls of a model response concurrently, each on
// a goroutine of its own, where neither the handler nor a recover middleware
// can catch a panic. Only capabilities are given the RunInfo and the tool
// calls, so a Run is installed as one, through the options that New returns.
type Run[Deps any] struct {
	deps     Deps
	onRunEnd func(deps Deps, run *ai.RunInfo, err error)

	info     atomic.Pointer[ai.RunInfo]
	panicked atomic.Pointer[error]
	ended    sync.Once

	ctx    context.Context
	cancel context.CancelCauseFunc
	stop   context.CancelFunc
	// expires is whether the run has a timeout or a shutdown to watch: only such
	// a run can be ended by the server.
	expires bool
}

// New starts to supervise a run. It returns the Run and the options that run the
// agent under it, which are ahead of the other run options. The slice that it
// returns is a new one, so that the slice that the caller made is not written
// to. parent is the context that the run derives its own from: its values are
// the run's, and its cancellation too, so a handler that wants the run to
// outlive the request passes a context without it.
//
// The caller has to end the supervision with Cancel, which releases the timer
// and the goroutine that waits for the shutdown.
func New[Deps any](parent context.Context, cfg Config[Deps], options []ai.RunOption) (*Run[Deps], []ai.RunOption) {
	r := &Run[Deps]{deps: cfg.Deps, onRunEnd: cfg.OnRunEnd, expires: cfg.Timeout > 0 || cfg.Shutdown != nil}
	ctx, cancel := context.WithCancelCause(parent)
	r.cancel = cancel
	r.ctx = ctx
	if cfg.Shutdown != nil {
		go func() { // waits for ctx, not for the context that Timeout makes
			select {
			case <-cfg.Shutdown:
				cancel(ErrShutdown)
			case <-ctx.Done():
			}
		}()
	}
	if cfg.Timeout > 0 {
		r.ctx, r.stop = context.WithTimeout(ctx, cfg.Timeout)
	}
	return r, append([]ai.RunOption{ai.WithRunCapabilities(capability[Deps]{r})}, options...)
}

// Context is the context of the run: it ends when the run is canceled, when its
// timeout elapses and when the server starts shutting down, if the run is
// supervised for that.
func (r *Run[Deps]) Context() context.Context { return r.ctx }

// AfterExpiry arranges for fn to be called once, on a goroutine of its own, when
// the server ends the run: its timeout elapsed, which a deadline of the context
// that New was given counts as, or the server started shutting down. A run that
// its owner or its client canceled has not expired, and a run that has neither a
// timeout nor a shutdown to watch never does, so fn is not called for it. The
// returned function stops the call, as the one of context.AfterFunc does.
func (r *Run[Deps]) AfterExpiry(fn func()) (stop func() bool) {
	if !r.expires {
		return func() bool { return false }
	}
	return context.AfterFunc(r.ctx, func() {
		cause := context.Cause(r.ctx)
		if errors.Is(cause, ErrShutdown) || errors.Is(cause, context.DeadlineExceeded) {
			fn()
		}
	})
}

// Panic returns the error of the first panic that a tool had, or nil.
func (r *Run[Deps]) Panic() error {
	if err := r.panicked.Load(); err != nil {
		return *err
	}
	return nil
}

// Cancel cancels the run, and releases what supervises it. It can be called
// more than once.
func (r *Run[Deps]) Cancel() {
	r.cancel(nil)
	if r.stop != nil {
		r.stop()
	}
}

// End reports the end of the run to OnRunEnd, with err as the way it ended and
// the run that the agent started, which is nil when it did not. It does that
// once, however often it is called, and only when OnRunEnd is set. A panic in
// OnRunEnd is logged and recovered.
func (r *Run[Deps]) End(err error) {
	if r.onRunEnd == nil {
		return
	}
	r.ended.Do(func() {
		Safely("OnRunEnd", func() { r.onRunEnd(r.deps, r.info.Load(), err) })
	})
}

// Execute runs agent once under supervision and returns its result and the error
// that ended it. The error is final: the first panic of a tool replaces the error
// that it caused, a run that ends deferred fails with cfg.Deferred, and a run that
// the shutdown canceled fails with ErrShutdown. The end of the run is reported to
// cfg.OnRunEnd with the same error, once.
//
// A panic of the model or of a callback on this goroutine is reported to
// cfg.OnRunEnd and goes on: the caller decides what that means.
func Execute[Deps, Output any](
	ctx context.Context,
	agent *ai.Agent[Deps, Output],
	prompt string,
	options []ai.RunOption,
	cfg Config[Deps],
) (result *ai.RunResult[Output], err error) {
	run, options := New(ctx, cfg, options)
	defer run.Cancel()
	if cfg.OnRunEnd != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				run.End(newPanicError(recovered))
				panic(recovered)
			}
		}()
	}

	result, err = agent.Run(run.Context(), prompt, cfg.Deps, options...)
	if panicErr := run.Panic(); panicErr != nil && err != nil {
		// The error of the run carries the value of the panic, or the error of a
		// tool that the panic canceled.
		err = panicErr
	}
	if err == nil && cfg.Deferred != nil && result.Deferred() != nil {
		err = cfg.Deferred
	}
	if err != nil && errors.Is(context.Cause(run.Context()), ErrShutdown) &&
		(errors.Is(err, ai.ErrRunCancelled) || errors.Is(err, context.Canceled)) {
		err = ErrShutdown // say why the run was canceled
	}
	run.End(err)
	return result, err
}

// capability installs a Run in the agent.
type capability[Deps any] struct{ run *Run[Deps] }

// Setup implements ai.Capability.
func (capability[Deps]) Setup(*ai.CapabilityRegistry) error { return nil }

// WrapRun implements ai.RunWrapper. It records the run, before any BeforeRun
// hook can fail it, and runs it.
func (c capability[Deps]) WrapRun(ctx context.Context, run *ai.RunInfo, next ai.RunFunc) (ai.RunOutcome, error) {
	c.run.info.Store(run)
	return next(ctx)
}

// WrapToolCall implements ai.ToolCallWrapper. It runs on the goroutine of the
// tool. The panic becomes an error that is neither a retry nor a tool failure,
// so the run fails with it, and the Run keeps it, so that the handler reports
// the run as one that panicked.
func (c capability[Deps]) WrapToolCall(
	ctx context.Context, _ *ai.RunInfo, call ai.ToolCallPart, next ai.ToolCallFunc,
) (result any, err error) {
	defer Recover(func(panicErr error) {
		c.run.panicked.CompareAndSwap(nil, &panicErr)
		result, err = nil, panicErr
	})
	return next(ctx, call)
}

// panicError is the error of a run that panicked, for the logs and the
// callbacks of the application. A client is never told what is in it, see Text.
type panicError struct{ message string }

// newPanicError describes the panic as it is now, and keeps neither the value,
// which a request or a large object can be behind and whose String method can
// run on any goroutine that logs the error, nor an error that was panicked with
// as its cause: that would turn the panic into a retry or a tool failure.
func newPanicError(recovered any) error {
	return &panicError{message: fmt.Sprintf("pydanticai: panic in the run: %v", recovered)}
}

func (e *panicError) Error() string { return e.message }

// Recover is deferred by code that runs on a goroutine where no recover
// middleware guards it. A panic is logged with the stack of the goroutine, and
// report is called with its error, which Text keeps from a client. Without a
// panic it does nothing.
//
//	defer agentrun.Recover(func(err error) { ... })
func Recover(report func(err error)) {
	if recovered := recover(); recovered != nil {
		err := newPanicError(recovered)
		log.Errorf("%v\n%s", err, debug.Stack())
		report(err)
	}
}

// Safely calls fn, a callback of the application, on a goroutine where no
// recover middleware guards it. A panic is logged instead of ending the process.
func Safely(name string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Errorf("pydanticai: panic in %s: %v\n%s", name, recovered, debug.Stack())
		}
	}()
	fn()
}

// Text is the message that a client receives for err. The error of a run that
// panicked is told as ErrPanicked, and never as what it holds. A panic in
// message is logged and recovered, and the client is then told what it is told
// for a panic in the run: not err.Error(), which message is there to keep from
// it.
func Text(message func(error) string, err error) string {
	if _, panicked := errors.AsType[*panicError](err); panicked {
		err = ErrPanicked
	}
	if message == nil {
		return err.Error()
	}
	text := ErrPanicked.Error()
	Safely("ErrorMessage", func() { text = message(err) })
	return text
}
