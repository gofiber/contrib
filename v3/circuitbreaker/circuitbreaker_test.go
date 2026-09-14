package circuitbreaker_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/circuitbreaker"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// fakeClock is the only source of time the circuit breaker reads when it is
// handed to Config.Clock, so advancing it drives recovery directly instead of
// standing in for a timer the test cannot reach.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// AdvanceToParity advances at least one second, stopping on a second whose
// Unix value has the requested parity. TestGetStateStatsIsNotTorn uses it to
// encode which transition a timestamp belongs to.
func (c *fakeClock) AdvanceToParity(parity int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		c.t = c.t.Add(time.Second)
		if c.t.Unix()%2 == parity {
			return
		}
	}
}

// noTimeout lets a handler block for as long as a test needs it to.
var noTimeout = fiber.TestConfig{Timeout: 0, FailOnTimeout: false}

// newApp mounts cb in front of an "/ok" route that succeeds and a "/fail"
// route that answers 500, which the default IsFailure counts as a failure.
func newApp(cb *circuitbreaker.CircuitBreaker) *fiber.App {
	app := fiber.New()
	app.Use(circuitbreaker.Middleware(cb))
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})
	app.Get("/fail", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusInternalServerError)
	})
	return app
}

func get(t *testing.T, app *fiber.App, target string) *http.Response {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, target, nil), noTimeout)
	require.NoError(t, err)
	return resp
}

// bgRequest is a request running on its own goroutine. Assertions stay on the
// test goroutine: require's FailNow is only defined there.
type bgRequest struct {
	done chan struct{}
	resp *http.Response
	err  error
}

func inBackground(app *fiber.App, target string) *bgRequest {
	r := &bgRequest{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.resp, r.err = app.Test(httptest.NewRequest(fiber.MethodGet, target, nil), noTimeout)
	}()
	return r
}

func (r *bgRequest) wait(t *testing.T) *http.Response {
	t.Helper()
	<-r.done
	require.NoError(t, r.err)
	return r.resp
}

// awaitEntry waits for a handler to report that it was admitted. Bounding the
// wait keeps a refused probe a test failure rather than a hang.
func awaitEntry(t *testing.T, entered <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal(msg)
	}
}

// releaser closes a handler's release channel exactly once, on the test's way
// out if an assertion did not get there first.
func releaser(t *testing.T, release chan struct{}) func() {
	t.Helper()
	done := sync.OnceFunc(func() { close(release) })
	t.Cleanup(done)
	return done
}

// trip drives the circuit open through the middleware.
func trip(t *testing.T, app *fiber.App, failures int) {
	t.Helper()
	for i := 0; i < failures; i++ {
		get(t, app, "/fail")
	}
}

func TestOpensAfterFailureThreshold(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 3,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	trip(t, app, 2)
	require.Equal(t, circuitbreaker.StateClosed, cb.GetState(), "two failures of three must not open the circuit")

	trip(t, app, 1)
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState())
	require.True(t, cb.IsOpen())
}

func TestOpenRejectsWithOnOpen(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	var openCalls int
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		Timeout:          time.Minute,
		Clock:            clock.Now,
		OnOpen: func(c fiber.Ctx) error {
			openCalls++
			return c.SendStatus(fiber.StatusServiceUnavailable)
		},
	})
	app := newApp(cb)

	trip(t, app, 1)
	require.Zero(t, openCalls, "OnOpen answers a refused request; it does not fire when the circuit opens")

	resp := get(t, app, "/ok")
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, 1, openCalls)
}

func TestRecoversToHalfOpenOnTheClock(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		Timeout:          30 * time.Second,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	trip(t, app, 1)
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState())

	clock.Advance(29 * time.Second)
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState(), "the circuit must stay open until the timeout elapses")

	clock.Advance(time.Second)
	require.Equal(t, circuitbreaker.StateHalfOpen, cb.GetState(), "the boundary instant counts as elapsed")
}

func TestHalfOpenProbeClosesTheCircuit(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	var closeCalls int
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 2,
		Timeout:          time.Minute,
		Clock:            clock.Now,
		OnClose: func(c fiber.Ctx) error {
			closeCalls++
			return nil
		},
	})
	app := newApp(cb)

	trip(t, app, 1)
	clock.Advance(time.Minute)

	resp := get(t, app, "/ok")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "a half-open probe reaches the handler")
	require.Equal(t, circuitbreaker.StateHalfOpen, cb.GetState(), "one success of two keeps the trial open")
	require.Zero(t, closeCalls)

	resp = get(t, app, "/ok")
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, circuitbreaker.StateClosed, cb.GetState())
	require.Equal(t, 1, closeCalls, "OnClose runs for the probe that closed the circuit")
}

