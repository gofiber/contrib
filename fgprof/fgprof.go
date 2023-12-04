package fgprof

import (
	"github.com/felixge/fgprof"
	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

func New(conf ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(conf...)

	fgProfPath := cfg.Prefix + "/debug/fgprof"

	var fgprofHandler = fasthttpadaptor.NewFastHTTPHandlerFunc(fgprof.Handler().ServeHTTP)

	// Return new handler
	return func(c *fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		if c.Path() == fgProfPath {
			fgprofHandler(c.Context())
			return nil
		}
		return c.Next()
	}
}
