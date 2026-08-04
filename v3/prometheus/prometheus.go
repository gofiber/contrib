// Package prometheus provides a Fiber middleware that exposes Prometheus
// metrics while instrumenting incoming HTTP traffic.
package prometheus

import (
	"errors"
	"maps"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/utils/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/model"

	"go.opentelemetry.io/otel/trace"
)

// middleware encapsulates all state required to expose metrics and instrument
// Fiber requests. Every metric vector is nil when its family is disabled
// through Config.DisabledMetrics.
//
// Only the configuration actually read per request is kept. Everything else
// Config carries is consumed by New, and holding the struct would retain
// configDefault's copies of it - the skip lists, the labels - for the lifetime
// of the process.
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
	exemplars         bool
	skipAll           bool
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

// reservedLabels are the label names Labels and DynamicLabels may not use:
// the four this middleware sets itself, plus "le", which Prometheus keeps for
// histogram bucket bounds. CounterVec and GaugeVec would accept "le" happily,
// but NewHistogramVec only builds the Desc - the rejection comes later, from
// the lazy newHistogram call on the first observation, on a connection
// goroutine no recover placed where this package recommends can reach.
var reservedLabels = map[string]struct{}{
	"status_code":  {},
	"status_class": {},
	"method":       {},
	"path":         {},
	"le":           {},
}

// validStatusClasses are the classes Config.SkipStatusClasses accepts, which is
// exactly the set statusClass can return - "unknown" included, since a handler
// is free to answer with a status outside 1xx-5xx.
var validStatusClasses = map[string]bool{
	"1xx":     true,
	"2xx":     true,
	"3xx":     true,
	"4xx":     true,
	"5xx":     true,
	"unknown": true,
}

