package ratelimiter

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
)

// New creates a new rate limiter middleware
func New(config ...Config) fiber.Handler {
	cfg := configDefault(config...)

	return func(c fiber.Ctx) error {
		// Skip middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		// Generate rate limit key
		key := cfg.KeyFunc(c)

		// Get current count and increment
		count, isNew, err := cfg.Storage.Increment(c.Context(), key, cfg.Expiration)
		if err != nil {
			return err
		}

		// Calculate remaining requests
		remaining := cfg.Max - count
		if remaining < 0 {
			remaining = 0
		}

		// Set rate limit headers if enabled
		if cfg.EnableDraftSpec {
			c.Set("X-RateLimit-Limit", strconv.Itoa(cfg.Max))
			c.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			
			if isNew {
				resetTime := time.Now().Add(cfg.Expiration).Unix()
				c.Set("X-RateLimit-Reset", strconv.FormatInt(resetTime, 10))
			}
		}

		// Check if limit is exceeded
		if count > cfg.Max {
			return cfg.LimitReached(c)
		}

		// Continue with the request
		err = c.Next()

		// Handle post-request logic for SkipFailedRequests and SkipSuccessfulRequests
		if err != nil && cfg.SkipFailedRequests {
			// Decrement counter for failed requests
			cfg.Storage.Increment(c.Context(), key+":failed", cfg.Expiration)
		} else if err == nil {
			statusCode := c.Response().StatusCode()
			
			if statusCode >= 400 && cfg.SkipFailedRequests {
				// Decrement counter for failed status codes
				cfg.Storage.Increment(c.Context(), key+":failed", cfg.Expiration)
			} else if statusCode < 400 && cfg.SkipSuccessfulRequests {
				// For successful requests when SkipSuccessfulRequests is true,
				// we need to decrement the counter
				cfg.Storage.Increment(c.Context(), key+":success", cfg.Expiration)
			}
		}

		return err
	}
}

// Reset resets the rate limit for a given key
func Reset(storage interface{}, key string) error {
	if s, ok := storage.(interface {
		Reset(ctx interface{}, key string) error
	}); ok {
		return s.Reset(nil, key)
	}
	return nil
}