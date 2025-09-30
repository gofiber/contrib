package circuitbreaker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// mockTime helps control time for deterministic testing
type mockTime struct {
	mu      sync.Mutex
	current time.Time
}

func newMockTime(t time.Time) *mockTime {
	return &mockTime{current: t}
}

func (m *mockTime) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

func (m *mockTime) Add(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = m.current.Add(d)
}

// TestCircuitBreakerStates tests each state transition of the circuit breaker
func TestCircuitBreakerStates(t *testing.T) {
	mockClock := newMockTime(time.Now())

	// Create circuit breaker with test config
	cb := New(Config{
		FailureThreshold:      2,
		Timeout:               5 * time.Second,
		SuccessThreshold:      2,
		HalfOpenMaxConcurrent: 1,
	})

	// Override the time function
	cb.now = mockClock.Now

	// Test initial state
	t.Run("Initial State", func(t *testing.T) {
		require.Equal(t, StateClosed, cb.GetState())
		allowed, state := cb.AllowRequest()
		require.True(t, allowed)
		require.Equal(t, StateClosed, state)
	})

	// Test transition to open state
	t.Run("Transition to Open", func(t *testing.T) {
		// Report failures to trip the circuit
		cb.ReportFailure()
		require.Equal(t, StateClosed, cb.GetState())

		cb.ReportFailure() // This should trip the circuit
		require.Equal(t, StateOpen, cb.GetState())

		allowed, state := cb.AllowRequest()
		require.False(t, allowed)
		require.Equal(t, StateOpen, state)
	})

	// Test transition to half-open state
	t.Run("Transition to HalfOpen", func(t *testing.T) {
		// Advance time past the timeout to trigger half-open
		mockClock.Add(6 * time.Second)

		// Force timer activation by checking state
		// (In real usage this would happen automatically with timer)
		if cb.openTimer != nil {
			cb.openTimer.Stop()
			cb.transitionToHalfOpen()
		}

		require.Equal(t, StateHalfOpen, cb.GetState())

		allowed, state := cb.AllowRequest()
		require.True(t, allowed)
		require.Equal(t, StateHalfOpen, state)

		// Release the semaphore for next test
		cb.ReleaseSemaphore()
	})

	// Test half-open limited concurrency
	t.Run("HalfOpen Limited Concurrency", func(t *testing.T) {
		// Try to allow two concurrent requests when only one is permitted
		allowed1, _ := cb.AllowRequest()
		allowed2, _ := cb.AllowRequest()

		require.True(t, allowed1)
		require.False(t, allowed2)

		// Release the semaphore
		cb.ReleaseSemaphore()
	})

	// Test transition back to open on failure in half-open
	t.Run("HalfOpen to Open on Failure", func(t *testing.T) {
		allowed, _ := cb.AllowRequest()
		require.True(t, allowed)

		cb.ReportFailure()
		require.Equal(t, StateOpen, cb.GetState())

		// Even though we took a semaphore, it should be cleared by state transition
		allowed, _ = cb.AllowRequest()
		require.False(t, allowed)
	})

	// Test transition to half-open again
	t.Run("Back to HalfOpen", func(t *testing.T) {
		mockClock.Add(6 * time.Second)

		// Force timer activation
		if cb.openTimer != nil {
			cb.openTimer.Stop()
			cb.transitionToHalfOpen()
		}

		require.Equal(t, StateHalfOpen, cb.GetState())
	})

	// Test transition to closed state
	t.Run("Transition to Closed", func(t *testing.T) {
		allowed, _ := cb.AllowRequest()
		require.True(t, allowed)

		cb.ReportSuccess()
		require.Equal(t, StateHalfOpen, cb.GetState())

		cb.ReleaseSemaphore()
		allowed, _ = cb.AllowRequest()
		require.True(t, allowed)

		cb.ReportSuccess() // This should close the circuit
		require.Equal(t, StateClosed, cb.GetState())

		cb.ReleaseSemaphore()
	})

	// Test proper cleanup
	t.Run("Cleanup", func(t *testing.T) {
		cb.Stop()
	})
}

