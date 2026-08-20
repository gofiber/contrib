// Package stats provides a real-time operational statistics dashboard for Fiber.
package stats

import (
	"errors"
	"fmt"
	"strings"
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

const initialDashboard = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Fiber Stats</title></head><body><main><h1>Fiber Stats</h1></main></body></html>`

type middleware struct {
	next    func(fiber.Ctx) bool
	path    string
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

// New creates a Fiber handler that records aggregate request statistics and
// serves the dashboard at Config.Path.
func New(config ...Config) fiber.Handler {
	m, err := newMiddleware(config...)
	if err != nil {
		panic(fmt.Errorf("fiber: stats middleware error -> %w", err))
	}
	return m.handler()
}

func newMiddleware(config ...Config) (*middleware, error) {
	cfg, err := configDefault(config...).normalized()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	m := &middleware{
		next:      cfg.Next,
		path:      cfg.Path,
		refresh:   cfg.Refresh,
		index:     initialDashboard,
		collector: newCollector(now),
		now:       time.Now,
	}
	m.collectFn = m.collectSnapshot
	return m, nil
}

func (m *middleware) handler() fiber.Handler {
	return func(c fiber.Ctx) error {
		if m.next != nil && m.next(c) {
			return c.Next()
		}
		if m.isStatsRoute(c) {
			return m.serveStats(c)
		}

		started := time.Now()
		sequence := m.requests.Add(1)
		m.inFlight.Add(1)
		defer m.inFlight.Add(^uint64(0))

		err := c.Next()
		m.recordStatus(resolveStatus(c, err))
		elapsed := time.Since(started)
		if elapsed < 0 {
			elapsed = 0
		}
		m.latency.observeSharded(uint64(elapsed.Nanoseconds()), sequence)
		return err
	}
}

func (m *middleware) isStatsRoute(c fiber.Ctx) bool {
	if matchStatsRoute(c.Path(), m.path) {
		return true
	}
	return matchStatsRoute(middlewareRelativePath(c), m.path)
}

func matchStatsRoute(path, statsPath string) bool {
	return path == statsPath || (statsPath != "/" && path == statsPath+"/")
}

func middlewareRelativePath(c fiber.Ctx) string {
	path := c.Path()
	if !c.IsMiddleware() {
		return path
	}
	prefix := strings.TrimSuffix(c.FullPath(), "/*")
	if prefix == "" || prefix == "/" {
		return path
	}
	if path == prefix {
		return "/"
	}
	if strings.HasPrefix(path, prefix+"/") {
		return path[len(prefix):]
	}
	return path
}

func (m *middleware) serveStats(c fiber.Ctx) error {
	method := c.Method()
	if method != fiber.MethodGet && method != fiber.MethodHead {
		c.Set(headerAllow, "GET, HEAD")
		return fiber.ErrMethodNotAllowed
	}
	if c.Accepts(fiber.MIMETextHTML, fiber.MIMEApplicationJSON) == fiber.MIMEApplicationJSON {
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
	snapshot := m.currentSnapshot(m.now())
	return c.Status(fiber.StatusOK).JSON(&snapshot)
}

func setDashboardHeaders(c fiber.Ctx) {
	c.Set(headerCacheControl, "no-store")
	c.Set(headerXContentTypeOptions, "nosniff")
}

func resolveStatus(c fiber.Ctx, err error) int {
	status := c.Response().StatusCode()
	if status != fiber.StatusOK {
		return status
	}
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return fiberErr.Code
	}
	if err != nil {
		return fiber.StatusInternalServerError
	}
	if status < 100 || status > 599 {
		return fiber.StatusOK
	}
	return status
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
