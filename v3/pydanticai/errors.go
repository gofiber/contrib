package pydanticai

import (
	"context"
	"errors"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3"
)

// errDeferred reports a run that stopped for approvals or external tool results.
var errDeferred = errors.New(
	"pydanticai: the run ended with deferred tool calls, which Run cannot resume; serve the agent with AGUI or Vercel",
)

// Error is returned by Run when the agent run fails.
//
// Its message is the reason phrase of Code, so the default Fiber error handler
// does not send provider error details to the client. The agent error is kept
// for logging and for errors.Is and errors.As, and Fiber error handlers find
// the status code through errors.As with a *fiber.Error.
type Error struct {
	// Err is the error that failed the run.
	Err error

	// Code is the HTTP status code that StatusCode derived from Err.
	Code int
}

// Error returns the reason phrase of the status code.
func (e *Error) Error() string {
	return fiber.NewError(e.Code).Message
}

// Unwrap returns the *fiber.Error that carries the status code and the agent error.
func (e *Error) Unwrap() []error {
	if e.Err == nil {
		return []error{fiber.NewError(e.Code)}
	}
	return []error{fiber.NewError(e.Code), e.Err}
}

// StatusCode returns the HTTP status code that describes err best. It returns
// the code of a *fiber.Error in the chain, so an error that chose its own
// status keeps it, and falls back to the codes below for agent errors.
//
//	ai.ErrUsageLimitExceeded                          429 Too Many Requests
//	context.DeadlineExceeded                          504 Gateway Timeout
//	ai.ErrRunCancelled, context.Canceled              503 Service Unavailable
//	*ai.ContentFilterError                            422 Unprocessable Entity
//	*ai.UnexpectedModelBehaviorError,
//	ai.ErrMaxRetriesExceeded, ai.ModelAPIError        502 Bad Gateway
//	anything else                                     500 Internal Server Error
//
// It returns 200 for a nil error.
func StatusCode(err error) int {
	if err == nil {
		return fiber.StatusOK
	}
	if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
		return fiberErr.Code
	}
	switch {
	case errors.Is(err, ai.ErrUsageLimitExceeded):
		return fiber.StatusTooManyRequests
	case errors.Is(err, context.DeadlineExceeded):
		return fiber.StatusGatewayTimeout
	case errors.Is(err, ai.ErrRunCancelled), errors.Is(err, context.Canceled):
		return fiber.StatusServiceUnavailable
	}
	if _, ok := errors.AsType[*ai.ContentFilterError](err); ok {
		return fiber.StatusUnprocessableEntity
	}
	if _, ok := errors.AsType[*ai.UnexpectedModelBehaviorError](err); ok || errors.Is(err, ai.ErrMaxRetriesExceeded) {
		return fiber.StatusBadGateway
	}
	if api, ok := errors.AsType[ai.ModelAPIError](err); ok && api.IsModelAPIError() {
		return fiber.StatusBadGateway
	}
	return fiber.StatusInternalServerError
}