// allMetrics is the set of families Config.DisabledMetrics may name.
var allMetrics = map[Metric]struct{}{
	MetricRequestsTotal:            {},
	MetricRequestsStatusClassTotal: {},
	MetricRequestDuration:          {},
	MetricRequestSize:              {},
	MetricResponseSize:             {},
	MetricRequestsInProgress:       {},
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
//
// Mount recover.New() after this middleware, not before it:
//
//	app.Use(prometheus.New(prometheus.Config{}))
//	app.Use(recover.New())
//
// A panic unwinds straight past the recording below, so with recover mounted
// first the middleware never sees the request complete and the resulting 500
// appears in no metric family at all - only the in-flight gauge, which is
// deferred, stays balanced. Recovering downstream turns the panic into an
// ordinary error return, which this middleware records as the 500 the client
// received.
func New(config ...Config) fiber.Handler {
	cfg := configDefault(config...)

	// Every panic below has to fire before the first Register call. A
	// configuration rejected half way through registration would leave a
	// caller-supplied Registerer holding the families this middleware had got to,
	// so the corrected retry would die on a duplicate registration instead -
	// a message pointing at the wrong problem, and one no amount of fixing the
	// original config resolves.
	//
	// configDefault already handed over a private copy of Labels, so the
	// "service" entry goes straight into it. ServiceName wins over a "service"
	// key supplied through Labels.
	if cfg.ServiceName != "" {
		cfg.Labels["service"] = cfg.ServiceName
	}
	labels := cfg.Labels

	// A const label sharing a name with a variable one, or carrying a value that
	// is not valid UTF-8, reaches client_golang as an invalid Desc - and its
	// panic names neither Labels nor ServiceName. Worse, it would fire from
	// inside the first promauto call, leaving a caller's registry holding the
	// collectors registered by then.
	for name, value := range labels {
		if _, reserved := reservedLabels[name]; reserved {
			panic("prometheus middleware: constant label " + strconv.Quote(name) + " collides with a reserved label")
		}
		validateLabelName("constant label", name)
		if !utf8.ValidString(value) {
			panic("prometheus middleware: constant label " + strconv.Quote(name) + " has a value that is not valid UTF-8")
		}
	}

	dynamic := resolveDynamicLabels(cfg)

	// promhttp tests its logger against nil, which a typed nil passes, and then
	// dereferences it the first time a gather fails - long after startup.
	if typedNil(cfg.MetricsErrorLog) {
		panic("prometheus middleware: MetricsErrorLog is a typed nil; leave it unset to discard errors")
	}

	// The label the middleware supplies itself for unmatched requests is the
	// only one client_golang does not see until a request arrives: const labels
	// are validated when the Desc is built, but this one reaches
	// WithLabelValues, which panics on invalid UTF-8 from the connection
	// goroutine.
	if !utf8.ValidString(cfg.UnmatchedRouteLabel) {
		panic("prometheus middleware: UnmatchedRouteLabel is not valid UTF-8")
	}

	disabled := make(map[Metric]struct{}, len(cfg.DisabledMetrics))
	for _, entry := range cfg.DisabledMetrics {
		// Trimmed and blank-skipped as the skip lists are, so that splitting an
		// environment variable on "," works whether it is unset or padded.
		metric := Metric(strings.TrimSpace(string(entry)))
		if metric == "" {
			continue
		}
		// An unrecognised name would disable nothing and say nothing, leaving
		// the family it was meant to drop registered and recording.
		if _, known := allMetrics[metric]; !known {
			panic("prometheus middleware: unknown metric " + strconv.Quote(string(entry)) + " in DisabledMetrics")
		}
		disabled[metric] = struct{}{}
	}
	m := &middleware{
		next:              cfg.Next,
		metricsPath:       normalizePath(cfg.MetricsPath),
		unmatchedLabel:    normalizePath(cfg.UnmatchedRouteLabel),
		skipURIs:          make(map[string]struct{}, len(cfg.SkipURIs)),
		skipStatusCodes:   make(map[int]struct{}, len(cfg.SkipStatusCodes)),
		skipStatusClasses: make(map[string]struct{}, len(cfg.SkipStatusClasses)),
		dynamicLabels:     dynamic,
		exemplars:         !cfg.DisableExemplars,
		trackUnmatched:    cfg.TrackUnmatchedRequests,
	}

	m.resolveFilters(cfg)

	// A "/*" entry excludes every route, so no family can receive a sample.
	// Registering them anyway would claim six metric names in a caller's
	// registry that nothing will ever write to, and make a second middleware
	// configured the same way collide with the first.
	enabled := func(metric Metric) bool {
		if m.skipAll {
			return false
		}
		_, off := disabled[metric]
		return !off
	}

	// Namespace and Subsystem reach the descriptor only through the assembled
	// metric names, so validating those covers both - and catches a name that a
	// stricter validation scheme would reject even though it is valid UTF-8.
	for metric := range allMetrics {
		if enabled(metric) {
			validateMetricName(cfg.Namespace, cfg.Subsystem, metric)
		}
	}

	// Bucket bounds belong to one family each, so a disabled family's bounds are
	// dead configuration and rejecting them would stop an application from
	// booting over settings that can no longer reach a histogram. The reserved
	// "le" label is deliberately not gated the same way: it applies to how the
	// middleware labels everything it exposes, not to one family's options.
	if enabled(MetricRequestDuration) {
		validateBuckets("RequestDurationBuckets", cfg.RequestDurationBuckets)
	}
	if enabled(MetricRequestSize) {
		validateBuckets("RequestSizeBuckets", cfg.RequestSizeBuckets)
	}
	if enabled(MetricResponseSize) {
		validateBuckets("ResponseSizeBuckets", cfg.ResponseSizeBuckets)
	}

	// Both label sets carry the dynamic names last so that the value buffer
	// built per request can be shared between them.
	byStatusCode := variableLabels("status_code", dynamic)
	byStatusClass := variableLabels("status_class", dynamic)

	registry, gatherer := resolveRegistry(cfg)

	m.metricsHandler = adaptor.HTTPHandler(newScrapeHandler(cfg, gatherer))

	// Past this point the config is known good and registration can begin.
	if !cfg.DisableGoCollector {
		registerCollector(registry, collectors.NewGoCollector())
	}

	if !cfg.DisableProcessCollector {
		registerCollector(registry, collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
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
	// A "/*" entry excludes every route, so nothing downstream of the handler
	// chain can ever be recorded. Saying so here lets instrument return the
	// moment the chain unwinds, rather than timing and labelling a request only
	// for skipped to discard it.
	// enabled reports false for every family when skipAll is set, so the vectors
	// are already nil in that case.
	m.records = m.requestsTotal != nil || m.requestsByClass != nil || m.observes

	return m.handle
}

// resolveFilters parses the three skip lists, rejecting entries that could never
// match rather than letting them sit in a filter the operator believes is doing
// something. Blank entries are the exception: splitting an unset environment
// variable on "," yields one, and treating that as a configuration error would
// stop the process from booting over an empty setting.
func (m *middleware) resolveFilters(cfg Config) {
	for _, path := range cfg.SkipURIs {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}

		// Fiber's register prepends "/" to every pattern, so an entry without
		// one could never match. Add it, as MetricsPath does.
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		// Normalize before looking for the star, so that a trailing slash after
		// it - "/admin/*/" - still registers the prefix. Trailing slashes are
		// documented as ignored.
		//
		// validLabel matches what routeLabel does to the pattern these are
		// compared against; without it an entry for a route whose pattern is not
		// valid UTF-8 would silently stop matching.
		normalized := validLabel(normalizePath(path))

		// Every entry is kept as an exact match, including one ending in "*".
		// Fiber route patterns may themselves end in "*", so registering only
		// the prefix would leave the route named "/static*" unskippable: the
		// stripped prefix "/static" matches neither it nor "/static/...".
		m.skipURIs[normalized] = struct{}{}

		prefix, found := strings.CutSuffix(normalized, "*")
		if !found {
			continue
		}

		// "/*" normalizes to "/" and excludes everything.
		if prefix = normalizePath(prefix); prefix == "/" {
			m.skipAll = true
			continue
		}

		// The prefix stands for the route named exactly that as well as
		// everything below it. Registering the bare form as an exact match
		// leaves the per-request scan a single HasPrefix against a separator
		// already appended here rather than rebuilt on every request.
		m.skipURIs[prefix] = struct{}{}
		m.skipPrefixes = append(m.skipPrefixes, prefix+"/")
	}

	for _, code := range cfg.SkipStatusCodes {
		// RFC 9110 makes a status code three digits, so anything else - a 4040
		// for 404, or a stray 0 - is a typo that would silently filter nothing.
		if code < 100 || code > 999 {
			panic("prometheus middleware: status code " + strconv.Itoa(code) + " is not a three-digit HTTP status")
		}
		m.skipStatusCodes[code] = struct{}{}
	}

	for _, class := range cfg.SkipStatusClasses {
		normalized := strings.ToLower(strings.TrimSpace(class))
		if normalized == "" {
			continue
		}
		// Anything statusClass never returns would sit in the map unmatched, so
		// the operator would believe a class was filtered while every response
		// in it kept being recorded.
		if !validStatusClasses[normalized] {
			panic("prometheus middleware: status class " + strconv.Quote(class) + ` is not one of "1xx" through "5xx" or "unknown"`)
		}
		m.skipStatusClasses[normalized] = struct{}{}
	}
}

// newScrapeHandler builds the net/http handler answering a scrape, detaching
// the request when the timeout that makes detaching necessary is in play.
func newScrapeHandler(cfg Config, gatherer prometheus.Gatherer) http.Handler {
	handler := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics:                   cfg.EnableOpenMetrics,
		EnableOpenMetricsTextCreatedSamples: cfg.EnableOpenMetricsTextCreatedSamples,
		DisableCompression:                  cfg.DisableCompression,
		MaxRequestsInFlight:                 cfg.MetricsMaxRequestsInFlight,
		Timeout:                             cfg.MetricsTimeout,
		ErrorLog:                            cfg.MetricsErrorLog,
		ErrorHandling:                       cfg.MetricsErrorHandling,
	})

	if cfg.MetricsTimeout > 0 {
		handler = detachRequest(handler)
	}

	return handler
}

