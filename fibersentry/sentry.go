package fibersentry

import (
	"context"
	"net/http"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

func New(config ...Config) fiber.Handler {
	cfg := Config{
		Repanic:         false,
		WaitForDelivery: false,
		Timeout:         time.Second * 2,
	}

	if len(config) > 0 {
		cfg = config[0]
	}

	return func(c *fiber.Ctx) error {
		var r http.Request
		if err := fasthttpadaptor.ConvertRequest(c.Context(), &r, true); err != nil {
			return err
		}

		hub := sentry.CurrentHub().Clone()
		scope := hub.Scope()
		scope.SetRequest(&r)
		scope.SetRequestBody(utils.CopyBytes(c.Body()))
		c.Locals(hubKey, hub)

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

		return c.Next()
	}
}

func GetHubFromContext(ctx *fiber.Ctx) *sentry.Hub {
	return ctx.Locals(hubKey).(*sentry.Hub)
}
