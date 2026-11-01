package cerbos

import (
	"context"

	"github.com/cerbos/cerbos-sdk-go/cerbos"
	"github.com/gofiber/fiber/v3"
)

// Client is the part of a Cerbos client the middleware needs.
// *cerbos.GRPCClient implements it.
type Client interface {
	IsAllowed(ctx context.Context, principal *cerbos.Principal, resource *cerbos.Resource, action string) (bool, error)
}

var _ Client = (*cerbos.GRPCClient)(nil)

// Config holds the configuration for the middleware.
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(fiber.Ctx) bool

	// Client checks access. The caller owns it and closes it on shutdown.
	//
	// Required.
	Client Client

	// Principal builds the principal from the request. A nil principal with a
	// nil error means the request is unauthenticated and goes to Unauthorized.
	//
	// Required.
	Principal func(fiber.Ctx) (*cerbos.Principal, error)

	// Resource builds the resource from the request.
	//
	// Required.
	Resource func(fiber.Ctx) (*cerbos.Resource, error)

	// Action names the action to check.
	//
	// Optional. Default: the HTTP method of the request.
	Action func(fiber.Ctx) string

	// Unauthorized handles a request without a principal.
	//
	// Optional. Default: 401 Unauthorized
	Unauthorized fiber.Handler

	// Forbidden handles a request Cerbos denies.
	//
	// Optional. Default: 403 Forbidden
	Forbidden fiber.Handler

	// ErrorHandler handles an error from Principal, Resource or the Client, and
	// ErrMissingResource. The request does not reach the next handler either way.
	//
	// Optional. Default: 500 Internal Server Error, without the error's text.
	ErrorHandler fiber.ErrorHandler
}

func configDefault(cfg Config) Config {
	if cfg.Client == nil {
		panic("cerbos: Client is required")
	}
	if cfg.Principal == nil {
		panic("cerbos: Principal is required")
	}
	if cfg.Resource == nil {
		panic("cerbos: Resource is required")
	}
	if cfg.Action == nil {
		cfg.Action = func(c fiber.Ctx) string { return c.Method() }
	}
	if cfg.Unauthorized == nil {
		cfg.Unauthorized = func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusUnauthorized) }
	}
	if cfg.Forbidden == nil {
		cfg.Forbidden = func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusForbidden) }
	}
	if cfg.ErrorHandler == nil {
		cfg.ErrorHandler = func(c fiber.Ctx, _ error) error { return c.SendStatus(fiber.StatusInternalServerError) }
	}
	return cfg
}
