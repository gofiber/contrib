package pydanticai

import (
	"context"
	"fmt"
	"iter"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
)

const defaultHeartbeatInterval = 5 * time.Second

var errStreamFailed = fmt.Errorf("pydanticai: the stream failed: %w", ai.ErrRunCancelled)

// StreamConfig defines the config of the AGUI and Vercel handlers. A is the type
// of the protocol adapter config, which AGUIConfig and VercelConfig fix.
type StreamConfig[Deps, A any] struct {
	// Next defines a function to skip this handler when it returns true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// Deps resolves the dependencies of the agent for one request, for example from
	// c.Locals. It runs before the body is read, and an error aborts the request
	// through the Fiber error handler: return fiber.ErrUnauthorized to turn away a
	// caller. c is valid until the run ends; do not keep it for longer.
	//
	// Optional. Default: the zero value of Deps
	Deps func(c fiber.Ctx) (Deps, error)

	// RunOptions returns extra agent run options for one request. The adapter sets
	// the history and conversation ID, so ai.WithConversation fails the run.
	//
	// Optional. Default: nil
	RunOptions func(c fiber.Ctx) ([]ai.RunOption, error)

	// ErrorMessage maps the error that failed a run to the message in the error
	// event of the protocol, to keep provider details out of the browser. It also
	// applies to panics and encoding errors.
	//
	// Optional. Default: err.Error()
	ErrorMessage func(err error) string

	// OnClose is called once when the stream is over, with the dependencies of the
	// run. err is nil for a normal end, otherwise the agent error, the context error
	// of Timeout or the write error of a client that left. The end of the response
	// waits for it, so keep it short. A panic in it is logged and recovered.
	//
	// Optional. Default: nil
	OnClose func(deps Deps, err error)

	// OnRunEnd is called once when the run has ended, before OnClose, with what the
	// run consumed and how it ended: the place to record token usage. run is nil if
	// the agent did not start the run. err is what OnClose receives. The response
	// waits for it. A panic in it is logged and recovered.
	//
	// Optional. Default: nil
	OnRunEnd func(deps Deps, run *ai.RunInfo, err error)

	// Adapter configures the protocol adapter, an agui.Config or a vercel.Config.
	//
	// Optional. Default: the zero value
	Adapter A

	// Timeout bounds the duration of one run: when it elapses the run is canceled
	// and the stream ends, and a client that stopped reading has its connection
	// closed 5 seconds later. The deadline of the request context does not apply.
	//
	// Optional. Default: 0 (no timeout)
	Timeout time.Duration

	// HeartbeatInterval is the interval of the comment lines that keep an idle
	// stream open and reveal a client that left. A value <= 0 uses the default.
	//
	// Optional. Default: 5 * time.Second
	HeartbeatInterval time.Duration

	// DisableHeartbeat turns the heartbeat off.
	//
	// Optional. Default: false
	DisableHeartbeat bool

	// CancelOnShutdown cancels the run when the server starts shutting down; the
	// stream then ends like one that timed out. Without it a shutdown waits for the
	// run to end on its own.
	//
	// Optional. Default: false
	CancelOnShutdown bool

	// grace overrides streamEndGrace; tests shorten it.
	grace time.Duration
}

func heartbeatInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return defaultHeartbeatInterval
	}
	return interval
}

// streamGuard cancels the run once its events are no longer consumed and drops
// the rest, as a stopped iterator resumed from a tool goroutine panics.
type streamGuard struct{ cancel context.CancelCauseFunc }

func (streamGuard) Setup(*ai.CapabilityRegistry) error { return nil }

func (g streamGuard) WrapRunEventStream(_ context.Context, _ *ai.RunInfo, stream ai.EventStream) ai.EventStream {
	return func(yield func(ai.StreamEvent, error) bool) {
		stopped := false
		for event, err := range stream {
			if stopped {
				continue
			}
			if !yield(event, err) {
				stopped = true
				g.cancel(errStreamFailed)
			}
		}
	}
}