// typedNil reports whether an interface holds a nil pointer or other nilable
// zero value, which `== nil` does not catch.
func typedNil(value any) bool {
	if value == nil {
		return false
	}

	// reflect.ValueOf unwraps the interface, so the kind here is always the
	// dynamic type's.
	rv := reflect.ValueOf(value)
	switch rv.Kind() { //nolint:exhaustive // only nilable kinds can be a typed nil
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// detachRequest hands the wrapped handler a request whose header no longer
// aliases the connection buffers.
//
// The adaptor builds the net/http request with zero-copy views into memory
// fasthttp reuses the moment the Fiber handler returns, which is safe only
// while that handler owns the request. Config.MetricsTimeout breaks the
// assumption: promhttp then wraps itself in http.TimeoutHandler, which answers
// 503 and returns as soon as the deadline fires while the gathering goroutine
// runs on. That goroutine negotiates the exposition format off req.Header only
// after Gather returns - by which point the Fiber handler is gone and the
// buffers have been recycled underneath it. Copying the header first leaves the
// straggler reading memory nothing else will touch.
func detachRequest(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rsp http.ResponseWriter, req *http.Request) {
		// Both names and values alias: the adaptor clones only Cookie, and a
		// header name already in canonical form is passed through untouched.
		header := make(http.Header, len(req.Header))
		for name, values := range req.Header {
			detached := make([]string, len(values))
			for i, value := range values {
				detached[i] = strings.Clone(value)
			}
			header[strings.Clone(name)] = detached
		}
		req.Header = header

		h.ServeHTTP(rsp, req)
	})
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

	// Sorting keeps the label order of a metric stable across restarts, which
	// map iteration order would not.
	names := slices.Sorted(maps.Keys(cfg.DynamicLabels))

	dynamic := make([]dynamicLabel, 0, len(names))
	for _, name := range names {
		if _, reserved := reservedLabels[name]; reserved {
			panic("prometheus middleware: dynamic label " + strconv.Quote(name) + " collides with a reserved label")
		}
		validateLabelName("dynamic label", name)
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

	// A typed nil - `var reg *prometheus.Registry` handed straight over - is a
	// non-nil interface, so it would sail past every check below and die on a
	// nil dereference inside client_golang instead.
	if typedNil(registerer) {
		panic("prometheus middleware: Registerer is a typed nil; leave it unset to use a private registry")
	}
	if typedNil(gatherer) {
		panic("prometheus middleware: Gatherer is a typed nil; leave it unset to use a private registry")
	}

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
	// Both interfaces are non-nil here, so neither Value can be invalid.
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)

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
	// With nothing left to record, taking over the error handler below would
	// change where the application's errors surface for no benefit at all: the
	// error would stop propagating to middleware mounted before this one, and
	// run at a different point in the stack. Stay out of the way instead.
	if !m.records && m.requestInFlight == nil {
		return ctx.Next()
	}

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
	values[0] = statusLabel(status)
	values[1] = method
	values[2] = routePath
	if !m.resolveDynamicValues(ctx, values) {
		return nil
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
		var exemplarLabels prometheus.Labels
		if m.exemplars {
			// Reading the request context is what costs: with no user context
			// set - the norm without tracing middleware - Fiber installs a
			// background one, which the request then has to clear on release.
			spanCtx := trace.SpanContextFromContext(ctx.Context())
			if traceID := spanCtx.TraceID(); traceID.IsValid() {
				exemplarLabels = prometheus.Labels{"traceID": traceID.String()}
			}
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
			if bodyless(method, status) {
				observe(m.responseSize.WithLabelValues(values...), 0)
			} else {
				resp := ctx.Response()
				if size, known := bodySize(resp.Header.ContentLength(), resp); known {
					observe(m.responseSize.WithLabelValues(values...), size)
				}
			}
		}
	}

	if m.requestsByClass != nil {
		values[0] = class
		m.requestsByClass.WithLabelValues(values...).Inc()
	}

	return nil
}

