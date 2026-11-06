package swaggo

import (
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cache"
	"github.com/swaggo/swag"
)

type mockedSwag struct{}

func (s *mockedSwag) ReadDoc() string {
	return `{
    "swagger": "2.0",
    "info": {
        "description": "This is a sample server.",
        "title": "Swagger Example API",
        "termsOfService": "http://swagger.io/terms/",
        "contact": {
            "name": "API Support",
            "url": "http://www.swagger.io/support",
            "email": "support@swagger.io"
        },
        "license": {
            "name": "Apache 2.0",
            "url": "http://www.apache.org/licenses/LICENSE-2.0.html"
        },
        "version": "1.0"
    },
    "host": "petstore.swagger.io",
    "basePath": "/v2",
    "paths": {}
}`
}

var (
	registrationOnce sync.Once
)

func Test_Swagger(t *testing.T) {
	app := fiber.New()

	registrationOnce.Do(func() {
		swag.Register(swag.Name, &mockedSwag{})
	})

	app.Get("/swag/*", HandlerDefault)

	tests := []struct {
		name        string
		url         string
		statusCode  int
		contentType string
		location    string
	}{
		{
			name:        "Should be returns status 200 with 'text/html' content-type",
			url:         "/swag/index.html",
			statusCode:  200,
			contentType: "text/html",
		},
		{
			name:        "Should be returns status 200 with 'application/json' content-type",
			url:         "/swag/doc.json",
			statusCode:  200,
			contentType: "application/json",
		},
		{
			name:        "Should be returns status 200 with 'image/png' content-type",
			url:         "/swag/favicon-16x16.png",
			statusCode:  200,
			contentType: "image/png",
		},
		{
			name:       "Should return status 301",
			url:        "/swag/",
			statusCode: 301,
			location:   "/swag/index.html",
		},
		{
			name:       "Should return status 404",
			url:        "/swag/notfound",
			statusCode: 404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tt.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "localhost"

			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != tt.statusCode {
				t.Fatalf(`StatusCode: got %v - expected %v`, resp.StatusCode, tt.statusCode)
			}

			if tt.contentType != "" {
				ct := resp.Header.Get("Content-Type")
				mediaType, _, err := mime.ParseMediaType(ct)
				if err != nil {
					t.Fatalf("invalid content type %q: %v", ct, err)
				}
				if mediaType != tt.contentType {
					t.Fatalf(`Content-Type: got %s - expected %s`, mediaType, tt.contentType)
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

// app.Test connects from 0.0.0.0.
var trustTestProxy = fiber.Config{
	TrustProxy:       true,
	TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0"}},
}

func Test_Swagger_Proxy_Redirect(t *testing.T) {
	app := fiber.New(trustTestProxy)

	registrationOnce.Do(func() {
		swag.Register(swag.Name, &mockedSwag{})
	})

	// Use new handler since the prefix is created only once per handler
	app.Get("/swag/*", New())

	statusCode := 301
	location := "/custom/path/swag/index.html"

	t.Run("Should return status 301 with proxy redirect", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "/swag/", nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Host = "localhost"
		req.Header.Set("X-Forwarded-Prefix", "/custom/path/")

		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}

		if resp.StatusCode != statusCode {
			t.Fatalf(`StatusCode: got %v - expected %v`, resp.StatusCode, statusCode)
		}

		if location != "" {
			responseLocation := resp.Header.Get("Location")
			if responseLocation != location {
				t.Fatalf(`Location: got %s - expected %s`, responseLocation, location)
			}
		}
	})
}

func swaggerRequest(t *testing.T, app *fiber.App, url, forwardedPrefix string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"
	if forwardedPrefix != "" {
		req.Header.Set("X-Forwarded-Prefix", forwardedPrefix)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func Test_Swagger_Forwarded_Prefix_Requires_Trusted_Proxy(t *testing.T) {
	registrationOnce.Do(func() {
		swag.Register(swag.Name, &mockedSwag{})
	})

	configs := map[string]fiber.Config{
		"TrustProxy disabled": {},
		"peer not listed": {
			TrustProxy:       true,
			TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"10.0.0.1"}},
		},
	}

	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			app := fiber.New(cfg)
			app.Get("/swag/*", New())

			resp, _ := swaggerRequest(t, app, "/swag/", "/custom/path")
			if location := resp.Header.Get("Location"); location != "/swag/index.html" {
				t.Fatalf(`Location: got %s - expected /swag/index.html`, location)
			}
			if cc := resp.Header.Get("Cache-Control"); cc != "" {
				t.Fatalf(`Cache-Control: got %q - expected none`, cc)
			}

			_, body := swaggerRequest(t, app, "/swag/index.html", "/custom/path")
			if !strings.Contains(body, `"url":"/swag/doc.json"`) {
				t.Fatal(`index.html does not load the spec from /swag/doc.json`)
			}
		})
	}
}

