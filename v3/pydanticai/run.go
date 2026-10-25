package pydanticai

import (
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
)

// RunConfig defines the config for the Run handler.
type RunConfig[Deps, Output any] struct {
	// Next defines a function to skip this handler when it returns true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// Deps resolves the dependencies of the agent for one request, for example from
	// c.Locals. It runs before the body is read, and an error is returned as it is:
	// return fiber.ErrUnauthorized to turn away a caller.
	//
	// Optional. Default: the zero value of Deps
	Deps func(c fiber.Ctx) (Deps, error)

	// RunOptions returns additional agent run options for one request, after the
	// ai.WithConversationID of the body. ai.WithConversation is rejected with 500
	// when the body has an ID.
	//
	// Optional. Default: nil
	RunOptions func(c fiber.Ctx) ([]ai.RunOption, error)

	// Prompt reads the user prompt from the request; an error is returned as it is.
	// With it the handler applies none of its request checks, so the route needs its
	// own protection against cross-site requests and GET, which would run the agent.
	//
	// Optional. Default: the "prompt" field of a POST application/json body; the
	// optional "conversation_id" field becomes the conversation ID of the run.
	Prompt func(c fiber.Ctx) (string, error)

	// Respond writes the response for a completed run, also one with deferred tool
	// calls (ai.RunResult.Deferred).
	//
	// Optional. Default: JSON {"output", "run_id", "conversation_id"}; 501 if deferred
	Respond func(c fiber.Ctx, result *ai.RunResult[Output]) error

	// OnRunEnd is called once the agent has run, with what the run consumed and the
	// error that ended it: the place to record token usage. run is nil if the agent
	// did not start the run. A panic in the model or a callback goes on to the app
	// after it, one in a tool fails with 500.
	//
	// Optional. Default: nil
	OnRunEnd func(deps Deps, run *ai.RunInfo, err error)

	// MaxRequestBytes bounds the body that the default Prompt reads. The BodyLimit of
	// the app applies too (the smaller wins), unless StreamRequestBody is enabled.
	//
	// Optional. Default: 10 MiB
	MaxRequestBytes int64

	// Timeout bounds the duration of one run, which fails with 504 Gateway Timeout.
	// A negative value turns it off.
	//
	// Optional. Default: 5 * time.Minute
	Timeout time.Duration
}

type runRequest struct {
	Prompt         string `json:"prompt"`
	ConversationID string `json:"conversation_id"`
}

type runResponse[Output any] struct {
	Output         Output `json:"output"`
	RunID          string `json:"run_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// Run returns a handler that runs agent once per request and answers with its
// output as JSON (see Prompt for the request). A failed run returns an *Error
// with the status that StatusCode derives. It panics when agent is nil or the
// config is invalid.
func Run[Deps, Output any](agent *ai.Agent[Deps, Output], config ...RunConfig[Deps, Output]) fiber.Handler {
	if agent == nil {
		panic("pydanticai: agent must not be nil")
	}
	var cfg RunConfig[Deps, Output]
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.MaxRequestBytes < 0 {
		panic("pydanticai: maximum request bytes must not be negative")
	}
	limits := newRequestLimits(nil, cfg.MaxRequestBytes, false)

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		var (
			prompt  string
			deps    Deps
			options []ai.RunOption
		)
		if cfg.Prompt != nil {
			var err error
			if deps, options, err = resolve(c, cfg.Deps, cfg.RunOptions); err != nil {
				return err
			}
			if prompt, err = cfg.Prompt(c); err != nil {
				return err
			}
		} else {
			request, err := admit[Deps, runRequest](c, limits, "run", cfg.Deps, cfg.RunOptions)
			if err != nil {
				return err
			}
			if request.input.Prompt == "" {
				return fiber.NewError(fiber.StatusBadRequest, "prompt is required")
			}
			prompt, deps, options = request.input.Prompt, request.deps, request.options
			if id := request.input.ConversationID; id != "" {
				options = append([]ai.RunOption{ai.WithConversationID(id)}, options...)
			}
		}

		// A panic of the model or a callback goes on, after OnRunEnd.
		result, err := agentrun.Execute(c.Context(), agent, prompt, options, agentrun.Config[Deps]{
			Deps:     deps,
			OnRunEnd: cfg.OnRunEnd,
			Timeout:  agentrun.OneShotTimeout(cfg.Timeout),
		})
		if err != nil {
			return newError(err)
		}
		if cfg.Respond != nil {
			return cfg.Respond(c, result)
		}
		return respondJSON(c, result)
	}
}

func respondJSON[Output any](c fiber.Ctx, result *ai.RunResult[Output]) error {
	if result.Deferred() != nil {
		return newError(errDeferred)
	}
	return c.JSON(runResponse[Output]{
		Output:         result.Output,
		RunID:          result.RunID(),
		ConversationID: result.ConversationID(),
	})
}
