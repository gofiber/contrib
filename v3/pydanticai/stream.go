package pydanticai

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"github.com/gofiber/utils/v2"
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
	// fail ends the stream with message as the text for the client, for a
	// failure that the adapter did not report itself. Best effort: the stream
	// ends either way.
	fail func(send func(data []byte) error, message string)
}

// stream serves an admitted request: it starts the run with run, and sends its
// events as server-sent events.
func stream[Deps, A, In, E any](
	c fiber.Ctx,
	cfg *StreamConfig[Deps, A],
	request *admission[Deps, In],
	p protocol[E],
	run func(ctx context.Context, input In, deps Deps, options ...ai.RunOption) iter.Seq2[E, error],
) error {
	guard, options := agentrun.NewGuard(request.options, request.deps, cfg.OnRunEnd)
	return serveStream(c, cfg, request.deps, guard, func(ctx context.Context, send func([]byte) error) error {
		return relay(ctx, p, func(ctx context.Context, options ...ai.RunOption) iter.Seq2[E, error] {
			return run(ctx, request.input, request.deps, options...)
		}, options, guard, cfg.ErrorMessage, send)
	})
}

// relay sends the events of a run to the client, and returns the error that
// ended the stream: nil when the run completed, and otherwise the error of the
// run, the error of the stream or why the run was canceled. run starts the run
// with the options it is given.
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
	p protocol[E],
	run func(ctx context.Context, options ...ai.RunOption) iter.Seq2[E, error],
	options []ai.RunOption,
	guard *agentrun.Guard[Deps],
	errorMessage func(error) string,
	send func(data []byte) error,
) (ended error) {
	// fail ends the stream with the error event of the protocol.
	fail := func(err error) { p.fail(send, agentrun.Text(errorMessage, err)) }
	defer func() {
		if recovered := recover(); recovered != nil {
			ended = agentrun.Recovered(recovered)
			fail(agentrun.ErrPanicked)
		}
	}()

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
	for event, runErr := range run(ctx, options...) {
		if runErr != nil {
			if panicErr := guard.Err(); panicErr != nil {
				// A tool panicked. The error of the run carries the value of the
				// panic, or the error of a tool that the panic canceled.
				failed = panicErr
				fail(agentrun.ErrPanicked)
				break
			}
			if errorMessage != nil && p.failed(event) {
				event = p.withMessage(event, agentrun.Text(errorMessage, runErr))
			}
		}
		scratch = event
		if encoded, err := p.encode(&scratch); err != nil {
			failed = fmt.Errorf("pydanticai: encode %s: %w", p.what, err)
			fail(failed)
		} else if err := send(encoded); err != nil {
			failed = err
		} else if runErr != nil {
			// The adapters end the stream after the event of an error.
			failed = runErr
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

// serveStream sends what run writes as a server-sent events response.
//
// The context passed to run is canceled when a write to the client fails, when
// a heartbeat detects that the client went away, when fasthttp gives up on the
// response and when the stream ends. fasthttp does not cancel the request
// context for a disconnected client, so these are the signals that stop a run
// nobody is listening to anymore. The cancellation of the context of the
// request is not among them, see requestHold.
//
// OnRunEnd and then OnClose are called after the response is over and the run
// has ended. guard is the one that recorded the run.
func serveStream[Deps, A any](
	c fiber.Ctx,
	opts *StreamConfig[Deps, A],
	deps Deps,
	guard *agentrun.Guard[Deps],
	run func(ctx context.Context, send func(data []byte) error) error,
) error {
	// The context of the request is only done before the handler returns when
	// a middleware gave up on the request, as the timeout middleware of Fiber
	// does when its deadline passes while Deps or RunOptions run. It answered
	// for the handler, and fasthttp never writes the response that the stream
	// would be sent with, so the run would never end.
	if err := context.Cause(c.Context()); err != nil {
		return err
	}

	// The run outlives the handler, but a middleware can scope the context of
	// the request to the handler and cancel it when the handler returns, as the
	// timeout middleware of Fiber does. That must not cancel the run, which has
	// only just started. The values of the context are kept.
	runCtx, abort := context.WithCancelCause(context.WithoutCancel(c.Context()))

	// ended receives how the stream ended, once the run has returned.
	ended := make(chan error, 1)
	hold := &requestHold{
		Ctx:   c,
		ctx:   runCtx,
		conn:  c.RequestCtx().Conn(),
		grace: cmp.Or(opts.grace, streamEndGrace),
		release: sync.OnceFunc(func() {
			abort(nil)
			err := <-ended
			guard.End(err)
			if opts.OnClose != nil {
				agentrun.Safely("OnClose", func() { opts.OnClose(deps, err) })
			}
		}),
	}

	// fasthttp closes this channel when the server starts shutting down, which
	// is before it waits for the connections to finish.
	if opts.CancelOnShutdown {
		if shutdown := agentrun.ServerDone(c.RequestCtx()); shutdown != nil {
			go func() {
				select {
				case <-shutdown:
					abort(agentrun.ErrShutdown)
					hold.expire()
				case <-runCtx.Done():
				}
			}()
		}
	}

	config := sse.Config{
		HeartbeatInterval: heartbeatInterval(opts.HeartbeatInterval),
		DisableHeartbeat:  opts.DisableHeartbeat,
		Handler: func(_ fiber.Ctx, stream *sse.Stream) error {
			ctx := stream.Context()
			if opts.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
				defer cancel()
				// The run ends when its deadline passes, unless it is stuck in a
				// write to a client that stopped reading.
				defer context.AfterFunc(ctx, func() {
					if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
						hold.expire()
					}
				})()
			}
			return run(ctx, func(data []byte) error {
				// sse copies a []byte into a string before it writes it, and
				// writes a string as it is. The event is written when Event
				// returns, and nothing changes data until then.
				return stream.Event(sse.Event{Data: utils.UnsafeString(data)})
			})
		},
		OnClose: func(_ fiber.Ctx, err error) { ended <- err },
	}
	return sse.New(config)(hold)
}
