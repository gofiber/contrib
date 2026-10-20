package pydanticai_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// providerError is a provider API failure as ModelAPIError describes it.
type providerError struct{ api bool }

func (providerError) Error() string           { return "provider failed" }
func (e providerError) IsModelAPIError() bool { return e.api }

func Test_StatusCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"nil", nil, fiber.StatusOK},
		{"unknown error", errors.New("boom"), fiber.StatusInternalServerError},
		{"no model", ai.ErrNoModel, fiber.StatusInternalServerError},
		{"fiber error", fiber.ErrTeapot, fiber.StatusTeapot},
		{"wrapped fiber error", fmt.Errorf("tool: %w", fiber.ErrForbidden), fiber.StatusForbidden},
		{"usage limit", fmt.Errorf("%w: too many requests", ai.ErrUsageLimitExceeded), fiber.StatusTooManyRequests},
		{"deadline", context.DeadlineExceeded, fiber.StatusGatewayTimeout},
		{"canceled", context.Canceled, fiber.StatusServiceUnavailable},
		{"run canceled", fmt.Errorf("%w", ai.ErrRunCancelled), fiber.StatusServiceUnavailable},
		{"content filter", ai.NewContentFilterError("blocked", ai.ModelResponse{}), fiber.StatusUnprocessableEntity},
		{"unexpected model behavior", &ai.UnexpectedModelBehaviorError{Message: "no output"}, fiber.StatusBadGateway},
		{"max retries", fmt.Errorf("%w: output", ai.ErrMaxRetriesExceeded), fiber.StatusBadGateway},
		{"provider error", fmt.Errorf("request: %w", providerError{api: true}), fiber.StatusBadGateway},
		{"not a provider error", providerError{api: false}, fiber.StatusInternalServerError},
		{
			"transport error",
			&ai.ModelTransportError{Operation: "request", Err: errors.New("connection reset")},
			fiber.StatusBadGateway,
		},
		{
			"transport timeout",
			&ai.ModelTransportError{Operation: "request", Err: context.DeadlineExceeded},
			fiber.StatusGatewayTimeout,
		},
		{"usage limit before timeout", errors.Join(ai.ErrUsageLimitExceeded, context.DeadlineExceeded), fiber.StatusTooManyRequests},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.status, pydanticai.StatusCode(test.err))
		})
	}
}

func Test_Error(t *testing.T) {
	t.Parallel()

	cause := errors.New("provider rejected the key sk-secret")
	err := error(&pydanticai.Error{Code: fiber.StatusBadGateway, Err: cause})

	require.Equal(t, "Bad Gateway", err.Error(), "the message must not repeat the cause")
	require.ErrorIs(t, err, cause)

	var fiberErr *fiber.Error
	require.ErrorAs(t, err, &fiberErr)
	require.Equal(t, fiber.StatusBadGateway, fiberErr.Code)

	var runErr *pydanticai.Error
	require.ErrorAs(t, fmt.Errorf("handler: %w", err), &runErr)
	require.Equal(t, cause, runErr.Err)

	t.Run("without a cause", func(t *testing.T) {
		t.Parallel()
		err := error(&pydanticai.Error{Code: fiber.StatusNotImplemented})
		require.Equal(t, "Not Implemented", err.Error())
		var fiberErr *fiber.Error
		require.ErrorAs(t, err, &fiberErr)
		require.Equal(t, fiber.StatusNotImplemented, fiberErr.Code)
	})
}

func Test_Error_DefaultErrorHandler(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Get("/", func(fiber.Ctx) error {
		return &pydanticai.Error{
			Code: fiber.StatusBadGateway,
			Err:  errors.New("provider rejected the key sk-secret"),
		}
	})

	resp, body := do(t, app, fiber.MethodGet, "/", "", "")
	require.Equal(t, fiber.StatusBadGateway, resp.StatusCode)
	require.Equal(t, "Bad Gateway", body)
}
