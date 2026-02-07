package hcaptcha

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	TestSecretKey     = "0x0000000000000000000000000000000000000000"
	TestResponseToken = "20000000-aaaa-bbbb-cccc-000000000002"
)

func newSiteVerifyServer(t *testing.T, success bool) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		_, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, r.Body.Close())

		_, err = w.Write([]byte(`{"success":` + map[bool]string{true: "true", false: "false"}[success] + `}`))
		require.NoError(t, err)
	}))
}

func TestHCaptchaDefaultValidation(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		server := newSiteVerifyServer(t, true)
		defer server.Close()

		app := fiber.New()
		m := New(Config{
			SecretKey:     TestSecretKey,
			SiteVerifyURL: server.URL,
			ResponseKeyFunc: func(c fiber.Ctx) (string, error) {
				return TestResponseToken, nil
			},
		})

		app.Get("/hcaptcha", m, func(c fiber.Ctx) error {
			return c.SendString("ok")
		})

		req := httptest.NewRequest(http.MethodGet, "/hcaptcha", nil)
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		require.NoError(t, err)
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		assert.Equal(t, fiber.StatusOK, res.StatusCode)
		assert.Equal(t, "ok", string(body))
	})

	t.Run("failure", func(t *testing.T) {
		server := newSiteVerifyServer(t, false)
		defer server.Close()

		app := fiber.New()
		m := New(Config{
			SecretKey:     TestSecretKey,
			SiteVerifyURL: server.URL,
			ResponseKeyFunc: func(c fiber.Ctx) (string, error) {
				return TestResponseToken, nil
			},
		})

		app.Get("/hcaptcha", m, func(c fiber.Ctx) error {
			return c.SendString("ok")
		})

		req := httptest.NewRequest(http.MethodGet, "/hcaptcha", nil)
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		require.NoError(t, err)
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusForbidden, res.StatusCode)
		assert.Equal(t, "unable to check that you are not a robot", string(body))
	})
}

func TestHCaptchaValidateFunc(t *testing.T) {
	t.Run("called with success and allows request", func(t *testing.T) {
		server := newSiteVerifyServer(t, true)
		defer server.Close()

		app := fiber.New()
		called := false
		m := New(Config{
			SecretKey:     TestSecretKey,
			SiteVerifyURL: server.URL,
			ResponseKeyFunc: func(c fiber.Ctx) (string, error) {
				return TestResponseToken, nil
			},
			ValidateFunc: func(success bool, c fiber.Ctx) error {
				called = true
				assert.True(t, success)
				return nil
			},
		})

		app.Get("/hcaptcha", m, func(c fiber.Ctx) error {
			return c.SendString("ok")
		})

		req := httptest.NewRequest(http.MethodGet, "/hcaptcha", nil)
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		require.NoError(t, err)
		defer res.Body.Close()

		assert.Equal(t, fiber.StatusOK, res.StatusCode)
		assert.True(t, called)
	})

	t.Run("custom status is preserved on validation error", func(t *testing.T) {
		server := newSiteVerifyServer(t, false)
		defer server.Close()

		app := fiber.New()
		m := New(Config{
			SecretKey:     TestSecretKey,
			SiteVerifyURL: server.URL,
			ResponseKeyFunc: func(c fiber.Ctx) (string, error) {
				return TestResponseToken, nil
			},
			ValidateFunc: func(success bool, c fiber.Ctx) error {
				assert.False(t, success)
				c.Status(fiber.StatusUnprocessableEntity)
				return errors.New("custom validation failed")
			},
		})

		app.Get("/hcaptcha", m, func(c fiber.Ctx) error {
			return c.SendString("ok")
		})

		req := httptest.NewRequest(http.MethodGet, "/hcaptcha", nil)
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		require.NoError(t, err)
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusUnprocessableEntity, res.StatusCode)
		assert.Equal(t, "custom validation failed", string(body))
	})

	t.Run("defaults to 403 and error body when validatefunc sets neither", func(t *testing.T) {
		server := newSiteVerifyServer(t, false)
		defer server.Close()

		app := fiber.New()
		m := New(Config{
			SecretKey:     TestSecretKey,
			SiteVerifyURL: server.URL,
			ResponseKeyFunc: func(c fiber.Ctx) (string, error) {
				return TestResponseToken, nil
			},
			ValidateFunc: func(success bool, c fiber.Ctx) error {
				assert.False(t, success)
				return errors.New("custom validation failed")
			},
		})

		app.Get("/hcaptcha", m, func(c fiber.Ctx) error {
			return c.SendString("ok")
		})

		req := httptest.NewRequest(http.MethodGet, "/hcaptcha", nil)
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		require.NoError(t, err)
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusForbidden, res.StatusCode)
		assert.Equal(t, "custom validation failed", string(body))
	})
}

func TestDefaultResponseKeyFunc(t *testing.T) {
	app := fiber.New()
	app.Post("/", func(c fiber.Ctx) error {
		token, err := DefaultResponseKeyFunc(c)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).SendString(err.Error())
		}

		return c.SendString(token)
	})

	t.Run("valid token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"hcaptcha_token":"abc"}`))
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		require.NoError(t, err)
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusOK, res.StatusCode)
		assert.Equal(t, "abc", string(body))
	})

	t.Run("empty token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"hcaptcha_token":"  "}`))
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		require.NoError(t, err)
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusBadRequest, res.StatusCode)
		assert.Equal(t, "hcaptcha token is empty", string(body))
	})
}
