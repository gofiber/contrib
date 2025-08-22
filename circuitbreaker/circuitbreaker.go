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

// Config holds the configurable parameters
type Config struct {
	// Failure threshold to trip the circuit
	FailureThreshold int
	// Duration circuit stays open before allowing test requests
	Timeout time.Duration
	// Success threshold to close the circuit from half-open
	SuccessThreshold int
	// Maximum concurrent requests allowed in half-open state
	HalfOpenMaxConcurrent int
	// Custom failure detector function (return true if response should count as failure)
	IsFailure func(c *fiber.Ctx, err error) bool
	// Callbacks for state transitions
	OnOpen     func(*fiber.Ctx) error // Called when circuit opens
	OnHalfOpen func(*fiber.Ctx) error // Called when circuit transitions to half-open
	OnClose    func(*fiber.Ctx) error // Called when circuit closes
}

// DefaultConfig provides sensible defaults for the circuit breaker
var DefaultConfig = Config{
	FailureThreshold:      5,
	Timeout:               5 * time.Second,
	SuccessThreshold:      1,
	HalfOpenMaxConcurrent: 1,
	IsFailure: func(c *fiber.Ctx, err error) bool {
		return err != nil || c.Response().StatusCode() >= http.StatusInternalServerError
	},
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
		return c.Next()
	},
}

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	failureCount      int64              // Count of failures (atomic)
	successCount      int64              // Count of successes in half-open state (atomic)
	totalRequests     int64              // Count of total requests (atomic)
	rejectedRequests  int64              // Count of rejected requests (atomic)
	state             State              // Current state of circuit breaker
	mutex             sync.RWMutex       // Protects state transitions
	failureThreshold  int                // Max failures before opening circuit
	timeout           time.Duration      // Duration to stay open before transitioning to half-open
	successThreshold  int                // Successes required to close circuit
	openTimer         *time.Timer        // Timer for state transition from open to half-open
	ctx               context.Context    // Context for cancellation
	cancel            context.CancelFunc // Cancel function for cleanup
	config            Config             // Configuration settings
	now               func() time.Time   // Function for getting current time (useful for testing)
	halfOpenSemaphore chan struct{}      // Controls limited requests in half-open state
	lastStateChange   time.Time          // Time of last state change
}

// New initializes a circuit breaker with the given configuration
func New(config Config) *CircuitBreaker {
	// Apply default values for zero values
	if config.FailureThreshold <= 0 {
		config.FailureThreshold = DefaultConfig.FailureThreshold
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultConfig.Timeout
	}
	if config.SuccessThreshold <= 0 {
		config.SuccessThreshold = DefaultConfig.SuccessThreshold
	}
	if config.HalfOpenMaxConcurrent <= 0 {
		config.HalfOpenMaxConcurrent = DefaultConfig.HalfOpenMaxConcurrent
	}
	if config.IsFailure == nil {
		config.IsFailure = DefaultConfig.IsFailure
	}
	if config.OnOpen == nil {
		config.OnOpen = DefaultConfig.OnOpen
	}
	if config.OnHalfOpen == nil {
		config.OnHalfOpen = DefaultConfig.OnHalfOpen
	}
	if config.OnClose == nil {
		config.OnClose = DefaultConfig.OnClose
	}

	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()

	return &CircuitBreaker{
		failureThreshold:  config.FailureThreshold,
		timeout:           config.Timeout,
		successThreshold:  config.SuccessThreshold,
		state:             StateClosed,
		ctx:               ctx,
		cancel:            cancel,
		config:            config,
		now:               time.Now,
		halfOpenSemaphore: make(chan struct{}, config.HalfOpenMaxConcurrent),
		lastStateChange:   now,
		totalRequests:     0,
		rejectedRequests:  0,
	}
}

// Stop cancels the circuit breaker and releases resources
func (cb *CircuitBreaker) Stop() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.openTimer != nil {
		cb.openTimer.Stop()
	}
	cb.cancel()
}

// GetState returns the current state of the circuit breaker
func (cb *CircuitBreaker) GetState() State {
	cb.mutex.RLock()
	defer cb.mutex.RUnlock()
	return cb.state
}

// IsOpen returns true if the circuit is open
func (cb *CircuitBreaker) IsOpen() bool {
	return cb.GetState() == StateOpen
}

// Reset resets the circuit breaker to its initial closed state
func (cb *CircuitBreaker) Reset() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	// Reset counters
	atomic.StoreInt64(&cb.failureCount, 0)
	atomic.StoreInt64(&cb.successCount, 0)

	// Reset state
	cb.state = StateClosed
	cb.lastStateChange = cb.now()

	// Cancel any pending state transitions
	if cb.openTimer != nil {
		cb.openTimer.Stop()
	}
}

// ForceOpen forcibly opens the circuit regardless of failure count
func (cb *CircuitBreaker) ForceOpen() {
	cb.transitionToOpen()
}

// ForceClose forcibly closes the circuit regardless of current state
func (cb *CircuitBreaker) ForceClose() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.state = StateClosed
	cb.lastStateChange = cb.now()
	atomic.StoreInt64(&cb.failureCount, 0)
	atomic.StoreInt64(&cb.successCount, 0)

	if cb.openTimer != nil {
		cb.openTimer.Stop()
	}
}

// SetTimeout updates the timeout duration
func (cb *CircuitBreaker) SetTimeout(timeout time.Duration) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.timeout = timeout
}

// transitionToOpen changes state to open and schedules transition to half-open
func (cb *CircuitBreaker) transitionToOpen() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.state != StateOpen {
		cb.state = StateOpen
		cb.lastStateChange = cb.now()

		// Stop existing timer if any
		if cb.openTimer != nil {
			cb.openTimer.Stop()
		}

		// Schedule transition to half-open after timeout
		cb.openTimer = time.AfterFunc(cb.timeout, func() {
			cb.transitionToHalfOpen()
		})

		// Reset failure counter
		atomic.StoreInt64(&cb.failureCount, 0)
	}
}

