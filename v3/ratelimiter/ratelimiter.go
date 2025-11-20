package ratelimiter

import (
	"context"
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
		shouldDecrement := false
		
		if err != nil && cfg.SkipFailedRequests {
			shouldDecrement = true
		} else if err == nil {
			statusCode := c.Response().StatusCode()
			
			if statusCode >= 400 && cfg.SkipFailedRequests {
				shouldDecrement = true
			} else if statusCode < 400 && cfg.SkipSuccessfulRequests {
				shouldDecrement = true
			}
		}

		// Decrement counter if the request should be skipped
		if shouldDecrement {
			cfg.Storage.Decrement(c.Context(), key)
		}

		return err
	}
}

// Reset resets the rate limit for a given key
func Reset(ctx context.Context, storage interface{}, key string) error {
	if s, ok := storage.(interface {
		Reset(ctx context.Context, key string) error
	}); ok {
		return s.Reset(ctx, key)
	}
	return nil
}