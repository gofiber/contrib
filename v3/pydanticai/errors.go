package pydanticai

import (
	"context"
	"errors"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
)

// errDeferred reports a run that stopped for approvals or external tool results.
var errDeferred = fiber.NewError(
	fiber.StatusNotImplemented,
	"pydanticai: the run ended with deferred tool calls, which Run cannot resume; serve the agent with AGUI or Vercel",
)

// Error is returned by Run when the agent run fails. Its message is the reason
// phrase of Code, so provider details do not reach the client; errors.Is and
// errors.As see the agent error, and errors.Is matches fiber.Error values by
// status code.
type Error struct {
	// Err is the error that failed the run, left out when an Error is encoded as JSON.
	Err error `json:"-"`

	// Code is the HTTP status code that StatusCode derived from Err.
	Code int `json:"code"`
}

func newError(err error) *Error {
	return &Error{Err: err, Code: StatusCode(err)}
}

// Error returns the reason phrase of the status code.
func (e *Error) Error() string {
	return utils.StatusMessage(e.Code)
}

// Unwrap returns the *fiber.Error that carries the status code and the agent error.
func (e *Error) Unwrap() []error {
	if e.Err == nil {
		return []error{fiber.NewError(e.Code)}
	}
	return []error{fiber.NewError(e.Code), e.Err}
}

// Is reports whether target is a *fiber.Error with the status code of e.
func (e *Error) Is(target error) bool {
	status, ok := target.(*fiber.Error)
	return ok && status != nil && status.Code == e.Code
}

// StatusCode returns the HTTP status code that describes err best: that of a
// *fiber.Error in the chain, else 429 for a usage limit, 504 for a deadline, 503
// for a cancellation, 422 for a content filter, 502 for a failing model and 500;
// 200 for nil.
func StatusCode(err error) int {
	if err == nil {
		return fiber.StatusOK
	}
	if fiberErr, ok := errors.AsType[*fiber.Error](err); ok && isErrorStatus(fiberErr) {
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

// isErrorStatus reports whether err is a client or server error with a reason phrase.
func isErrorStatus(err *fiber.Error) bool {
	return err != nil && err.Code >= 400 && err.Code <= 599 && utils.StatusMessage(err.Code) != ""
}
