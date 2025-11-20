package storage

import (
	"context"
	"time"
)

// Storage defines the interface for rate limiter backends
type Storage interface {
	// Get retrieves the count for the given key
	Get(ctx context.Context, key string) (int, error)
	
	// Increment increments the count for the given key and sets expiration
	// Returns new count and whether the key was newly created
	Increment(ctx context.Context, key string, expiration time.Duration) (int, bool, error)
	
	// Decrement decrements the count for the given key
	// Returns new count after decrement
	Decrement(ctx context.Context, key string) (int, error)
	
	// Reset resets the count for the given key
	Reset(ctx context.Context, key string) error
	
	// Close closes the storage connection
	Close() error
}