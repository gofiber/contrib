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
	// anonymous caller.
	//
	// The stream outlives the handler, so copy what the dependencies need out
	// of c instead of keeping c.
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
	// the browser.
	//
	// Optional. Default: err.Error()
	ErrorMessage func(err error) string

	// OnClose is called once when the stream has ended. err is nil when the run
	// completed, the agent error when the run failed, and the write error when
	// the client went away.
	//
	// The stream outlives the handler, so do not use c after OnClose returns.
	//
	// Optional. Default: nil
	OnClose func(c fiber.Ctx, err error)

	// Adapter configures the AG-UI adapter: the protocol version, how untrusted
	// client history is sanitized and the request limits (MaxRequestBytes,
	// AllowedContentTypes and DisableContentTypeCheck).
	//
	// Optional. Default: agui.Config{}
	Adapter agui.Config

	// Timeout bounds the duration of one run. When it elapses the run is
	// canceled and the stream ends, as it does when the client goes away.
	//
	// Optional. Default: 0 (no timeout)
	Timeout time.Duration

	// HeartbeatInterval is the interval of the comment lines that keep an idle
	// stream open and let the handler notice a client that went away while the
	// agent is busy. A value less than or equal to zero uses the default.
	//
	// Optional. Default: 15 * time.Second
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
	limits := newRequestLimits(
		cfg.Adapter.MaxRequestBytes, cfg.Adapter.AllowedContentTypes, cfg.Adapter.DisableContentTypeCheck,
	)
	stream := streamOptions{
		onClose:          cfg.OnClose,
		timeout:          cfg.Timeout,
		heartbeat:        cfg.HeartbeatInterval,
		disableHeartbeat: cfg.DisableHeartbeat,
	}

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		body, err := limits.body(c)
		if err != nil {
			return err
		}
		// Resolving the dependencies comes first, so that a caller that is
		// turned away does not get its payload parsed.
		deps, options, err := resolve(c, cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}
		var input agui.RunAgentInput
		if err := decodeJSON(body, &input, "AG-UI"); err != nil {
			return err
		}

		return serveStream(c, stream, func(ctx context.Context, send func([]byte) error) error {
			for event, runErr := range adapter.RunStream(ctx, input, deps, options...) {
				if runErr != nil && cfg.ErrorMessage != nil && event.Type == agui.EventRunError {
					event.Message = cfg.ErrorMessage(runErr)
				}
				encoded, err := json.Marshal(event)
				if err != nil {
					return failAGUI(send, cfg.ErrorMessage, fmt.Errorf("pydanticai: encode AG-UI event: %w", err))
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

// failAGUI ends the stream with a RUN_ERROR event for an event that could not
// be encoded. It returns err for OnClose.
func failAGUI(send func([]byte) error, message func(error) string, err error) error {
	text := err.Error()
	if message != nil {
		text = message(err)
	}
	// Best effort: the stream ends either way.
	if encoded, marshalErr := json.Marshal(agui.Event{Type: agui.EventRunError, Message: text}); marshalErr == nil {
		_ = send(encoded)
	}
	return err
}
