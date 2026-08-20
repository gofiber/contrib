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
)

func TestMonitorRouteAndNegotiation(t *testing.T) {
	m, err := newMiddleware()
	mustNoError(t, err)
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
			mustNoError(t, requestErr)
			defer func() { mustNoError(t, resp.Body.Close()) }()
			body, readErr := io.ReadAll(resp.Body)
			mustNoError(t, readErr)
			mustEqual(t, fiber.StatusOK, resp.StatusCode)
			mustContain(t, resp.Header.Get(fiber.HeaderContentType), test.contentType)
			mustEqual(t, "no-store", resp.Header.Get(headerCacheControl))
			mustEqual(t, "nosniff", resp.Header.Get(headerXContentTypeOptions))
			mustContain(t, string(body), test.contains)
		})
	}
	mustZero(t, m.requests.Load(), "monitor endpoint requests must not be counted")
}

func TestJSONUsesFinalSnapshotShape(t *testing.T) {
	m, err := newMiddleware()
	mustNoError(t, err)
	app := fiber.New()
	app.Get("/metrics", m.handler())

	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	mustNoError(t, err)
	defer func() { mustNoError(t, resp.Body.Close()) }()
	var current snapshot
	body, err := io.ReadAll(resp.Body)
	mustNoError(t, err)
	mustNoError(t, json.Unmarshal(body, &current))
	mustFalse(t, current.CollectedAt.IsZero())
	mustNotNil(t, current.Collection.Errors)
	mustNil(t, current.HTTP.RPS)
	mustNil(t, current.HTTP.Latency.P50NS)
	mustNil(t, current.Process.CPUPercent)
	mustNil(t, current.System.NetworkReceiveBPS)
	mustFalse(t, current.Runtime.GCPauseMetricsEnabled)
	mustNil(t, current.Runtime.GCPauseLastNS)
	mustNil(t, current.Runtime.GCPauseWindowNS)
	mustNil(t, current.Runtime.GCPauseTotalNS)
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
		mustContain(t, string(body), field)
	}
}

func TestMonitorMethodHandling(t *testing.T) {
	m, err := newMiddleware()
	mustNoError(t, err)
	app := fiber.New()
	app.Use("/metrics", m.handler())

	headResp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/metrics", nil))
	mustNoError(t, err)
	mustNoError(t, headResp.Body.Close())
	mustEqual(t, fiber.StatusOK, headResp.StatusCode)

	postResp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/metrics", nil))
	mustNoError(t, err)
	mustNoError(t, postResp.Body.Close())
	mustEqual(t, fiber.StatusMethodNotAllowed, postResp.StatusCode)
	mustEqual(t, "GET, HEAD", postResp.Header.Get(headerAllow))
	mustZero(t, m.requests.Load())
}

func TestAPIOnlyAlwaysReturnsJSON(t *testing.T) {
	m, err := newMiddleware(Config{APIOnly: true})
	mustNoError(t, err)
	app := fiber.New()
	app.Get("/metrics", m.handler())

	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMETextHTML)
	resp, err := app.Test(req)
	mustNoError(t, err)
	defer func() { mustNoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	mustNoError(t, err)
	mustEqual(t, fiber.StatusOK, resp.StatusCode)
	mustContain(t, resp.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON)
	mustContain(t, string(body), `"process"`)
}

func TestNextPassesThroughAndRecordsBusinessRequests(t *testing.T) {
	m, err := newMiddleware(Config{Next: func(c fiber.Ctx) bool { return c.Path() != "/metrics" }})
	mustNoError(t, err)
	app := fiber.New()
	app.Use(m.handler())
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	business, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ok", nil))
	mustNoError(t, err)
	mustNoError(t, business.Body.Close())
	mustEqual(t, fiber.StatusNoContent, business.StatusCode)

	dashboard, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil))
	mustNoError(t, err)
	mustNoError(t, dashboard.Body.Close())
	mustEqual(t, fiber.StatusOK, dashboard.StatusCode)

	mustEqual(t, uint64(1), m.requests.Load())
	mustEqual(t, uint64(1), m.status2.Load())
	mustEqual(t, uint64(1), m.latency.snapshotAndReset().count)
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
		mustNoError(t, requestErr)
		mustNoError(t, resp.Body.Close())
	}
	mustEqual(t, uint64(4), m.requests.Load())
	mustZero(t, m.inFlight.Load())
	mustEqual(t, uint64(1), m.status2.Load())
	mustEqual(t, uint64(1), m.status3.Load())
	mustEqual(t, uint64(1), m.status4.Load())
	mustEqual(t, uint64(1), m.status5.Load())
	mustEqual(t, uint64(4), m.latency.snapshotAndReset().count)
}

func TestCustomErrorHandlerStatusIsRecorded(t *testing.T) {
	m := mustInstrumentingMiddleware(t)
	app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, _ error) error {
		return c.Status(fiber.StatusUnprocessableEntity).SendString("custom response")
	}})
	app.Use(m.handler())
	app.Get("/error", func(fiber.Ctx) error { return errors.New("boom") })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/error", nil))
	mustNoError(t, err)
	defer func() { mustNoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	mustNoError(t, err)
	mustEqual(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
	mustEqual(t, "custom response", string(body))
	mustEqual(t, uint64(1), m.status4.Load())
	mustZero(t, m.status5.Load())
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
	mustNoError(t, err)
	mustNoError(t, resp.Body.Close())
	mustEqual(t, fiber.StatusRequestTimeout, resp.StatusCode)
	mustEqual(t, uint64(1), m.status4.Load())
	mustZero(t, m.status2.Load())
}

func TestRecoveredPanicDoesNotLeakInFlight(t *testing.T) {
	m := mustInstrumentingMiddleware(t)
	app := fiber.New()
	app.Use(m.handler(), recovermw.New())
	app.Get("/panic", func(fiber.Ctx) error { panic("boom") })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/panic", nil))
	mustNoError(t, err)
	mustNoError(t, resp.Body.Close())
	mustEqual(t, fiber.StatusInternalServerError, resp.StatusCode)
	mustZero(t, m.inFlight.Load())
	mustEqual(t, uint64(1), m.requests.Load())
	mustEqual(t, uint64(1), m.status5.Load())
	mustEqual(t, uint64(1), m.latency.snapshotAndReset().count)
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
		mustNoError(t, requestErr)
	}
	mustEqual(t, uint64(requests), m.requests.Load())
	mustEqual(t, uint64(requests), m.status2.Load())
	mustZero(t, m.inFlight.Load())
	mustEqual(t, uint64(requests), m.latency.snapshotAndReset().count)
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
		mustNoError(t, requestErr)
	case <-time.After(500 * time.Millisecond):
		m.collectMu.Unlock()
		t.Fatal("business request blocked on collection mutex")
	}
}

func mustInstrumentingMiddleware(t testing.TB) *middleware {
	t.Helper()
	m, err := newMiddleware(Config{Next: func(fiber.Ctx) bool { return true }})
	mustNoError(t, err)
	return m
}
