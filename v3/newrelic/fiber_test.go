package newrelic

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/newrelic/go-agent/v3/newrelic"
	"github.com/stretchr/testify/assert"
)

func TestNewRelicAppConfig(t *testing.T) {
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

			app.Get("/", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
			assert.Equal(t, http.StatusOK, resp.StatusCode)
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

			app.Get("/", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
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

			app.Get("/", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/invalid-url", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
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

			app.Get("/", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
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

			app.Get("/", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
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

			app.Get("/", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
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

			app.Get("/", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			assert.NoError(t, err)
			assert.NotNil(t, newrelicApp)

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
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

				app.Get("/", func(ctx fiber.Ctx) error {
					return ctx.SendStatus(200)
				})

				assert.Error(t, err)
				assert.Nil(t, newrelicApp)

				r := httptest.NewRequest("GET", "/", nil)
				resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
				assert.Equal(t, 200, resp.StatusCode)
			})
		})

	t.Run("config should use default error status code handler", func(t *testing.T) {
		// given
		app := fiber.New()

		app.Use(New(Config{
			License: "0123456789abcdef0123456789abcdef01234567",
			AppName: "",
			Enabled: true,
		}))

		app.Get("/", func(ctx fiber.Ctx) error { return errors.New("system error") })

		// when
		resp, err := app.Test(httptest.NewRequest("GET", "/", nil), fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		assert.NoError(t, err)
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("config should use custom error status code handler when error status code handler is provided", func(t *testing.T) {
		// given
		var (
			app                          = fiber.New()
			errorStatusCodeHandlerCalled = false
		)

		errorStatusCodeHandler := func(c fiber.Ctx, err error) int {
			errorStatusCodeHandlerCalled = true
			return http.StatusInternalServerError
		}

		app.Use(New(Config{
			License:                "0123456789abcdef0123456789abcdef01234567",
			AppName:                "",
			Enabled:                true,
			ErrorStatusCodeHandler: errorStatusCodeHandler,
		}))

		app.Get("/", func(ctx fiber.Ctx) error { return errors.New("system error") })

		// when
		resp, err := app.Test(httptest.NewRequest("GET", "/", nil), fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		assert.NoError(t, err)
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.True(t, errorStatusCodeHandlerCalled)
	})

	t.Run("Skip New Relic execution if next function is set",
		func(t *testing.T) {
			app := fiber.New()

			cfg := Config{
				License: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
				AppName: "",
				Enabled: true,
				Next: func(c fiber.Ctx) bool {
					return c.OriginalURL() == "/jump"
				},
			}

			newRelicApp, _ := newrelic.NewApplication(
				newrelic.ConfigAppName(cfg.AppName),
				newrelic.ConfigLicense(cfg.License),
				newrelic.ConfigEnabled(cfg.Enabled),
			)

			cfg.Application = newRelicApp

			app.Use(New(cfg))

			app.Get("/jump", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/jump", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
			assert.Equal(t, 200, resp.StatusCode)
		})

	t.Run("Continue New Relic execution if next function is set",
		func(t *testing.T) {
			app := fiber.New()

			cfg := Config{
				License: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
				AppName: "",
				Enabled: true,
				Next: func(c fiber.Ctx) bool {
					return c.OriginalURL() == "/jump"
				},
			}

			newRelicApp, _ := newrelic.NewApplication(
				newrelic.ConfigAppName(cfg.AppName),
				newrelic.ConfigLicense(cfg.License),
				newrelic.ConfigEnabled(cfg.Enabled),
			)

			cfg.Application = newRelicApp

			app.Use(New(cfg))

			app.Get("/", func(ctx fiber.Ctx) error {
				return ctx.SendStatus(200)
			})

			r := httptest.NewRequest("GET", "/", nil)
			resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
			assert.Equal(t, 200, resp.StatusCode)
		})
}

func TestDefaultErrorStatusCodeHandler(t *testing.T) {
	t.Run("should return fiber status code when error is fiber error", func(t *testing.T) {
		// given
		err := &fiber.Error{
			Code: http.StatusNotFound,
		}

		// when
		statusCode := DefaultErrorStatusCodeHandler(nil, err)

		// then
		assert.Equal(t, http.StatusNotFound, statusCode)
	})

	t.Run("should return context status code when error is not fiber error", func(t *testing.T) {
		// given
		app := fiber.New()

		app.Use(New(Config{
			License: "0123456789abcdef0123456789abcdef01234567",
			AppName: "",
			Enabled: true,
		}))

		app.Get("/", func(ctx fiber.Ctx) error {
			err := ctx.SendStatus(http.StatusNotFound)
			assert.Equal(t, http.StatusNotFound, DefaultErrorStatusCodeHandler(ctx, err))
			return err
		})

		// when
		resp, err := app.Test(httptest.NewRequest("GET", "/", nil), fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		assert.NoError(t, err)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestFromContext(t *testing.T) {
	// given
	cfg := Config{
		License: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
		AppName: "",
		Enabled: true,
	}
	app := fiber.New()
	app.Use(New(cfg))
	app.Get("/foo", func(ctx fiber.Ctx) error {
		tx := FromContext(ctx)
		assert.NotNil(t, tx)

		if tx != nil {
			segment := tx.StartSegment("foo")
			defer segment.End()
		}

		return ctx.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/foo", http.NoBody)

	// when
	res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

	// then
	assert.Nil(t, err)
	assert.Equal(t, http.StatusOK, res.StatusCode)
}

func TestCreateWebRequest(t *testing.T) {
	t.Run("should include inbound headers for distributed tracing", func(t *testing.T) {
		app := fiber.New()
		app.Get("/", func(ctx fiber.Ctx) error {
			req := createWebRequest(ctx, ctx.Hostname(), ctx.Method(), string(ctx.Request().URI().Scheme()))
			assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", req.Header.Get("traceparent"))
			assert.ElementsMatch(t, []string{"abc", "def"}, req.Header.Values("X-Custom"))
			return ctx.SendStatus(http.StatusNoContent)
		})

		r := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		r.Header.Add("X-Custom", "abc")
		r.Header.Add("X-Custom", "def")

		resp, err := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		assert.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	})
}
