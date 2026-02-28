package otel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestMiddleware_StoreTracerInContextWithPassLocalsToContext(t *testing.T) {
	app := fiber.New(fiber.Config{PassLocalsToContext: true})
	app.Use(Middleware())

	app.Get("/", func(c fiber.Ctx) error {
		tracerFromContext, ok := fiber.ValueFromContext[oteltrace.Tracer](c.Context(), tracerKey)
		require.True(t, ok)
		require.NotNil(t, tracerFromContext)
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
