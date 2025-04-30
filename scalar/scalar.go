package scalar

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"text/template"

	"github.com/gofiber/fiber/v2"
	"gopkg.in/yaml.v2"
)

func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := ConfigDefault

	// Override config if provided
	if len(config) > 0 {
		cfg = config[0]

		// Set default values
		if len(cfg.BasePath) == 0 {
			cfg.BasePath = ConfigDefault.BasePath
		}
		if len(cfg.FilePath) == 0 {
			cfg.FilePath = ConfigDefault.FilePath
		}
		if len(cfg.Path) == 0 {
			cfg.Path = ConfigDefault.Path
		}
		if len(cfg.Title) == 0 {
			cfg.Title = ConfigDefault.Title
		}
		if len(cfg.ProxyUrl) == 0 {
			cfg.ProxyUrl = ConfigDefault.ProxyUrl
		}
	}

	rawSpec := cfg.FileContent
	if len(rawSpec) == 0 {
		// Verify OpenAPI file exists
		_, err := os.Stat(cfg.FilePath)
		if os.IsNotExist(err) {
			panic(fmt.Errorf("%s file does not exist", cfg.FilePath))
		}

		// Read OpenAPI Spec into memory
		rawSpec, err = os.ReadFile(cfg.FilePath)
		if err != nil {
			panic(fmt.Errorf("Failed to read provided OpenAPI file (%s): %v", cfg.FilePath, err.Error()))
		}
	}

	// Validate we have valid JSON or YAML
	var jsonData map[string]interface{}
	errJSON := json.Unmarshal(rawSpec, &jsonData)
	var yamlData map[string]interface{}
	errYAML := yaml.Unmarshal(rawSpec, &yamlData)

	if errJSON != nil && errYAML != nil {
		fmt.Printf("Failed to parse the OpenAPI spec as JSON or YAML: JSON error: %s, YAML error: %s", errJSON, errYAML)
		if len(cfg.FileContent) != 0 {
			panic(fmt.Errorf("Invalid OpenAPI spec: %s", string(rawSpec)))
		}
		panic(fmt.Errorf("Invalid OpenAPI spec file: %s", cfg.FilePath))
	}

	cfg.FileContent = rawSpec
	cfg.FileContentString = string(rawSpec)

	// Generate URL path's for the middleware
	specURL := path.Join(cfg.BasePath, cfg.FilePath)
	scalarUIPath := path.Join(cfg.BasePath, cfg.Path)

	html, err := template.New("index.html").Parse(templateHTML)
	if err != nil {
		panic(fmt.Errorf("Failed to parse html template:%v", err))
	}

	return func(ctx *fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(ctx) {
			return ctx.Next()
		}

		// fallback js
		jsPath := path.Join(cfg.BasePath, "js/api-reference.min.js")
		if ctx.Path() == jsPath {
			scalarJS := "./scalar.min.js"
			if _, err := os.Stat(scalarJS); os.IsNotExist(err) {
				return ctx.Status(fiber.StatusNotFound).
					SendString("Scalar JS file not found")
			}
			return ctx.SendFile(scalarJS)
		}

		if cfg.CacheAge > 0 {
			ctx.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cfg.CacheAge))
		} else {
			ctx.Set("Cache-Control", "no-store")
		}

		if ctx.Path() == specURL {
			return ctx.Send(rawSpec)
		}

		if !(ctx.Path() == scalarUIPath || ctx.Path() == specURL) {
			return ctx.Next()
		}

		ctx.Type("html")
		return html.Execute(ctx, cfg)
	}
}
