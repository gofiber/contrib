package fibersentry

import (
	"context"
	"net/http"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Return new handler
	return func(c *fiber.Ctx) error {
		// Convert fiber request to http request
		var r http.Request
		if err := fasthttpadaptor.ConvertRequest(c.Context(), &r, true); err != nil {
			return err
		}

		// Init sentry hub
		hub := sentry.CurrentHub().Clone()
		scope := hub.Scope()
		scope.SetRequest(&r)
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
	return ctx.Locals(hubKey).(*sentry.Hub)
}