// resolveDynamicValues fills the dynamic part of the label buffer, reporting
// whether the request can be recorded at all.
//
// The functions are supplied by the application and Config invites them to read
// whatever the handlers left on the context, so a nil Locals cast is an easy
// mistake to make. It would be a costly one: this runs after the handler chain
// has unwound, past any recover the application mounted where this package
// prescribes, so the panic would reach the fasthttp connection goroutine and the
// client would get no response at all. Instrumentation must not be able to take
// the request down, so a panicking function drops the sample instead - inventing
// a label value would quietly put a fabricated series in the registry.
func (m *middleware) resolveDynamicValues(ctx fiber.Ctx, values []string) (ok bool) {
	if len(m.dynamicLabels) == 0 {
		return true
	}

	defer func() {
		if recover() != nil {
			ok = false
		}
	}()

	for i, label := range m.dynamicLabels {
		values[3+i] = detachedLabel(label.fn(ctx))
	}

	return true
}

// replacementRune stands in for bytes Prometheus will not accept.
const replacementRune = "�"

// validLabel replaces invalid UTF-8 so that a value can never be rejected by
// Prometheus, which panics on one. The common case is a scan and no allocation.
// Use it for values the middleware already owns, such as a registered route
// pattern; a value read off the request needs detachedLabel instead.
func validLabel(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	return strings.ToValidUTF8(value, replacementRune)
}

