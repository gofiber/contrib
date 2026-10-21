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
	// fiber.ErrUnauthorized to turn away an anonymous caller. Deps runs before
	// the body of the request is read, so a caller that is turned away does not
	// get its payload parsed.
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
	// With a Prompt function the handler applies none of its request checks,
	// neither the method, nor the content type and the encoding, nor the size.
	// The function owns the request. The content type check keeps cross-site
	// form posts from starting runs, so a route that reads the body itself needs
	// its own protection.
	//
	// Optional. Default: the "prompt" field of the JSON request body, which has
	// to be sent with POST and application/json. The optional "conversation_id"
	// field becomes the conversation ID of the run.
	Prompt func(c fiber.Ctx) (string, error)

	// Respond writes the response for a completed run. It also receives runs
	// that ended with deferred tool calls, see ai.RunResult.Deferred.
	//
	// Optional. Default: the JSON object {"output", "run_id", "conversation_id"}.
	// It answers 501 Not Implemented for a run that ended with deferred tool
	// calls, because the response cannot carry the decision that resumes them.
	Respond func(c fiber.Ctx, result *ai.RunResult[Output]) error

	// OnRunEnd is called once the agent has run, with the dependencies of the
	// run, what the run consumed and the error that ended it, which is nil when
	// the run completed. It is called for a run that failed as well, which makes
	// it the place to record token usage. run gives the usage, the messages and
	// the IDs of the run, see ai.RunInfo, and is nil when the agent did not
	// start. It is not called for a request that was rejected before the agent
	// ran. A panic in OnRunEnd is logged and recovered.
	//
	// Optional. Default: nil
	OnRunEnd func(deps Deps, run *ai.RunInfo, err error)

	// MaxRequestBytes bounds the body of the request that the default Prompt
	// reads. The BodyLimit of the app applies as well, 4 MiB by default, and
	// the smaller of the two wins.
	//
	// Optional. Default: 10 MiB
	MaxRequestBytes int64

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
// It panics when agent is nil or the config is invalid.
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
	limits := requestLimits{maxBytes: cfg.MaxRequestBytes}

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

		ctx := c.Context()
		if cfg.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()
		}
		options, tracker := trackRun(options, cfg.OnRunEnd != nil)
		result, err := agent.Run(ctx, prompt, deps, options...)
		if cfg.OnRunEnd != nil {
			safely("OnRunEnd", func() { cfg.OnRunEnd(deps, tracker.run(), err) })
		}
		if err != nil {
			return newError(err)
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
		return newError(errDeferred)
	}
	return c.JSON(runResponse[Output]{
		Output:         result.Output,
		RunID:          result.RunID(),
		ConversationID: result.ConversationID(),
	})
}
