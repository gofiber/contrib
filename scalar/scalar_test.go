package scalar

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/swaggo/swag"
)

type mock struct{}

func (m *mock) ReadDoc() string {
	return `
	{
  "openapi": "3.1.0",
  "info": {
    "title": "TestApi",
    "description": "Documentation for TestApi",
    "version": "1.0.0"
  },
  "servers": [
    {
      "url": "http://localhost/"
    }
  ],
  "paths": {},
  "components": {}
}
	`
}

var (
	registrationOnce sync.Once
)

func setupApp() *fiber.App {
	app := fiber.New()

	registrationOnce.Do(func() {
		swag.Register(swag.Name, &mock{})
	})

	return app
}

func TestDefault(t *testing.T) {
	app := setupApp()
	app.Use(New())

	tests := []struct {
		name        string
		url         string
		statusCode  int
		contentType string
		location    string
	}{
		{
			name:        "Should be returns status 200 with 'text/html' content-type",
			url:         "/docs",
			statusCode:  200,
			contentType: "text/html",
		},
		{
			name:        "Should be returns status 200 with 'application/json' content-type",
			url:         "/docs/doc.json",
			statusCode:  200,
			contentType: "application/json",
		},
		{
			name:       "Should return status 404",
			url:        "/docs/notfound",
			statusCode: 404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tt.url, nil)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != tt.statusCode {
				t.Fatalf(`StatusCode: got %v - expected %v`, resp.StatusCode, tt.statusCode)
			}

			if tt.contentType != "" {
				ct := resp.Header.Get("Content-Type")
				if ct != tt.contentType {
					t.Fatalf(`Content-Type: got %s - expected %s`, ct, tt.contentType)
				}
			}

			if tt.location != "" {
				location := resp.Header.Get("Location")
				if location != tt.location {
					t.Fatalf(`Location: got %s - expected %s`, location, tt.location)
				}
			}
		})
	}
}

func TestCustomBasePath(t *testing.T) {
	app := setupApp()
	app.Use(New(Config{
		BasePath: "/api",
	}))

	tests := []struct {
		name        string
		url         string
		statusCode  int
		contentType string
	}{
		{
			name:        "Should be returns status 200 with custom base path",
			url:         "/api/docs",
			statusCode:  200,
			contentType: "text/html",
		},
		{
			name:        "Should be returns status 200 for spec with custom base path",
			url:         "/api/docs/doc.json",
			statusCode:  200,
			contentType: "application/json",
		},
		{
			name:       "Should return status 404 for original path",
			url:        "/docs",
			statusCode: 404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			resp, err := app.Test(req)
			assert.NoError(t, err)
			assert.Equal(t, tt.statusCode, resp.StatusCode)

			if tt.contentType != "" {
				assert.Equal(t, tt.contentType, resp.Header.Get("Content-Type"))
			}
		})
	}
}

func TestCustomPath(t *testing.T) {
	app := setupApp()
	app.Use(New(Config{
		Path: "api-docs",
	}))

	tests := []struct {
		name        string
		url         string
		statusCode  int
		contentType string
	}{
		{
			name:        "Should be returns status 200 with custom path",
			url:         "/api-docs",
			statusCode:  200,
			contentType: "text/html",
		},
		{
			name:        "Should be returns status 200 for spec with custom path",
			url:         "/api-docs/doc.json",
			statusCode:  200,
			contentType: "application/json",
		},
		{
			name:       "Should return status 404 for original path",
			url:        "/docs",
			statusCode: 404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			resp, err := app.Test(req)
			assert.NoError(t, err)
			assert.Equal(t, tt.statusCode, resp.StatusCode)

			if tt.contentType != "" {
				assert.Equal(t, tt.contentType, resp.Header.Get("Content-Type"))
			}
		})
	}
}

func TestCustomTitle(t *testing.T) {
	app := setupApp()
	customTitle := "Custom API Documentation"
	app.Use(New(Config{
		Title: customTitle,
	}))

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Create a buffer to store the response body
	buf := new(strings.Builder)
	_, err = io.Copy(buf, resp.Body)
	assert.NoError(t, err)

	// Check if the custom title is in the HTML
	assert.Contains(t, buf.String(), fmt.Sprintf("<title>%s</title>", customTitle))
}

func TestCustomSpecUrl(t *testing.T) {
	app := setupApp()
	app.Use(New(Config{
		RawSpecUrl: "swagger.json",
	}))

	tests := []struct {
		name        string
		url         string
		statusCode  int
		contentType string
	}{
		{
			name:        "Should be returns status 200 with custom spec URL",
			url:         "/docs/swagger.json",
			statusCode:  200,
			contentType: "application/json",
		},
		{
			name:       "Should return status 404 for original spec URL",
			url:        "/docs/doc.json",
			statusCode: 404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			resp, err := app.Test(req)
			assert.NoError(t, err)
			assert.Equal(t, tt.statusCode, resp.StatusCode)

			if tt.contentType != "" {
				assert.Equal(t, tt.contentType, resp.Header.Get("Content-Type"))
			}
		})
	}
}