func TestHalfOpenFailureReopens(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 5,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	trip(t, app, 5)
	clock.Advance(time.Minute)
	require.Equal(t, circuitbreaker.StateHalfOpen, cb.GetState())

	get(t, app, "/fail")
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState(), "one failed probe ends the trial regardless of the threshold")

	clock.Advance(time.Minute)
	require.Equal(t, circuitbreaker.StateHalfOpen, cb.GetState(), "reopening starts a fresh recovery deadline")
}

func TestHalfOpenAdmitsAtMostMaxConcurrent(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	var halfOpenCalls int64
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:      1,
		SuccessThreshold:      99, // never close, so the window stays open
		HalfOpenMaxConcurrent: 2,
		Timeout:               time.Minute,
		Clock:                 clock.Now,
		OnHalfOpen: func(c fiber.Ctx) error {
			atomic.AddInt64(&halfOpenCalls, 1)
			return c.SendStatus(fiber.StatusTooManyRequests)
		},
	})

	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	app := fiber.New()
	app.Use(circuitbreaker.Middleware(cb))
	app.Get("/block", func(c fiber.Ctx) error {
		entered <- struct{}{}
		<-release
		return c.SendString("OK")
	})
	app.Get("/fail", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusInternalServerError)
	})

	trip(t, app, 1)
	clock.Advance(time.Minute)

	releaseAll := releaser(t, release)

	// Fill both slots and keep them held.
	held := []*bgRequest{inBackground(app, "/block"), inBackground(app, "/block")}
	awaitEntry(t, entered, "the first probe of two was not admitted")
	awaitEntry(t, entered, "the second probe of two was not admitted")

	resp := get(t, app, "/block")
	require.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode, "a third concurrent probe is refused")
	require.Equal(t, int64(1), atomic.LoadInt64(&halfOpenCalls))

	releaseAll()
	for _, r := range held {
		r.wait(t)
	}
}

// TestHalfOpenSlotsAreNotLeakedAcrossWindows pins the guarantee that a probe
// which outlives its half-open window cannot free a slot in a later one.
//
// The stale release has to land while the new window is already at capacity,
// which is the only moment the two behaviours differ: a release that ignores
// which window its slot came from decrements the new window's count and lets
// an extra probe in.
func TestHalfOpenSlotsAreNotLeakedAcrossWindows(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:      1,
		SuccessThreshold:      99, // never close, so a window ends only by failing
		HalfOpenMaxConcurrent: 2,
		Timeout:               time.Minute,
		Clock:                 clock.Now,
	})

	strandedIn := make(chan struct{}, 1)
	strandedOut := make(chan struct{})
	filledIn := make(chan struct{}, 4)
	filledOut := make(chan struct{})

	app := fiber.New()
	app.Use(circuitbreaker.Middleware(cb))
	app.Get("/stranded", func(c fiber.Ctx) error {
		strandedIn <- struct{}{}
		<-strandedOut
		return c.SendString("OK")
	})
	app.Get("/hold", func(c fiber.Ctx) error {
		filledIn <- struct{}{}
		<-filledOut
		return c.SendString("OK")
	})
	app.Get("/probe", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})
	app.Get("/fail", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusInternalServerError)
	})

	releaseStranded := releaser(t, strandedOut)
	releaseFilled := releaser(t, filledOut)

	trip(t, app, 1)
	clock.Advance(time.Minute)
	require.Equal(t, circuitbreaker.StateHalfOpen, cb.GetState())

	// A probe takes a slot in this window and stays in flight.
	stranded := inBackground(app, "/stranded")
	awaitEntry(t, strandedIn, "the stranded probe was not admitted")

	// End the window underneath it: the second slot is free, so this failing
	// probe is admitted and reopens the circuit.
	get(t, app, "/fail")
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState())

	// A new window, filled to capacity by two fresh probes.
	clock.Advance(time.Minute)
	require.Equal(t, circuitbreaker.StateHalfOpen, cb.GetState())
	filled := []*bgRequest{inBackground(app, "/hold"), inBackground(app, "/hold")}
	awaitEntry(t, filledIn, "the new half-open window admitted fewer probes than HalfOpenMaxConcurrent")
	awaitEntry(t, filledIn, "the new half-open window admitted fewer probes than HalfOpenMaxConcurrent")

	// Now let the stranded probe finish. Its slot belonged to the window that
	// has already ended, so returning it must not free one of these two.
	releaseStranded()
	stranded.wait(t)

	resp := get(t, app, "/probe")
	require.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode,
		"a slot from the previous half-open window was returned into this one, admitting more probes than HalfOpenMaxConcurrent")

	releaseFilled()
	for _, r := range filled {
		r.wait(t)
	}
}

