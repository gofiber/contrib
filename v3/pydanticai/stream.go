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

// defaultHeartbeatInterval is how often an idle stream is written to. A client
// that went away is only noticed by the second or third write after it left, so
// a run that nobody waits for is canceled after a few intervals.
const defaultHeartbeatInterval = 5 * time.Second

// errStreamFailed cancels a run whose events can no longer be sent. It wraps
// ai.ErrRunCancelled as well, so the agent stops at its next step.
var errStreamFailed = fmt.Errorf("pydanticai: the stream failed: %w", ai.ErrRunCancelled)

// StreamConfig defines the config for the handlers that stream a run as
// server-sent events, AGUI and Vercel, which take the same settings. A is the
// type of the config of the protocol adapter: AGUIConfig and VercelConfig are
// this type for the adapter of each protocol.
type StreamConfig[Deps, A any] struct {
	// Next defines a function to skip this handler when it returns true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// Deps resolves the dependencies passed to the agent for one request, for
	// example from the user that an earlier middleware stored in c.Locals. An
	// error aborts the request before the stream starts and goes through the
	// Fiber error handler, so return fiber.ErrUnauthorized to turn away an
	// anonymous caller. Deps runs before the body of the request is read, so a
	// caller that is turned away does not get its payload parsed.
	//
	// The request stays alive until the run has ended, so the strings that c
	// returns remain valid for the whole run. Do not keep c, or those strings,
	// for work that outlives the run.
	//
	// Optional. Default: the zero value of Deps
	Deps func(c fiber.Ctx) (Deps, error)

	// RunOptions returns additional agent run options for one request, such as
	// ai.WithRunUsageLimits or ai.WithRunMetadata. The adapter sets the message
	// history and the conversation ID itself, whatever the options say, so a
	// run fails when they contain ai.WithConversation, which pydantic-ai-go
	// does not combine with either.
	//
	// Optional. Default: nil
	RunOptions func(c fiber.Ctx) ([]ai.RunOption, error)

	// ErrorMessage maps the error that failed a run to the message that the
	// client receives in the error event of the protocol: RUN_ERROR for AG-UI
	// and the error chunk for Vercel. Use it to keep provider error details,
	// which can carry request IDs or parts of credentials, away from the
	// browser. It also applies when the run panics or an event cannot be
	// encoded. A panic in ErrorMessage is logged and recovered, and the client
	// then receives "internal error".
	//
	// Optional. Default: err.Error()
	ErrorMessage func(err error) string

	// OnClose is called once when the stream is over, with the dependencies of
	// the run. err is nil when the stream ended normally, which includes a run
	// that a tool canceled. Otherwise it is the agent error when the run
	// failed, the context error when Timeout elapsed, and the write error when
	// the client went away.
	//
	// It runs once the run has ended and the events are written, while the
	// request is still alive. The client has every event by then, but the end of
	// the response and the next request on the connection wait for OnClose, so
	// keep it short, or hand the work to a goroutine and copy what that needs. A
	// panic in OnClose is logged and recovered.
	//
	// Optional. Default: nil
	OnClose func(deps Deps, err error)

	// OnRunEnd is called once when the run has ended, with the dependencies of
	// the run, what the run consumed and how it ended. It is called for every
	// run that the handler started, whether it completed, failed, panicked,
	// timed out or lost its client, which makes it the place to record token
	// usage. err is what OnClose receives. run gives the usage, the messages
	// and the IDs of the run, see ai.RunInfo, and is nil when the agent did not
	// start the run, because the adapter rejected the request history or a run
	// wrapper of the agent answered without starting it.
	//
	// It runs before OnClose, under the same conditions: the events are
	// written and the request is still alive, and the end of the response
	// waits for it, so keep it short, or hand the work to a goroutine and copy
	// what that needs. A panic in OnRunEnd is logged and recovered.
	//
	// Optional. Default: nil
	OnRunEnd func(deps Deps, run *ai.RunInfo, err error)

	// Adapter configures the protocol adapter, an agui.Config for AGUI and a
	// vercel.Config for Vercel: the protocol version, how untrusted client
	// history is sanitized and the request limits (MaxRequestBytes,
	// AllowedContentTypes and DisableContentTypeCheck).
	//
	// Optional. Default: the zero value
	Adapter A

	// Timeout bounds the duration of one run. When it elapses the run is
	// canceled and the stream ends, as it does when the client goes away. A
	// client that stopped reading has its connection closed when a write to it
	// stays blocked for 5 seconds after that. Neither the cancellation nor the
	// deadline of the context of the request apply to the run, as the handler
	// has returned by the time the run starts. Timeout does. A request whose
	// context is already done when the stream would start, because a timeout
	// middleware gave up on it while Deps or RunOptions ran, is not run.
	//
	// Optional. Default: 0 (no timeout)
	Timeout time.Duration

	// HeartbeatInterval is the interval of the comment lines that keep an idle
	// stream open and let the handler notice a client that went away while the
	// agent is busy. A client that left is noticed by the second or third
	// heartbeat after it did, so a run that nobody waits for is canceled after
	// a few intervals. A value less than or equal to zero uses the default.
	//
	// Optional. Default: 5 * time.Second
	HeartbeatInterval time.Duration

	// DisableHeartbeat turns the heartbeat off. A client that goes away while
	// the agent is busy then goes unnoticed until a write to it fails, which
	// can be after the run has ended.
	//
	// Optional. Default: false
	DisableHeartbeat bool

	// CancelOnShutdown cancels the run when the server starts shutting down.
	// The stream then ends like one that timed out, with the end-of-stream event
	// of its protocol, and OnClose receives an error that wraps
	// ai.ErrRunCancelled, or the write error for a client that stopped reading
	// and whose connection is closed after 5 seconds. Without it a shutdown
	// waits for the run to end on its own, until the timeout of the shutdown
	// elapses.
	//
	// Optional. Default: false
	CancelOnShutdown bool

	// grace is how long a write to the client may block once the run was told
	// to end, see streamEndGrace. Zero is streamEndGrace. Tests shorten it.
	grace time.Duration
}

