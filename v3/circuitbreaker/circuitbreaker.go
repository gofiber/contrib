package circuitbreaker

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
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
	// Interval for resetting failure counts in closed state.
	// Zero means failures accumulate until the circuit opens.
	Interval time.Duration
	// Custom failure detector function (return true if response should count as failure)
	IsFailure func(c fiber.Ctx, err error) bool

	// Clock reads the current time. Recovery is derived from it rather than
	// scheduled, so substituting a clock drives the circuit through every
	// state without waiting.
	//
	// Optional. Default: time.Now
	Clock func() time.Time

	// OnOpen and OnHalfOpen answer a request refused because the circuit is
	// open, or because half-open is already at HalfOpenMaxConcurrent. None of
	// the three fires on a transition alone.
	//
	// OnClose is a notification, not a response writer: it runs after the
	// probe that closed the circuit, by which point the protected handler has
	// already answered. Fiber writes to the response eagerly, so a Send, JSON
	// or Status call here replaces that answer even though the error OnClose
	// returns is discarded. Record the recovery, do not write to the response
	// and do not advance the chain.
	OnOpen     func(fiber.Ctx) error
	OnHalfOpen func(fiber.Ctx) error
	OnClose    func(fiber.Ctx) error
}

// DefaultConfig provides sensible defaults for the circuit breaker
var DefaultConfig = Config{
	FailureThreshold:      5,
	Timeout:               5 * time.Second,
	SuccessThreshold:      1,
	HalfOpenMaxConcurrent: 1,
	Interval:              0,
	Clock:                 time.Now,
	IsFailure: func(c fiber.Ctx, err error) bool {
		return err != nil || c.Response().StatusCode() >= http.StatusInternalServerError
	},
	OnOpen: func(c fiber.Ctx) error {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "service unavailable",
		})
	},
	OnHalfOpen: func(c fiber.Ctx) error {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"error": "service under recovery",
		})
	},
	OnClose: func(c fiber.Ctx) error {
		return nil
	},
}

