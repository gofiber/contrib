package loadshed

import (
	"context"
	"github.com/stretchr/testify/assert"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
)

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
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	app.Use(New(cfg))
	app.Get("/", ReturnOK)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)

	status := resp.StatusCode
	if status != fiber.StatusOK && status != fiber.StatusServiceUnavailable {
		t.Fatalf("Expected status code %d or %d but got %d", fiber.StatusOK, fiber.StatusServiceUnavailable, status)
	}
}

func Test_Loadshed_MiddleValue(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{93.0}}
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	app.Use(New(cfg))
	app.Get("/", ReturnOK)

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
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	app.Use(New(cfg))
	app.Get("/", ReturnOK)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}

func Test_Loadshed_CustomOnShed(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}
	cfg.OnShed = func(c fiber.Ctx) error {
		return c.Status(fiber.StatusTooManyRequests).Send([]byte{})
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithResponse(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}

	// This OnShed directly sets a response without returning it
	cfg.OnShed = func(c fiber.Ctx) error {
		c.Status(fiber.StatusTooManyRequests)
		return nil
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithNilReturn(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}

	// OnShed returns nil without setting a response
	cfg.OnShed = func(c fiber.Ctx) error {
		return nil
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithCustomError(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}

	// OnShed returns a custom error
	cfg.OnShed = func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusForbidden, "Custom error message")
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func Test_Loadshed_CustomOnShedWithResponseAndCustomError(t *testing.T) {
	app := fiber.New()

	mockGetter := &MockCPUPercentGetter{MockedPercentage: []float64{96.0}}
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}

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
	var cfg Config
	cfg.Criteria = &CPULoadCriteria{
		LowerThreshold: 0.90,
		UpperThreshold: 0.95,
		Interval:       time.Second,
		Getter:         mockGetter,
	}

	// OnShed returns JSON response
	cfg.OnShed = func(c fiber.Ctx) error {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error":       "Service is currently unavailable due to high load",
			"retry_after": 30,
		})
	}

	app.Use(New(cfg))
	app.Get("/", ReturnOK)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
}
