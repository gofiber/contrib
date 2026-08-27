// Package monitor provides real-time operational metrics for Fiber services.
package monitor

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	headerCacheControl        = "Cache-Control"
	headerXContentTypeOptions = "X-Content-Type-Options"
	headerAllow               = "Allow"
)

// middleware keeps the business-request path limited to time reads and
// atomics. collectMu protects the mutable collector baselines and histogram
// reset only while a fresh dashboard snapshot is being built; business
// requests never acquire it.
type middleware struct {
	next    func(fiber.Ctx) bool
	apiOnly bool
	refresh time.Duration
	index   string

	requests atomic.Uint64
	inFlight atomic.Uint64
	status1  atomic.Uint64
	status2  atomic.Uint64
	status3  atomic.Uint64
	status4  atomic.Uint64
	status5  atomic.Uint64
	latency  latencyHistogram

	collectMu sync.Mutex
	cache     atomic.Pointer[cacheEntry]
	collector collector
	collectFn func(time.Time) snapshot
	now       func() time.Time
}

// New creates a Fiber handler that serves the dashboard and JSON snapshot on
// whichever route it is mounted. When Config.Next returns true, the request is
// passed downstream and included in the aggregate HTTP metrics instead.
func New(config ...Config) fiber.Handler {
	m, err := newMiddleware(config...)
	if err != nil {
		panic(fmt.Errorf("fiber: monitor middleware error -> %w", err))
	}
	return m.handler()
}

func newMiddleware(config ...Config) (*middleware, error) {
	cfg, err := configDefault(config...).normalized()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	index, err := renderDashboard(cfg)
	if err != nil {
		return nil, err
	}
	m := &middleware{
		next:      cfg.Next,
		apiOnly:   cfg.APIOnly,
		refresh:   cfg.Refresh,
		index:     index,
		collector: newCollector(now, cfg.EnableGCPauseMetrics),
		now:       time.Now,
	}
	m.collectFn = m.collectSnapshot
	return m, nil
}

func (m *middleware) handler() fiber.Handler {
	return func(c fiber.Ctx) error {
		// This preserves monitor's route-agnostic mounting contract. A false
		// Next result identifies the monitor endpoint; a true result passes the
		// request through and records it as application traffic.
		if m.next != nil && m.next(c) {
			return m.instrument(c)
		}
		return m.serveMonitor(c)
	}
}

// instrument records the response that Fiber ultimately sends. Fiber normally
// runs the application ErrorHandler only after the whole handler chain returns,
// so invoking it here is necessary to observe custom client-visible statuses.
func (m *middleware) instrument(c fiber.Ctx) error {
	started := time.Now()
	sequence := m.requests.Add(1)
	m.inFlight.Add(1)
	// Keep in-flight balanced when downstream panics. Panic recovery remains the
	// application's responsibility and should be mounted after monitor.
	defer m.inFlight.Add(^uint64(0))

	err := c.Next()
	if err != nil {
		if handlerErr := c.App().ErrorHandler(c, err); handlerErr != nil {
			_ = c.SendStatus(fiber.StatusInternalServerError) //nolint:errcheck // mirrors Fiber's fallback
		}
	}

	// Fiber's timeout middleware parks its response separately because the timed
	// out handler can still own the live response buffer. Read that response when
	// present so a client-visible 408 is not classified as a 200.
	response := c.Response()
	if timedOut := c.RequestCtx().LastTimeoutErrorResponse(); timedOut != nil {
		response = timedOut
	}
	m.recordStatus(response.StatusCode())

	elapsed := time.Since(started)
	if elapsed < 0 {
		elapsed = 0
	}
	m.latency.observeSharded(uint64(elapsed.Nanoseconds()), sequence)
	return nil
}

func (m *middleware) serveMonitor(c fiber.Ctx) error {
	method := c.Method()
	if method != fiber.MethodGet && method != fiber.MethodHead {
		c.Set(headerAllow, "GET, HEAD")
		return fiber.ErrMethodNotAllowed
	}
	if m.apiOnly || c.Accepts(fiber.MIMETextHTML, fiber.MIMEApplicationJSON) == fiber.MIMEApplicationJSON {
		return m.serveJSON(c)
	}
	return m.serveHTML(c)
}

func (m *middleware) serveHTML(c fiber.Ctx) error {
	setDashboardHeaders(c)
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.Status(fiber.StatusOK).SendString(m.index)
}

func (m *middleware) serveJSON(c fiber.Ctx) error {
	setDashboardHeaders(c)
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
	current := m.currentSnapshot(m.now())
	return c.Status(fiber.StatusOK).JSON(&current)
}

func setDashboardHeaders(c fiber.Ctx) {
	c.Set(headerCacheControl, "no-store")
	c.Set(headerXContentTypeOptions, "nosniff")
}

func (m *middleware) recordStatus(status int) {
	switch status / 100 {
	case 1:
		m.status1.Add(1)
	case 2:
		m.status2.Add(1)
	case 3:
		m.status3.Add(1)
	case 4:
		m.status4.Add(1)
	case 5:
		m.status5.Add(1)
	}
}