// detachedLabel is validLabel for a value that may alias fasthttp's read buffer.
//
// Unless the app sets fiber.Config.Immutable, Fiber hands out strings pointing
// into the connection read buffer. Prometheus keeps label values for the
// lifetime of the series, so without a copy the buffer is reused by the next
// request and every series rewrites itself to whatever arrived last, collapsing
// distinct series onto one label set and leaving the registry unable to gather
// at all.
func detachedLabel(value string) string {
	if utf8.ValidString(value) {
		return strings.Clone(value)
	}
	// ToValidUTF8 allocates whenever it substitutes, so its result is already
	// detached.
	return strings.ToValidUTF8(value, replacementRune)
}

// statusStrings holds the decimal form of every status code a response can
// carry, because strconv.Itoa allocates above 99 - which is every HTTP status -
// and this is the hottest path in the middleware.
var statusStrings = func() [1000]string {
	var table [1000]string
	for code := 100; code < 1000; code++ {
		table[code] = strconv.Itoa(code)
	}
	return table
}()

// statusLabel renders the status code without allocating for the codes a
// response can actually carry.
func statusLabel(status int) string {
	if status >= 100 && status < 1000 {
		return statusStrings[status]
	}
	return strconv.Itoa(status)
}

// payload is the part of *fasthttp.Request and *fasthttp.Response that bodySize
// needs.
type payload interface {
	IsBodyStream() bool
	Body() []byte
}

// bodySize reports the payload size in bytes and whether it could be determined
// at all.
//
// A body stream cannot be measured without draining it into memory, so the
// announced Content-Length is all there is to go on; without one the size stays
// unknown and the caller leaves the payload out of the histogram rather than
// observing zero. Recording a zero would be worse than recording nothing: it
// still increments _count and lands in the lowest bucket while adding nothing to
// _sum, so an app serving files or SSE would report a median response of no
// bytes at all.
//
// Anything else is buffered, and then the buffer is the ground truth rather than
// the header: fasthttp recomputes Content-Length from the body it is about to
// write, so a stale or hand-set value never reaches the client.
func bodySize(contentLength int, p payload) (float64, bool) {
	if p.IsBodyStream() {
		if contentLength > 0 {
			return float64(contentLength), true
		}
		return 0, false
	}

	return float64(len(p.Body())), true
}

// bodyless reports whether fasthttp will drop the body the handler generated, so
// that no payload bytes reach the client however much the handler wrote. It
// answers from the method and status because Response.SkipBody is set only after
// the handler chain returns.
func bodyless(method string, status int) bool {
	if method == fiber.MethodHead {
		return true
	}

	// The set RFC 9110 forbids a body on, which fasthttp enforces in
	// ResponseHeader.mustSkipContentLength.
	return (status >= 100 && status < 200) ||
		status == fiber.StatusNoContent ||
		status == fiber.StatusNotModified
}