// CircuitBreaker implements the circuit breaker pattern. Requests reach it
// through Middleware; the state machine is not driven from outside.
type CircuitBreaker struct {
	failureCount     int64 // Count of failures (atomic)
	successCount     int64 // Count of successes in half-open state (atomic)
	totalRequests    int64 // Count of total requests (atomic)
	rejectedRequests int64 // Count of rejected requests (atomic)

	mutex sync.RWMutex // Protects every field below

	state           State
	lastStateChange time.Time
	openedAt        time.Time // with timeout, the recovery deadline
	forced          bool      // ForceOpen suspends recovery until closed explicitly

	// A slot is released by generation, so a probe outliving its half-open
	// window cannot free one belonging to a later window.
	halfOpenInFlight int
	halfOpenGen      uint64

	expiry time.Time // when the failure count is dropped, if Interval is set

	config                Config
	clock                 func() time.Time
	failureThreshold      int
	successThreshold      int
	halfOpenMaxConcurrent int
	timeout               time.Duration
	interval              time.Duration
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
	if config.Clock == nil {
		config.Clock = DefaultConfig.Clock
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

	now := config.Clock()

	var expiry time.Time
	if config.Interval > 0 {
		expiry = now.Add(config.Interval)
	}

	return &CircuitBreaker{
		state:                 StateClosed,
		lastStateChange:       now,
		expiry:                expiry,
		config:                config,
		clock:                 config.Clock,
		failureThreshold:      config.FailureThreshold,
		successThreshold:      config.SuccessThreshold,
		halfOpenMaxConcurrent: config.HalfOpenMaxConcurrent,
		timeout:               config.Timeout,
		interval:              config.Interval,
	}
}

// Middleware wraps the fiber handler with circuit breaker logic
func Middleware(cb *CircuitBreaker) fiber.Handler {
	return func(c fiber.Ctx) error {
		return cb.serve(c, c.Next)
	}
}

// admission records how a request was let in, so its outcome is applied to the
// circuit that admitted it rather than to whatever the circuit has become by
// the time it finishes.
type admission struct {
	state State
	gen   uint64
	// any belongs to the deprecated protocol, which reports an outcome with no
	// admission to match it against.
	any bool
}

// current reports whether the circuit is still in the state, and the half-open
// window, that admitted the request. An outcome that fails this describes a
// circuit that no longer exists: the request may have started before the
// circuit even opened, or have been probing a window that has since ended.
func (a admission) current(state State, gen uint64) bool {
	if a.any {
		return true
	}
	if a.state != state {
		return false
	}
	return state != StateHalfOpen || a.gen == gen
}

// serve admits one request, runs next, reports the outcome and releases the
// half-open slot it took, so no caller has to pair those steps up.
func (cb *CircuitBreaker) serve(c fiber.Ctx, next func() error) error {
	allowed, adm, release := cb.admit(cb.clock())
	if !allowed {
		// New never leaves these nil; the guards spare a hand-built one.
		if adm.state == StateHalfOpen && cb.config.OnHalfOpen != nil {
			return cb.config.OnHalfOpen(c)
		}
		if adm.state == StateOpen && cb.config.OnOpen != nil {
			return cb.config.OnOpen(c)
		}
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	defer release()

	err := next()

	if cb.config.IsFailure(c, err) {
		cb.recordFailure(cb.clock(), adm)
		return err
	}

	if cb.recordSuccess(cb.clock(), adm) && cb.config.OnClose != nil {
		// The handler has answered; OnClose must not overwrite that.
		_ = cb.config.OnClose(c)
	}
	return err
}

// admit reports whether one request may proceed, how it was admitted, and the
// release for what it took - a no-op unless it took a probe slot.
func (cb *CircuitBreaker) admit(now time.Time) (bool, admission, func()) {
	atomic.AddInt64(&cb.totalRequests, 1)

	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.recoverLocked(now)

	switch cb.state {
	case StateOpen:
		atomic.AddInt64(&cb.rejectedRequests, 1)
		return false, admission{state: StateOpen}, func() {}
	case StateHalfOpen:
		if cb.halfOpenInFlight >= cb.halfOpenMaxConcurrent {
			atomic.AddInt64(&cb.rejectedRequests, 1)
			return false, admission{state: StateHalfOpen}, func() {}
		}
		cb.halfOpenInFlight++
		gen := cb.halfOpenGen
		return true, admission{state: StateHalfOpen, gen: gen}, func() { cb.releaseProbe(gen) }
	default:
		return true, admission{state: StateClosed}, func() {}
	}
}

// releaseProbe gives back a slot taken in generation gen. The transition that
// ends a window reclaims every slot of it, so a probe outliving its window has
// nothing to return: releasing would free a slot another probe now holds.
func (cb *CircuitBreaker) releaseProbe(gen uint64) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.halfOpenGen != gen || cb.halfOpenInFlight == 0 {
		return
	}
	cb.halfOpenInFlight--
}

// recoveryDueLocked reports whether the open to half-open transition has come
// due. Either lock is enough to read it.
func (cb *CircuitBreaker) recoveryDueLocked(now time.Time) bool {
	return cb.state == StateOpen && !cb.forced && !now.Before(cb.openedAt.Add(cb.timeout))
}

// recoverLocked moves an open circuit to half-open once its deadline has
// passed. Nothing schedules it - the first request or state read after the
// deadline applies it - so a circuit with no traffic keeps reporting open.
func (cb *CircuitBreaker) recoverLocked(now time.Time) {
	if !cb.recoveryDueLocked(now) {
		return
	}

	cb.state = StateHalfOpen
	cb.lastStateChange = now
	atomic.StoreInt64(&cb.failureCount, 0)
	atomic.StoreInt64(&cb.successCount, 0)
	cb.beginHalfOpenWindowLocked()
}

// beginHalfOpenWindowLocked frees every slot and starts a new window, which
// makes the slots of the window that just ended unreleasable.
func (cb *CircuitBreaker) beginHalfOpenWindowLocked() {
	cb.halfOpenInFlight = 0
	cb.halfOpenGen++
}

// openLocked opens the circuit on a fresh deadline; a forced open suspends
// recovery until the circuit is closed explicitly.
func (cb *CircuitBreaker) openLocked(now time.Time, forced bool) {
	cb.state = StateOpen
	cb.openedAt = now
	cb.lastStateChange = now
	cb.forced = forced
	atomic.StoreInt64(&cb.failureCount, 0)
	cb.beginHalfOpenWindowLocked()
}

// closeLocked returns the circuit to normal operation.
func (cb *CircuitBreaker) closeLocked(now time.Time) {
	cb.state = StateClosed
	cb.lastStateChange = now
	cb.forced = false
	cb.openedAt = time.Time{}
	atomic.StoreInt64(&cb.failureCount, 0)
	atomic.StoreInt64(&cb.successCount, 0)
	if cb.interval > 0 {
		cb.expiry = now.Add(cb.interval)
	}
	cb.beginHalfOpenWindowLocked()
}

// recordFailure counts a failure and opens the circuit if that was enough.
//
// A failure from a request the circuit has since moved past is ignored rather
// than treated as a failed probe. Ignoring it cannot make the circuit
// optimistic - only a success closes it, and those are matched the same way -
// while acting on it would end a trial that has not run and push the recovery
// deadline out again, so a backlog of stale failures could starve recovery
// indefinitely.
func (cb *CircuitBreaker) recordFailure(now time.Time, adm admission) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.recoverLocked(now)

	if !adm.current(cb.state, cb.halfOpenGen) {
		return
	}

	switch cb.state {
	case StateHalfOpen:
		// One failure ends the trial.
		cb.openLocked(now, false)
	case StateClosed:
		cb.dropExpiredFailuresLocked(now)
		if int(atomic.AddInt64(&cb.failureCount, 1)) >= cb.failureThreshold {
			cb.openLocked(now, false)
		}
	}
}

