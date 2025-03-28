package circuitbreaker

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
)

// State represents the state of the circuit breaker
type State string

const (
	StateClosed   State = "closed"    // Normal operation
	StateOpen     State = "open"      // Requests are blocked
	StateHalfOpen State = "half-open" // Limited requests allowed to check recovery
)

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	failureCount      int32              // Count of failures
	successCount      int32              // Count of successes in half-open state
	state             State              // Current state of circuit breaker
	mutex             sync.RWMutex       // Protects state transitions
	failureThreshold  int                // Max failures before opening circuit
	timeout           time.Duration      // Duration to stay open before transitioning to half-open
	successThreshold  int                // Successes required to close circuit
	openExpiry        time.Time          // Time when open state expires
	ctx               context.Context    // Context for cancellation
	cancel            context.CancelFunc // Cancel function for cleanup
	config            Config             // Configuration settings
	now               func() time.Time   // Function for getting current time (useful for testing)
	halfOpenSemaphore chan struct{}      // Controls limited requests in half-open state
}

// Config holds the configurable parameters
type Config struct {
	FailureThreshold int
	Timeout          time.Duration
	SuccessThreshold int
	OnOpen           func(*fiber.Ctx) error
	OnHalfOpen       func(*fiber.Ctx) error
	OnClose          func(*fiber.Ctx) error
}

// Default configuration values for the circuit breaker
var DefaultConfig = Config{
	FailureThreshold: 5,
	Timeout:          5 * time.Second,
	SuccessThreshold: 1,
	OnOpen: func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "service unavailable",
		})
	},
	OnHalfOpen: func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"error": "service under recovery",
		})
	},
	OnClose: func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"message": "circuit closed"})
	},
}

// New initializes a circuit breaker with the given configuration
func New(config Config) *CircuitBreaker {
	ctx, cancel := context.WithCancel(context.Background())
	cb := &CircuitBreaker{
		failureThreshold:  config.FailureThreshold,
		timeout:           config.Timeout,
		successThreshold:  config.SuccessThreshold,
		state:             StateClosed,
		ctx:               ctx,
		cancel:            cancel,
		config:            config,
		now:               time.Now,
		halfOpenSemaphore: make(chan struct{}, 1),
	}
	go cb.monitor()
	return cb
}

// monitor checks the state periodically and transitions if needed
func (cb *CircuitBreaker) monitor() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cb.mutex.Lock()
			if cb.state == StateOpen && cb.now().After(cb.openExpiry) {
				cb.state = StateHalfOpen
				atomic.StoreInt32(&cb.failureCount, 0)
				atomic.StoreInt32(&cb.successCount, 0)
			}
			cb.mutex.Unlock()
		case <-cb.ctx.Done():
			return
		}
	}
}

// Stop cancels the circuit breaker monitoring
func (cb *CircuitBreaker) Stop() {
	cb.cancel()
}

// AllowRequest determines if a request is allowed based on circuit state
func (cb *CircuitBreaker) AllowRequest() bool {
	cb.mutex.RLock()
	defer cb.mutex.RUnlock()

	if cb.state == StateOpen {
		return false
	}

	if cb.state == StateHalfOpen {
		select {
		case cb.halfOpenSemaphore <- struct{}{}:
			return true
		default:
			return false
		}
	}

	return true
}

// ReportSuccess increments success count and closes circuit if threshold met
func (cb *CircuitBreaker) ReportSuccess() {
	atomic.AddInt32(&cb.successCount, 1)

	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.state == StateHalfOpen && int(atomic.LoadInt32(&cb.successCount)) >= cb.successThreshold {
		cb.state = StateClosed
		atomic.StoreInt32(&cb.failureCount, 0)
		atomic.StoreInt32(&cb.successCount, 0)
	}
}

// ReportFailure increments failure count and opens circuit if threshold met
func (cb *CircuitBreaker) ReportFailure() {
	atomic.AddInt32(&cb.failureCount, 1)

	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := cb.now()
	switch cb.state {
	case StateHalfOpen:
		cb.state = StateOpen
		cb.openExpiry = now.Add(cb.timeout)
		atomic.StoreInt32(&cb.failureCount, 0)
	case StateClosed:
		if int(atomic.LoadInt32(&cb.failureCount)) >= cb.failureThreshold {
			cb.state = StateOpen
			cb.openExpiry = now.Add(cb.timeout)
		}
	}
}

// Middleware wraps the fiber handler with circuit breaker logic
func Middleware(cb *CircuitBreaker) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !cb.AllowRequest() {
			if cb.config.OnOpen != nil {
				return cb.config.OnOpen(c)
			}
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}

		if cb.state == StateHalfOpen {
			defer func() { <-cb.halfOpenSemaphore }()
		}

		err := c.Next()

		status := c.Response().StatusCode()
		if err != nil || status >= http.StatusInternalServerError {
			cb.ReportFailure()
		} else {
			cb.ReportSuccess()
		}

		return err
	}
}
