// Package agentrun supervises one run of an agent for pydanticai: it bounds the
// run, cancels it on shutdown, keeps a panic from reaching the process or a
// client, and reports the end once. Execute serves a handler that answers once,
// New and Run a stream.
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

// errPanicked is what a client is told when the run panics.
var errPanicked = errors.New("internal error")

// errShutdown is why a run was canceled on shutdown. It wraps ai.ErrRunCancelled,
// which stops the agent.
var errShutdown = fmt.Errorf("pydanticai: the server is shutting down: %w", ai.ErrRunCancelled)

const oneShotTimeout = 5 * time.Minute

// OneShotTimeout returns the Timeout of a handler that cannot notice a client that
// left, as Run and MCP: zero is five minutes, negative is none (returned as zero).
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

	// OnRunEnd is called once with the end of the run, see Run.End.
	OnRunEnd func(deps Deps, run *ai.RunInfo, err error)

	// Timeout bounds the run with the context error. Zero is unbounded.
	Timeout time.Duration

	// Shutdown cancels the run when it is closed, with a cause that matches
	// ai.ErrRunCancelled.
	Shutdown <-chan struct{}

	// Deferred is the error that Execute fails a run with when it ends with deferred
	// tool calls. Nil lets it complete.
	Deferred error
}

// Run supervises one run of an agent: it records the run for OnRunEnd, keeps the
// panic of a tool, owns the context and reports the end once.
type Run[Deps any] struct {
	deps     Deps
	onRunEnd func(deps Deps, run *ai.RunInfo, err error)

	info     atomic.Pointer[ai.RunInfo]
	panicked atomic.Pointer[error]
	ended    sync.Once

	ctx     context.Context
	cancel  context.CancelCauseFunc
	stop    context.CancelFunc
	expires bool
}

// New starts to supervise a run and returns the Run and the options that run the
// agent under it, ahead of the others, in a new slice. The run derives its context
// from parent, cancellation included. End the supervision with Cancel.
func New[Deps any](parent context.Context, cfg Config[Deps], options []ai.RunOption) (*Run[Deps], []ai.RunOption) {
	r := &Run[Deps]{deps: cfg.Deps, onRunEnd: cfg.OnRunEnd, expires: cfg.Timeout > 0 || cfg.Shutdown != nil}
	// Each context layer costs the agent, which records every context it is given, so
	// make only the layers that the run needs.
	ctx := parent
	if cfg.Shutdown != nil || cfg.Timeout <= 0 {
		ctx, r.cancel = context.WithCancelCause(parent)
	}
	if cfg.Shutdown != nil {
		go cancelOnShutdown(ctx, r.cancel, cfg.Shutdown)
	}
	if cfg.Timeout > 0 {
		ctx, r.stop = context.WithTimeout(ctx, cfg.Timeout)
	}
	r.ctx = ctx
	return r, append([]ai.RunOption{ai.WithRunCapabilities(capability[Deps]{r})}, options...)
}

func cancelOnShutdown(ctx context.Context, cancel context.CancelCauseFunc, shutdown <-chan struct{}) {
	select {
	case <-shutdown:
		cancel(errShutdown)
	case <-ctx.Done():
	}
}

// Context is the context of the run: canceled, timed out or shut down.
func (r *Run[Deps]) Context() context.Context { return r.ctx }

// AfterExpiry calls fn once, on its own goroutine, when the server ends the run (a
// deadline passed or it shut down), not when the run is canceled. The returned
// function stops the call.
func (r *Run[Deps]) AfterExpiry(fn func()) (stop func() bool) {
	if !r.expires {
		return notStopped
	}
	return context.AfterFunc(r.ctx, func() {
		cause := context.Cause(r.ctx)
		if errors.Is(cause, errShutdown) || errors.Is(cause, context.DeadlineExceeded) {
			fn()
		}
	})
}

func notStopped() bool { return false }

// Failure returns the error that a run which failed with err reports, and whether
// a panic of a tool is why: it outranks the error that it caused.
func (r *Run[Deps]) Failure(err error) (final error, panicked bool) {
	if panicErr := r.panicked.Load(); panicErr != nil && err != nil {
		return *panicErr, true
	}
	return err, false
}

// Cancel cancels the run and releases what supervises it, any number of times.
func (r *Run[Deps]) Cancel() {
	if r.cancel != nil {
		r.cancel(nil)
	}
	if r.stop != nil {
		r.stop()
	}
}

// End reports the end of the run to OnRunEnd with err, once however often it is
// called. A panic in OnRunEnd is logged and recovered.
func (r *Run[Deps]) End(err error) {
	if r.onRunEnd == nil {
		return
	}
	r.ended.Do(func() {
		Safely("OnRunEnd", func() { r.onRunEnd(r.deps, r.info.Load(), err) })
	})
}

// Execute runs agent once under supervision and returns its result and the final
// error, where the first panic of a tool outranks the error it caused. A panic of
// the model or a callback is reported to cfg.OnRunEnd and goes on.
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
	err, _ = run.Failure(err)
	if err == nil && cfg.Deferred != nil && result.Deferred() != nil {
		err = cfg.Deferred
	}
	if err != nil && errors.Is(context.Cause(run.Context()), errShutdown) &&
		(errors.Is(err, ai.ErrRunCancelled) || errors.Is(err, context.Canceled)) {
		err = errShutdown
	}
	run.End(err)
	return result, err
}

type capability[Deps any] struct{ run *Run[Deps] }

func (capability[Deps]) Setup(*ai.CapabilityRegistry) error { return nil }

// WrapRun records the run before any BeforeRun hook can fail it.
func (c capability[Deps]) WrapRun(ctx context.Context, run *ai.RunInfo, next ai.RunFunc) (ai.RunOutcome, error) {
	c.run.info.Store(run)
	return next(ctx)
}

// WrapToolCall turns a panic of the tool into an error that fails the run, which
// the Run keeps for the handler.
func (c capability[Deps]) WrapToolCall(
	ctx context.Context, _ *ai.RunInfo, call ai.ToolCallPart, next ai.ToolCallFunc,
) (result any, err error) {
	defer Recover(func(panicErr error) {
		c.run.panicked.CompareAndSwap(nil, &panicErr)
		result, err = nil, panicErr
	})
	return next(ctx, call)
}

// panicError is the error of a run that panicked, for logs and callbacks only.
type panicError struct{ message string }

// newPanicError keeps neither the value nor an error as the cause, which would
// turn the panic into a retry.
func newPanicError(recovered any) error {
	return &panicError{message: fmt.Sprintf("pydanticai: panic in the run: %v", recovered)}
}

func (e *panicError) Error() string { return e.message }

// Recover is deferred on a goroutine that no recover middleware guards: it logs a
// panic with its stack and reports an error that Text keeps from a client.
func Recover(report func(err error)) {
	if recovered := recover(); recovered != nil {
		err := newPanicError(recovered)
		log.Errorf("%v\n%s", err, debug.Stack())
		report(err)
	}
}

// Safely calls the callback fn and logs a panic instead of ending the process.
func Safely(name string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Errorf("pydanticai: panic in %s: %v\n%s", name, recovered, debug.Stack())
		}
	}()
	fn()
}

// Text is the message that a client receives for err. A panic, or a panic in
// message, is told as "internal error".
func Text(message func(error) string, err error) string {
	if _, panicked := errors.AsType[*panicError](err); panicked {
		err = errPanicked
	}
	if message == nil {
		return err.Error()
	}
	text := errPanicked.Error()
	Safely("ErrorMessage", func() { text = message(err) })
	return text
}
