package pydanticai

import (
	"context"
	"encoding/json"
	"iter"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/gofiber/fiber/v3"
)

// AGUIConfig defines the config for the AGUI handler.
type AGUIConfig[Deps any] struct {
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
	// client receives in the RUN_ERROR event. Use it to keep provider error
	// details, which can carry request IDs or parts of credentials, away from
	// the browser. It also applies when the run panics or an event cannot be
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

	// Adapter configures the AG-UI adapter: the protocol version, how untrusted
	// client history is sanitized and the request limits (MaxRequestBytes,
	// AllowedContentTypes and DisableContentTypeCheck).
	//
	// Optional. Default: agui.Config{}
	Adapter agui.Config

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
}

// AGUI returns a handler that serves agent over the AG-UI protocol.
//
// The handler accepts a POST request with an AG-UI RunAgentInput JSON body and
// answers with the AG-UI events of the run as server-sent events.
//
//	app.Post("/agui", pydanticai.AGUI(agent))
//
// It panics when agent is nil or the adapter config is invalid.
func AGUI[Deps, Output any](agent *ai.Agent[Deps, Output], config ...AGUIConfig[Deps]) fiber.Handler {
	var cfg AGUIConfig[Deps]
	if len(config) > 0 {
		cfg = config[0]
	}
	adapter := agui.NewAdapter(agent, cfg.Adapter)
	limits := newRequestLimits(cfg.Adapter.AllowedContentTypes, cfg.Adapter.MaxRequestBytes, cfg.Adapter.DisableContentTypeCheck)
	stream := streamOptions[Deps]{
		onClose:          cfg.OnClose,
		onRunEnd:         cfg.OnRunEnd,
		timeout:          cfg.Timeout,
		heartbeat:        cfg.HeartbeatInterval,
		disableHeartbeat: cfg.DisableHeartbeat,
		cancelOnShutdown: cfg.CancelOnShutdown,
	}
	// fail ends the stream with a RUN_ERROR event for a failure that the
	// adapter did not report itself. Best effort: the stream ends either way.
	fail := func(send func([]byte) error, err error) {
		event := agui.Event{Type: agui.EventRunError, Message: errorText(cfg.ErrorMessage, err)}
		if encoded, marshalErr := json.Marshal(event); marshalErr == nil {
			_ = send(encoded)
		}
	}

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		request, err := admit[Deps, agui.RunAgentInput](c, limits, "AG-UI", cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}

		options, tracker := trackRun(request.options, cfg.OnRunEnd != nil)
		run := func(ctx context.Context, options ...ai.RunOption) iter.Seq2[agui.Event, error] {
			return adapter.RunStream(ctx, request.input, request.deps, options...)
		}
		return serveStream(c, stream, request.deps, tracker, fail, func(ctx context.Context, send func([]byte) error) error {
			return relay(ctx, aguiProtocol, run, options, cfg.ErrorMessage, send, fail)
		})
	}
}

// aguiProtocol describes the AG-UI events for relay.
var aguiProtocol = protocol[agui.Event]{
	what:   "AG-UI event",
	encode: func(event agui.Event) ([]byte, error) { return json.Marshal(event) },
	failed: func(event agui.Event) bool { return event.Type == agui.EventRunError },
	withMessage: func(event agui.Event, message string) agui.Event {
		event.Message = message
		return event
	},
	// A canceled run ends with RUN_FINISHED as well, but without an outcome.
	completed: func(event agui.Event) bool { return event.Type == agui.EventRunFinished && event.Outcome != nil },
}
