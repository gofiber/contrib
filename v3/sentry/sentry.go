package sentry

import (
	"context"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/utils/v2"
)

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Return new handler
	return func(c fiber.Ctx) error {
		// Convert fiber request to http request
		r, err := adaptor.ConvertRequest(c, true)
		if err != nil {
			return err
		}

		// Init sentry hub
		hub := sentry.CurrentHub().Clone()
		scope := hub.Scope()
		scope.SetRequest(r)
		scope.SetRequestBody(utils.CopyBytes(c.Body()))
		fiber.StoreInContext(c, hubKey, hub)

		// Catch panics
		defer func() {
			if err := recover(); err != nil {
				eventID := hub.RecoverWithContext(
					context.WithValue(context.Background(), sentry.RequestContextKey, c),
					err,
				)

				if eventID != nil && cfg.WaitForDelivery {
					hub.Flush(cfg.Timeout)
				}

				if cfg.Repanic {
					panic(err)
				}
			}
		}()

		// Return err if exist, else move to next handler
		return c.Next()
	}
}

// MustGetHubFromContext returns the Sentry hub from context.
// It accepts fiber.CustomCtx, fiber.Ctx, *fasthttp.RequestCtx, and context.Context.
func MustGetHubFromContext(ctx any) *sentry.Hub {
	hub, ok := fiber.ValueFromContext[*sentry.Hub](ctx, hubKey)
	if !ok {
		panic("interface conversion: interface {} is nil, not *sentry.Hub")
	}

	return hub
}

// GetHubFromContext returns the Sentry hub from context.
// It accepts fiber.CustomCtx, fiber.Ctx, *fasthttp.RequestCtx, and context.Context.
func GetHubFromContext(ctx any) *sentry.Hub {
	hub, ok := fiber.ValueFromContext[*sentry.Hub](ctx, hubKey)
	if !ok {
		return nil
	}
	return hub
}
