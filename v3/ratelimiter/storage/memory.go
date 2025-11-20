package storage

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type entry struct {
	count      int64
	expiration time.Time
	mutex      sync.RWMutex
}

// Memory is an in-memory storage implementation for rate limiter
type Memory struct {
	data   sync.Map
	ticker *time.Ticker
	done   chan struct{}
	mu     sync.RWMutex
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
		e, ok := v.(*entry)
		if !ok {
			return 0, fmt.Errorf("invalid entry type")
		}
		e.mutex.RLock()
		defer e.mutex.RUnlock()
		if time.Now().Before(e.expiration) {
			return int(atomic.LoadInt64(&e.count)), nil
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
		e, ok := v.(*entry)
		if !ok {
			return 0, false, fmt.Errorf("invalid entry type")
		}
		e.mutex.RLock()
		expired := now.After(e.expiration)
		e.mutex.RUnlock()
		
		if !expired {
			newCount := atomic.AddInt64(&e.count, 1)
			return int(newCount), false, nil
		}
		m.data.Delete(key)
	}
	
	// Create new entry
	newEntry := &entry{
		count:      1,
		expiration: newExpiration,
	}
	m.data.Store(key, newEntry)
	return 1, true, nil
}

// Decrement decrements the count for the given key
func (m *Memory) Decrement(ctx context.Context, key string) (int, error) {
	if v, ok := m.data.Load(key); ok {
		e, ok := v.(*entry)
		if !ok {
			return 0, fmt.Errorf("invalid entry type")
		}
		e.mutex.RLock()
		expired := time.Now().After(e.expiration)
		e.mutex.RUnlock()
		
		if !expired {
			newCount := atomic.AddInt64(&e.count, -1)
			if newCount < 0 {
				atomic.StoreInt64(&e.count, 0)
				return 0, nil
			}
			return int(newCount), nil
		}
		m.data.Delete(key)
	}
	return 0, nil
}

// Reset resets the count for the given key
func (m *Memory) Reset(ctx context.Context, key string) error {
	m.data.Delete(key)
	return nil
}

// Close stops the cleanup goroutine and clears all data
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if m.done != nil {
		close(m.done)
		m.done = nil
	}
	if m.ticker != nil {
		m.ticker.Stop()
		m.ticker = nil
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
		m.mu.RLock()
		ticker := m.ticker
		done := m.done
		m.mu.RUnlock()
		
		if ticker == nil || done == nil {
			return
		}
		
		select {
		case <-ticker.C:
			now := time.Now()
			m.data.Range(func(key, value interface{}) bool {
				if e, ok := value.(*entry); ok {
					e.mutex.RLock()
					expired := now.After(e.expiration)
					e.mutex.RUnlock()
					if expired {
						m.data.Delete(key)
					}
				}
				return true
			})
		case <-done:
			return
		}
	}
}