func Test_Swagger_Forwarded_Prefix_Stays_On_Origin(t *testing.T) {
	registrationOnce.Do(func() {
		swag.Register(swag.Name, &mockedSwag{})
	})

	app := fiber.New(trustTestProxy)
	app.Get("/swag/*", New())

	tests := []struct {
		prefix   string
		location string
	}{
		{prefix: "/custom/../other//path/", location: "/other/path/swag/index.html"},
		{prefix: `\\other.example`, location: "/swag/index.html"},
		{prefix: `/\other.example`, location: "/swag/index.html"},
		{prefix: "//other.example", location: "/swag/index.html"},
		{prefix: "https://other.example", location: "/swag/index.html"},
		{prefix: "other.example", location: "/swag/index.html"},
		{prefix: "/custom?x=1", location: "/swag/index.html"},
		{prefix: "/custom#frag", location: "/swag/index.html"},
		{prefix: "/custom\tpath", location: "/swag/index.html"},
	}

	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			resp, _ := swaggerRequest(t, app, "/swag/", tt.prefix)
			if resp.StatusCode != fiber.StatusMovedPermanently {
				t.Fatalf(`StatusCode: got %v - expected %v`, resp.StatusCode, fiber.StatusMovedPermanently)
			}
			if location := resp.Header.Get("Location"); location != tt.location {
				t.Fatalf(`Location: got %s - expected %s`, location, tt.location)
			}

			_, body := swaggerRequest(t, app, "/swag/index.html", tt.prefix)
			specURL := strings.TrimSuffix(tt.location, "index.html") + "doc.json"
			if !strings.Contains(body, `"url":"`+specURL+`"`) {
				t.Fatalf(`index.html does not load the spec from %s`, specURL)
			}
		})
	}
}

func Test_Swagger_Forwarded_Prefix_Not_Shared_By_Cache(t *testing.T) {
	registrationOnce.Do(func() {
		swag.Register(swag.Name, &mockedSwag{})
	})

	app := fiber.New(trustTestProxy)
	app.Get("/swag/*", cache.New(), New())

	for _, url := range []string{"/swag/", "/swag/index.html"} {
		t.Run(url, func(t *testing.T) {
			resp, _ := swaggerRequest(t, app, url, "/other")
			if vary := resp.Header.Get("Vary"); !strings.Contains(vary, "X-Forwarded-Prefix") {
				t.Fatalf(`Vary: got %q - expected X-Forwarded-Prefix`, vary)
			}
			if cc := resp.Header.Get("Cache-Control"); cc != "private" {
				t.Fatalf(`Cache-Control: got %q - expected private`, cc)
			}

			resp, body := swaggerRequest(t, app, url, "")
			if location := resp.Header.Get("Location"); strings.Contains(location, "/other") {
				t.Fatalf(`Location: got %s from another client's cached response`, location)
			}
			if strings.Contains(body, "/other") {
				t.Fatal(`index.html is another client's cached response`)
			}
		})
	}
}
