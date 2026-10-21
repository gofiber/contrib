package pydanticai

import (
	"context"
	"encoding/json"
	"fmt"
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
	// caller that is turned away costs next to nothing.
	//
	// The request stays alive until the run has ended, so the strings that c
	// returns remain valid for the whole run. Do not keep c, or those strings,
	// for work that outlives the run.
	//
	// Optional. Default: the zero value of Deps
	Deps func(c fiber.Ctx) (Deps, error)

	// RunOptions returns additional agent run options for one request, such as
	// ai.WithRunUsageLimits or ai.WithRunMetadata. The adapter sets the message
	// history and the conversation ID itself, whatever the options say.
	//
	// Optional. Default: nil
	RunOptions func(c fiber.Ctx) ([]ai.RunOption, error)

	// ErrorMessage maps the error that failed a run to the message that the
	// client receives in the RUN_ERROR event. Use it to keep provider error
	// details, which can carry request IDs or parts of credentials, away from
	// the browser. It also applies when the run panics or an event cannot be
	// encoded.
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

	// Adapter configures the AG-UI adapter: the protocol version, how untrusted
	// client history is sanitized and the request limits (MaxRequestBytes,
	// AllowedContentTypes and DisableContentTypeCheck).
	//
	// Optional. Default: agui.Config{}
	Adapter agui.Config

	// Timeout bounds the duration of one run. When it elapses the run is
	// canceled and the stream ends, as it does when the client goes away.
	// Neither the cancellation nor the deadline of the context of the request
	// apply to the run, as the handler has returned by the time the run
	// starts. Timeout does.
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
	limits := requestLimits{
		contentTypes:         cfg.Adapter.AllowedContentTypes,
		maxBytes:             cfg.Adapter.MaxRequestBytes,
		skipContentTypeCheck: cfg.Adapter.DisableContentTypeCheck,
	}
	stream := streamOptions[Deps]{
		onClose:          cfg.OnClose,
		timeout:          cfg.Timeout,
		heartbeat:        cfg.HeartbeatInterval,
		disableHeartbeat: cfg.DisableHeartbeat,
	}
	fail := func(send func([]byte) error, err error) {
		failAGUI(send, cfg.ErrorMessage, err)
	}

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		request, err := admit[Deps, agui.RunAgentInput](c, limits, "AG-UI", cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}

		return serveStream(c, stream, request.deps, fail, func(ctx context.Context, send func([]byte) error) error {
			for event, runErr := range adapter.RunStream(ctx, request.input, request.deps, request.options...) {
				if runErr != nil && cfg.ErrorMessage != nil && event.Type == agui.EventRunError {
					event.Message = cfg.ErrorMessage(runErr)
				}
				encoded, err := json.Marshal(event)
				if err != nil {
					err = fmt.Errorf("pydanticai: encode AG-UI event: %w", err)
					fail(send, err)
					return err
				}
				if err := send(encoded); err != nil {
					return err
				}
				if runErr != nil {
					return runErr
				}
			}
			// A canceled run ends the stream without an error, so report why it
			// was canceled.
			return ctx.Err()
		})
	}
}

// failAGUI ends the stream with a RUN_ERROR event for a failure that the
// adapter did not report itself.
func failAGUI(send func([]byte) error, message func(error) string, err error) {
	// Best effort: the stream ends either way.
	if encoded, marshalErr := json.Marshal(agui.Event{Type: agui.EventRunError, Message: errorText(message, err)}); marshalErr == nil {
		_ = send(encoded)
	}
}
