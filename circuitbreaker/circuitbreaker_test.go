package circuitbreaker

import (
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

// TestNewCircuitBreaker ensures circuit breaker initializes with correct defaults
func TestNewCircuitBreaker(t *testing.T) {
	cb := New(DefaultConfig)
	require.Equal(t, StateClosed, cb.state)
	require.Equal(t, DefaultConfig.FailureThreshold, cb.failureThreshold)
	require.Equal(t, DefaultConfig.Timeout, cb.timeout)
	require.Equal(t, DefaultConfig.SuccessThreshold, cb.successThreshold)
	t.Cleanup(cb.Stop)
}

// TestAllowRequest checks request allowance in different states
func TestAllowRequest(t *testing.T) {
	cb := New(Config{FailureThreshold: 3})

	require.True(t, cb.AllowRequest())
	cb.ReportFailure()
	cb.ReportFailure()
	cb.ReportFailure()
	require.False(t, cb.AllowRequest())
	t.Cleanup(cb.Stop)
}

// TestReportSuccess verifies state transitions after successful requests
func TestReportSuccess(t *testing.T) {
	cb := New(Config{FailureThreshold: 2, SuccessThreshold: 2})
	cb.state = StateHalfOpen
	cb.ReportSuccess()
	require.Equal(t, StateHalfOpen, cb.state)
	cb.ReportSuccess()
	require.Equal(t, StateClosed, cb.state)
	t.Cleanup(cb.Stop)
}

// TestReportFailure checks state transitions after failures
func TestReportFailureThreshold(t *testing.T) {
	cb := New(Config{FailureThreshold: 2})
	cb.ReportFailure()
	require.Equal(t, StateClosed, cb.state)
	cb.ReportFailure()
	require.Equal(t, StateOpen, cb.state)
	t.Cleanup(cb.Stop)
}

func TestCircuitBreakerMiddleware(t *testing.T) {
	app := fiber.New()
	cb := New(DefaultConfig)

	app.Use(Middleware(cb))

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("OK")
	})

	t.Run("Initial state is closed", func(t *testing.T) {
		require.Equal(t, StateClosed, cb.state)
	})

	t.Run("Trips to open state after threshold", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			cb.ReportFailure()
		}
		require.Equal(t, StateOpen, cb.state)
	})

	t.Run("Allows requests in half-open state", func(t *testing.T) {
		cb.state = StateHalfOpen
		require.True(t, cb.AllowRequest())
	})

	t.Run("Recovers to closed state after success", func(t *testing.T) {
		cb.state = StateHalfOpen
		cb.ReportSuccess()
		require.Equal(t, StateClosed, cb.state)
	})
}
