package ratelimiter

import (
	"time"

	"github.com/gofiber/contrib/v3/ratelimiter/storage"
	"github.com/gofiber/fiber/v3"
)

// Config defines the configuration for rate limiter middleware
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(fiber.Ctx) bool

	// Max number of requests allowed within the expiration duration.
	//
	// Optional. Default: 10
	Max int

	// Expiration defines the duration for which the limit is enforced.
	//
	// Optional. Default: 1 minute
	Expiration time.Duration

	// KeyFunc defines a function to generate the rate limit key.
	// The key is used to identify the client for rate limiting.
	//
	// Optional. Default: func(c fiber.Ctx) string { return c.IP() }
	KeyFunc func(fiber.Ctx) string

	// LimitReached defines the response when rate limit is exceeded.
	//
	// Optional. Default: func(c fiber.Ctx) error {
	//   return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
	//     "error": "Too many requests",
	//   })
	// }
	LimitReached func(fiber.Ctx) error

	// Storage defines the storage backend for rate limiting data.
	//
	// Optional. Default: In-memory storage
	Storage storage.Storage

	// SkipFailedRequests when set to true, requests that return an error
	// or have a status code >= 400 won't consume the rate limit.
	//
	// Optional. Default: false
	SkipFailedRequests bool

	// SkipSuccessfulRequests when set to true, successful requests
	// won't consume the rate limit (status code < 400).
	//
	// Optional. Default: false
	SkipSuccessfulRequests bool

	// EnableDraftSpec when set to true, will add the following headers:
	// - X-RateLimit-Limit: maximum requests allowed
	// - X-RateLimit-Remaining: remaining requests in current window
	// - X-RateLimit-Reset: time when window resets (Unix timestamp)
	//
	// Optional. Default: false
	EnableDraftSpec bool
}

// ConfigDefault is the default config
var ConfigDefault = Config{
	Max:        10,
	Expiration: 1 * time.Minute,
	KeyFunc: func(c fiber.Ctx) string {
		return c.IP()
	},
	LimitReached: func(c fiber.Ctx) error {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"error": "Too many requests",
		})
	},
	SkipFailedRequests:     false,
	SkipSuccessfulRequests: false,
	EnableDraftSpec:        false,
}

// Helper function to set default config values
func configDefault(config ...Config) Config {
	if len(config) < 1 {
		return ConfigDefault
	}

	cfg := config[0]

	if cfg.Max <= 0 {
		cfg.Max = ConfigDefault.Max
	}
	if cfg.Expiration <= 0 {
		cfg.Expiration = ConfigDefault.Expiration
	}
	if cfg.KeyFunc == nil {
		cfg.KeyFunc = ConfigDefault.KeyFunc
	}
	if cfg.LimitReached == nil {
		cfg.LimitReached = ConfigDefault.LimitReached
	}
	if cfg.Storage == nil {
		cfg.Storage = storage.NewMemory()
	}

	return cfg
}