// Package prometheus provides a Fiber middleware that exposes Prometheus
// metrics while instrumenting incoming HTTP traffic.
package prometheus

import (
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
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

// middleware encapsulates all state required to expose metrics and instrument
// Fiber requests. Every metric vector is nil when its family is disabled
// through Config.DisabledMetrics.
type middleware struct {
	cfg              Config
	requestsTotal    *prometheus.CounterVec
	requestsByClass  *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestSize      *prometheus.HistogramVec
	responseSize     *prometheus.HistogramVec
	requestInFlight  *prometheus.GaugeVec
	metricsHandler   fiber.Handler
	metricsPath      string
	unmatchedLabel   string
	skipURIs         map[string]struct{}
	skipPrefixes     []string
	ignoreStatusCode map[int]struct{}
	ignoreClasses    map[string]struct{}
	dynamicLabels    []dynamicLabel
	records          bool
}

// dynamicLabel binds a configured label name to the function producing its
// value. The slice held by middleware is sorted by name so that the label order
// of a metric never depends on map iteration order.
type dynamicLabel struct {
	name string
	fn   func(fiber.Ctx) string
}

// reservedLabels are the label names the middleware sets itself.
var reservedLabels = map[string]struct{}{
	"status_code":  {},
	"status_class": {},
	"method":       {},
	"path":         {},
}

// New creates a new Prometheus middleware handler.
//
// The handler is meant to be mounted globally so that it observes every
// request the application serves:
//
//	app.Use(prometheus.New(prometheus.Config{Service: "my-service"}))
//
// Requests whose path equals Config.MetricsPath ("/metrics" by default) are
// answered with the Prometheus exposition format instead of being forwarded to
// the application; every other request is instrumented and passed along.
// Config.Next is evaluated first, so returning true from it passes even a
// scrape straight through to the application.
//
// Because Fiber only runs the application error handler after the whole
// handler chain has unwound, the middleware invokes it itself when a
// downstream handler returns an error. This mirrors Fiber's own logger
// middleware and is what allows the recorded status code and response size to
// match what the client received. As a consequence the error is consumed by
// this middleware and is not propagated to handlers mounted before it.
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

	dynamic := resolveDynamicLabels(cfg, labels)

	disabled := make(map[Metric]struct{}, len(cfg.DisabledMetrics))
	for _, metric := range cfg.DisabledMetrics {
		disabled[metric] = struct{}{}
	}
	enabled := func(metric Metric) bool {
		_, off := disabled[metric]
		return !off
	}

	// Both label sets carry the dynamic names last so that the value buffer
	// built per request can be shared between them.
	byStatusCode := variableLabels("status_code", dynamic)
	byStatusClass := variableLabels("status_class", dynamic)

	metricsHandler := adaptor.HTTPHandler(promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics:                   cfg.EnableOpenMetrics,
		EnableOpenMetricsTextCreatedSamples: cfg.EnableOpenMetricsTextCreatedSamples,
		DisableCompression:                  cfg.DisableCompression,
		MaxRequestsInFlight:                 cfg.MetricsMaxRequestsInFlight,
		Timeout:                             cfg.MetricsTimeout,
		ErrorLog:                            cfg.MetricsErrorLog,
		ErrorHandling:                       cfg.MetricsErrorHandling,
	}))

	m := &middleware{
		cfg:              cfg,
		metricsHandler:   metricsHandler,
		metricsPath:      normalizePath(cfg.MetricsPath),
		unmatchedLabel:   normalizePath(cfg.UnmatchedRouteLabel),
		skipURIs:         make(map[string]struct{}, len(cfg.SkipURIs)),
		ignoreStatusCode: make(map[int]struct{}, len(cfg.IgnoreStatusCodes)),
		ignoreClasses:    make(map[string]struct{}, len(cfg.IgnoreStatusClasses)),
		dynamicLabels:    dynamic,
	}

	if enabled(MetricRequestsTotal) {
		m.requestsTotal = promauto.With(registry).NewCounterVec(
			prometheus.CounterOpts{
				Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, string(MetricRequestsTotal)),
				Help:        "Count all http requests by status code, method and path.",
				ConstLabels: labels,
			},
			byStatusCode,
		)
	}

	if enabled(MetricRequestsStatusClassTotal) {
		m.requestsByClass = promauto.With(registry).NewCounterVec(
			prometheus.CounterOpts{
				Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, string(MetricRequestsStatusClassTotal)),
				Help:        "Count all http requests grouped by status class, method and path.",
				ConstLabels: labels,
			},
			byStatusClass,
		)
	}

	if enabled(MetricRequestDuration) {
		m.requestDuration = promauto.With(registry).NewHistogramVec(
			histogramOpts(cfg, MetricRequestDuration,
				"Duration of all HTTP requests by status code, method and path.",
				labels, cfg.RequestDurationBuckets),
			byStatusCode,
		)
	}

	if enabled(MetricRequestSize) {
		m.requestSize = promauto.With(registry).NewHistogramVec(
			histogramOpts(cfg, MetricRequestSize,
				"Size of all HTTP requests by status code, method and path.",
				labels, cfg.RequestSizeBuckets),
			byStatusCode,
		)
	}

	if enabled(MetricResponseSize) {
		m.responseSize = promauto.With(registry).NewHistogramVec(
			histogramOpts(cfg, MetricResponseSize,
				"Size of all HTTP responses by status code, method and path.",
				labels, cfg.ResponseSizeBuckets),
			byStatusCode,
		)
	}

	if enabled(MetricRequestsInProgress) {
		// The in-flight gauge has to be incremented before the router picks a
		// handler, at which point the route pattern is still unknown. Labelling
		// it by method only keeps the gauge balanced and its cardinality
		// bounded, and is also why dynamic labels are not applied to it.
		m.requestInFlight = promauto.With(registry).NewGaugeVec(
			prometheus.GaugeOpts{
				Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, string(MetricRequestsInProgress)),
				Help:        "All the requests in progress by method.",
				ConstLabels: labels,
			},
			[]string{"method"},
		)
	}

	m.records = m.requestsTotal != nil || m.requestsByClass != nil ||
		m.requestDuration != nil || m.requestSize != nil || m.responseSize != nil

	for _, path := range cfg.SkipURIs {
		if prefix, found := strings.CutSuffix(path, "*"); found {
			m.skipPrefixes = append(m.skipPrefixes, normalizePath(prefix))
			continue
		}
		m.skipURIs[normalizePath(path)] = struct{}{}
	}

	for _, code := range cfg.IgnoreStatusCodes {
		m.ignoreStatusCode[code] = struct{}{}
	}

	for _, class := range cfg.IgnoreStatusClasses {
		m.ignoreClasses[strings.ToLower(strings.TrimSpace(class))] = struct{}{}
	}

	return m.handle
}

