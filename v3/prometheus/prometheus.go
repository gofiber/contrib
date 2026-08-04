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
// Only the two configuration fields read per request are kept; everything else
// Config carries is consumed by New and would otherwise be retained - along
// with configDefault's defensive copies of it - for the lifetime of the process.
type middleware struct {
	requestsTotal     *prometheus.CounterVec
	requestsByClass   *prometheus.CounterVec
	requestDuration   *prometheus.HistogramVec
	requestSize       *prometheus.HistogramVec
	responseSize      *prometheus.HistogramVec
	requestInFlight   *prometheus.GaugeVec
	metricsHandler    fiber.Handler
	next              func(fiber.Ctx) bool
	metricsPath       string
	unmatchedLabel    string
	skipURIs          map[string]struct{}
	skipPrefixes      []string
	skipStatusCodes   map[int]struct{}
	skipStatusClasses map[string]struct{}
	dynamicLabels     []dynamicLabel
	trackUnmatched    bool
	records           bool
	observes          bool
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
//	app.Use(prometheus.New(prometheus.Config{ServiceName: "my-service"}))
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

	// configDefault already handed over a private copy of Labels, so the
	// "service" entry goes straight into it. ServiceName wins over a "service"
	// key supplied through Labels.
	if cfg.ServiceName != "" {
		cfg.Labels["service"] = cfg.ServiceName
	}
	labels := cfg.Labels

	dynamic := resolveDynamicLabels(cfg)

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
		metricsHandler:    metricsHandler,
		next:              cfg.Next,
		metricsPath:       normalizePath(cfg.MetricsPath),
		unmatchedLabel:    normalizePath(cfg.UnmatchedRouteLabel),
		skipURIs:          make(map[string]struct{}, len(cfg.SkipURIs)),
		skipStatusCodes:   make(map[int]struct{}, len(cfg.SkipStatusCodes)),
		skipStatusClasses: make(map[string]struct{}, len(cfg.SkipStatusClasses)),
		dynamicLabels:     dynamic,
		trackUnmatched:    cfg.TrackUnmatchedRequests,
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

	m.observes = m.requestDuration != nil || m.requestSize != nil || m.responseSize != nil
	m.records = m.requestsTotal != nil || m.requestsByClass != nil || m.observes

	for _, path := range cfg.SkipURIs {
		// Fiber's register prepends "/" to every pattern, so an entry without
		// one could never match. Add it, as MetricsPath does.
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		// Normalize before looking for the star, so that a trailing slash after
		// it - "/admin/*/" - still registers the prefix. Trailing slashes are
		// documented as ignored.
		normalized := normalizePath(path)

		// Every entry is kept as an exact match, including one ending in "*".
		// Fiber route patterns may themselves end in "*", so registering only
		// the prefix would leave the route named "/static*" unskippable: the
		// stripped prefix "/static" matches neither it nor "/static/...".
		m.skipURIs[normalized] = struct{}{}

		if prefix, found := strings.CutSuffix(normalized, "*"); found {
			m.skipPrefixes = append(m.skipPrefixes, normalizePath(prefix))
		}
	}

	for _, code := range cfg.SkipStatusCodes {
		m.skipStatusCodes[code] = struct{}{}
	}

	for _, class := range cfg.SkipStatusClasses {
		m.skipStatusClasses[strings.ToLower(strings.TrimSpace(class))] = struct{}{}
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
func resolveDynamicLabels(cfg Config) []dynamicLabel {
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
		if _, ok := cfg.Labels[name]; ok {
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

	// Both were supplied. Pairing a wrapping Registerer with the registry it
	// wraps is the main reason to do that, and those wrappers - anything from
	// prometheus.WrapRegistererWith or WrapRegistererWithPrefix - are not
	// Gatherers, so requiring one here would reject the very configuration the
	// panic above recommends. Only provably distinct pairs are rejected.
	if regGatherer, ok := registerer.(prometheus.Gatherer); ok {
		if differentSource(regGatherer, gatherer) {
			panic("prometheus middleware: Registerer and Gatherer must reference the same metrics source")
		}
	}

	return registerer, gatherer
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

	// Mismatched dynamic types say nothing about the underlying source: a
	// gathering wrapper delegating to the registry it was handed is a different
	// type but the same source. Only like can be compared with like.
	//
	// Pointer identity is then the only comparison worth making. Equality on
	// anything else risks panicking at startup: reflect.Type.Comparable reports
	// true for a struct holding an interface field, yet comparing two of them
	// panics when that field carries an uncomparable dynamic value such as a
	// prometheus.Gatherers slice. Everything else is treated as undecidable.
	if av.Type() != bv.Type() || av.Kind() != reflect.Pointer {
		return false
	}

	return av.Pointer() != bv.Pointer()
}

// handle serves the metrics endpoint or instruments the request, depending on
// the requested path.
func (m *middleware) handle(ctx fiber.Ctx) error {
	if m.next != nil && m.next(ctx) {
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
	if _, ignore := m.skipStatusCodes[status]; ignore {
		return nil
	}

	class := statusClass(status)
	if _, ignore := m.skipStatusClasses[class]; ignore {
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
		// Two hazards, both reachable from a value taken off the request.
		//
		// Prometheus rejects label values that are not valid UTF-8 by panicking
		// inside WithLabelValues, so a single header of raw bytes would take the
		// process down. Invalid sequences are replaced instead.
		//
		// And unless the app sets fiber.Config.Immutable, Fiber hands out
		// strings aliasing the connection read buffer. Prometheus keeps label
		// values for the lifetime of the series, so without a copy the buffer is
		// reused by the next request and every series rewrites itself to
		// whatever arrived last, collapsing distinct series onto one label set
		// and leaving the registry unable to gather at all. ToValidUTF8 returns
		// its input untouched when the value is already valid, so the clone has
		// to stay.
		values[3+i] = strings.Clone(strings.ToValidUTF8(label.fn(ctx), "�"))
	}

	if m.requestsTotal != nil {
		m.requestsTotal.WithLabelValues(values...).Inc()
	}

	// Everything below feeds the histograms only, and reading the request
	// context is not free: Fiber's DefaultCtx.Context installs a background
	// context on the request when none was set, which then costs the request an
	// extra user value to clear on release. Skip the lot when every histogram
	// family is disabled.
	if m.observes {
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

		if m.requestSize != nil {
			req := ctx.Request()
			if size, known := bodySize(req.Header.ContentLength(), req); known {
				observe(m.requestSize.WithLabelValues(values...), size)
			}
		}

		if m.responseSize != nil {
			resp := ctx.Response()
			if size, known := bodySize(resp.Header.ContentLength(), resp); known {
				observe(m.responseSize.WithLabelValues(values...), size)
			}
		}
	}

	if m.requestsByClass != nil {
		values[0] = class
		m.requestsByClass.WithLabelValues(values...).Inc()
	}

	return nil
}

// payload is the part of *fasthttp.Request and *fasthttp.Response that bodySize
// needs.
type payload interface {
	IsBodyStream() bool
	Body() []byte
}

// bodySize reports the payload size in bytes and whether it could be determined
// at all. Content-Length is authoritative when present and is the only usable
// source for file-backed or streamed payloads. Otherwise the buffered body is
// measured - but never for a body stream, because reading it would drain the
// stream into memory just to size it.
//
// A stream of unknown length is therefore left out of the histogram rather than
// observed as zero: an app serving files or SSE would otherwise report a median
// response of no bytes at all, since a zero still increments _count and lands in
// the lowest bucket while adding nothing to _sum.
func bodySize(contentLength int, p payload) (float64, bool) {
	if contentLength > 0 {
		return float64(contentLength), true
	}

	if p.IsBodyStream() {
		return 0, false
	}

	return float64(len(p.Body())), true
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
//
// Known limitation: Fiber only ever tracks the route currently executing, so if
// the endpoint delegates onwards with ctx.Next() and a trailing Use middleware
// runs last, that middleware's mount path is what remains on the context and
// the request is attributed to it. The endpoint pattern is not recoverable at
// that point and Fiber exposes no way to tell a Use route apart from any other
// - ctx.IsMiddleware() also reports true for a route whose own handler chain
// stopped early, which is what a short-circuiting per-route guard leaves
// behind, so it cannot be used to detect this.
//
// A request answered entirely by Use handlers - static.New, or a Use-mounted
// guard returning 401 - never matches a non-Use route, so Matched stays false
// and it is treated as unmatched: dropped by default, or recorded under
// UnmatchedRouteLabel when TrackUnmatchedRequests is set.
func (m *middleware) routeLabel(ctx fiber.Ctx) (string, bool) {
	if ctx.Matched() {
		if route := ctx.Route(); route != nil {
			return normalizePath(route.Path), true
		}
		// A Ctx implementation installed through app.NewCtxFunc may report a
		// match without exposing the route. Nothing identifies the endpoint
		// then, so the request counts as unmatched and obeys the same opt-in.
	}

	if !m.trackUnmatched {
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
