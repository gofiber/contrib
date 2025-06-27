package scalar

import (
	_ "embed"
	"fmt"
	"path"
	"text/template"

	"github.com/gofiber/fiber/v2"
	"github.com/swaggo/swag"
)

//go:embed scalar.min.js
var embeddedJS []byte

func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault

	// Override config if provided
	if len(config) > 0 {
		cfg = config[0]

		// Set default values
		if len(cfg.BasePath) == 0 {
			cfg.BasePath = configDefault.BasePath
		}
		if len(cfg.Path) == 0 {
			cfg.Path = configDefault.Path
		}
		if len(cfg.Title) == 0 {
			cfg.Title = configDefault.Title
		}
		if len(cfg.RawSpecUrl) == 0 {
			cfg.RawSpecUrl = configDefault.RawSpecUrl
		}
		if !cfg.ForceOffline {
			cfg.ForceOffline = configDefault.ForceOffline
		}
		if cfg.FallbackCacheAge == 0 {
			cfg.FallbackCacheAge = configDefault.FallbackCacheAge
		}
	}

	rawSpec := cfg.FileContentString
	if len(rawSpec) == 0 {
		doc, err := swag.ReadDoc()
		if err != nil {
			panic(err)
		}
		rawSpec = doc
	}

	cfg.FileContentString = string(rawSpec)

	scalarUIPath := path.Join(cfg.BasePath, cfg.Path)
	specURL := path.Join(scalarUIPath, cfg.RawSpecUrl)
	jsFallbackPath := path.Join(scalarUIPath, "/js/api-reference.min.js")

	html, err := template.New("index.html").Parse(templateHTML)
	if err != nil {
		panic(fmt.Errorf("failed to parse html template:%v", err))
	}

	htmlData := struct {
		Config
		Extra map[string]any
	}{
		Config: cfg,
		Extra:  map[string]any{},
	}

	htmlData.Extra["FallbackUrl"] = jsFallbackPath

	return func(ctx *fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(ctx) {
			return ctx.Next()
		}

		// fallback js
		if ctx.Path() == jsFallbackPath {
			ctx.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cfg.FallbackCacheAge))
			return ctx.Send(embeddedJS)
		}

		if cfg.CacheAge > 0 {
			ctx.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cfg.CacheAge))
		} else {
			ctx.Set("Cache-Control", "no-store")
		}

		if ctx.Path() == specURL {
			ctx.Set("Content-Type", "application/json")
			return ctx.SendString(rawSpec)
		}

		if ctx.Path() != scalarUIPath && ctx.Path() != specURL {
			return ctx.Next()
		}

		ctx.Type("html")
		return html.Execute(ctx, htmlData)
	}
}
