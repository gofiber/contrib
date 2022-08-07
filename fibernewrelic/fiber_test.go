package fibernewrelic

import (
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"net/http/httptest"
	"testing"
)

func TestNewrelicAppConfig(t *testing.T) {
	t.Run("Panic occurs when License empty",
		func(t *testing.T) {
			assert.Panics(t, func() {
				New(Config{
					License:       "",
					AppName:       "",
					Enabled:       false,
					TransportType: "",
				})
			})
		})

	t.Run("Run without panic when License not empty",
		func(t *testing.T) {
			assert.NotPanics(t, func() {
				New(Config{
					License:       "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
					AppName:       "",
					Enabled:       false,
					TransportType: "",
				})
			})
		})

	t.Run("Run successfully as middleware",
		func(t *testing.T) {
			app := fiber.New()

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			cfg := Config{
				License:       "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
				AppName:       "",
				Enabled:       true,
				TransportType: "",
			}

			app.Use(New(cfg))

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)

			assert.Equal(t, 200, resp.StatusCode)
		})
}
