// Package prometheus provides a Fiber middleware that exposes Prometheus
// metrics while instrumenting incoming HTTP traffic.
package prometheus

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/utils/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.opentelemetry.io/otel/trace"
)

// middleware encapsulates all mutable state required to expose metrics and
// instrument Fiber requests.
type middleware struct {
	cfg              Config
	gatherer         prometheus.Gatherer
	requestsTotal    *prometheus.CounterVec
	requestsByClass  *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestSize      *prometheus.HistogramVec
	responseSize     *prometheus.HistogramVec
	requestInFlight  *prometheus.GaugeVec
	metricsHandler   fiber.Handler
	skipURIs         map[string]struct{}
	ignoreStatusCode map[int]struct{}
	registeredRoutes map[string]struct{}
	routesVersion    int
	routesMu         sync.RWMutex
}

// New creates a new Prometheus middleware handler.
//
// The returned handler records request/response metrics for all routes that are
// mounted in the current Fiber application and serves the Prometheus endpoint
// when it detects that the registered metrics route is being invoked.
func New(config ...Config) fiber.Handler {
	cfg := configDefault(config...)

	registry, gatherer := resolveRegistry(cfg)

	if !cfg.DisableGoCollector {
		registerCollector(registry, collectors.NewGoCollector())
	}

	if !cfg.DisableProcessCollector {
		registerCollector(registry, collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}

	labels := make(prometheus.Labels, len(cfg.Labels)+1)
	for key, value := range cfg.Labels {
		labels[key] = value
	}
	if cfg.Service != "" {
		labels["service"] = cfg.Service
	}

	counter := promauto.With(registry).NewCounterVec(
		prometheus.CounterOpts{
			Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, "requests_total"),
			Help:        "Count all http requests by status code, method and path.",
			ConstLabels: labels,
		},
		[]string{"status_code", "method", "path"},
	)

	statusClassCounter := promauto.With(registry).NewCounterVec(
		prometheus.CounterOpts{
			Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, "requests_status_class_total"),
			Help:        "Count all http requests grouped by status class, method and path.",
			ConstLabels: labels,
		},
		[]string{"status_class", "method", "path"},
	)

	histogram := promauto.With(registry).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, "request_duration_seconds"),
			Help:        "Duration of all HTTP requests by status code, method and path.",
			ConstLabels: labels,
			Buckets:     cfg.RequestDurationBuckets,
		},
		[]string{"status_code", "method", "path"},
	)

	requestHistogram := promauto.With(registry).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, "request_size_bytes"),
			Help:        "Size of all HTTP requests by status code, method and path.",
			ConstLabels: labels,
			Buckets:     cfg.RequestSizeBuckets,
		},
		[]string{"status_code", "method", "path"},
	)

	responseHistogram := promauto.With(registry).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, "response_size_bytes"),
			Help:        "Size of all HTTP responses by status code, method and path.",
			ConstLabels: labels,
			Buckets:     cfg.ResponseSizeBuckets,
		},
		[]string{"status_code", "method", "path"},
	)

	gauge := promauto.With(registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, "requests_in_progress_total"),
			Help:        "All the requests in progress",
			ConstLabels: labels,
		},
		[]string{"method", "path"},
	)

	metricsHandler := adaptor.HTTPHandler(promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics:                   cfg.EnableOpenMetrics,
		EnableOpenMetricsTextCreatedSamples: cfg.EnableOpenMetricsTextCreatedSamples,
		DisableCompression:                  cfg.DisableCompression,
	}))

	m := &middleware{
		cfg:              cfg,
		gatherer:         gatherer,
		requestsTotal:    counter,
		requestsByClass:  statusClassCounter,
		requestDuration:  histogram,
		requestSize:      requestHistogram,
		responseSize:     responseHistogram,
		requestInFlight:  gauge,
		metricsHandler:   metricsHandler,
		skipURIs:         make(map[string]struct{}, len(cfg.SkipURIs)),
		ignoreStatusCode: make(map[int]struct{}, len(cfg.IgnoreStatusCodes)),
	}

	for _, path := range cfg.SkipURIs {
		m.skipURIs[normalizePath(path)] = struct{}{}
	}

	for _, code := range cfg.IgnoreStatusCodes {
		m.ignoreStatusCode[code] = struct{}{}
	}

	return func(ctx fiber.Ctx) error {
		return m.handle(ctx)
	}
}

