package swagger

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/go-openapi/runtime/middleware"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"gopkg.in/yaml.v2"
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
	// Optional. Default: ./swagger.json
	FilePath string

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
	// Optional. Default: 3600 (1 hour)
	CacheAge int
}

// ConfigDefault is the default config
var ConfigDefault = Config{
	Next:     nil,
	BasePath: "/",
	FilePath: "./swagger.json",
	Path:     "docs",
	Title:    "Fiber API documentation",
	CacheAge: 3600, // Default to 1 hour
}

// New creates a new middleware handler
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
		if cfg.CacheAge == 0 {
			cfg.CacheAge = ConfigDefault.CacheAge
		}
	}

	// Verify Swagger file exists
	if _, err := os.Stat(cfg.FilePath); os.IsNotExist(err) {
		panic(fmt.Errorf("%s file does not exist", cfg.FilePath))
	}

	// Read Swagger Spec into memory
	rawSpec, err := os.ReadFile(cfg.FilePath)
	if err != nil {
		log.Fatalf("Failed to read provided Swagger file (%s): %v", cfg.FilePath, err.Error())
		panic(err)
	}

	// Validate we have valid JSON or YAML
	var jsonData map[string]interface{}
	errJSON := json.Unmarshal(rawSpec, &jsonData)
	var yamlData map[string]interface{}
	errYAML := yaml.Unmarshal(rawSpec, &yamlData)

	if errJSON != nil && errYAML != nil {
		log.Fatalf("Failed to parse the Swagger spec as JSON or YAML: JSON error: %s, YAML error: %s", errJSON, errYAML)
		panic(fmt.Errorf("Invalid Swagger spec file: %s", cfg.FilePath))
	}

	// Generate URL path's for the middleware
	specURL := path.Join(cfg.BasePath, cfg.FilePath)
	swaggerUIPath := path.Join(cfg.BasePath, cfg.Path)

	// Serve the Swagger spec from memory
	swaggerSpecHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".yaml") || strings.HasSuffix(r.URL.Path, ".yml") {
			w.Header().Set("Content-Type", "application/yaml")
			w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cfg.CacheAge))
			_, err := w.Write(rawSpec)
			if err != nil {
				http.Error(w, "Error processing YAML Swagger Spec", http.StatusInternalServerError)
				return
			}
		} else if strings.HasSuffix(r.URL.Path, ".json") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cfg.CacheAge))
			_, err := w.Write(rawSpec)
			if err != nil {
				http.Error(w, "Error processing JSON Swagger Spec", http.StatusInternalServerError)
				return
			}
		} else {
			http.NotFound(w, r)
		}
	})

	// Define UI Options
	swaggerUIOpts := middleware.SwaggerUIOpts{
		BasePath: cfg.BasePath,
		SpecURL:  specURL,
		Path:     cfg.Path,
		Title:    cfg.Title,
	}

	// Create UI middleware
	middlewareHandler := adaptor.HTTPHandler(middleware.SwaggerUI(swaggerUIOpts, swaggerSpecHandler))

	// Return new handler
	return func(c *fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		// Only respond to requests to SwaggerUI and SpecURL (swagger.json)
		if !(c.Path() == swaggerUIPath || c.Path() == specURL) {
			return c.Next()
		}

		// Pass Fiber context to handler
		return middlewareHandler(c)
	}
}