// transitionToHalfOpen changes state from open to half-open
func (cb *CircuitBreaker) transitionToHalfOpen() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.state == StateOpen {
		cb.state = StateHalfOpen
		cb.lastStateChange = cb.now()

		// Reset counters
		atomic.StoreInt64(&cb.failureCount, 0)
		atomic.StoreInt64(&cb.successCount, 0)

		// Empty the semaphore channel
		select {
		case <-cb.halfOpenSemaphore:
		default:
		}
	}
}

// transitionToClosed changes state from half-open to closed
func (cb *CircuitBreaker) transitionToClosed() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.state == StateHalfOpen {
		cb.state = StateClosed
		cb.lastStateChange = cb.now()

		// Reset counters
		atomic.StoreInt64(&cb.failureCount, 0)
		atomic.StoreInt64(&cb.successCount, 0)
	}
}

// AllowRequest determines if a request is allowed based on circuit state
func (cb *CircuitBreaker) AllowRequest() (bool, State) {
	atomic.AddInt64(&cb.totalRequests, 1)

	cb.mutex.RLock()
	state := cb.state
	cb.mutex.RUnlock()

	switch state {
	case StateOpen:
		atomic.AddInt64(&cb.rejectedRequests, 1)
		return false, state
	case StateHalfOpen:
		select {
		case cb.halfOpenSemaphore <- struct{}{}:
			return true, state
		default:
			atomic.AddInt64(&cb.rejectedRequests, 1)
			return false, state
		}
	default: // StateClosed
		return true, state
	}
}

// ReleaseSemaphore releases a slot in the half-open semaphore
func (cb *CircuitBreaker) ReleaseSemaphore() {
	select {
	case <-cb.halfOpenSemaphore:
	default:
	}
}

// ReportSuccess increments success count and closes circuit if threshold met
func (cb *CircuitBreaker) ReportSuccess() {
	cb.mutex.RLock()
	currentState := cb.state
	cb.mutex.RUnlock()

	if currentState == StateHalfOpen {
		newSuccessCount := atomic.AddInt64(&cb.successCount, 1)
		if int(newSuccessCount) >= cb.successThreshold {
			cb.transitionToClosed()
		}
	}
}

// ReportFailure increments failure count and opens circuit if threshold met
func (cb *CircuitBreaker) ReportFailure() {
	cb.mutex.RLock()
	currentState := cb.state
	cb.mutex.RUnlock()

	switch currentState {
	case StateHalfOpen:
		// In half-open, a single failure trips the circuit
		cb.transitionToOpen()
	case StateClosed:
		newFailureCount := atomic.AddInt64(&cb.failureCount, 1)
		if int(newFailureCount) >= cb.failureThreshold {
			cb.transitionToOpen()
		}
	}
}

// Metrics returns basic metrics about the circuit breaker
func (cb *CircuitBreaker) Metrics() fiber.Map {
	return fiber.Map{
		"state":            cb.GetState(),
		"failures":         atomic.LoadInt64(&cb.failureCount),
		"successes":        atomic.LoadInt64(&cb.successCount),
		"totalRequests":    atomic.LoadInt64(&cb.totalRequests),
		"rejectedRequests": atomic.LoadInt64(&cb.rejectedRequests),
	}
}

// GetStateStats returns detailed statistics about the circuit breaker
func (cb *CircuitBreaker) GetStateStats() fiber.Map {
	state := cb.GetState()

	return fiber.Map{
		"state":            state,
		"failures":         atomic.LoadInt64(&cb.failureCount),
		"successes":        atomic.LoadInt64(&cb.successCount),
		"totalRequests":    atomic.LoadInt64(&cb.totalRequests),
		"rejectedRequests": atomic.LoadInt64(&cb.rejectedRequests),
		"lastStateChange":  cb.lastStateChange,
		"openDuration":     cb.timeout,
		"failureThreshold": cb.failureThreshold,
		"successThreshold": cb.successThreshold,
	}
}

// HealthHandler returns a Fiber handler for checking circuit breaker status
func (cb *CircuitBreaker) HealthHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		state := cb.GetState()

		data := fiber.Map{
			"state":   state,
			"healthy": state == StateClosed,
		}

		if state == StateOpen {
			return c.Status(fiber.StatusServiceUnavailable).JSON(data)
		}

		return c.JSON(data)
	}
}

// Middleware wraps the fiber handler with circuit breaker logic
func Middleware(cb *CircuitBreaker) fiber.Handler {
	return func(c *fiber.Ctx) error {
		allowed, state := cb.AllowRequest()

		if !allowed {
			// Call appropriate callback based on state
			if state == StateHalfOpen && cb.config.OnHalfOpen != nil {
				return cb.config.OnHalfOpen(c)
			} else if state == StateOpen && cb.config.OnOpen != nil {
				return cb.config.OnOpen(c)
			}
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}

		// If request allowed in half-open state, ensure semaphore is released
		halfOpen := state == StateHalfOpen
		if halfOpen {
			defer cb.ReleaseSemaphore()
		}

		// Execute the request
		err := c.Next()

		// Check if the response should be considered a failure
		if cb.config.IsFailure(c, err) {
			cb.ReportFailure()
		} else {
			cb.ReportSuccess()

			// If transition to closed state just happened, trigger callback
			if halfOpen && cb.GetState() == StateClosed && cb.config.OnClose != nil {
				// We don't return this error as it would override the actual response
				_ = cb.config.OnClose(c)
			}
		}

		return err
	}
}
