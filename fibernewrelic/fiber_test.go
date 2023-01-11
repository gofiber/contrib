package fibernewrelic

import (
	"github.com/gofiber/fiber/v2"
	"github.com/newrelic/go-agent/v3/newrelic"
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

	t.Run("Panic when License is invalid length",
		func(t *testing.T) {
			assert.Panics(t, func() {
				New(Config{
					License:       "invalid_key",
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

	t.Run("Test for invalid URL",
		func(t *testing.T) {
			app := fiber.New()

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			cfg := Config{
				License:       "0123456789abcdef0123456789abcdef01234567",
				AppName:       "",
				Enabled:       true,
				TransportType: "",
			}
			app.Use(New(cfg))

			r := httptest.NewRequest("GET", "/invalid-url", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 404, resp.StatusCode)
		})

	t.Run("Test HTTP transport type",
		func(t *testing.T) {
			app := fiber.New()

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			cfg := Config{
				License:       "0123456789abcdef0123456789abcdef01234567",
				AppName:       "",
				Enabled:       true,
				TransportType: "HTTP",
			}
			app.Use(New(cfg))

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Test http transport type (lowercase)",
		func(t *testing.T) {
			app := fiber.New()

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			cfg := Config{
				License:       "0123456789abcdef0123456789abcdef01234567",
				AppName:       "",
				Enabled:       true,
				TransportType: "http",
			}
			app.Use(New(cfg))

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Test HTTPS transport type",
		func(t *testing.T) {
			app := fiber.New()

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			cfg := Config{
				License:       "0123456789abcdef0123456789abcdef01234567",
				AppName:       "",
				Enabled:       true,
				TransportType: "HTTPS",
			}
			app.Use(New(cfg))

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Test invalid transport type",
		func(t *testing.T) {
			app := fiber.New()

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			cfg := Config{
				License:       "0123456789abcdef0123456789abcdef01234567",
				AppName:       "",
				Enabled:       true,
				TransportType: "InvalidTransport",
			}
			app.Use(New(cfg))

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Test using existing newrelic application (configured)",
		func(t *testing.T) {
			app := fiber.New()

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			newrelicApp, err := newrelic.NewApplication(
				newrelic.ConfigAppName("testApp"),
				newrelic.ConfigLicense("0123456789abcdef0123456789abcdef01234567"),
			)

			assert.NoError(t, err)
			assert.NotNil(t, newrelicApp)

			cfg := Config{
				Application: newrelicApp,
			}
			app.Use(New(cfg))

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Assert panic with existing newrelic application (no config)",
		func(t *testing.T) {
			assert.Panics(t, func() {
				app := fiber.New()

				app.Get("/", func(ctx *fiber.Ctx) error {
					return ctx.SendStatus(200)
				})

				newrelicApp, err := newrelic.NewApplication()
				assert.Error(t, err)
				assert.Nil(t, newrelicApp)

				cfg := Config{
					Application: newrelicApp,
				}
				app.Use(New(cfg))

				r := httptest.NewRequest("GET", "/", nil)
				resp, _ := app.Test(r, -1)
				assert.Equal(t, 200, resp.StatusCode)
			})
		})
}
