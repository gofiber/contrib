package fibernewrelic

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/newrelic/go-agent/v3/newrelic"
	"github.com/stretchr/testify/assert"
)

func TestNewrelicAppConfig(t *testing.T) {
	t.Run("Panic occurs when License empty",
		func(t *testing.T) {
			assert.Panics(t, func() {
				New(Config{
					License: "",
					AppName: "",
					Enabled: false,
				})
			})
		})

	t.Run("Run without panic when License not empty",
		func(t *testing.T) {
			assert.NotPanics(t, func() {
				New(Config{
					License: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
					AppName: "",
					Enabled: false,
				})
			})
		})

	t.Run("Panic when License is invalid length",
		func(t *testing.T) {
			assert.Panics(t, func() {
				New(Config{
					License: "invalid_key",
					AppName: "",
					Enabled: false,
				})
			})
		})

	t.Run("Run successfully as middleware",
		func(t *testing.T) {
			app := fiber.New()

			cfg := Config{
				License: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
				AppName: "",
				Enabled: true,
			}

			app.Use(New(cfg))

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Run successfully as middleware",
		func(t *testing.T) {
			app := fiber.New()

			cfg := Config{
				License: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
				AppName: "",
				Enabled: true,
			}

			newRelicApp, _ := newrelic.NewApplication(
				newrelic.ConfigAppName(cfg.AppName),
				newrelic.ConfigLicense(cfg.License),
				newrelic.ConfigEnabled(cfg.Enabled),
			)

			cfg.Application = newRelicApp

			app.Use(New(cfg))

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Test for invalid URL",
		func(t *testing.T) {
			app := fiber.New()

			cfg := Config{
				License: "0123456789abcdef0123456789abcdef01234567",
				AppName: "",
				Enabled: true,
			}
			app.Use(New(cfg))

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/invalid-url", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 404, resp.StatusCode)
		})

	t.Run("Test HTTP transport type",
		func(t *testing.T) {
			app := fiber.New()

			cfg := Config{
				License: "0123456789abcdef0123456789abcdef01234567",
				AppName: "",
				Enabled: true,
			}
			app.Use(New(cfg))

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Test http transport type (lowercase)",
		func(t *testing.T) {
			app := fiber.New()

			cfg := Config{
				License: "0123456789abcdef0123456789abcdef01234567",
				AppName: "",
				Enabled: true,
			}
			app.Use(New(cfg))

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Test HTTPS transport type",
		func(t *testing.T) {
			app := fiber.New()

			cfg := Config{
				License: "0123456789abcdef0123456789abcdef01234567",
				AppName: "",
				Enabled: true,
			}
			app.Use(New(cfg))

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Test using existing newrelic application (configured)",
		func(t *testing.T) {
			app := fiber.New()

			newrelicApp, err := newrelic.NewApplication(
				newrelic.ConfigAppName("testApp"),
				newrelic.ConfigLicense("0123456789abcdef0123456789abcdef01234567"),
				newrelic.ConfigEnabled(true),
			)

			cfg := Config{
				Application: newrelicApp,
			}
			app.Use(New(cfg))

			app.Get("/", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			assert.NoError(t, err)
			assert.NotNil(t, newrelicApp)

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, -1)
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Assert panic with existing newrelic application (no config)",
		func(t *testing.T) {
			assert.Panics(t, func() {
				app := fiber.New()

				newrelicApp, err := newrelic.NewApplication()

				cfg := Config{
					Application: newrelicApp,
				}
				app.Use(New(cfg))

				app.Get("/", func(ctx *fiber.Ctx) error {
					return ctx.SendStatus(200)
				})

				assert.Error(t, err)
				assert.Nil(t, newrelicApp)

				r := httptest.NewRequest("GET", "/", nil)
				resp, _ := app.Test(r, -1)
				assert.Equal(t, 200, resp.StatusCode)
			})
		})
}
