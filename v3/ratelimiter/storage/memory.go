package storage

import (
	"context"
	"sync"
	"time"
)

type entry struct {
	count      int
	expiration time.Time
}

// Memory is an in-memory storage implementation for rate limiter
type Memory struct {
	data   sync.Map
	ticker *time.Ticker
	done   chan struct{}
}

// NewMemory creates a new in-memory storage with automatic cleanup
func NewMemory() *Memory {
	m := &Memory{
		ticker: time.NewTicker(time.Minute),
		done:   make(chan struct{}),
	}
	
	// Start cleanup goroutine
	go m.cleanup()
	
	return m
}

// Get retrieves the count for the given key
func (m *Memory) Get(ctx context.Context, key string) (int, error) {
	if v, ok := m.data.Load(key); ok {
		e := v.(entry)
		if time.Now().Before(e.expiration) {
			return e.count, nil
		}
		m.data.Delete(key)
	}
	return 0, nil
}

// Increment increments the count for the given key and sets expiration
func (m *Memory) Increment(ctx context.Context, key string, expiration time.Duration) (int, bool, error) {
	now := time.Now()
	newExpiration := now.Add(expiration)
	
	if v, ok := m.data.Load(key); ok {
		e := v.(entry)
		if now.Before(e.expiration) {
			e.count++
			m.data.Store(key, e)
			return e.count, false, nil
		}
	}
	
	// Create new entry
	newEntry := entry{
		count:      1,
		expiration: newExpiration,
	}
	m.data.Store(key, newEntry)
	return 1, true, nil
}

// Reset resets the count for the given key
func (m *Memory) Reset(ctx context.Context, key string) error {
	m.data.Delete(key)
	return nil
}

// Close stops the cleanup goroutine and clears all data
func (m *Memory) Close() error {
	if m.done != nil {
		close(m.done)
	}
	if m.ticker != nil {
		m.ticker.Stop()
	}
	
	// Clear all data
	m.data.Range(func(key, value interface{}) bool {
		m.data.Delete(key)
		return true
	})
	
	return nil
}

// cleanup removes expired entries periodically
func (m *Memory) cleanup() {
	for {
		select {
		case <-m.ticker.C:
			now := time.Now()
			m.data.Range(func(key, value interface{}) bool {
				if e := value.(entry); now.After(e.expiration) {
					m.data.Delete(key)
				}
				return true
			})
		case <-m.done:
			return
		}
	}
}