// recordSuccess counts a success and reports whether this one closed the
// circuit, so OnClose does not depend on a second state read that another
// request could win.
//
// Only a probe of the current half-open window vouches for recovery, by the
// same matching rule the failure path uses.
func (cb *CircuitBreaker) recordSuccess(now time.Time, adm admission) bool {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.recoverLocked(now)

	if cb.state != StateHalfOpen || !adm.current(cb.state, cb.halfOpenGen) {
		return false
	}
	if int(atomic.AddInt64(&cb.successCount, 1)) < cb.successThreshold {
		return false
	}

	cb.closeLocked(now)
	return true
}

// dropExpiredFailuresLocked forgets accumulated failures once the Interval
// window has elapsed, the boundary instant included.
func (cb *CircuitBreaker) dropExpiredFailuresLocked(now time.Time) {
	if cb.interval <= 0 || cb.expiry.After(now) {
		return
	}
	atomic.StoreInt64(&cb.failureCount, 0)
	cb.expiry = now.Add(cb.interval)
}

// stateAt reports the state at now, applying a due recovery first. The common
// case does not need the write lock.
func (cb *CircuitBreaker) stateAt(now time.Time) State {
	cb.mutex.RLock()
	state := cb.state
	due := cb.recoveryDueLocked(now)
	cb.mutex.RUnlock()

	if !due {
		return state
	}

	cb.mutex.Lock()
	defer cb.mutex.Unlock()
	cb.recoverLocked(now)
	return cb.state
}

// GetState returns the current state of the circuit breaker
func (cb *CircuitBreaker) GetState() State {
	return cb.stateAt(cb.clock())
}

// IsOpen returns true if the circuit is open
func (cb *CircuitBreaker) IsOpen() bool {
	return cb.GetState() == StateOpen
}

