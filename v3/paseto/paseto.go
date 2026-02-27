package pasetoware

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
)

// The contextKey type is unexported to prevent collisions with context keys defined in
// other packages.
type contextKey int

// The following contextKey values are defined to store values in context.
const (
	payloadKey contextKey = iota
)

// New PASETO middleware, returns a handler that takes a token in selected lookup param and in case token is valid
// it saves the decrypted token on ctx.Locals, take a look on Config to know more configuration options
func New(authConfigs ...Config) fiber.Handler {
	// Set default authConfig
	config := configDefault(authConfigs...)

	// Return middleware handler
	return func(c fiber.Ctx) error {
		// Filter request to skip middleware
		if config.Next != nil && config.Next(c) {
			return c.Next()
		}

		token, err := config.Extractor.Extract(c)
		if err != nil {
			if errors.Is(err, extractors.ErrNotFound) {
				return config.ErrorHandler(c, ErrMissingToken)
			}
			return config.ErrorHandler(c, err)
		}

		var outData []byte

		if config.SymmetricKey != nil {
			if err := pasetoObject.Decrypt(token, config.SymmetricKey, &outData, nil); err != nil {
				return config.ErrorHandler(c, err)
			}
		} else {
			if err := pasetoObject.Verify(token, config.PublicKey, &outData, nil); err != nil {
				return config.ErrorHandler(c, err)
			}
		}

		payload, err := config.Validate(outData)
		if err == nil {
			// Store user information from token into context.
			fiber.StoreInContext(c, payloadKey, payload)

			return config.SuccessHandler(c)
		}

		return config.ErrorHandler(c, err)
	}
}

// FromContext returns the payload from the context.
// It accepts fiber.CustomCtx, fiber.Ctx, *fasthttp.RequestCtx, and context.Context.
func FromContext(ctx any) interface{} {
	payload, _ := fiber.ValueFromContext[interface{}](ctx, payloadKey)
	return payload
}
