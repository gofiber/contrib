package hcaptcha

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestHCaptchaTokenTooLong(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		_, err = w.Write([]byte(`{"success":true}`))
		assert.NoError(t, err)
	}))
	defer server.Close()

	tests := []struct {
		name            string
		responseKeyFunc func(fiber.Ctx) (string, error)
		prefix, suffix  string
	}{
		{name: "default response key func", prefix: `{"hcaptcha_token":"`, suffix: `"}`},
		{name: "custom response key func", responseKeyFunc: func(c fiber.Ctx) (string, error) {
			return string(c.Body()), nil
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			app.Post("/hcaptcha", New(Config{
				SecretKey:       TestSecretKey,
				SiteVerifyURL:   server.URL,
				ResponseKeyFunc: tt.responseKeyFunc,
			}), func(c fiber.Ctx) error {
				return c.SendString("ok")
			})

			post := func(tokenLength int) (int, string) {
				body := tt.prefix + strings.Repeat("a", tokenLength) + tt.suffix
				req := httptest.NewRequest(http.MethodPost, "/hcaptcha", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
				require.NoError(t, err)
				defer res.Body.Close()

				resBody, err := io.ReadAll(res.Body)
				require.NoError(t, err)
				return res.StatusCode, string(resBody)
			}

			calls.Store(0)
			status, body := post(maxTokenLength)
			assert.Equal(t, fiber.StatusOK, status)
			assert.Equal(t, "ok", body)
			assert.Equal(t, int32(1), calls.Load())

			status, body = post(maxTokenLength + 1)
			assert.Equal(t, fiber.StatusBadRequest, status)
			assert.Equal(t, "Bad Request: hcaptcha token is too long", body)
			assert.Equal(t, int32(1), calls.Load(), "an oversized token must not reach the HCaptcha API")
		})
	}
}

func TestHCaptchaErrorResponses(t *testing.T) {
	tokenFunc := func(c fiber.Ctx) (string, error) {
		return TestResponseToken, nil
	}

	send := func(t *testing.T, app *fiber.App, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/hcaptcha", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
		require.NoError(t, err)
		defer res.Body.Close()

		resBody, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, string(resBody)
	}

	newApp := func(config Config, appConfig ...fiber.Config) *fiber.App {
		app := fiber.New(appConfig...)
		app.Post("/hcaptcha", New(config), func(c fiber.Ctx) error {
			return c.SendString("ok")
		})
		return app
	}

	t.Run("transport error detail is not sent to the client", func(t *testing.T) {
		server := newSiteVerifyServer(t, true)
		address := server.Listener.Addr().String()
		server.Close()

		status, body := send(t, newApp(Config{
			SecretKey:       TestSecretKey,
			SiteVerifyURL:   "http://" + address,
			ResponseKeyFunc: tokenFunc,
		}), "")
		assert.Equal(t, fiber.StatusInternalServerError, status)
		assert.Equal(t, "error sending request to HCaptcha API", body)
		assert.NotContains(t, body, address)
	})

	t.Run("decode error detail is not sent to the client", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>"))
		}))
		defer server.Close()

		status, body := send(t, newApp(Config{
			SecretKey:       TestSecretKey,
			SiteVerifyURL:   server.URL,
			ResponseKeyFunc: tokenFunc,
		}), "")
		assert.Equal(t, fiber.StatusInternalServerError, status)
		assert.Equal(t, "error decoding HCaptcha API response", body)
	})

	t.Run("missing token returns 400", func(t *testing.T) {
		status, body := send(t, newApp(Config{SecretKey: TestSecretKey}), `{}`)
		assert.Equal(t, fiber.StatusBadRequest, status)
		assert.Equal(t, "Bad Request: error retrieving HCaptcha token: hcaptcha token is empty", body)
	})

	t.Run("ResponseKeyFunc error stays reachable with errors.Is", func(t *testing.T) {
		errNoToken := errors.New("no token")
		var handled error
		app := newApp(Config{
			SecretKey: TestSecretKey,
			ResponseKeyFunc: func(fiber.Ctx) (string, error) {
				return "", errNoToken
			},
		}, fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error {
			handled = err
			return fiber.DefaultErrorHandler(c, err)
		}})

		status, _ := send(t, app, "")
		assert.Equal(t, fiber.StatusBadRequest, status)
		require.ErrorIs(t, handled, errNoToken)
		require.ErrorIs(t, handled, fiber.ErrBadRequest)
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
