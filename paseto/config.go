package pasetoware

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/o1egl/paseto"
	"golang.org/x/crypto/chacha20poly1305"
)

// Config defines the config for PASETO middleware
type Config struct {
	// Filter defines a function to skip middleware.
	// Optional. Default: nil
	Next func(*fiber.Ctx) bool

	// SuccessHandler defines a function which is executed for a valid token.
	// Optional. Default: c.Next()
	SuccessHandler fiber.Handler

	// ErrorHandler defines a function which is executed for an invalid token.
	// It may be used to define a custom PASETO error.
	// Optional. Default: 401 Invalid or expired PASETO
	ErrorHandler fiber.ErrorHandler

	// Validate defines a function to validate if payload is valid
	// Optional. In case payload used is created using CreateToken function
	// If token is created using another function, this function must be provided
	Validate PayloadValidator

	// SymmetricKey to validate token.
	// Required.
	SymmetricKey []byte

	// ContextKey to store user information from the token into context.
	// Optional. Default: DefaultContextKey.
	ContextKey string

	// TokenLookup is a string slice with size 2, that is used to extract token from the request.
	// Optional. Default value ["header","Authorization"].
	// Possible values:
	// - ["header","<name>"]
	// - ["query","<name>"]
	// - ["param","<name>"]
	// - ["cookie","<name>"]
	TokenLookup [2]string
}

// ConfigDefault is the default config
var ConfigDefault = Config{
	Next:           nil,
	SuccessHandler: nil,
	ErrorHandler:   nil,
	Validate:       nil,
	SymmetricKey:   nil,
	ContextKey:     DefaultContextKey,
	TokenLookup:    [2]string{LookupHeader, fiber.HeaderAuthorization},
}

// Helper function to set default values
func configDefault(authConfigs ...Config) Config {
	// Return default authConfigs if nothing provided

	config := ConfigDefault
	if len(authConfigs) > 0 {
		// Override default authConfigs
		config = authConfigs[0]
	}

	// Set default values
	if config.Next == nil {
		config.Next = ConfigDefault.Next
	}

	if config.SuccessHandler == nil {
		config.SuccessHandler = func(c *fiber.Ctx) error {
			return c.Next()
		}
	}

	if config.ErrorHandler == nil {
		config.ErrorHandler = func(c *fiber.Ctx, err error) error {
			errorStatus := fiber.StatusUnauthorized
			if errors.Is(err, ErrMissingToken) {
				errorStatus = fiber.StatusBadRequest
			}
			return c.Status(errorStatus).SendString(err.Error())
		}
	}

	if config.Validate == nil {
		config.Validate = func(data []byte) (interface{}, error) {
			var payload paseto.JSONToken
			if err := json.Unmarshal(data, &payload); err != nil {
				return nil, ErrDataUnmarshal
			}

			if err := payload.Validate(
				paseto.ValidAt(time.Now()), paseto.Subject(pasetoTokenSubject),
				paseto.ForAudience(pasetoTokenAudience),
			); err != nil {
				return "", err
			}

			if time.Now().After(payload.Expiration) {
				return nil, ErrExpiredToken
			}
			return payload.Get(pasetoTokenField), nil
		}
	}

	if config.ContextKey == "" {
		config.ContextKey = ConfigDefault.ContextKey
	}

	if config.TokenLookup[0] == "" {
		config.TokenLookup[0] = ConfigDefault.TokenLookup[0]
	}
	if config.TokenLookup[1] == "" {
		config.TokenLookup[1] = ConfigDefault.TokenLookup[1]
	}

	if len(config.SymmetricKey) != chacha20poly1305.KeySize {
		panic(
			fmt.Sprintf(
				"Fiber: PASETO middleware requires a symmetric key with size %d",
				chacha20poly1305.KeySize,
			),
		)
	}

	return config
}
