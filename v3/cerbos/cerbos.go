// Package cerbos authorizes Fiber requests with a Cerbos policy decision point.
package cerbos

import (
	"errors"

	"github.com/gofiber/fiber/v3"
)

// ErrMissingResource is passed to ErrorHandler when Resource returns neither a
// resource nor an error.
var ErrMissingResource = errors.New("cerbos: Resource returned no resource")

// New creates a middleware that lets a request through only if Cerbos allows
// its principal to perform its action on its resource. It panics if Client,
// Principal or Resource is missing.
func New(config Config) fiber.Handler {
	cfg := configDefault(config)

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		principal, err := cfg.Principal(c)
		if err != nil {
			return cfg.ErrorHandler(c, err)
		}
		if principal == nil {
			return cfg.Unauthorized(c)
		}

		resource, err := cfg.Resource(c)
		if err != nil {
			return cfg.ErrorHandler(c, err)
		}
		if resource == nil {
			return cfg.ErrorHandler(c, ErrMissingResource)
		}

		allowed, err := cfg.Client.IsAllowed(c.Context(), principal, resource, cfg.Action(c))
		if err != nil {
			return cfg.ErrorHandler(c, err)
		}
		if !allowed {
			return cfg.Forbidden(c)
		}

		return c.Next()
	}
}