// protocol describes a streaming protocol for relay: how an event is encoded,
// which events fail or complete a run, and which close the stream after one.
type protocol[E any] struct {
	what        string
	encode      func(event *E) ([]byte, error)
	failed      func(event E) bool
	withMessage func(event E, message string) E
	completed   func(event E) bool
	failure     E
	closing     []E
}

// emit sends event. Best effort: a stream that fails ends either way.
func (p *protocol[E]) emit(send func(data []byte) error, event *E) {
	if encoded, err := p.encode(event); err == nil {
		_ = send(encoded)
	}
}

func (p *protocol[E]) closeStream(send func(data []byte) error) {
	for i := range p.closing {
		p.emit(send, &p.closing[i])
	}
}

// fail sends the error event for err and closes the stream.
func (p *protocol[E]) fail(send func(data []byte) error, errorMessage func(error) string, err error) {
	event := p.withMessage(p.failure, agentrun.Text(errorMessage, err))
	p.emit(send, &event)
	p.closeStream(send)
}

func stream[Deps, A, In, E any](
	c fiber.Ctx,
	cfg *StreamConfig[Deps, A],
	request *admission[Deps, In],
	p *protocol[E],
	runStream func(ctx context.Context, input In, deps Deps, options ...ai.RunOption) iter.Seq2[E, error],
) error {
	return serveStream(c, cfg, request.deps, request.options,
		func(ctx context.Context, run *agentrun.Run[Deps], options []ai.RunOption, send func([]byte) error) error {
			return relay(ctx, p, func(ctx context.Context, options ...ai.RunOption) iter.Seq2[E, error] {
				return runStream(ctx, request.input, request.deps, options...)
			}, options, run, cfg.ErrorMessage, send)
		})
}

// relay sends the events of a run and returns why the stream ended: nil when the
// run completed, else the error of the run or of the stream, or why it was
// canceled. After a failure it stops taking events; streamGuard drops the rest.
func relay[E, Deps any](
	ctx context.Context,
	p *protocol[E],
	start func(ctx context.Context, options ...ai.RunOption) iter.Seq2[E, error],
	options []ai.RunOption,
	run *agentrun.Run[Deps],
	errorMessage func(error) string,
	send func(data []byte) error,
) (ended error) {
	defer agentrun.Recover(func(panicErr error) {
		ended = panicErr
		p.fail(send, errorMessage, panicErr)
	})

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	options = append([]ai.RunOption{ai.WithRunCapabilities(streamGuard{cancel: cancel})}, options...)

	var (
		failed    error // why the stream ended before the run
		completed bool
		// scratch is encoded by address: json.Marshal would copy the value per event.
		scratch E
	)
	for event, runErr := range start(ctx, options...) {
		if runErr != nil {
			if panicErr, panicked := run.Failure(runErr); panicked {
				failed = panicErr
				p.fail(send, errorMessage, panicErr)
				break
			}
			if errorMessage != nil && p.failed(event) {
				event = p.withMessage(event, agentrun.Text(errorMessage, runErr))
			}
		}
		scratch = event
		if encoded, err := p.encode(&scratch); err != nil {
			failed = fmt.Errorf("pydanticai: encode %s: %w", p.what, err)
			p.fail(send, errorMessage, failed)
		} else if err := send(encoded); err != nil {
			failed = err
		} else if runErr != nil {
			// The adapters end the stream after the event of an error.
			failed = runErr
			p.closeStream(send)
		} else if p.completed(event) {
			completed = true
		}
		if failed != nil {
			break
		}
	}
	switch {
	case failed != nil:
		return failed
	case completed:
		// A deadline or a shutdown after the run completed does not fail it.
		return nil
	default:
		// A canceled run ends without an error, so report why it was canceled.
		return context.Cause(ctx)
	}
}