// Reset resets the circuit breaker to closed, ForceOpen included.
func (cb *CircuitBreaker) Reset() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.closeLocked(cb.clock())
}

// ForceOpen forcibly opens the circuit regardless of failure count, and keeps
// it open: a forced-open circuit does not recover on its own when Timeout
// elapses, only when Reset or ForceClose is called.
func (cb *CircuitBreaker) ForceOpen() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.openLocked(cb.clock(), true)
}

// ForceClose forcibly closes the circuit regardless of current state
func (cb *CircuitBreaker) ForceClose() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.closeLocked(cb.clock())
}

// SetTimeout updates the timeout duration. A circuit that is already open
// recovers on the new deadline, since the deadline is derived, not scheduled.
func (cb *CircuitBreaker) SetTimeout(timeout time.Duration) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.timeout = timeout
}

// Stop releases the circuit breaker's resources.
//
// Deprecated: recovery is derived from the clock rather than scheduled, so
// there is nothing to stop. This does nothing.
func (cb *CircuitBreaker) Stop() {}

// AllowRequest determines if a request is allowed based on circuit state.
//
// Deprecated: use Middleware, which admits, reports and releases as one
// operation. A slot taken here is held until ReleaseSemaphore returns it or
// the next state change reclaims it.
func (cb *CircuitBreaker) AllowRequest() (bool, State) {
	allowed, adm, _ := cb.admit(cb.clock())
	return allowed, adm.state
}

// ReleaseSemaphore releases a slot in the half-open semaphore.
//
// Deprecated: use Middleware. This releases a slot in the current half-open
// window, which is not necessarily the one it was taken in.
func (cb *CircuitBreaker) ReleaseSemaphore() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.halfOpenInFlight > 0 {
		cb.halfOpenInFlight--
	}
}

// ReportSuccess increments success count and closes circuit if threshold met.
//
// Deprecated: use Middleware, which reports the outcome of the request it
// admitted and runs OnClose when that success closes the circuit.
func (cb *CircuitBreaker) ReportSuccess() {
	// No admission to match, so the outcome applies to whatever state is
	// current - the behaviour this method has always had.
	cb.recordSuccess(cb.clock(), admission{any: true})
}

// ReportFailure increments failure count and opens circuit if threshold met.
//
// Deprecated: use Middleware, which reports the outcome of the request it
// admitted.
func (cb *CircuitBreaker) ReportFailure() {
	// As with ReportSuccess, there is no admission to match.
	cb.recordFailure(cb.clock(), admission{any: true})
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

// GetStateStats returns detailed statistics about the circuit breaker.
//
// The state and the fields describing it are read under one lock, so a
// transition racing the read cannot pair the old state with the new
// timestamps.
func (cb *CircuitBreaker) GetStateStats() fiber.Map {
	now := cb.clock()

	cb.mutex.RLock()
	if !cb.recoveryDueLocked(now) {
		defer cb.mutex.RUnlock()
		return cb.statsLocked()
	}
	cb.mutex.RUnlock()

	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.recoverLocked(now)
	return cb.statsLocked()
}

// statsLocked builds the statistics map. Caller holds either lock.
func (cb *CircuitBreaker) statsLocked() fiber.Map {
	return fiber.Map{
		"state":            cb.state,
		"failures":         atomic.LoadInt64(&cb.failureCount),
		"successes":        atomic.LoadInt64(&cb.successCount),
		"totalRequests":    atomic.LoadInt64(&cb.totalRequests),
		"rejectedRequests": atomic.LoadInt64(&cb.rejectedRequests),
		"lastStateChange":  cb.lastStateChange,
		"openDuration":     cb.timeout,
		"failureThreshold": cb.failureThreshold,
		"successThreshold": cb.successThreshold,
		"expiry":           cb.expiry,
	}
}

// HealthHandler returns a Fiber handler for checking circuit breaker status
func (cb *CircuitBreaker) HealthHandler() fiber.Handler {
	return func(c fiber.Ctx) error {
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
