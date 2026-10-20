package pydanticai

import (
	"context"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3"
)

// RunConfig defines the config for the Run handler.
type RunConfig[Deps, Output any] struct {
	// Next defines a function to skip this handler when it returns true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// Deps resolves the dependencies passed to the agent for one request, for
	// example from the user that an earlier middleware stored in c.Locals. An
	// error is returned from the handler as it is, so return
	// fiber.ErrUnauthorized to turn away an anonymous caller.
	//
	// Optional. Default: the zero value of Deps
	Deps func(c fiber.Ctx) (Deps, error)

	// RunOptions returns additional agent run options for one request, such as
	// ai.WithRunUsageLimits, ai.WithRunMetadata or ai.WithMessageHistory. They
	// take precedence over the conversation ID of the request body.
	//
	// Optional. Default: nil
	RunOptions func(c fiber.Ctx) ([]ai.RunOption, error)

	// Prompt reads the user prompt from the request. An error is returned from
	// the handler as it is.
	//
	// Optional. Default: the "prompt" field of the JSON request body, which
	// must be sent as application/json and be at most 10 MiB. The optional
	// "conversation_id" field becomes the conversation ID of the run.
	Prompt func(c fiber.Ctx) (string, error)

	// Respond writes the response for a completed run. It also receives runs
	// that ended with deferred tool calls, see ai.RunResult.Deferred.
	//
	// Optional. Default: the JSON object {"output", "run_id", "conversation_id"}.
	// It answers 501 Not Implemented for a run that ended with deferred tool
	// calls, because the response cannot carry the decision that resumes them.
	Respond func(c fiber.Ctx, result *ai.RunResult[Output]) error

	// Timeout bounds the duration of one run. A run that exceeds it fails with
	// 504 Gateway Timeout. Unlike the streaming handlers, Run cannot notice a
	// client that went away, so the timeout is what ends a run nobody waits
	// for.
	//
	// Optional. Default: 0 (no timeout)
	Timeout time.Duration
}

// runRequest is the default request body of Run.
type runRequest struct {
	Prompt         string `json:"prompt"`
	ConversationID string `json:"conversation_id"`
}

// runResponse is the default response body of Run.
type runResponse[Output any] struct {
	Output         Output `json:"output"`
	RunID          string `json:"run_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// Run returns a handler that runs agent once per request and answers with its
// output as JSON.
//
//	app.Post("/ask", pydanticai.Run(agent))
//
// The default request body is {"prompt": "...", "conversation_id": "..."}. When
// the run fails, the handler returns an *Error that carries the HTTP status
// code StatusCode derived from the agent error, and the Fiber error handler
// turns it into the response.
//
// It panics when agent is nil.
func Run[Deps, Output any](agent *ai.Agent[Deps, Output], config ...RunConfig[Deps, Output]) fiber.Handler {
	if agent == nil {
		panic("pydanticai: agent must not be nil")
	}
	var cfg RunConfig[Deps, Output]
	if len(config) > 0 {
		cfg = config[0]
	}
	limits := newRequestLimits(0, nil, false)

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		// Without a Prompt function the prompt comes from the JSON body.
		var body []byte
		if cfg.Prompt == nil {
			var err error
			if body, err = limits.body(c); err != nil {
				return err
			}
		}
		// Resolving the dependencies comes first, so that a caller that is
		// turned away does not get its payload parsed.
		deps, extra, err := resolve(c, cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}

		var (
			prompt  string
			options []ai.RunOption
		)
		if cfg.Prompt != nil {
			if prompt, err = cfg.Prompt(c); err != nil {
				return err
			}
		} else {
			var request runRequest
			if err := decodeJSON(body, &request, "run"); err != nil {
				return err
			}
			if request.Prompt == "" {
				return fiber.NewError(fiber.StatusBadRequest, "prompt is required")
			}
			prompt = request.Prompt
			if request.ConversationID != "" {
				options = append(options, ai.WithConversationID(request.ConversationID))
			}
		}
		options = append(options, extra...)

		ctx := c.Context()
		if cfg.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()
		}
		result, err := agent.Run(ctx, prompt, deps, options...)
		if err != nil {
			return &Error{Err: err, Code: StatusCode(err)}
		}
		if cfg.Respond != nil {
			return cfg.Respond(c, result)
		}
		return respondJSON(c, result)
	}
}

// respondJSON is the default Respond of Run.
func respondJSON[Output any](c fiber.Ctx, result *ai.RunResult[Output]) error {
	if result.Deferred() != nil {
		return &Error{Err: errDeferred, Code: fiber.StatusNotImplemented}
	}
	return c.JSON(runResponse[Output]{
		Output:         result.Output,
		RunID:          result.RunID(),
		ConversationID: result.ConversationID(),
	})
}