// resolveRegistry selects the registerer/gatherer pair used for collector
// registration and metrics exposure, enforcing that both interfaces point to the
// same metrics source.
func resolveRegistry(cfg Config) (prometheus.Registerer, prometheus.Gatherer) {
	registerer := cfg.Registerer
	gatherer := cfg.Gatherer

	if registerer == nil && gatherer == nil {
		reg := prometheus.NewRegistry()
		return reg, reg
	}

	if registerer == nil && gatherer != nil {
		if reg, ok := gatherer.(prometheus.Registerer); ok {
			return reg, gatherer
		}
		panic("prometheus middleware: provided Gatherer does not implement prometheus.Registerer; supply a matching Registerer")
	}

	if registerer != nil && gatherer == nil {
		if g, ok := registerer.(prometheus.Gatherer); ok {
			return registerer, g
		}
		panic("prometheus middleware: provided Registerer does not implement prometheus.Gatherer; supply a matching Gatherer or use prometheus.Registry")
	}

	if regGatherer, ok := registerer.(prometheus.Gatherer); ok {
		if regGatherer != gatherer {
			panic("prometheus middleware: Registerer and Gatherer must reference the same metrics source")
		}
		return registerer, gatherer
	}

	panic("prometheus middleware: Registerer must implement prometheus.Gatherer when a custom Gatherer is provided")
}

// handle dispatches the request to the next middleware and serves the metrics
// endpoint when the current route matches the registered metrics path.
func (m *middleware) handle(ctx fiber.Ctx) error {
	if m.isMetricsRequest(ctx) {
		method := ctx.Method()
		if method != fiber.MethodGet && method != fiber.MethodHead {
			return fiber.ErrMethodNotAllowed
		}
		_ = m.metricsHandler(ctx)
		return nil
	}

	if m.cfg.Next != nil && m.cfg.Next(ctx) {
		return ctx.Next()
	}

	return m.instrument(ctx)
}

// isMetricsRequest returns true when the current request is routed to the
// Prometheus endpoint exposed by this middleware.
func (m *middleware) isMetricsRequest(ctx fiber.Ctx) bool {
	route := ctx.Route()
	if route == nil {
		return false
	}

	registered := route.Path
	if registered == "" {
		registered = "/"
	} else if registered != "/" {
		registered = normalizePath(registered)
	}

	return registered == normalizePath(ctx.Path())
}

// instrument wraps the downstream handler, recording duration, request/response
// sizes, in-flight counts, and status code metrics for the active route.
func (m *middleware) instrument(ctx fiber.Ctx) error {
	method := utils.CopyString(ctx.Method())
	routePath := m.resolveRoutePath(ctx)
	routeKey := method + " " + routePath

	registered := m.refreshRoutes(ctx, routeKey)
	trackUnmatched := false
	if !registered && m.cfg.TrackUnmatchedRequests {
		routePath = normalizePath(m.cfg.UnmatchedRouteLabel)
		trackUnmatched = true
	}

	inflightPath := routePath
	m.requestInFlight.WithLabelValues(method, inflightPath).Inc()
	deleteGauge := false
	defer func() {
		m.requestInFlight.WithLabelValues(method, inflightPath).Dec()
		if deleteGauge {
			m.requestInFlight.DeleteLabelValues(method, inflightPath)
		}
	}()

	start := time.Now()

	err := ctx.Next()

	if !registered && !trackUnmatched {
		deleteGauge = true
		return err
	}

	if _, ok := m.skipURIs[routePath]; ok {
		deleteGauge = true
		return err
	}

	status := fiber.StatusInternalServerError
	if err != nil {
		if e, ok := err.(*fiber.Error); ok {
			status = e.Code
		}
	} else {
		status = ctx.Response().StatusCode()
	}

	if _, ok := m.ignoreStatusCode[status]; ok {
		deleteGauge = true
		return err
	}

	statusCode := strconv.Itoa(status)

	m.requestsTotal.WithLabelValues(statusCode, method, routePath).Inc()

	statusClass := strconv.Itoa(status/100) + "xx"
	m.requestsByClass.WithLabelValues(statusClass, method, routePath).Inc()

	elapsed := float64(time.Since(start).Nanoseconds()) / 1e9

	spanCtx := trace.SpanContextFromContext(ctx.Context())
	traceID := spanCtx.TraceID()
	var exemplarLabels prometheus.Labels
	if traceID.IsValid() {
		exemplarLabels = prometheus.Labels{"traceID": traceID.String()}
	}

	observe := func(observer prometheus.Observer, value float64) {
		if exemplarLabels != nil {
			if exemplarObserver, ok := observer.(prometheus.ExemplarObserver); ok {
				exemplarObserver.ObserveWithExemplar(value, exemplarLabels)
				return
			}
		}
		observer.Observe(value)
	}

	histogram := m.requestDuration.WithLabelValues(statusCode, method, routePath)
	observe(histogram, elapsed)

	requestLength := ctx.Request().Header.ContentLength()
	if requestLength < 0 {
		requestLength = len(ctx.Request().Body())
	}
	requestHistogram := m.requestSize.WithLabelValues(statusCode, method, routePath)
	observe(requestHistogram, float64(requestLength))

	responseLength := ctx.Response().Header.ContentLength()
	if responseLength < 0 {
		responseLength = len(ctx.Response().Body())
	}
	responseHistogram := m.responseSize.WithLabelValues(statusCode, method, routePath)
	observe(responseHistogram, float64(responseLength))

	return err
}

