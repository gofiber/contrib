package scalar

import (
	_ "embed"
	"net/http"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"
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

func TestDefault(t *testing.T) {
	app := fiber.New()

	registrationOnce.Do(func() {
		swag.Register(swag.Name, &mock{})
	})

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
		// {
		// 	name:        "Should be returns status 200 with 'image/png' content-type",
		// 	url:         "/swag/favicon-16x16.png",
		// 	statusCode:  200,
		// 	contentType: "image/png",
		// },
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