func TestForceOpenIsSticky(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 5,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	cb.ForceOpen()
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState())

	clock.Advance(10 * time.Minute)
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState(), "a forced-open circuit does not recover on the timeout")

	resp := get(t, app, "/ok")
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	cb.Reset()
	require.Equal(t, circuitbreaker.StateClosed, cb.GetState(), "only an explicit close ends a forced open")
}

func TestForceOpenOverridesAnOpenCircuitsRecovery(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	trip(t, app, 1)
	cb.ForceOpen() // already open, but now pinned

	clock.Advance(10 * time.Minute)
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState())
}

func TestForceCloseAndReset(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		close func(*circuitbreaker.CircuitBreaker)
	}{
		{"ForceClose", func(cb *circuitbreaker.CircuitBreaker) { cb.ForceClose() }},
		{"Reset", func(cb *circuitbreaker.CircuitBreaker) { cb.Reset() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clock := newFakeClock()
			cb := circuitbreaker.New(circuitbreaker.Config{
				FailureThreshold: 2,
				Interval:         10 * time.Second,
				Timeout:          time.Minute,
				Clock:            clock.Now,
			})
			app := newApp(cb)

			trip(t, app, 2)
			require.Equal(t, circuitbreaker.StateOpen, cb.GetState())

			clock.Advance(5 * time.Second)
			tc.close(cb)

			require.Equal(t, circuitbreaker.StateClosed, cb.GetState())

			stats := cb.GetStateStats()
			require.Equal(t, int64(0), stats["failures"])
			require.Equal(t, int64(0), stats["successes"])
			require.Equal(t, clock.Now(), stats["lastStateChange"])
			require.Equal(t, clock.Now().Add(10*time.Second), stats["expiry"], "closing starts a new failure-count window")

			resp := get(t, app, "/ok")
			require.Equal(t, fiber.StatusOK, resp.StatusCode, "a closed circuit passes requests through")
		})
	}
}

func TestSetTimeoutAppliesToAnOpenCircuit(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		Timeout:          time.Hour,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	trip(t, app, 1)
	clock.Advance(time.Minute)
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState())

	// The deadline is derived from the timeout, so shortening it recovers a
	// circuit that is already open.
	cb.SetTimeout(30 * time.Second)
	require.Equal(t, circuitbreaker.StateHalfOpen, cb.GetState())
}

func TestIntervalDropsFailuresBetweenWindows(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 3,
		Interval:         10 * time.Second,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	require.Equal(t, clock.Now().Add(10*time.Second), cb.GetStateStats()["expiry"],
		"New derives the first window from the configured clock")

	trip(t, app, 2)
	require.Equal(t, int64(2), cb.Metrics()["failures"])

	// The boundary instant counts as elapsed, so the next failure starts over.
	clock.Advance(10 * time.Second)
	trip(t, app, 1)
	require.Equal(t, int64(1), cb.Metrics()["failures"], "the window elapsed, so the earlier failures are forgotten")
	require.Equal(t, circuitbreaker.StateClosed, cb.GetState())

	trip(t, app, 2)
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState(), "three failures inside one window still open the circuit")
}

func TestIntervalWindowIsNotDroppedEarly(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 3,
		Interval:         10 * time.Second,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	trip(t, app, 2)
	clock.Advance(9 * time.Second)
	trip(t, app, 1)

	require.Equal(t, circuitbreaker.StateOpen, cb.GetState(), "failures inside the window accumulate")
}

func TestCustomFailureDetection(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	// Count 404 as a failure and ignore 500, the opposite of the default.
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 2,
		Timeout:          time.Minute,
		Clock:            clock.Now,
		IsFailure: func(c fiber.Ctx, err error) bool {
			return c.Response().StatusCode() == fiber.StatusNotFound
		},
	})

	app := fiber.New()
	app.Use(circuitbreaker.Middleware(cb))
	app.Get("/missing", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNotFound)
	})
	app.Get("/fail", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusInternalServerError)
	})

	trip(t, app, 5)
	require.Equal(t, circuitbreaker.StateClosed, cb.GetState(), "500 is not a failure under this detector")

	for i := 0; i < 2; i++ {
		get(t, app, "/missing")
	}
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState(), "404 is a failure under this detector")
}

