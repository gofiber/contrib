// Package agentrun holds what the handlers of pydanticai and pydanticaimcp do
// around one agent run, so that a panic in the run, in a tool or in a callback
// of the application neither takes the process down nor reaches a client.
package agentrun

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3/log"
)

// ErrPanicked is what a client is told when the run panics. The panic itself
// goes to the log and to the callbacks of the application.
var ErrPanicked = errors.New("internal error")

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

// Tracker records the ai.RunInfo of a run for OnRunEnd. Only capabilities are
// given the RunInfo, so the tracker is a capability that starts with the run.
// It wraps the run, so it is given the run before any BeforeRun hook can fail it.
type Tracker struct{ info atomic.Pointer[ai.RunInfo] }

// Setup implements ai.Capability.
func (*Tracker) Setup(*ai.CapabilityRegistry) error { return nil }

// WrapRun implements ai.RunWrapper. It records the run and runs it.
func (t *Tracker) WrapRun(ctx context.Context, run *ai.RunInfo, next ai.RunFunc) (ai.RunOutcome, error) {
	t.info.Store(run)
	return next(ctx)
}

// Track adds the run capability that records the run to options when wanted.
// The returned tracker is nil otherwise, and nothing asks it for the run.
func Track(options []ai.RunOption, wanted bool) ([]ai.RunOption, *Tracker) {
	if !wanted {
		return options, nil
	}
	tracker := new(Tracker)
	// A new slice, so that the slice that RunOptions returned is not written to.
	return append([]ai.RunOption{ai.WithRunCapabilities(tracker)}, options...), tracker
}

// Run returns the recorded run, or nil when the agent did not start.
func (t *Tracker) Run() *ai.RunInfo { return t.info.Load() }

// ToolGuard recovers a panic in a tool. pydantic-ai-go runs the tool calls of a
// model response concurrently, each on a goroutine of its own, where neither
// the handler nor a recover middleware can catch a panic. The guard turns the
// panic into an error that fails the run, and keeps it, so that the handler
// reports the run as one that panicked.
type ToolGuard struct{ panicked atomic.Pointer[error] }

// GuardTools adds a ToolGuard to options, ahead of the other run capabilities.
// The returned slice is a new one.
func GuardTools(options []ai.RunOption) ([]ai.RunOption, *ToolGuard) {
	guard := new(ToolGuard)
	return append([]ai.RunOption{ai.WithRunCapabilities(guard)}, options...), guard
}

// Setup implements ai.Capability.
func (*ToolGuard) Setup(*ai.CapabilityRegistry) error { return nil }

// WrapToolCall implements ai.ToolCallWrapper. It runs on the goroutine of the
// tool. The error that it returns for a panic is neither a retry nor a tool
// failure, so the run fails with it.
func (g *ToolGuard) WrapToolCall(
	ctx context.Context, _ *ai.RunInfo, call ai.ToolCallPart, next ai.ToolCallFunc,
) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := PanicError(recovered)
			log.Errorf("%v\n%s", panicErr, debug.Stack())
			g.panicked.CompareAndSwap(nil, &panicErr)
			result, err = nil, panicErr
		}
	}()
	return next(ctx, call)
}

// Err returns the error of the first panic that the guard recovered, or nil.
func (g *ToolGuard) Err() error {
	if err := g.panicked.Load(); err != nil {
		return *err
	}
	return nil
}
