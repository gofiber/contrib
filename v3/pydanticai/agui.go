// Package pydanticai provides Fiber v3 handlers for agents built with
// pydantic-ai-go: AGUI and Vercel stream a run to a chat frontend, Run answers
// with one JSON response and MCP serves the agent as an MCP server. The
// dependencies of the agent come from the Fiber context of each request.
package pydanticai

import (
	"encoding/json"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/gofiber/fiber/v3"
)

// AGUIConfig is the StreamConfig of AGUI, with an agui.Config as Adapter.
type AGUIConfig[Deps any] = StreamConfig[Deps, agui.Config]

// AGUI returns a handler that serves agent over the AG-UI protocol: a POST with a
// RunAgentInput body is answered with the events of the run as server-sent
// events. It panics when agent is nil or the adapter config is invalid.
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
		return stream(c, &cfg, request, &aguiProtocol, adapter.RunStream)
	}
}

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
	// The RUN_ERROR event closes the stream, so nothing follows it.
	failure: agui.Event{Type: agui.EventRunError},
}