// TestDefaultOnCloseDoesNotAdvanceChainTwice guards the regression where a
// default OnClose that called c.Next() advanced the chain past the middleware
// that had already answered, reaching the protected handler a second time.
func TestDefaultOnCloseDoesNotAdvanceChainTwice(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		Timeout:          time.Minute,
		Clock:            clock.Now,
		// OnClose deliberately left nil so New installs the default.
	})

	var protectedCalls int
	app := fiber.New()
	app.Use(circuitbreaker.Middleware(cb))
	app.Get("/fail", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusInternalServerError)
	})
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendString("OK")
	}, func(c fiber.Ctx) error {
		protectedCalls++
		return c.SendString("protected")
	})

	trip(t, app, 1)
	clock.Advance(time.Minute)

	resp := get(t, app, "/ok")
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, circuitbreaker.StateClosed, cb.GetState(), "the probe closed the circuit, so OnClose ran")
	require.Zero(t, protectedCalls, "the default OnClose must not advance the handler chain")
}

func TestHealthHandler(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})

	app := fiber.New()
	app.Get("/health", cb.HealthHandler())
	protected := fiber.New()
	protected.Use(circuitbreaker.Middleware(cb))
	protected.Get("/fail", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusInternalServerError)
	})

	resp := get(t, app, "/health")
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON)

	trip(t, protected, 1)

	resp = get(t, app, "/health")
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	// Recovery is visible through the health endpoint without any traffic.
	clock.Advance(time.Minute)
	resp = get(t, app, "/health")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "half-open is not unhealthy")
}

func TestMetricsCountRequestsAndRejections(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	trip(t, app, 1) // one admitted request that failed
	get(t, app, "/ok")
	get(t, app, "/ok") // two refused

	metrics := cb.Metrics()
	require.Equal(t, circuitbreaker.StateOpen, metrics["state"])
	require.Equal(t, int64(3), metrics["totalRequests"])
	require.Equal(t, int64(2), metrics["rejectedRequests"])
}

func TestConcurrentFailuresOpenOnce(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 5,
		Timeout:          time.Minute,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	fired := make([]*bgRequest, 0, 20)
	for i := 0; i < 20; i++ {
		fired = append(fired, inBackground(app, "/fail"))
	}
	for _, r := range fired {
		r.wait(t)
	}

	require.Equal(t, circuitbreaker.StateOpen, cb.GetState())
	require.Equal(t, int64(20), cb.Metrics()["totalRequests"])
}

func TestConcurrentProbesCloseTheCircuitOnce(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	var closeCalls int64
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:      1,
		SuccessThreshold:      2,
		HalfOpenMaxConcurrent: 4,
		Timeout:               time.Minute,
		Clock:                 clock.Now,
		OnClose: func(c fiber.Ctx) error {
			atomic.AddInt64(&closeCalls, 1)
			return nil
		},
	})
	app := newApp(cb)

	trip(t, app, 1)
	clock.Advance(time.Minute)

	fired := make([]*bgRequest, 0, 20)
	for i := 0; i < 20; i++ {
		fired = append(fired, inBackground(app, "/ok"))
	}
	for _, r := range fired {
		r.wait(t)
	}

	require.Equal(t, circuitbreaker.StateClosed, cb.GetState())
	require.Equal(t, int64(1), atomic.LoadInt64(&closeCalls), "only the probe that crossed the threshold closes the circuit")
}

// The deprecated protocol keeps working for callers that have not moved to
// Middleware yet.
func TestDeprecatedProtocolStillDrivesTheCircuit(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:      2,
		SuccessThreshold:      1,
		HalfOpenMaxConcurrent: 1,
		Timeout:               time.Minute,
		Clock:                 clock.Now,
	})

	allowed, state := cb.AllowRequest()
	require.True(t, allowed)
	require.Equal(t, circuitbreaker.StateClosed, state)

	cb.ReportFailure()
	cb.ReportFailure()
	require.Equal(t, circuitbreaker.StateOpen, cb.GetState())

	allowed, state = cb.AllowRequest()
	require.False(t, allowed)
	require.Equal(t, circuitbreaker.StateOpen, state)

	clock.Advance(time.Minute)

	allowed, state = cb.AllowRequest()
	require.True(t, allowed, "the one half-open slot is available")
	require.Equal(t, circuitbreaker.StateHalfOpen, state)

	allowed, _ = cb.AllowRequest()
	require.False(t, allowed, "the slot is still held until it is released")

	cb.ReleaseSemaphore()
	allowed, _ = cb.AllowRequest()
	require.True(t, allowed, "releasing the slot admits the next probe")

	cb.ReportSuccess()
	require.Equal(t, circuitbreaker.StateClosed, cb.GetState())
}