// refreshRoutes ensures the registeredRoutes map reflects the current Fiber
// stack. If the requested route key is missing or the stack length changes, the
// cache is rebuilt before returning the registration status for the provided
// key.
func (m *middleware) refreshRoutes(ctx fiber.Ctx, routeKey string) bool {
	stack := ctx.App().Stack()
	stackVersion := stackSize(stack)

	m.routesMu.RLock()
	currentVersion := m.routesVersion
	_, registered := m.registeredRoutes[routeKey]
	m.routesMu.RUnlock()

	if registered && currentVersion == stackVersion {
		return true
	}

	routes := make(map[string]struct{})
	for i := range stack {
		routesList := stack[i]
		for j := range routesList {
			r := routesList[j]
			if r == nil {
				continue
			}

			path := utils.CopyString(r.Path)
			if path == "" {
				path = "/"
			} else if path != "/" {
				path = normalizePath(path)
			}

			routes[r.Method+" "+path] = struct{}{}
			if r.Method == fiber.MethodGet {
				routes[fiber.MethodHead+" "+path] = struct{}{}
			}
		}
	}

	m.routesMu.Lock()
	m.registeredRoutes = routes
	m.routesVersion = stackVersion
	_, registered = routes[routeKey]
	m.routesMu.Unlock()

	return registered
}

// resolveRoutePath returns the normalized route path associated with the
// current request. When Fiber has not resolved a route, the request path is
// used as a fallback so metrics can still be attributed.
func (m *middleware) resolveRoutePath(ctx fiber.Ctx) string {
	routePath := "/"
	if route := ctx.Route(); route != nil {
		routePath = utils.CopyString(route.Path)
	}
	if routePath == "" || routePath == "/" {
		routePath = utils.CopyString(ctx.Path())
	}
	if routePath != "" && routePath != "/" {
		routePath = normalizePath(routePath)
	}
	return routePath
}

// normalizePath trims trailing slashes and converts empty paths to "/" so
// routes can be matched consistently.
func normalizePath(routePath string) string {
	normalized := strings.TrimRight(routePath, "/")
	if normalized == "" {
		return "/"
	}
	return normalized
}

// stackSize returns the total number of routes present in the Fiber stack.
func stackSize(stack [][]*fiber.Route) int {
	size := 0
	for i := range stack {
		size += len(stack[i])
	}
	return size
}

// registerCollector attempts to register the provided collector, suppressing
// the AlreadyRegistered error so callers can opt-in without coordination.
func registerCollector(registry prometheus.Registerer, collector prometheus.Collector) {
	if err := registry.Register(collector); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return
		}
		panic(err)
	}
}
