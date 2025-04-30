package scalar

import (
	"html/template"

	"github.com/gofiber/fiber/v2"
)

// Config defines the config for middleware.
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(c *fiber.Ctx) bool

	// BasePath for the UI path
	//
	// Optional. Default: /
	BasePath string

	// FilePath for the swagger.json or swagger.yaml file
	//
	// Optional. Default: ./docs/swagger.json
	FilePath string

	// FileContent for the content of the swagger.json or swagger.yaml file.
	//
	// Optional. Default: nil
	FileContent []byte

	// FileContent for the content of the swagger.json or swagger.yaml file.
	//
	// Optional. Default: nil
	FileContentString string

	// Path combines with BasePath for the full UI path
	//
	// Optional. Default: docs
	Path string

	// Title for the documentation site
	//
	// Optional. Default: Fiber API documentation
	Title string

	// CacheAge defines the max-age for the Cache-Control header in seconds.
	//
	// Optional. Default: 1 min (no cache)
	CacheAge int

	// Custom Scalar Style
	// Ref: https://github.com/scalar/scalar/blob/main/packages/themes/src/variables.css
	// Optional. Default: ""
	CustomStyle template.CSS

	// Proxy to avoid CORS issues
	// Optional. Default: "https://proxy.scalar.com"
	ProxyUrl string
}

var ConfigDefault = Config{
	Next:     nil,
	BasePath: "/",
	FilePath: "./docs/swagger.json",
	Path:     "docs",
	Title:    "Fiber API documentation",
	CacheAge: 60,
	ProxyUrl: "https://proxy.scalar.com",
}