// TestCircuitBreakerCallbacks tests the callback functions
func TestCircuitBreakerCallbacks(t *testing.T) {
	var (
		openCalled     bool
		halfOpenCalled bool
		closedCalled   bool
	)

	cb := New(Config{
		FailureThreshold:      2,
		Timeout:               1 * time.Millisecond, // Short timeout for quick tests
		SuccessThreshold:      1,
		HalfOpenMaxConcurrent: 1,
		OnOpen: func(c fiber.Ctx) error {
			openCalled = true
			return c.SendStatus(fiber.StatusServiceUnavailable)
		},
		OnHalfOpen: func(c fiber.Ctx) error {
			halfOpenCalled = true
			return c.SendStatus(fiber.StatusTooManyRequests)
		},
		OnClose: func(c fiber.Ctx) error {
			closedCalled = true
			return c.Next()
		},
	})

	app := fiber.New()

	app.Use(Middleware(cb))

	app.Get("/test", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	// Test OnOpen callback
	t.Run("OnOpen Callback", func(t *testing.T) {
		// Trip the circuit
		cb.ReportFailure()
		cb.ReportFailure()

		// Request should be rejected
		req := httptest.NewRequest("GET", "/test", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
		require.True(t, openCalled)
	})

	// Test OnHalfOpen callback
	t.Run("OnHalfOpen Callback", func(t *testing.T) {
		cb.transitionToHalfOpen()

		// Acquire the one allowed request
		allowed, state := cb.AllowRequest()
		require.True(t, allowed)
		require.Equal(t, StateHalfOpen, state)

		// Second request should be rejected with OnHalfOpen callback
		req := httptest.NewRequest("GET", "/test", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode)
		require.True(t, halfOpenCalled)

		// Release the semaphore
		cb.ReleaseSemaphore()
	})

	// Test OnClose callback
	t.Run("OnClose Callback", func(t *testing.T) {
		// Reset for clean test
		closedCalled = false

		// Get to half-open state
		cb.transitionToHalfOpen()

		// Create a test request
		req := httptest.NewRequest("GET", "/test", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		require.True(t, closedCalled) // OnClose should be called after successful request
	})

	// Clean up
	cb.Stop()
}

// TestMiddleware tests the middleware functionality
func TestMiddleware(t *testing.T) {
	customErr := errors.New("custom error")

	cb := New(Config{
		FailureThreshold: 2,
		Timeout:          5 * time.Second,
		SuccessThreshold: 2,
		IsFailure: func(c fiber.Ctx, err error) bool {
			// Count as failure if status >= 400 or has error
			return err != nil || c.Response().StatusCode() >= 400
		},
	})

	app := fiber.New()
	app.Use(Middleware(cb))

	// Success handler
	app.Get("/success", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	// Client error handler - 400 series
	app.Get("/client-error", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusBadRequest)
	})

	// Server error handler - 500 series
	app.Get("/server-error", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusInternalServerError)
	})

	// Error handler
	app.Get("/error", func(c fiber.Ctx) error {
		return customErr
	})

	t.Run("Successful Request", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/success", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
	})

	t.Run("Client Error Counts as Failure", func(t *testing.T) {
		// Reset to closed state
		cb.transitionToClosed()

		// Send client error requests
		req := httptest.NewRequest("GET", "/client-error", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

		// Should increment failure count - check state remains closed
		require.Equal(t, StateClosed, cb.GetState())

		// Second failure should trip circuit
		resp, err = app.Test(req)
		require.NoError(t, err)
		require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

		// Circuit should now be open
		require.Equal(t, StateOpen, cb.GetState())
	})

	t.Run("Circuit Open Rejects Requests", func(t *testing.T) {
		// Circuit should be open from previous test
		req := httptest.NewRequest("GET", "/success", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	})

	// Clean up
	cb.Stop()
}

// TestConcurrentAccess tests the circuit breaker under concurrent load
func TestConcurrentAccess(t *testing.T) {
	cb := New(Config{
		FailureThreshold:      5,
		Timeout:               100 * time.Millisecond,
		SuccessThreshold:      3,
		HalfOpenMaxConcurrent: 2,
	})

	t.Run("Concurrent Failures", func(t *testing.T) {
		var wg sync.WaitGroup

		// Simulate 10 goroutines reporting failures
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cb.ReportFailure()
			}()
		}

		wg.Wait()

		// Circuit should be open after enough failures
		require.Equal(t, StateOpen, cb.GetState())
	})

	t.Run("Concurrent Half-Open Requests", func(t *testing.T) {
		// Force transition to half-open
		cb.transitionToHalfOpen()

		var wg sync.WaitGroup
		requestAllowed := make(chan bool, 10)

		// Try 10 concurrent requests
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				allowed, _ := cb.AllowRequest()
				requestAllowed <- allowed

				if allowed {
					// Simulate request processing
					time.Sleep(10 * time.Millisecond)
					cb.ReleaseSemaphore()
				}
			}()
		}

		wg.Wait()
		close(requestAllowed)

		// Count allowed requests
		allowedCount := 0
		for allowed := range requestAllowed {
			if allowed {
				allowedCount++
			}
		}

		// Only HalfOpenMaxConcurrent (2) requests should be allowed
		require.Equal(t, cb.config.HalfOpenMaxConcurrent, allowedCount)
	})

	t.Run("Concurrent Successes to Close Circuit", func(t *testing.T) {
		// Force transition to half-open
		cb.transitionToHalfOpen()

		var wg sync.WaitGroup

		// Simulate 10 goroutines reporting successes
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cb.ReportSuccess()
			}()
		}

		wg.Wait()

		// Circuit should be closed after enough successes
		require.Equal(t, StateClosed, cb.GetState())
	})

	// Clean up
	cb.Stop()
}

