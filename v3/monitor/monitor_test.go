package monitor

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	recovermw "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/timeout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMonitorRouteAndNegotiation(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	app := fiber.New()
	app.Get("/metrics", m.handler())

	tests := []struct {
		name        string
		path        string
		accept      string
		contentType string
		contains    string
	}{
		{name: "default html", path: "/metrics", contentType: fiber.MIMETextHTML, contains: "Fiber Monitor"},
		{name: "trailing slash", path: "/metrics/", accept: "*/*", contentType: fiber.MIMETextHTML, contains: "Fiber Monitor"},
		{name: "html preferred", path: "/metrics", accept: "text/html,application/json", contentType: fiber.MIMETextHTML, contains: "Fiber Monitor"},
		{name: "json", path: "/metrics", accept: fiber.MIMEApplicationJSON, contentType: fiber.MIMEApplicationJSON, contains: `"http"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(fiber.MethodGet, test.path, nil)
			if test.accept != "" {
				req.Header.Set(fiber.HeaderAccept, test.accept)
			}
			resp, requestErr := app.Test(req)
			require.NoError(t, requestErr)
			defer func() { assert.NoError(t, resp.Body.Close()) }()
			body, readErr := io.ReadAll(resp.Body)
			require.NoError(t, readErr)
			assert.Equal(t, fiber.StatusOK, resp.StatusCode)
			assert.Contains(t, resp.Header.Get(fiber.HeaderContentType), test.contentType)
			assert.Equal(t, "no-store", resp.Header.Get(headerCacheControl))
			assert.Equal(t, "nosniff", resp.Header.Get(headerXContentTypeOptions))
			assert.Contains(t, string(body), test.contains)
		})
	}
	assert.Zero(t, m.requests.Load(), "monitor endpoint requests must not be counted")
}

func TestJSONUsesFinalSnapshotShape(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	app := fiber.New()
	app.Get("/metrics", m.handler())

	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	var current snapshot
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &current))
	assert.False(t, current.CollectedAt.IsZero())
	assert.NotNil(t, current.Collection.Errors)
	assert.Nil(t, current.HTTP.RPS)
	assert.Nil(t, current.HTTP.Latency.P50NS)
	assert.Nil(t, current.Process.CPUPercent)
	assert.Nil(t, current.System.NetworkReceiveBPS)
	assert.False(t, current.Runtime.GCPauseMetricsEnabled)
	assert.Nil(t, current.Runtime.GCPauseLastNS)
	assert.Nil(t, current.Runtime.GCPauseWindowNS)
	assert.Nil(t, current.Runtime.GCPauseTotalNS)
	for _, field := range []string{
		`"collected_at"`,
		`"collection"`,
		`"process"`,
		`"runtime"`,
		`"system"`,
		`"http"`,
		`"heap_sys_bytes"`,
		`"gc_pause_window_ns"`,
		`"memory_available_bytes"`,
		`"disk_fstype"`,
		`"1xx"`,
	} {
		assert.Contains(t, string(body), field)
	}
}

func TestMonitorMethodHandling(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	app := fiber.New()
	app.Use("/metrics", m.handler())

	headResp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/metrics", nil))
	require.NoError(t, err)
	assert.NoError(t, headResp.Body.Close())
	assert.Equal(t, fiber.StatusOK, headResp.StatusCode)

	postResp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/metrics", nil))
	require.NoError(t, err)
	assert.NoError(t, postResp.Body.Close())
	assert.Equal(t, fiber.StatusMethodNotAllowed, postResp.StatusCode)
	assert.Equal(t, "GET, HEAD", postResp.Header.Get(headerAllow))
	assert.Zero(t, m.requests.Load())
}

func TestAPIOnlyAlwaysReturnsJSON(t *testing.T) {
	m, err := newMiddleware(Config{APIOnly: true})
	require.NoError(t, err)
	app := fiber.New()
	app.Get("/metrics", m.handler())

	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMETextHTML)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON)
	assert.Contains(t, string(body), `"process"`)
}

func TestNextPassesThroughAndRecordsBusinessRequests(t *testing.T) {
	m, err := newMiddleware(Config{Next: func(c fiber.Ctx) bool { return c.Path() != "/metrics" }})
	require.NoError(t, err)
	app := fiber.New()
	app.Use(m.handler())
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	business, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ok", nil))
	require.NoError(t, err)
	assert.NoError(t, business.Body.Close())
	assert.Equal(t, fiber.StatusNoContent, business.StatusCode)

	dashboard, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil))
	require.NoError(t, err)
	assert.NoError(t, dashboard.Body.Close())
	assert.Equal(t, fiber.StatusOK, dashboard.StatusCode)

	assert.Equal(t, uint64(1), m.requests.Load())
	assert.Equal(t, uint64(1), m.status2.Load())
	assert.Equal(t, uint64(1), m.latency.snapshotAndReset().count)
}

func TestBusinessRequestStatusClassification(t *testing.T) {
	m := mustInstrumentingMiddleware(t)
	app := fiber.New()
	app.Use(m.handler())
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	app.Get("/redirect", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusFound) })
	app.Get("/fiber-error", func(fiber.Ctx) error { return fiber.ErrNotFound })
	app.Get("/generic-error", func(fiber.Ctx) error { return errors.New("boom") })

	for _, path := range []string{"/ok", "/redirect", "/fiber-error", "/generic-error"} {
		resp, requestErr := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		require.NoError(t, requestErr)
		assert.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, uint64(4), m.requests.Load())
	assert.Zero(t, m.inFlight.Load())
	assert.Equal(t, uint64(1), m.status2.Load())
	assert.Equal(t, uint64(1), m.status3.Load())
	assert.Equal(t, uint64(1), m.status4.Load())
	assert.Equal(t, uint64(1), m.status5.Load())
	assert.Equal(t, uint64(4), m.latency.snapshotAndReset().count)
}

func TestCustomErrorHandlerStatusIsRecorded(t *testing.T) {
	m := mustInstrumentingMiddleware(t)
	app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, _ error) error {
		return c.Status(fiber.StatusUnprocessableEntity).SendString("custom response")
	}})
	app.Use(m.handler())
	app.Get("/error", func(fiber.Ctx) error { return errors.New("boom") })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/error", nil))
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "custom response", string(body))
	assert.Equal(t, uint64(1), m.status4.Load())
	assert.Zero(t, m.status5.Load())
}

func TestFiberTimeoutIsRecordedAsTimeout(t *testing.T) {
	m := mustInstrumentingMiddleware(t)
	app := fiber.New()
	app.Use(m.handler())
	app.Get("/slow", timeout.New(func(c fiber.Ctx) error {
		select {
		case <-c.Context().Done():
			return c.Context().Err()
		case <-time.After(2 * time.Second):
			return c.SendString("too late")
		}
	}, timeout.Config{Timeout: 20 * time.Millisecond}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/slow", nil))
	require.NoError(t, err)
	assert.NoError(t, resp.Body.Close())
	assert.Equal(t, fiber.StatusRequestTimeout, resp.StatusCode)
	assert.Equal(t, uint64(1), m.status4.Load())
	assert.Zero(t, m.status2.Load())
}

func TestRecoveredPanicDoesNotLeakInFlight(t *testing.T) {
	m := mustInstrumentingMiddleware(t)
	app := fiber.New()
	app.Use(m.handler(), recovermw.New())
	app.Get("/panic", func(fiber.Ctx) error { panic("boom") })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/panic", nil))
	require.NoError(t, err)
	assert.NoError(t, resp.Body.Close())
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	assert.Zero(t, m.inFlight.Load())
	assert.Equal(t, uint64(1), m.requests.Load())
	assert.Equal(t, uint64(1), m.status5.Load())
	assert.Equal(t, uint64(1), m.latency.snapshotAndReset().count)
}

func TestConcurrentBusinessRequests(t *testing.T) {
	m := mustInstrumentingMiddleware(t)
	app := fiber.New()
	app.Use(m.handler())
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	const requests = 100
	var group sync.WaitGroup
	errorsCh := make(chan error, requests*2)
	for range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			resp, requestErr := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
			if requestErr != nil {
				errorsCh <- requestErr
				return
			}
			if closeErr := resp.Body.Close(); closeErr != nil {
				errorsCh <- closeErr
			}
		}()
	}
	group.Wait()
	close(errorsCh)
	for requestErr := range errorsCh {
		assert.NoError(t, requestErr)
	}
	assert.Equal(t, uint64(requests), m.requests.Load())
	assert.Equal(t, uint64(requests), m.status2.Load())
	assert.Zero(t, m.inFlight.Load())
	assert.Equal(t, uint64(requests), m.latency.snapshotAndReset().count)
}

func TestBusinessHotPathDoesNotUseCollectionMutex(t *testing.T) {
	m := mustInstrumentingMiddleware(t)
	app := fiber.New()
	app.Use(m.handler())
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	m.collectMu.Lock()
	done := make(chan error, 1)
	go func() {
		resp, requestErr := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
		if requestErr == nil {
			requestErr = resp.Body.Close()
		}
		done <- requestErr
	}()
	select {
	case requestErr := <-done:
		m.collectMu.Unlock()
		assert.NoError(t, requestErr)
	case <-time.After(500 * time.Millisecond):
		m.collectMu.Unlock()
		t.Fatal("business request blocked on collection mutex")
	}
}

func mustInstrumentingMiddleware(t testing.TB) *middleware {
	t.Helper()
	m, err := newMiddleware(Config{Next: func(fiber.Ctx) bool { return true }})
	require.NoError(t, err)
	return m
}