func TestCustomFileContent(t *testing.T) {
	app := setupApp()
	customSpec := `{"openapi":"3.0.0","info":{"title":"Custom API","version":"1.0.0"}}`

	app.Use(New(Config{
		FileContentString: customSpec,
	}))

	req := httptest.NewRequest(http.MethodGet, "/docs/doc.json", nil)
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	buf := new(strings.Builder)
	_, err = io.Copy(buf, resp.Body)
	assert.NoError(t, err)

	assert.Equal(t, customSpec, strings.TrimSpace(buf.String()))
}

func TestCacheControl(t *testing.T) {
	tests := []struct {
		name           string
		cacheAge       int
		expectedHeader string
	}{
		{
			name:           "Should set Cache-Control with custom max-age",
			cacheAge:       3600,
			expectedHeader: "public, max-age=3600",
		},
		{
			name:           "Should set Cache-Control to no-store when cache age is 0",
			cacheAge:       0,
			expectedHeader: "no-store",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := setupApp()
			app.Use(New(Config{
				CacheAge: tt.cacheAge,
			}))

			req := httptest.NewRequest(http.MethodGet, "/docs", nil)
			resp, err := app.Test(req)
			assert.NoError(t, err)
			assert.Equal(t, 200, resp.StatusCode)
			assert.Equal(t, tt.expectedHeader, resp.Header.Get("Cache-Control"))
		})
	}
}

func TestCustomStyle(t *testing.T) {
	app := setupApp()
	customStyle := "--primary-color: #ff0000; --font-size: 16px;"

	app.Use(New(Config{
		CustomStyle: template.CSS(customStyle),
	}))

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	buf := new(strings.Builder)
	_, err = io.Copy(buf, resp.Body)
	assert.NoError(t, err)

	assert.Contains(t, buf.String(), customStyle)
}

func TestNextFunction(t *testing.T) {
	app := setupApp()

	app.Use(New(Config{
		Next: func(c *fiber.Ctx) bool {
			return true
		},
	}))

	app.Get("/docs", func(c *fiber.Ctx) error {
		return c.SendString("Next handler called")
	})

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	buf := new(strings.Builder)
	_, err = io.Copy(buf, resp.Body)
	assert.NoError(t, err)

	assert.Equal(t, "Next handler called", buf.String())
}

func TestJSFallbackPath(t *testing.T) {
	app := setupApp()
	app.Use(New())

	req := httptest.NewRequest(http.MethodGet, "/docs/js/api-reference.min.js", nil)
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	buf := new(strings.Builder)
	_, err = io.Copy(buf, resp.Body)
	assert.NoError(t, err)
	assert.Greater(t, len(buf.String()), 0)
}

func TestCombinedConfigurations(t *testing.T) {
	app := setupApp()
	app.Use(New(Config{
		BasePath:   "/api",
		Path:       "swagger",
		RawSpecUrl: "openapi.json",
		CacheAge:   3600,
	}))

	tests := []struct {
		name        string
		url         string
		statusCode  int
		contentType string
	}{
		{
			name:        "Should be returns status 200 with combined configurations",
			url:         "/api/swagger",
			statusCode:  200,
			contentType: "text/html",
		},
		{
			name:        "Should be returns status 200 for spec with combined configurations",
			url:         "/api/swagger/openapi.json",
			statusCode:  200,
			contentType: "application/json",
		},
		{
			name:       "Should return status 404 for original paths",
			url:        "/docs",
			statusCode: 404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			resp, err := app.Test(req)
			assert.NoError(t, err)
			assert.Equal(t, tt.statusCode, resp.StatusCode)

			if tt.contentType != "" {
				assert.Equal(t, tt.contentType, resp.Header.Get("Content-Type"))
			}

			if tt.statusCode == 200 && tt.url == "/api/swagger" {
				assert.Equal(t, "public, max-age=3600", resp.Header.Get("Cache-Control"))
			}
		})
	}
}

func TestCorrectHtmlRendering(t *testing.T) {
	app := setupApp()
	app.Use(New())

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	buf := new(strings.Builder)
	_, err = io.Copy(buf, resp.Body)
	assert.NoError(t, err)

	htmlContent := buf.String()

	assert.Contains(t, htmlContent, "<!doctype html>")
	assert.Contains(t, htmlContent, "<div id=\"app\"></div>")
	assert.Contains(t, htmlContent, "function initScalar()")
	assert.Contains(t, htmlContent, "createApiReference('#app'")
}

func TestFallbackCache(t *testing.T) {
	app := setupApp()
	app.Use(New())

	req := httptest.NewRequest(http.MethodGet, "/docs/js/api-reference.min.js", nil)
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	assert.Equal(t, "public, max-age=86400", resp.Header.Get("Cache-Control"))
}
