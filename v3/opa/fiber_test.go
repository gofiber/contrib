package opa

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	"github.com/stretchr/testify/assert"
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

	app.Get("/", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

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

	app.Get("/", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

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

allow if {
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

	app.Get("/", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("POST", "/", nil)
	resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

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

allow if {
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

	app.Post("/path", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("POST", "/path", nil)
	resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

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

allow if {
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

	app.Get("/test", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/test?testKey=testVal", nil)
	resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

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

allow if {
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

	app.Get("/headers", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/headers", nil)
	r.Header.Set("testHeaderKey", "testHeaderVal")
	resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

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

allow if {
        input.custom == "test"
}
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		IncludeQueryString:    true,
		DeniedStatusCode:      fiber.StatusBadRequest,
		DeniedResponseMessage: "bad request",
		InputCreationMethod: func(c fiber.Ctx) (map[string]interface{}, error) {
			return map[string]interface{}{
				"custom": "test",
			}, nil
		},
	}
	app.Use(New(cfg))

	app.Get("/headers", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/headers", nil)
	resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

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

allow if {
        input.custom == "test"
}
`

	cfg := Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		IncludeQueryString:    true,
		DeniedStatusCode:      fiber.StatusBadRequest,
		DeniedResponseMessage: "bad request",
		InputCreationMethod: func(c fiber.Ctx) (map[string]interface{}, error) {
			return nil, errors.New("test error")
		},
	}
	app.Use(New(cfg))

	app.Get("/headers", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	r := httptest.NewRequest("GET", "/headers", nil)
	resp, _ := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

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

func TestOpaRequestPathRegoPolicyShouldCoverRouterSpelling(t *testing.T) {
	module := `
package example.authz

default allow := false

allow if {
	not startswith(input.path, "/admin")
	input.path != "/private"
	input.path != "/doc/ABC"
}
`

	testCases := []struct {
		config     fiber.Config
		path       string
		statusCode int
	}{
		{path: "/path", statusCode: 200},
		{path: "/PATH/", statusCode: 200},
		{path: "/admin/secret", statusCode: 400},
		{path: "/ADMIN/secret", statusCode: 400},
		{path: "/%41dmin/secret/", statusCode: 400},
		{path: "/private", statusCode: 400},
		{path: "/private/", statusCode: 400},
		{path: "/doc/abc", statusCode: 200},
		{path: "/doc/ABC", statusCode: 400},
		{config: fiber.Config{CaseSensitive: true}, path: "/ADMIN/secret", statusCode: 404},
		{config: fiber.Config{StrictRouting: true}, path: "/private/", statusCode: 404},
	}

	ok := func(ctx fiber.Ctx) error {
		return ctx.SendStatus(200)
	}

	for _, tC := range testCases {
		app := fiber.New(tC.config)
		app.Use(New(Config{
			RegoQuery:  "data.example.authz.allow",
			RegoPolicy: bytes.NewBufferString(module),
		}))
		app.Get("/path", ok)
		app.Get("/admin/secret", ok)
		app.Get("/private", ok)
		app.Get("/doc/:id", ok)

		r := httptest.NewRequest("GET", tC.path, nil)
		resp, err := app.Test(r, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})

		assert.NoError(t, err)
		assert.Equal(t, tC.statusCode, resp.StatusCode, tC.path)
	}
}
