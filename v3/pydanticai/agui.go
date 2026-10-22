package pydanticai

import (
	"encoding/json"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/gofiber/fiber/v3"
)

// AGUIConfig defines the config for the AGUI handler. It is a StreamConfig whose
// Adapter is an agui.Config.
type AGUIConfig[Deps any] = StreamConfig[Deps, agui.Config]

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

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		request, err := admit[Deps, agui.RunAgentInput](c, limits, "AG-UI", cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}
		return stream(c, &cfg, request, aguiProtocol, adapter.RunStream)
	}
}

// aguiProtocol describes the AG-UI events for relay.
var aguiProtocol = protocol[agui.Event]{
	what:   "AG-UI event",
	encode: func(event *agui.Event) ([]byte, error) { return json.Marshal(event) },
	failed: func(event agui.Event) bool { return event.Type == agui.EventRunError },
	withMessage: func(event agui.Event, message string) agui.Event {
		event.Message = message
		return event
	},
	// A canceled run ends with RUN_FINISHED as well, but without an outcome.
	completed: func(event agui.Event) bool { return event.Type == agui.EventRunFinished && event.Outcome != nil },
	// The stream ends with a RUN_ERROR event.
	fail: func(send func([]byte) error, message string) {
		if encoded, err := json.Marshal(agui.Event{Type: agui.EventRunError, Message: message}); err == nil {
			_ = send(encoded)
		}
	},
}
