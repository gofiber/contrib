// Package agentrun holds what the handlers of pydanticai and pydanticaimcp do
// around one agent run: they resolve what the run needs from the request, and
// see to it that a panic in the run, in a tool or in a callback of the
// application neither takes the process down nor reaches a client.
package agentrun

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/valyala/fasthttp"
)

// ErrPanicked is what a client is told when the run panics. The panic itself
// goes to the log and to the callbacks of the application.
var ErrPanicked = errors.New("internal error")

// ErrShutdown is why a run was canceled when the server started shutting down.
// It wraps ai.ErrRunCancelled, which the agent treats as a request to stop: a
// streamed run ends with the messages and the usage it has, and a run that is
// not streamed ends with an error that matches ai.ErrRunCancelled.
var ErrShutdown = fmt.Errorf("pydanticai: the server is shutting down: %w", ai.ErrRunCancelled)

// Resolve runs the per-request callbacks of a handler, Deps and then
// RunOptions. Both are optional: the dependencies default to the zero value of
// Deps and there are no extra options.
func Resolve[Deps any](
	c fiber.Ctx,
	deps func(fiber.Ctx) (Deps, error),
	options func(fiber.Ctx) ([]ai.RunOption, error),
) (value Deps, extra []ai.RunOption, err error) {
	if deps != nil {
		if value, err = deps(c); err != nil {
			return value, nil, err
		}
	}
	if options != nil {
		if extra, err = options(c); err != nil {
			return value, nil, err
		}
	}
	return value, extra, nil
}

// ServerDone returns the channel that fasthttp closes when the server of ctx
// starts shutting down, which is before it waits for the connections to finish,
// or nil when ctx has no server, as a RequestCtx that a test or a benchmark
// builds itself: Done of fasthttp dereferences the server.
func ServerDone(ctx *fasthttp.RequestCtx) (done <-chan struct{}) {
	defer func() {
		if recover() != nil {
			done = nil
		}
	}()
	return ctx.Done()
}

// PanicError returns the error that reports a panic in the run, for the logs
// and the callbacks of the application.
func PanicError(recovered any) error {
	// %v and not %w: an error that was panicked with must not turn the panic
	// into a retry or a tool failure.
	return fmt.Errorf("pydanticai: panic in the run: %v", recovered)
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

// Text is the message that a client receives for err. A panic in message is
// logged and recovered, and the client is then told what it is told for a panic
// in the run: not err.Error(), which message is there to keep from it.
func Text(message func(error) string, err error) string {
	if message == nil {
		return err.Error()
	}
	text := ErrPanicked.Error()
	Safely("ErrorMessage", func() { text = message(err) })
	return text
}

// Recovered returns the error that reports a recovered panic, for the logs and
// the callbacks of the application, and logs it with the stack of the goroutine.
// Call it from the deferred function that recovered.
func Recovered(recovered any) error {
	err := PanicError(recovered)
	log.Errorf("%v\n%s", err, debug.Stack())
	return err
}

// Guard is what a handler keeps for one run of the agent. It records the run
// for OnRunEnd, recovers a panic in a tool, and reports the end of the run once.
//
// Only capabilities are given the RunInfo and the tool calls. pydantic-ai-go
// runs the tool calls of a model response concurrently, each on a goroutine of
// its own, where neither the handler nor a recover middleware can catch a
// panic, so the guard is a capability.
type Guard[Deps any] struct {
	info     atomic.Pointer[ai.RunInfo]
	panicked atomic.Pointer[error]
	deps     Deps
	onRunEnd func(deps Deps, run *ai.RunInfo, err error)
	ended    sync.Once
}

// NewGuard returns the guard of a run and the options that install it ahead of
// the other run options. The slice that it returns is a new one, so that the
// slice that RunOptions returned is not written to. onRunEnd is the callback of
// the application, if any.
func NewGuard[Deps any](
	options []ai.RunOption, deps Deps, onRunEnd func(deps Deps, run *ai.RunInfo, err error),
) (*Guard[Deps], []ai.RunOption) {
	guard := &Guard[Deps]{deps: deps, onRunEnd: onRunEnd}
	return guard, append([]ai.RunOption{ai.WithRunCapabilities(guard)}, options...)
}

// Setup implements ai.Capability.
func (*Guard[Deps]) Setup(*ai.CapabilityRegistry) error { return nil }

// WrapRun implements ai.RunWrapper. It records the run, before any BeforeRun
// hook can fail it, and runs it.
func (g *Guard[Deps]) WrapRun(ctx context.Context, run *ai.RunInfo, next ai.RunFunc) (ai.RunOutcome, error) {
	g.info.Store(run)
	return next(ctx)
}

// WrapToolCall implements ai.ToolCallWrapper. It runs on the goroutine of the
// tool. The panic becomes an error that is neither a retry nor a tool failure,
// so the run fails with it, and the guard keeps it, so that the handler reports
// the run as one that panicked.
func (g *Guard[Deps]) WrapToolCall(
	ctx context.Context, _ *ai.RunInfo, call ai.ToolCallPart, next ai.ToolCallFunc,
) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := Recovered(recovered)
			g.panicked.CompareAndSwap(nil, &panicErr)
			result, err = nil, panicErr
		}
	}()
	return next(ctx, call)
}

// Err returns the error of the first panic that a tool had, or nil.
func (g *Guard[Deps]) Err() error {
	if err := g.panicked.Load(); err != nil {
		return *err
	}
	return nil
}

// End reports the end of the run to OnRunEnd, with err as the way it ended and
// the run that the guard recorded, which is nil when the agent did not start.
// It does that once, however often it is called, and only when OnRunEnd is set.
// A panic in OnRunEnd is logged and recovered.
func (g *Guard[Deps]) End(err error) {
	if g.onRunEnd == nil {
		return
	}
	g.ended.Do(func() {
		Safely("OnRunEnd", func() { g.onRunEnd(g.deps, g.info.Load(), err) })
	})
}