// variableLabels builds a metric's variable label names: the status label the
// family is keyed by, the request labels, then the dynamic names in sorted
// order.
func variableLabels(statusLabel string, dynamic []dynamicLabel) []string {
	names := make([]string, 0, 3+len(dynamic))
	names = append(names, statusLabel, "method", "path")
	for _, label := range dynamic {
		names = append(names, label.name)
	}
	return names
}

// histogramOpts assembles the options shared by the three histogram families,
// including the native histogram settings.
func histogramOpts(cfg Config, name Metric, help string, labels prometheus.Labels, buckets []float64) prometheus.HistogramOpts {
	return prometheus.HistogramOpts{
		Name:                            prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, string(name)),
		Help:                            help,
		ConstLabels:                     labels,
		Buckets:                         buckets,
		NativeHistogramBucketFactor:     cfg.NativeHistogramBucketFactor,
		NativeHistogramMaxBucketNumber:  cfg.NativeHistogramMaxBucketNumber,
		NativeHistogramMinResetDuration: cfg.NativeHistogramMinResetDuration,
	}
}

// resolveDynamicLabels sorts the configured dynamic labels by name and rejects
// the ones that would clash with a label the middleware already sets. Sorting
// keeps the label order of a metric stable across restarts, which map iteration
// order would not.
func resolveDynamicLabels(cfg Config, constLabels prometheus.Labels) []dynamicLabel {
	if len(cfg.DynamicLabels) == 0 {
		return nil
	}

	names := make([]string, 0, len(cfg.DynamicLabels))
	for name := range cfg.DynamicLabels {
		names = append(names, name)
	}
	sort.Strings(names)

	dynamic := make([]dynamicLabel, 0, len(names))
	for _, name := range names {
		if _, reserved := reservedLabels[name]; reserved {
			panic("prometheus middleware: dynamic label " + strconv.Quote(name) + " collides with a built-in label")
		}
		if _, ok := constLabels[name]; ok {
			panic("prometheus middleware: dynamic label " + strconv.Quote(name) + " collides with a constant label")
		}
		fn := cfg.DynamicLabels[name]
		if fn == nil {
			panic("prometheus middleware: dynamic label " + strconv.Quote(name) + " has no function")
		}
		dynamic = append(dynamic, dynamicLabel{name: name, fn: fn})
	}

	return dynamic
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

	if registerer == nil {
		if reg, ok := gatherer.(prometheus.Registerer); ok {
			return reg, gatherer
		}
		panic("prometheus middleware: provided Gatherer does not implement prometheus.Registerer; supply a matching Registerer")
	}

	if gatherer == nil {
		if g, ok := registerer.(prometheus.Gatherer); ok {
			return registerer, g
		}
		panic("prometheus middleware: provided Registerer does not implement prometheus.Gatherer; supply a matching Gatherer or use prometheus.Registry")
	}

	if regGatherer, ok := registerer.(prometheus.Gatherer); ok {
		if differentSource(regGatherer, gatherer) {
			panic("prometheus middleware: Registerer and Gatherer must reference the same metrics source")
		}
		return registerer, gatherer
	}

	panic("prometheus middleware: Registerer must implement prometheus.Gatherer when a custom Gatherer is provided")
}

