package scalar

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"

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
	// Optional. Default: ./docs/swagger.json
	FilePath string

	// FileContent for the content of the swagger.json or swagger.yaml file.
	// If provided, FilePath will not be read.
	//
	// Optional. Default: nil
	FileContent []byte

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
	// Optional. Default: 0 (no cache)
	CacheAge int
}

var ConfigDefault = Config{
	Next:     nil,
	BasePath: "/",
	FilePath: "./docs/swagger.json",
	Path:     "docs",
	Title:    "Fiber API documentation",
	CacheAge: 0,
}

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

	rawSpec := cfg.FileContent
	if len(rawSpec) == 0 {
		// Verify Swagger file exists
		_, err := os.Stat(cfg.FilePath)
		if os.IsNotExist(err) {
			panic(fmt.Errorf("%s file does not exist", cfg.FilePath))
		}

		// Read Swagger Spec into memory
		rawSpec, err = os.ReadFile(cfg.FilePath)
		if err != nil {
			log.Fatalf("Failed to read provided Swagger file (%s): %v", cfg.FilePath, err.Error())
			panic(err)
		}
	}

	// Validate we have valid JSON or YAML
	var jsonData map[string]interface{}
	errJSON := json.Unmarshal(rawSpec, &jsonData)
	var yamlData map[string]interface{}
	errYAML := yaml.Unmarshal(rawSpec, &yamlData)

	if errJSON != nil && errYAML != nil {
		log.Fatalf("Failed to parse the Swagger spec as JSON or YAML: JSON error: %s, YAML error: %s", errJSON, errYAML)
		if len(cfg.FileContent) != 0 {
			panic(fmt.Errorf("Invalid Swagger spec: %s", string(rawSpec)))
		}
		panic(fmt.Errorf("Invalid Swagger spec file: %s", cfg.FilePath))
	}

	// Generate URL path's for the middleware
	specURL := path.Join(cfg.BasePath, cfg.FilePath)
	swaggerUIPath := path.Join(cfg.BasePath, cfg.Path)

	// Serve the Swagger spec from memory
	swaggerSpecHandler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "docs") || strings.HasSuffix(r.URL.Path, "docs/") {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cfg.CacheAge))
			_, err := w.Write([]byte(getHtmlByContent(string(rawSpec))))
			if err != nil {
				http.Error(w, "Error processing HTML Swagger Spec", http.StatusInternalServerError)
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
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
	}

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
		return adaptor.HTTPHandlerFunc(swaggerSpecHandler)(c)
	}
}

func getHtmlByContent(content string) string {
	return fmt.Sprintf(`
	<!doctype html>
<html>
  <head>
    <title>Scalar API Reference</title>
    <meta charset="utf-8" />
    <meta
      name="viewport"
      content="width=device-width, initial-scale=1" />
  </head>

  <body>
    <div id="app"></div>

    <!-- Load the Script -->
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>

    <!-- Initialize the Scalar API Reference -->
    <script>
      Scalar.createApiReference('#app', {
        // The URL of the OpenAPI/Swagger document
        content: %q,
        // Avoid CORS issues
        proxyUrl: 'https://proxy.scalar.com',
      })
    </script>
  </body>
</html>`, content)
}