func TestStopIsANoOp(t *testing.T) {
	t.Parallel()

	cb := circuitbreaker.New(circuitbreaker.Config{Clock: newFakeClock().Now})
	cb.Stop()
	require.Equal(t, circuitbreaker.StateClosed, cb.GetState(), "Stop leaves a usable circuit breaker")
}

func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()

	cb := circuitbreaker.New(circuitbreaker.Config{})
	stats := cb.GetStateStats()

	require.Equal(t, circuitbreaker.DefaultConfig.FailureThreshold, stats["failureThreshold"])
	require.Equal(t, circuitbreaker.DefaultConfig.SuccessThreshold, stats["successThreshold"])
	require.Equal(t, circuitbreaker.DefaultConfig.Timeout, stats["openDuration"])
	require.Equal(t, circuitbreaker.StateClosed, stats["state"])
	require.True(t, stats["expiry"].(time.Time).IsZero(), "a zero Interval sets no window")
}

// TestGetStateStatsIsNotTorn pins that the reported state and the timestamps
// describing it come from the same moment.
//
// The gap between reading the state and reading its metadata cannot be entered
// on demand through the interface, so the test makes a torn pairing detectable
// instead: every transition to open lands on an even second and every
// transition to closed on an odd one, so "closed" reported with an even
// lastStateChange is proof the two halves came from different moments.
func TestGetStateStatsIsNotTorn(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		Timeout:          time.Hour, // recovery must not move the state here
		Clock:            clock.Now,
	})
	app := newApp(cb)

	// The circuit starts closed at the clock's base instant, whose parity is
	// arbitrary; close it once on an odd second so the starting pairing obeys
	// the rule the reader checks.
	clock.AdvanceToParity(1)
	cb.ForceClose()

	const rounds = 400
	torn := make(chan string, 1)
	stop := make(chan struct{})
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}

			stats := cb.GetStateStats()
			state, _ := stats["state"].(circuitbreaker.State)
			changed, _ := stats["lastStateChange"].(time.Time)
			parity := changed.Unix() % 2

			switch state {
			case circuitbreaker.StateOpen:
				if parity != 0 {
					select {
					case torn <- "open reported with a lastStateChange from a close":
					default:
					}
					return
				}
			case circuitbreaker.StateClosed:
				if parity != 1 {
					select {
					case torn <- "closed reported with a lastStateChange from an open":
					default:
					}
					return
				}
			}
		}
	}()

	for i := 0; i < rounds; i++ {
		clock.AdvanceToParity(0)
		get(t, app, "/fail") // opens on an even second
		clock.AdvanceToParity(1)
		cb.ForceClose() // closes on an odd second
	}

	close(stop)
	<-readerDone

	select {
	case msg := <-torn:
		t.Fatalf("GetStateStats returned a torn snapshot: %s", msg)
	default:
	}
}

// TestGetStateStatsSettlesDueRecovery pins that reading the stats applies a
// recovery that has come due, and reports it with the timestamps that belong
// to it - the branch where the read has to take the write lock.
func TestGetStateStatsSettlesDueRecovery(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold: 1,
		Interval:         time.Minute,
		Timeout:          30 * time.Second,
		Clock:            clock.Now,
	})
	app := newApp(cb)

	trip(t, app, 1)
	openedAt := clock.Now()
	require.Equal(t, circuitbreaker.StateOpen, cb.GetStateStats()["state"])
	require.Equal(t, openedAt, cb.GetStateStats()["lastStateChange"])

	clock.Advance(30 * time.Second)
	recoveredAt := clock.Now()

	stats := cb.GetStateStats()
	require.Equal(t, circuitbreaker.StateHalfOpen, stats["state"], "the stats read settles a due recovery")
	require.Equal(t, recoveredAt, stats["lastStateChange"], "and reports the moment it happened")
	require.Equal(t, 30*time.Second, stats["openDuration"])
	require.Equal(t, int64(0), stats["failures"], "entering half-open clears the counters")
}
