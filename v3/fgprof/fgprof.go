package fgprof

import (
	"github.com/felixge/fgprof"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
)

func New(conf ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(conf...)

	fgProfPath := cfg.Prefix + "/debug/fgprof"

	var fgprofHandler = adaptor.HTTPHandler(fgprof.Handler())

	// Return new handler
	return func(c fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		if c.Path() == fgProfPath {
			return fgprofHandler(c)
		}
		return c.Next()
	}
}
