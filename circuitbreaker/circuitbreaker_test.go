package circuitbreaker

import (
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
)

// TestNewCircuitBreaker ensures circuit breaker initializes with correct defaults
func TestNewCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig)
	assert.Equal(t, StateClosed, cb.state)
	assert.Equal(t, DefaultCircuitBreakerConfig.Threshold, cb.threshold)
	assert.Equal(t, DefaultCircuitBreakerConfig.Timeout, cb.timeout)
	assert.Equal(t, DefaultCircuitBreakerConfig.SuccessReset, cb.successReset)
}

// TestAllowRequest checks request allowance in different states
func TestAllowRequest(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 3})

	assert.True(t, cb.AllowRequest())
	cb.ReportFailure()
	cb.ReportFailure()
	cb.ReportFailure()
	assert.False(t, cb.AllowRequest())
}

// TestReportSuccess verifies state transitions after successful requests
func TestReportSuccess(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 2, SuccessReset: 2})
	cb.state = StateHalfOpen
	cb.ReportSuccess()
	assert.Equal(t, StateHalfOpen, cb.state)
	cb.ReportSuccess()
	assert.Equal(t, StateClosed, cb.state)
}

// TestReportFailure checks state transitions after failures
func TestReportFailureThreshold(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 2})
	cb.ReportFailure()
	assert.Equal(t, StateClosed, cb.state)
	cb.ReportFailure()
	assert.Equal(t, StateOpen, cb.state)
}

func TestCircuitBreakerMiddleware(t *testing.T) {
	app := fiber.New()
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig)

	app.Use(Middleware(cb))

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("OK")
	})

	t.Run("Initial state is closed", func(t *testing.T) {
		assert.Equal(t, StateClosed, cb.state)
	})

	t.Run("Trips to open state after threshold", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			cb.ReportFailure()
		}
		assert.Equal(t, StateOpen, cb.state)
	})

	t.Run("Allows requests in half-open state", func(t *testing.T) {
		cb.state = StateHalfOpen
		assert.True(t, cb.AllowRequest())
	})

	t.Run("Recovers to closed state after success", func(t *testing.T) {
		cb.state = StateHalfOpen
		cb.ReportSuccess()
		assert.Equal(t, StateClosed, cb.state)
	})
}
