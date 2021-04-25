package pasetoware

import (
	"errors"
	"github.com/gofiber/fiber/v2"
)

// New ...
func New(authConfigs ...Config) fiber.Handler {
	// Set default authConfig
	config := configDefault(authConfigs...)

	var extractor acquireToken
	switch config.TokenLookup[0] {
	case LookupHeader:
		extractor = acquireFromHeader
	case LookupQuery:
		extractor = acquireFromQuery
	case LookupParam:
		extractor = acquireFromParams
	case LookupCookie:
		extractor = acquireFromCookie
	}

	// Return middleware handler
	return func(c *fiber.Ctx) error {
		token := extractor(c, config.TokenLookup[1])
		// Filter request to skip middleware
		if config.Next != nil && config.Next(c) {
			return c.Next()
		}
		if token == "" {
			return config.ErrorHandler(c, errors.New("bad: missing PASETO token"))
		}

		var decryptedData []byte
		err := pasetoObject.Decrypt(token, config.SymmetricKey, &decryptedData, nil)
		if err == nil {
			var payload interface{}
			err, payload = config.Validate(decryptedData)

			if err == nil {
				// Store user information from token into context.
				c.Locals(config.ContextKey, payload)
				return config.SuccessHandler(c)
			}
		}
		return config.ErrorHandler(c, err)
	}
}
