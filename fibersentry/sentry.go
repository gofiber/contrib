package fibersentry

import (
	"context"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/gofiber/fiber/v2/utils"
)

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Return new handler
	return func(c *fiber.Ctx) error {
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
		c.Locals(hubKey, hub)

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

func GetHubFromContext(ctx *fiber.Ctx) *sentry.Hub {
	hub, ok := ctx.Locals(hubKey).(*sentry.Hub)
	if !ok {
		return nil
	}
	return hub
}