// heartbeatInterval returns the interval to use for the configured one.
func heartbeatInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return defaultHeartbeatInterval
	}
	return interval
}

// streamGuard keeps the events of a run flowing after its consumer stopped
// taking them, as the adapters do after an event that they cannot convert.
// pydantic-ai-go goes on emitting the events of tool calls that run
// concurrently, and an iterator that is resumed after its consumer stopped
// panics on the goroutine of the tool, where nothing recovers. The guard
// cancels the run instead, and drops the rest of its events.
type streamGuard struct{ cancel context.CancelCauseFunc }

// Setup implements ai.Capability.
func (streamGuard) Setup(*ai.CapabilityRegistry) error { return nil }

// WrapRunEventStream implements ai.RunEventStreamWrapper.
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

// protocol describes the events of a streaming protocol for relay.
type protocol[E any] struct {
	// what names an event of the protocol in errors.
	what string
	// encode returns the data of the server-sent event that carries event.
	encode func(event *E) ([]byte, error)
	// failed reports whether event is the event that reports a failed run, and
	// withMessage returns it with message as the text for the client. Only the
	// message changes: the adapter can have set other fields of the event.
	failed      func(event E) bool
	withMessage func(event E, message string) E
	// completed reports whether event ends a run that completed. A run that was
	// canceled ends with an event of its own.
	completed func(event E) bool
	// failure is the event that reports a failed run, for a failure that the
	// adapter did not report itself: withMessage gives it the text for the client.
	failure E
	// closing are the events that close the stream after the event that reports
	// a failure, whoever reported it: the adapter yields none after its own.
	// There are none when that event closes the stream.
	closing []E
}

// emit sends event. Best effort: a stream that fails ends either way.
func (p *protocol[E]) emit(send func(data []byte) error, event *E) {
	if encoded, err := p.encode(event); err == nil {
		_ = send(encoded)
	}
}

// closeStream sends the events that close the stream after a failure.
func (p *protocol[E]) closeStream(send func(data []byte) error) {
	for i := range p.closing {
		p.emit(send, &p.closing[i])
	}
}

// fail ends the stream with the error event of the protocol, which tells the
// client what errorMessage makes of err, and closes it.
func (p *protocol[E]) fail(send func(data []byte) error, errorMessage func(error) string, err error) {
	event := p.withMessage(p.failure, agentrun.Text(errorMessage, err))
	p.emit(send, &event)
	p.closeStream(send)
}

// stream serves an admitted request: it starts the run with runStream, and sends
// its events as server-sent events.
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

// relay sends the events of a run to the client, and returns the error that
// ended the stream: nil when the run completed, and otherwise the error of the
// run, the error of the stream or why the run was canceled. start starts the run
// with the options it is given, and run is its supervision.
//
// When the stream fails, relay stops taking events. pydantic-ai-go goes on
// emitting the events of tool calls that run concurrently from their
// goroutines, and an iterator that is resumed after its loop stopped panics
// there, where nothing recovers. streamGuard cancels the run and takes the
// events that follow.
//
// A panic on this goroutine, in the model or in a callback, ends the stream with
// an error event, as one in a tool does. sse would recover it for OnClose only.
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
		completed bool  // whether the run completed
		// scratch is the event being encoded. encode takes it by address, as
		// json.Marshal boxes what it is given: one copy for the stream, not one
		// for each event.
		scratch E
	)
	for event, runErr := range start(ctx, options...) {
		if runErr != nil {
			if panicErr, panicked := run.Failure(runErr); panicked {
				// A tool panicked.
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
		// A deadline or a shutdown that comes once the run has completed does
		// not fail it.
		return nil
	default:
		// A canceled run ends the stream without an error, so report why it was
		// canceled.
		return context.Cause(ctx)
	}
}
