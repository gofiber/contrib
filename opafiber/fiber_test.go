package opafiber

import (
	"bytes"
	"errors"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/stretchr/testify/assert"
	"io"
	"net/http/httptest"
	"testing"
)

func TestPanicWhenRegoQueryEmpty(t *testing.T) {
	app := fiber.New()

	assert.Panics(t, func() {
		app.Use(New(Config{}))
	})
}

func TestDefaultDeniedStatusCode400WhenConfigDeniedStatusCodeEmpty(t *testing.T) {
	app := fiber.New()
	module := `
package example.authz

import future.keywords

default allow := false
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		DeniedResponseMessage: "not allowed",
	}
	app.Use(New(cfg))

	app.Get("/", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(r, -1)

	assert.Equal(t, 400, resp.StatusCode)

	readedBytes, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "not allowed", utils.UnsafeString(readedBytes))
}

func TestOpaNotAllowedRegoPolicyShouldReturnConfigDeniedStatusCode(t *testing.T) {
	app := fiber.New()
	module := `
package example.authz

import future.keywords

default allow := false
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		DeniedStatusCode:      401,
		DeniedResponseMessage: "not allowed",
	}
	app.Use(New(cfg))

	app.Get("/", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(r, -1)

	assert.Equal(t, 401, resp.StatusCode)

	readedBytes, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "not allowed", utils.UnsafeString(readedBytes))
}

func TestOpaRequestMethodRegoPolicyShouldReturnConfigDeniedStatusCode(t *testing.T) {
	app := fiber.New()
	module := `
package example.authz

default allow := false

allow {
	input.method == "GET"
}
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		DeniedStatusCode:      fiber.StatusMethodNotAllowed,
		DeniedResponseMessage: "method not allowed",
	}
	app.Use(New(cfg))

	app.Get("/", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("POST", "/", nil)
	resp, _ := app.Test(r, -1)

	assert.Equal(t, fiber.StatusMethodNotAllowed, resp.StatusCode)

	readedBytes, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "method not allowed", utils.UnsafeString(readedBytes))
}

func TestOpaRequestPathRegoPolicyShouldReturnOK(t *testing.T) {
	app := fiber.New()
	module := `
package example.authz

default allow := false

allow {
	input.path == "/path"
}
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		DeniedStatusCode:      fiber.StatusOK,
		DeniedResponseMessage: "OK",
	}
	app.Use(New(cfg))

	app.Post("/path", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("POST", "/path", nil)
	resp, _ := app.Test(r, -1)

	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	readedBytes, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "OK", utils.UnsafeString(readedBytes))
}

func TestOpaQueryStringRegoPolicyShouldReturnOK(t *testing.T) {
	app := fiber.New()
	module := `
package example.authz

import future.keywords.in

default allow := false

allow {
	input.query == {"testKey": ["testVal"]}
}
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		IncludeQueryString:    true,
		DeniedStatusCode:      fiber.StatusBadRequest,
		DeniedResponseMessage: "bad request",
	}
	app.Use(New(cfg))

	app.Get("/test", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/test?testKey=testVal", nil)
	resp, _ := app.Test(r, -1)

	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	readedBytes, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "OK", utils.UnsafeString(readedBytes))
}

func TestOpaRequestHeadersRegoPolicyShouldReturnOK(t *testing.T) {
	app := fiber.New()
	module := `
package example.authz

import future.keywords.in

default allow := false

allow {
	input.headers == {"testHeaderKey": "testHeaderVal"}
}
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		IncludeQueryString:    true,
		DeniedStatusCode:      fiber.StatusBadRequest,
		DeniedResponseMessage: "bad request",
		IncludeHeaders:        []string{"testHeaderKey"},
	}
	app.Use(New(cfg))

	app.Get("/headers", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/headers", nil)
	r.Header.Set("testHeaderKey", "testHeaderVal")
	resp, _ := app.Test(r, -1)

	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	readedBytes, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "OK", utils.UnsafeString(readedBytes))
}

func TestOpaRequestWithCustomInput(t *testing.T) {
	app := fiber.New()
	module := `
package example.authz

default allow := false

allow {
	input.custom == "test"
}
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		IncludeQueryString:    true,
		DeniedStatusCode:      fiber.StatusBadRequest,
		DeniedResponseMessage: "bad request",
		InputCreationMethod: func(c *fiber.Ctx) (map[string]interface{}, error) {
			return map[string]interface{}{
				"custom": "test",
			}, nil
		},
	}
	app.Use(New(cfg))

	app.Get("/headers", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/headers", nil)
	resp, _ := app.Test(r, -1)

	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	readedBytes, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "OK", utils.UnsafeString(readedBytes))
}

func TestOpaRequestWithCustomInputError(t *testing.T) {
	app := fiber.New()
	module := `
package example.authz

default allow := false

allow {
	input.custom == "test"
}
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		IncludeQueryString:    true,
		DeniedStatusCode:      fiber.StatusBadRequest,
		DeniedResponseMessage: "bad request",
		InputCreationMethod: func(c *fiber.Ctx) (map[string]interface{}, error) {
			return nil, errors.New("test error")
		},
	}
	app.Use(New(cfg))

	app.Get("/headers", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/headers", nil)
	resp, _ := app.Test(r, -1)

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)

	readedBytes, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "test error", utils.UnsafeString(readedBytes))
}

func TestFillAndValidate(t *testing.T) {
	cfg := Config{}
	err := cfg.fillAndValidate()
	assert.Error(t, err)
	cfg = Config{
		RegoPolicy: bytes.NewBufferString("test"),
	}
	err = cfg.fillAndValidate()
	assert.Error(t, err)
	cfg = Config{
		RegoPolicy: bytes.NewBufferString("test"),
		RegoQuery:  "test",
	}
	err = cfg.fillAndValidate()
	assert.NoError(t, err)

	assert.Equal(t, cfg.DeniedStatusCode, fiber.StatusBadRequest)
	assert.Equal(t, cfg.DeniedResponseMessage, fiber.ErrBadRequest.Error())
	assert.IsType(t, cfg.InputCreationMethod, InputCreationFunc(nil))
	assert.IsType(t, cfg.IncludeHeaders, []string(nil))
}