// differentSource reports whether two gatherers provably reference distinct
// metrics sources. Comparing interface values directly panics when the dynamic
// type is not comparable, so identity is established through reflection and
// cases that cannot be decided are accepted rather than aborting startup.
func differentSource(a, b prometheus.Gatherer) bool {
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if !av.IsValid() || !bv.IsValid() {
		return false
	}

	if av.Kind() == reflect.Pointer && bv.Kind() == reflect.Pointer {
		return av.Pointer() != bv.Pointer()
	}

	if !av.Type().Comparable() || !bv.Type().Comparable() {
		return false
	}

	return av.Interface() != bv.Interface()
}

// handle serves the metrics endpoint or instruments the request, depending on
// the requested path.
func (m *middleware) handle(ctx fiber.Ctx) error {
	if m.cfg.Next != nil && m.cfg.Next(ctx) {
		return ctx.Next()
	}

	if normalizePath(ctx.Path()) == m.metricsPath {
		return m.serveMetrics(ctx)
	}

	return m.instrument(ctx)
}

// serveMetrics answers a scrape request, rejecting methods the Prometheus
// exposition endpoint does not support.
func (m *middleware) serveMetrics(ctx fiber.Ctx) error {
	if method := ctx.Method(); method != fiber.MethodGet && method != fiber.MethodHead {
		ctx.Set(fiber.HeaderAllow, fiber.MethodGet+", "+fiber.MethodHead)
		return fiber.ErrMethodNotAllowed
	}

	return m.metricsHandler(ctx)
}

