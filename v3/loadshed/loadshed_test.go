package loadshed

import (
	"context"
	"io"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gofiber/fiber/v3"
)

// waitForSample polls the criteria's cached value until it has a real sample
// (i.e., is not NaN) or the timeout expires. We store NaN first so that a
// legitimate 0.0% CPU sample is still detected as "sampler has run".
func waitForSample(t *testing.T, criteria *CPULoadCriteria) {
	t.Helper()

	// Mark the cached value as unknown so we can reliably detect when
	// the background sampler writes a fresh sample, even if that sample
	// is legitimately 0.0.
	criteria.cached.Store(math.Float64bits(math.NaN()))

	// The sampler may be mid-sleep when we store NaN, so allow enough
	// time for the current sleep cycle to finish plus the next sample.
	interval := criteria.Interval
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(interval + 500*time.Millisecond)
	for time.Now().Before(deadline) {
		v := math.Float64frombits(criteria.cached.Load())
		if !math.IsNaN(v) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for background sampler to populate cached metric")
}

type MockCPUPercentGetter struct {
	MockedPercentage []float64
}

func (m *MockCPUPercentGetter) PercentWithContext(_ context.Context, _ time.Duration, _ bool) ([]float64, error) {
	return m.MockedPercentage, nil
}

func ReturnOK(c fiber.Ctx) error {
	return c.SendStatus(fiber.StatusOK)
}

func Test_Loadshed_LowerThreshold(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{89.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria
	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)

	status := resp.StatusCode
	if status != fiber.StatusOK && status != fiber.StatusServiceUnavailable {
		t.Fatalf("Expected status code %d or %d but got %d", fiber.StatusOK, fiber.StatusServiceUnavailable, status)
	}
}

func Test_Loadshed_DefaultCriteriaWhenNil(t *testing.T) {
	cfg := configWithDefaults(Config{})

	// configWithDefaults should clone the default CPULoadCriteria, not share it.
	criteria, ok := cfg.Criteria.(*CPULoadCriteria)
	require.True(t, ok)
	assert.NotSame(t, ConfigDefault.Criteria, criteria)

	def := ConfigDefault.Criteria.(*CPULoadCriteria)
	assert.Equal(t, def.LowerThreshold, criteria.LowerThreshold)
	assert.Equal(t, def.UpperThreshold, criteria.UpperThreshold)
	assert.Equal(t, def.Interval, criteria.Interval)
}

func Test_Loadshed_DefaultCriteriaNoArgs(t *testing.T) {
	cfg := configWithDefaults()

	// The no-args path should also clone, not share the default singleton.
	criteria, ok := cfg.Criteria.(*CPULoadCriteria)
	require.True(t, ok)
	assert.NotSame(t, ConfigDefault.Criteria, criteria)

	def := ConfigDefault.Criteria.(*CPULoadCriteria)
	assert.Equal(t, def.LowerThreshold, criteria.LowerThreshold)
	assert.Equal(t, def.UpperThreshold, criteria.UpperThreshold)
	assert.Equal(t, def.Interval, criteria.Interval)
}

func Test_Loadshed_MiddleValue(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{93.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria
	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	rejectedCount := 0
	acceptedCount := 0
	iterations := 100000

	for i := 0; i < iterations; i++ {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
		assert.Equal(t, nil, err)

		if resp.StatusCode == fiber.StatusServiceUnavailable {
			rejectedCount++
		} else {
			acceptedCount++
		}
	}

	t.Logf("Accepted: %d, Rejected: %d", acceptedCount, rejectedCount)
	if acceptedCount == 0 || rejectedCount == 0 {
		t.Fatalf("Expected both accepted and rejected requests, but got Accepted: %d, Rejected: %d", acceptedCount, rejectedCount)
	}
}

func Test_Loadshed_UpperThreshold(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria
	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}

func Test_Loadshed_CustomOnShed(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria
	cfg.OnShed = func(c fiber.Ctx) error {
		return c.Status(fiber.StatusTooManyRequests).Send([]byte{})
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithResponse(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria

	// This OnShed directly sets a response without returning it
	cfg.OnShed = func(c fiber.Ctx) error {
		c.Status(fiber.StatusTooManyRequests)
		return nil
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithNilReturn(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria

	// OnShed returns nil without setting a response
	cfg.OnShed = func(c fiber.Ctx) error {
		return nil
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithCustomError(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria

	// OnShed returns a custom error
	cfg.OnShed = func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusForbidden, "Custom error message")
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithResponseAndCustomError(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria

	// OnShed sets a response and returns a different error
	// The NewError have higher priority since executed last
	cfg.OnShed = func(c fiber.Ctx) error {
		c.
			Status(fiber.StatusTooManyRequests).
			SendString("Too many requests")

		return fiber.NewError(
			fiber.StatusInternalServerError,
			"Shed happened",
		)
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	payload, readErr := io.ReadAll(resp.Body)
	defer resp.Body.Close()

	assert.Equal(t, string(payload), "Shed happened")
	assert.Equal(t, nil, err)
	assert.Equal(t, nil, readErr)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithJSON(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	criteria := &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	var cfg Config
	cfg.Criteria = criteria

	// OnShed returns JSON response
	cfg.OnShed = func(c fiber.Ctx) error {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error":       "Service is currently unavailable due to high load",
			"retry_after": 30,
		})
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)
	t.Cleanup(criteria.Stop)
	waitForSample(t, criteria)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "application/json; charset=utf-8", resp.Header.Get("Content-Type"))
}