// skipped reports whether the route pattern is excluded by Config.SkipURIs,
// either as an exact match or through a "*" prefix entry.
func (m *middleware) skipped(routePath string) bool {
	if _, ok := m.skipURIs[routePath]; ok {
		return true
	}

	// Each prefix already carries its trailing separator, so a route equal to
	// the bare prefix is caught by the exact map above rather than here.
	for _, prefix := range m.skipPrefixes {
		if strings.HasPrefix(routePath, prefix) {
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
		// A registered route always carries at least one handler. DefaultCtx
		// substitutes a synthetic Route holding the raw request path when it has
		// no route to report, and a Ctx installed through
		// fiber.NewWithCustomCtx may return nil outright; taking either at face
		// value would put one series per distinct URL in the registry, which is
		// the unbounded cardinality this label exists to prevent. Neither names
		// an endpoint, so both count as unmatched and obey the same opt-in.
		if route := ctx.Route(); route != nil && len(route.Handlers) > 0 {
			// Registration-time data, so no copy is needed - but an application
			// registering routes from external config can still put raw bytes
			// in a pattern, and Prometheus panics on a label value that is not
			// valid UTF-8. That panic would land on the connection goroutine,
			// where neither fasthttp nor Fiber installs a recover, and take the
			// process with it.
			return validLabel(normalizePath(route.Path)), true
		}
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

// validateLabelName applies client_golang's own rule rather than approximating
// it, so the two cannot disagree when the process selects a stricter validation
// scheme than the UTF-8 default. client_golang checks names while building a
// descriptor, which happens after the collectors have gone into the registry, so
// a caller who supplied their own would otherwise be left with a registry they
// never successfully configured.
func validateLabelName(kind, name string) {
	//nolint:staticcheck // matching client_golang's own deprecated usage
	if !model.NameValidationScheme.IsValidLabelName(name) {
		panic("prometheus middleware: " + kind + " " + strconv.Quote(name) + " is not a valid label name")
	}
	// model.ReservedLabelPrefix is Prometheus's own namespace.
	if strings.HasPrefix(name, model.ReservedLabelPrefix) {
		panic("prometheus middleware: " + kind + " " + strconv.Quote(name) + " uses the reserved " +
			strconv.Quote(model.ReservedLabelPrefix) + " prefix")
	}
}

// validateMetricName rejects a fully qualified name client_golang would refuse,
// for the same reason and under the same scheme. Namespace and Subsystem reach
// the descriptor only through this, so checking the assembled name covers both.
func validateMetricName(namespace, subsystem string, metric Metric) {
	name := prometheus.BuildFQName(namespace, subsystem, string(metric))
	//nolint:staticcheck // matching client_golang's own deprecated usage
	if !model.NameValidationScheme.IsValidMetricName(name) {
		panic("prometheus middleware: " + strconv.Quote(name) +
			" is not a valid metric name; check Namespace and Subsystem")
	}
}

// validateBuckets rejects bucket bounds client_golang would reject, which it
// only does inside newHistogram - and HistogramVec calls that lazily, on the
// first observation for a label set. Left to it, a bad slice lets New return,
// the application boot and the scrape endpoint serve, then panics on every
// instrumented request from the connection goroutine, where a recover mounted
// as this package prescribes cannot catch it.
func validateBuckets(field string, buckets []float64) {
	for i, upper := range buckets {
		if math.IsNaN(upper) {
			panic("prometheus middleware: " + field + " contains NaN")
		}
		// A +Inf bound is allowed, but only last: client_golang drops it there
		// and rejects it anywhere else through the ordering check below.
		if i > 0 && buckets[i-1] >= upper {
			panic("prometheus middleware: " + field + " must be strictly increasing, got " +
				strconv.FormatFloat(buckets[i-1], 'g', -1, 64) + " before " +
				strconv.FormatFloat(upper, 'g', -1, 64))
		}
	}
}

// registerCollector attempts to register the provided collector, suppressing
// the AlreadyRegistered error so callers can opt-in without coordination.
func registerCollector(registry prometheus.Registerer, collector prometheus.Collector) {
	if err := registry.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			return
		}
		panic("prometheus middleware: registering the Go or process collector failed, " +
			"disable it with DisableGoCollector or DisableProcessCollector: " + err.Error())
	}
}