// TestCustomFailureDetection tests the custom failure detection logic
func TestCustomFailureDetection(t *testing.T) {
	customFailureDetection := false

	cb := New(Config{
		FailureThreshold: 1,
		IsFailure: func(c fiber.Ctx, err error) bool {
			// Custom logic: mark as failure only if our flag is set
			return customFailureDetection
		},
	})

	app := fiber.New()
	app.Use(Middleware(cb))

	app.Get("/test", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	t.Run("Custom Success Logic", func(t *testing.T) {
		customFailureDetection = false

		// Even 500 status should be success with our custom logic
		app.Get("/server-error", func(c fiber.Ctx) error {
			c.Status(500)
			return nil
		})

		req := httptest.NewRequest("GET", "/server-error", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, 500, resp.StatusCode)

		// Circuit should remain closed
		require.Equal(t, StateClosed, cb.GetState())
	})

	t.Run("Custom Failure Logic", func(t *testing.T) {
		customFailureDetection = true

		// Now even 200 status should be failure with our custom logic
		req := httptest.NewRequest("GET", "/test", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)

		// Circuit should be open
		require.Equal(t, StateOpen, cb.GetState())
	})

	// Clean up
	cb.Stop()
}

// TestHalfOpenConcurrencyConfig tests that HalfOpenMaxConcurrent setting works
func TestHalfOpenConcurrencyConfig(t *testing.T) {
	// Create circuit breaker with 3 concurrent requests in half-open
	cb := New(Config{
		FailureThreshold:      2,
		Timeout:               5 * time.Second,
		SuccessThreshold:      2,
		HalfOpenMaxConcurrent: 3,
	})

	// Put circuit in half-open state
	cb.transitionToOpen()
	cb.transitionToHalfOpen()

	// Try to get more than allowed concurrent requests
	allowed1, _ := cb.AllowRequest()
	allowed2, _ := cb.AllowRequest()
	allowed3, _ := cb.AllowRequest()
	allowed4, _ := cb.AllowRequest()

	require.True(t, allowed1)
	require.True(t, allowed2)
	require.True(t, allowed3)
	require.False(t, allowed4)

	// Release all permits
	cb.ReleaseSemaphore()
	cb.ReleaseSemaphore()
	cb.ReleaseSemaphore()

	// Clean up
	cb.Stop()
}

// TestCircuitBreakerReset tests the Reset method
func TestCircuitBreakerReset(t *testing.T) {
	mockClock := newMockTime(time.Now())

	cb := New(Config{
		FailureThreshold:      2,
		Timeout:               5 * time.Second,
		SuccessThreshold:      2,
		HalfOpenMaxConcurrent: 1,
	})
	cb.now = mockClock.Now

	t.Run("Reset From Open State", func(t *testing.T) {
		// Put circuit in open state
		cb.ReportFailure()
		cb.ReportFailure()
		require.Equal(t, StateOpen, cb.GetState())

		// Reset the circuit
		cb.Reset()

		// Verify state and counters
		require.Equal(t, StateClosed, cb.GetState())
		require.Equal(t, int64(0), atomic.LoadInt64(&cb.failureCount))
		require.Equal(t, int64(0), atomic.LoadInt64(&cb.successCount))
	})

	t.Run("Reset From HalfOpen State", func(t *testing.T) {
		// Put circuit in half-open state
		cb.ReportFailure()
		cb.ReportFailure()
		cb.transitionToHalfOpen()
		require.Equal(t, StateHalfOpen, cb.GetState())

		// Take a semaphore
		allowed, _ := cb.AllowRequest()
		require.True(t, allowed)

		// Reset the circuit
		cb.Reset()

		// Verify state and that new requests are allowed
		require.Equal(t, StateClosed, cb.GetState())
		allowed, _ = cb.AllowRequest()
		require.True(t, allowed)
	})

	t.Run("Reset With Active Timer", func(t *testing.T) {
		// Put circuit in open state with active timer
		cb.ReportFailure()
		cb.ReportFailure()
		require.Equal(t, StateOpen, cb.GetState())

		// Reset before timer expires
		cb.Reset()

		// Advance time past original timeout
		mockClock.Add(6 * time.Second)

		// Verify circuit remains closed
		require.Equal(t, StateClosed, cb.GetState())
	})

	t.Run("Reset Updates LastStateChange", func(t *testing.T) {
		initialTime := cb.lastStateChange

		// Wait a moment
		mockClock.Add(1 * time.Second)

		// Reset the circuit
		cb.Reset()

		// Verify lastStateChange was updated
		require.True(t, cb.lastStateChange.After(initialTime))
	})

	// Clean up
	cb.Stop()
}

// TestCircuitBreakerForceOpen tests the ForceOpen method
func TestForceOpen(t *testing.T) {
	mockClock := newMockTime(time.Now())

	cb := New(Config{
		FailureThreshold:      2,
		Timeout:               5 * time.Second,
		SuccessThreshold:      2,
		HalfOpenMaxConcurrent: 1,
	})
	cb.now = mockClock.Now

	t.Run("Force Open From Closed State", func(t *testing.T) {
		require.Equal(t, StateClosed, cb.GetState())
		cb.ForceOpen()
		require.Equal(t, StateOpen, cb.GetState())

		// Verify requests are rejected
		allowed, state := cb.AllowRequest()
		require.False(t, allowed)
		require.Equal(t, StateOpen, state)
	})

	t.Run("Force Open From HalfOpen State", func(t *testing.T) {
		// First get to half-open state
		cb.transitionToOpen()
		cb.transitionToHalfOpen()

		require.Equal(t, StateHalfOpen, cb.GetState())

		// Take a semaphore
		allowed, _ := cb.AllowRequest()
		require.True(t, allowed)

		// Force open should clear semaphore
		cb.ForceOpen()
		require.Equal(t, StateOpen, cb.GetState())

		// Verify new requests are rejected
		allowed, _ = cb.AllowRequest()
		require.False(t, allowed)
	})

	t.Run("Force Open With Active Timer", func(t *testing.T) {
		cb.transitionToClosed()
		cb.ForceOpen()

		// Advance time past timeout
		mockClock.Add(6 * time.Second)

		// Should still be open since ForceOpen overrides normal timeout
		require.Equal(t, StateOpen, cb.GetState())
	})

	t.Run("Force Open Multiple Times", func(t *testing.T) {
		// Multiple force open calls should maintain open state
		cb.ForceOpen()
		cb.ForceOpen()
		require.Equal(t, StateOpen, cb.GetState())

		// Verify counters are reset each time
		require.Equal(t, int64(0), atomic.LoadInt64(&cb.failureCount))
		require.Equal(t, int64(0), atomic.LoadInt64(&cb.successCount))
	})

	// Clean up
	cb.Stop()
}

// TestHealthHandler tests the health check endpoint handler
func TestHealthHandler(t *testing.T) {
	cb := New(Config{
		FailureThreshold:      2,
		Timeout:               5 * time.Second,
		SuccessThreshold:      2,
		HalfOpenMaxConcurrent: 1,
	})

	app := fiber.New()
	app.Get("/health", cb.HealthHandler())

	t.Run("Healthy When Closed", func(t *testing.T) {
		cb.transitionToClosed()

		req := httptest.NewRequest("GET", "/health", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)

		var result fiber.Map
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		err = json.Unmarshal(body, &result)
		require.NoError(t, err)
		require.Equal(t, string(StateClosed), result["state"])
		require.Equal(t, true, result["healthy"])
	})

	t.Run("Unhealthy When Open", func(t *testing.T) {
		cb.transitionToOpen()

		req := httptest.NewRequest("GET", "/health", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

		var result fiber.Map
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		err = json.Unmarshal(body, &result)
		require.NoError(t, err)
		require.Equal(t, string(StateOpen), result["state"])
		require.Equal(t, false, result["healthy"])
	})

	t.Run("Response Content Type", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/health", nil)
		resp, err := app.Test(req)

		require.NoError(t, err)
		require.Equal(t, fiber.MIMEApplicationJSONCharsetUTF8, resp.Header.Get("Content-Type"))
	})

	// Clean up
	cb.Stop()
}
