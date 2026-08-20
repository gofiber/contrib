package stats

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	recovermw "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/stretchr/testify/require"
)

func TestStatsRoutesAndNegotiation(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	app := fiber.New()
	app.Use(m.handler())
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	tests := []struct {
		name        string
		path        string
		accept      string
		contentType string
		contains    string
	}{
		{name: "default html", path: "/stats", contentType: fiber.MIMETextHTML, contains: "Fiber Stats"},
		{name: "slash html", path: "/stats/", accept: "*/*", contentType: fiber.MIMETextHTML, contains: "Fiber Stats"},
		{name: "html preferred", path: "/stats", accept: "text/html,application/json", contentType: fiber.MIMETextHTML, contains: "Fiber Stats"},
		{name: "json", path: "/stats", accept: fiber.MIMEApplicationJSON, contentType: fiber.MIMEApplicationJSON, contains: `"http"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(fiber.MethodGet, test.path, nil)
			if test.accept != "" {
				req.Header.Set(fiber.HeaderAccept, test.accept)
			}
			resp, requestErr := app.Test(req)
			require.NoError(t, requestErr)
			t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
			body, readErr := io.ReadAll(resp.Body)
			require.NoError(t, readErr)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
			require.Contains(t, resp.Header.Get(fiber.HeaderContentType), test.contentType)
			require.Equal(t, "no-store", resp.Header.Get(headerCacheControl))
			require.Equal(t, "nosniff", resp.Header.Get(headerXContentTypeOptions))
			require.Contains(t, string(body), test.contains)
		})
	}
	require.Zero(t, m.requests.Load(), "dashboard requests must not be counted")
}

func TestJSONUsesFinalSnapshotShape(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	app := fiber.New()
	app.Use(m.handler())

	req := httptest.NewRequest(fiber.MethodGet, "/stats", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	var current snapshot
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&current))
	require.False(t, current.CollectedAt.IsZero())
	require.NotNil(t, current.Collection.Errors)
	require.Nil(t, current.HTTP.RPS)
	require.Nil(t, current.HTTP.Latency.P50NS)
	require.Nil(t, current.Process.CPUPercent)
	require.Nil(t, current.System.NetworkReceiveBPS)
}

func TestStatsMethodHandling(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	app := fiber.New()
	app.Use(m.handler())

	headResp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/stats", nil))
	require.NoError(t, err)
	require.NoError(t, headResp.Body.Close())
	require.Equal(t, fiber.StatusOK, headResp.StatusCode)

	postResp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/stats", nil))
	require.NoError(t, err)
	require.NoError(t, postResp.Body.Close())
	require.Equal(t, fiber.StatusMethodNotAllowed, postResp.StatusCode)
	require.Equal(t, "GET, HEAD", postResp.Header.Get(headerAllow))
	require.Zero(t, m.requests.Load())
}

func TestNextBypassesServingAndCounting(t *testing.T) {
	m, err := newMiddleware(Config{Next: func(c fiber.Ctx) bool { return c.Get("X-Skip") == "yes" }})
	require.NoError(t, err)
	app := fiber.New()
	app.Use(m.handler())
	app.Use(func(c fiber.Ctx) error { return c.SendString("downstream") })

	req := httptest.NewRequest(fiber.MethodGet, "/stats", nil)
	req.Header.Set("X-Skip", "yes")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "downstream", string(body))
	require.Zero(t, m.requests.Load())
}

func TestBusinessRequestInstrumentation(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	app := fiber.New()
	app.Use(m.handler())
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	app.Get("/redirect", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusFound) })
	app.Get("/fiber-error", func(fiber.Ctx) error { return fiber.ErrNotFound })
	app.Get("/generic-error", func(fiber.Ctx) error { return errors.New("boom") })

	for _, path := range []string{"/ok", "/redirect", "/fiber-error", "/generic-error"} {
		resp, requestErr := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		require.NoError(t, requestErr)
		require.NoError(t, resp.Body.Close())
	}
	require.Equal(t, uint64(4), m.requests.Load())
	require.Zero(t, m.inFlight.Load())
	require.Equal(t, uint64(1), m.status2.Load())
	require.Equal(t, uint64(1), m.status3.Load())
	require.Equal(t, uint64(1), m.status4.Load())
	require.Equal(t, uint64(1), m.status5.Load())
	require.Equal(t, uint64(4), m.latency.snapshotAndReset().count)
}

func TestRelativeMiddlewareMount(t *testing.T) {
	m, err := newMiddleware(Config{Path: "/stats"})
	require.NoError(t, err)
	app := fiber.New()
	app.Use("/internal", m.handler())

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/internal/stats", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(body), "Fiber Stats"))
}

func TestRecoveredPanicDoesNotLeakInFlight(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	app := fiber.New()
	app.Use(m.handler(), recovermw.New())
	app.Get("/panic", func(fiber.Ctx) error { panic("boom") })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/panic", nil))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	require.Zero(t, m.inFlight.Load())
	require.Equal(t, uint64(1), m.requests.Load())
	require.Equal(t, uint64(1), m.status5.Load())
	require.Equal(t, uint64(1), m.latency.snapshotAndReset().count)
}

func TestConcurrentBusinessRequests(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
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
		require.NoError(t, requestErr)
	}
	require.Equal(t, uint64(requests), m.requests.Load())
	require.Equal(t, uint64(requests), m.status2.Load())
	require.Zero(t, m.inFlight.Load())
	require.Equal(t, uint64(requests), m.latency.snapshotAndReset().count)
}

func TestBusinessHotPathDoesNotUseCollectionMutex(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
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
		require.NoError(t, requestErr)
	case <-time.After(500 * time.Millisecond):
		m.collectMu.Unlock()
		t.Fatal("business request blocked on collection mutex")
	}
}