// instrument wraps the downstream handler, recording duration, request/response
// sizes, in-flight counts, and status code metrics for the matched route.
func (m *middleware) instrument(ctx fiber.Ctx) error {
	method := ctx.Method()

	if m.requestInFlight != nil {
		inFlight := m.requestInFlight.WithLabelValues(method)
		inFlight.Inc()
		defer inFlight.Dec()
	}

	start := time.Now()

	// Fiber runs the application error handler only after the entire handler
	// chain has unwound, so the response is still empty when Next reports an
	// error. Running it here - as Fiber's own logger middleware does - means
	// the status code and response size below are the ones the client sees.
	if chainErr := ctx.Next(); chainErr != nil {
		if err := ctx.App().ErrorHandler(ctx, chainErr); err != nil {
			_ = ctx.SendStatus(fiber.StatusInternalServerError) //nolint:errcheck // mirrors Fiber's own fallback
		}
	}

	if !m.records {
		return nil
	}

	elapsed := time.Since(start).Seconds()

	routePath, ok := m.routeLabel(ctx)
	if !ok {
		return nil
	}

	if m.skipped(routePath) {
		return nil
	}

	status := ctx.Response().StatusCode()
	if _, ignore := m.ignoreStatusCode[status]; ignore {
		return nil
	}

	class := statusClass(status)
	if _, ignore := m.ignoreClasses[class]; ignore {
		return nil
	}

	// One buffer shared by every family: the metric vectors copy the values
	// they are given, so element 0 can be swapped from the status code to the
	// status class for the last counter.
	values := make([]string, 3+len(m.dynamicLabels))
	values[0] = strconv.Itoa(status)
	values[1] = method
	values[2] = routePath
	for i, label := range m.dynamicLabels {
		values[3+i] = label.fn(ctx)
	}

	if m.requestsTotal != nil {
		m.requestsTotal.WithLabelValues(values...).Inc()
	}

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

	if m.requestDuration != nil {
		observe(m.requestDuration.WithLabelValues(values...), elapsed)
	}

	// Content-Length is authoritative when present and is the only usable
	// source for file-backed or streamed payloads. Otherwise fall back to the
	// buffered body, but never for a body stream: reading it would drain the
	// stream into memory just to measure it.
	if m.requestSize != nil {
		req := ctx.Request()
		requestLength := req.Header.ContentLength()
		if requestLength <= 0 {
			requestLength = 0
			if !req.IsBodyStream() {
				requestLength = len(req.Body())
			}
		}
		observe(m.requestSize.WithLabelValues(values...), float64(requestLength))
	}

	if m.responseSize != nil {
		resp := ctx.Response()
		responseLength := resp.Header.ContentLength()
		if responseLength <= 0 {
			responseLength = 0
			if !resp.IsBodyStream() {
				responseLength = len(resp.Body())
			}
		}
		observe(m.responseSize.WithLabelValues(values...), float64(responseLength))
	}

	if m.requestsByClass != nil {
		values[0] = class
		m.requestsByClass.WithLabelValues(values...).Inc()
	}

	return nil
}

// skipped reports whether the route pattern is excluded by Config.SkipURIs,
// either as an exact match or through a "*" prefix entry.
func (m *middleware) skipped(routePath string) bool {
	if _, ok := m.skipURIs[routePath]; ok {
		return true
	}

	for _, prefix := range m.skipPrefixes {
		// "/*" normalizes to "/" and excludes everything.
		if prefix == "/" || routePath == prefix || strings.HasPrefix(routePath, prefix+"/") {
			return true
		}
	}

	return false
}

// routeLabel returns the path label for the current request and whether the
// request should be recorded at all. Matched requests are attributed to the
// registered route pattern (for example "/user/:id") so that the label
// cardinality stays bounded; unmatched requests are only recorded when
// TrackUnmatchedRequests is enabled.
//
// The route is read after the handler chain has run because Fiber sets the
// matched route on the context as it walks the stack: while this middleware
// runs, the context still points at the middleware's own route.
func (m *middleware) routeLabel(ctx fiber.Ctx) (string, bool) {
	if ctx.Matched() {
		if route := ctx.Route(); route != nil {
			return normalizePath(route.Path), true
		}
	}

	if !m.cfg.TrackUnmatchedRequests {
		return "", false
	}

	return m.unmatchedLabel, true
}

// statusClass maps a status code onto its "Nxx" class label.
func statusClass(status int) string {
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "unknown"
	}
}

// normalizePath trims trailing slashes and converts empty paths to "/" so
// routes can be matched consistently.
func normalizePath(routePath string) string {
	normalized := utils.TrimRight(routePath, '/')
	if normalized == "" {
		return "/"
	}
	return normalized
}

// registerCollector attempts to register the provided collector, suppressing
// the AlreadyRegistered error so callers can opt-in without coordination.
func registerCollector(registry prometheus.Registerer, collector prometheus.Collector) {
	if err := registry.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			return
		}
		panic(err)
	}
}
