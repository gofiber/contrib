// Package prometheus provides a Fiber middleware that exposes Prometheus
// metrics while instrumenting incoming HTTP traffic.
package prometheus

import (
	"bytes"
	"errors"
	"maps"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// middleware holds the state needed to serve metrics and instrument requests. A
// vector is nil when its family is disabled, and only per-request config is kept:
// retaining Config would pin configDefault's copies for the process life.
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
	errorLog          promhttp.Logger
	reportNextPanic   sync.Once
	reportLabelPanic  sync.Once
	exemplars         bool
	recordUnmatched   bool
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

// reservedLabels are the names Labels and DynamicLabels may not use: the four set
// here, plus "le", which Prometheus keeps for histogram bucket bounds.
var reservedLabels = map[string]struct{}{
	"status_code":             {},
	"status_class":            {},
	"method":                  {},
	"path":                    {},
	string(model.BucketLabel): {},
}

// validStatusClasses are the classes Config.SkipStatusClasses accepts - exactly
// what statusClass can return, "unknown" included.
var validStatusClasses = map[string]bool{
	"1xx":     true,
	"2xx":     true,
	"3xx":     true,
	"4xx":     true,
	"5xx":     true,
	"unknown": true,
}

// allMetrics lists every family Config.DisabledMetrics may name, in a fixed order
// so validation reports the same problem the same way on every run.
var allMetrics = []Metric{
	MetricRequestsTotal,
	MetricRequestsStatusClassTotal,
	MetricRequestDuration,
	MetricRequestSize,
	MetricResponseSize,
	MetricRequestsInProgress,
}

// New creates a new Prometheus middleware handler. Mount it globally with app.Use
// so it observes every request; requests to Config.MetricsPath are answered with
// the exposition format. Mount recover.New() after it, never before.
func New(config ...Config) fiber.Handler {
	cfg := configDefault(config...)

	// Every panic below fires before the first Register call: a config rejected part
	// way through registration leaves a caller's Registerer holding what got in, so
	// the corrected retry would die on a duplicate instead. ServiceName wins here.
	if cfg.ServiceName != "" {
		// Checked before it becomes a label, so the panic names the field the
		// caller set rather than a "service" key they never wrote.
		if !utf8.ValidString(cfg.ServiceName) {
			panic("prometheus middleware: ServiceName is not valid UTF-8")
		}
		cfg.Labels["service"] = cfg.ServiceName
	}
	labels := cfg.Labels

	// A const label clashing with a variable one, or holding invalid UTF-8, reaches
	// client_golang as a bad Desc - whose panic names neither field and fires mid
	// registration. Sorted so a config with two bad entries always names the same one.
	for _, name := range slices.Sorted(maps.Keys(labels)) {
		value := labels[name]
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

	// promhttp reads either as "unlimited", so an operator whose env parse
	// produced a negative believes scrapes are bounded when they are not.
	if cfg.MetricsTimeout < 0 {
		panic("prometheus middleware: MetricsTimeout is negative; leave it zero to disable the bound")
	}
	if cfg.MetricsMaxRequestsInFlight < 0 {
		panic("prometheus middleware: MetricsMaxRequestsInFlight is negative; leave it zero for no limit")
	}

	// promhttp switches on this with no default case, so a value outside the
	// three it knows silently serves a partial exposition as if healthy -
	// neither reporting the failure to the scraper nor logging it.
	switch cfg.MetricsErrorHandling {
	case promhttp.HTTPErrorOnError, promhttp.ContinueOnError, promhttp.PanicOnError:
	default:
		panic("prometheus middleware: MetricsErrorHandling " + strconv.Itoa(int(cfg.MetricsErrorHandling)) +
			" is not one of promhttp's HTTPErrorOnError, ContinueOnError or PanicOnError")
	}

	// The unmatched label is the only one client_golang does not see until a request
	// arrives: it reaches WithLabelValues, which panics on invalid UTF-8 from the
	// connection goroutine.
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
		if !slices.Contains(allMetrics, metric) {
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
		errorLog:          cfg.MetricsErrorLog,
		exemplars:         !cfg.DisableExemplars,
		recordUnmatched:   cfg.TrackUnmatchedRequests,
	}

	skipAll := m.resolveFilters(cfg)

	// A "/*" entry excludes every route, so registering families would claim six
	// names nothing will write to and collide with a second middleware like it.
	enabled := func(metric Metric) bool {
		if skipAll {
			return false
		}
		_, off := disabled[metric]
		return !off
	}

	// Namespace and Subsystem reach the descriptor only through the assembled names.
	// Checked for every family whatever is disabled, since the prefixes are shared -
	// gating it would hide a bad Namespace until someone enables a family.
	for _, metric := range allMetrics {
		validateMetricName(cfg.Namespace, cfg.Subsystem, metric)
	}

	// Checked whatever is disabled, as above. A factor of 0 disables native
	// histograms and anything above 1 sets their resolution; in between client_golang
	// enables nothing, then substitutes its latency defaults for empty buckets.
	if factor := cfg.NativeHistogramBucketFactor; math.IsNaN(factor) || math.IsInf(factor, 0) ||
		(factor != 0 && factor <= 1) {
		panic("prometheus middleware: NativeHistogramBucketFactor must be 0 to disable native histograms, or greater than 1")
	}

	validateBuckets("RequestDurationBuckets", cfg.RequestDurationBuckets)
	validateBuckets("RequestSizeBuckets", cfg.RequestSizeBuckets)
	validateBuckets("ResponseSizeBuckets", cfg.ResponseSizeBuckets)

	// Both label sets carry the dynamic names last so that the value buffer
	// built per request can be shared between them.
	byStatusCode := variableLabels("status_code", dynamic)
	byStatusClass := variableLabels("status_class", dynamic)

	registry, gatherer := resolveRegistry(cfg)

	m.metricsHandler = adaptor.HTTPHandler(newScrapeHandler(cfg, gatherer))

	// Past this point the config is known good and registration can begin.
	if !cfg.DisableGoCollector {
		registerCollector(registry, collectors.NewGoCollector(), cfg.MetricsErrorLog)
	}

	if !cfg.DisableProcessCollector {
		registerCollector(registry, collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), cfg.MetricsErrorLog)
	}

	// One shape each, so the metric, its help and its buckets stay on one line and
	// cannot be transposed between families.
	newCounter := func(metric Metric, help string, labelNames []string) *prometheus.CounterVec {
		if !enabled(metric) {
			return nil
		}
		return promauto.With(registry).NewCounterVec(prometheus.CounterOpts{
			Name:        prometheus.BuildFQName(cfg.Namespace, cfg.Subsystem, string(metric)),
			Help:        help,
			ConstLabels: labels,
		}, labelNames)
	}

	newHistogram := func(metric Metric, help string, buckets []float64) *prometheus.HistogramVec {
		if !enabled(metric) {
			return nil
		}
		return promauto.With(registry).NewHistogramVec(
			histogramOpts(cfg, metric, help, labels, buckets), byStatusCode)
	}

	m.requestsTotal = newCounter(MetricRequestsTotal,
		"Count all http requests by status code, method and path.", byStatusCode)
	m.requestsByClass = newCounter(MetricRequestsStatusClassTotal,
		"Count all http requests grouped by status class, method and path.", byStatusClass)
	m.requestDuration = newHistogram(MetricRequestDuration,
		"Duration of all HTTP requests by status code, method and path.", cfg.RequestDurationBuckets)
	m.requestSize = newHistogram(MetricRequestSize,
		"Size of all HTTP requests by status code, method and path.", cfg.RequestSizeBuckets)
	m.responseSize = newHistogram(MetricResponseSize,
		"Size of all HTTP responses by status code, method and path.", cfg.ResponseSizeBuckets)

	if enabled(MetricRequestsInProgress) {
		// Incremented before the router picks a handler, when the route pattern is still
		// unknown, so it is labelled by method only - which also bounds its cardinality
		// and is why dynamic labels are not applied to it.
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
	// A "/*" entry excludes every route, so nothing can be recorded; saying so here
	// lets instrument return as soon as the chain unwinds. enabled already reports
	// false for every family when skipAll is set, so the vectors are nil anyway.
	m.records = m.requestsTotal != nil || m.requestsByClass != nil || m.observes

	return m.handle
}

// resolveFilters parses the three skip lists, rejecting entries that could never
// match. Blank entries are allowed: splitting an unset env var yields one. It
// reports whether an entry excludes every route, which only New needs.
func (m *middleware) resolveFilters(cfg Config) bool {
	skipAll := false

	for _, path := range cfg.SkipURIs {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}

		// The unmatched label is a value, not a route, so it is recognised before any path
		// fixup and kept as its own flag rather than a skipURIs key - the two namespaces
		// must not collide. Deliberately not a "continue": an entry may mean both.
		normalizedEntry := normalizePath(path)
		if normalizedEntry == m.unmatchedLabel ||
			strings.TrimRight(normalizedEntry, "*") == m.unmatchedLabel {
			m.recordUnmatched = false
		}

		// Fiber's register prepends "/" to every pattern, so an entry without
		// one could never match. Add it, as MetricsPath does.
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		// Normalize before finding the star so "/admin/*/" still registers the prefix.
		// validLabel matches what routeLabel does to the pattern. Cloned because these
		// become map keys for the process life and normalizePath returns a subslice.
		normalized := strings.Clone(validLabel(normalizePath(path)))

		// Kept as an exact match too, including a trailing "*": Fiber patterns may end in
		// one, so the stripped prefix alone would leave route "/static*" unskippable.
		m.skipURIs[normalized] = struct{}{}

		// Trailing stars are stripped as a group, so that the "/**" an operator
		// reaches for out of glob habit means what "/*" means rather than
		// quietly excluding almost nothing.
		prefix := strings.TrimRight(normalized, "*")
		if prefix == normalized {
			continue
		}

		// "/*" normalizes to "/" and excludes everything.
		if prefix = normalizePath(prefix); prefix == "/" {
			skipAll = true
			continue
		}

		// The prefix stands for the route named exactly that and everything below it, so
		// the per-request scan is one HasPrefix against a separator appended here.
		m.skipURIs[prefix] = struct{}{}
		// Several spellings normalize to one prefix - "/admin/*", "/admin/**"
		// and "/admin/*/" all do - and each duplicate would repeat an identical
		// HasPrefix on every instrumented request for the process lifetime.
		if withSeparator := prefix + "/"; !slices.Contains(m.skipPrefixes, withSeparator) {
			m.skipPrefixes = append(m.skipPrefixes, withSeparator)
		}
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
		// Cloned for the same reason as the SkipURIs keys: ToLower and TrimSpace
		// both hand back a view of the caller's string when there is nothing to
		// change, and these are map keys for the process lifetime.
		m.skipStatusClasses[strings.Clone(normalized)] = struct{}{}
	}

	return skipAll
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
	// Only nilable kinds can hold a typed nil.
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// detachRequest hands the wrapped handler a request whose header no longer aliases
// the connection buffers. Under MetricsTimeout promhttp answers 503 and returns
// while the gather goroutine still reads req.Header from recycled memory.
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
func variableLabels(statusLabelName string, dynamic []dynamicLabel) []string {
	names := make([]string, 0, 3+len(dynamic))
	names = append(names, statusLabelName, "method", "path")
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

// resolveDynamicLabels sorts the configured dynamic labels by name and rejects the
// ones clashing with a label the middleware sets. Sorting keeps the order stable
// across restarts, which map iteration would not.
func resolveDynamicLabels(cfg Config) []dynamicLabel {
	if len(cfg.DynamicLabels) == 0 {
		return nil
	}

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

	// Both were supplied. Wrapping Registerers - WrapRegistererWith and friends - are
	// not Gatherers, so requiring one would reject the very pairing the panic above
	// recommends. Only provably distinct pairs are rejected.
	if regGatherer, ok := registerer.(prometheus.Gatherer); ok {
		if differentSource(regGatherer, gatherer) {
			panic("prometheus middleware: Registerer and Gatherer must reference the same metrics source")
		}
	}

	return registerer, gatherer
}

// differentSource reports whether two gatherers provably reference distinct
// sources. Identity goes through reflection because comparing interface values
// panics on an uncomparable dynamic type; undecidable cases are accepted.
func differentSource(a, b prometheus.Gatherer) bool {
	// Both interfaces are non-nil here, so neither Value can be invalid.
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)

	// Mismatched dynamic types say nothing: a wrapper delegating to the registry it
	// was handed is a different type, same source. Pointer identity is then the only
	// safe comparison - equality on a struct holding an interface can panic.
	if av.Type() != bv.Type() || av.Kind() != reflect.Pointer {
		return false
	}

	return av.Pointer() != bv.Pointer()
}

// handle serves the metrics endpoint or instruments the request, depending on
// the requested path.
func (m *middleware) handle(ctx fiber.Ctx) error {
	if m.next != nil && m.skipRequested(ctx) {
		return ctx.Next()
	}

	if normalizePath(ctx.Path()) == m.metricsPath {
		return m.serveMetrics(ctx)
	}

	return m.instrument(ctx)
}

// skipRequested asks Config.Next whether to stand aside, reading a panic there as
// "no". It runs before the handler chain, so an escaping panic would reach the
// connection goroutine that neither fasthttp nor Fiber guards.
func (m *middleware) skipRequested(ctx fiber.Ctx) (skip bool) {
	defer func() {
		if r := recover(); r != nil {
			skip = false
			m.reportNextPanic.Do(func() {
				if m.errorLog != nil {
					m.errorLog.Println("prometheus middleware: Config.Next panicked; "+
						"instrumenting the request anyway, reported only once:", r)
				}
			})
		}
	}()

	return m.next(ctx)
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
		inFlight := m.requestInFlight.WithLabelValues(inFlightMethod(method))
		inFlight.Inc()
		defer inFlight.Dec()
	}

	// The gauge never reads the status, so nothing below serves it. Taking over the
	// error handler anyway would move where the application's errors surface, and run
	// them at a different depth in the stack, for no benefit.
	if !m.records {
		return ctx.Next()
	}

	// Only the duration histogram needs the clock.
	var start time.Time
	if m.requestDuration != nil {
		start = time.Now()
	}

	chainErr := ctx.Next()

	// Read here, not at the observation below: everything between is this middleware's
	// own bookkeeping, and charging it to the request would put instrumentation
	// overhead into the metric people set latency alerts on.
	var chainTime time.Duration
	if m.requestDuration != nil {
		chainTime = time.Since(start)
	}

	routePath, ok := m.pathLabel(ctx)
	if !ok {
		// Nothing will be recorded, so there is no status code to buy by taking over the
		// error handler - and the two paths above that also stand aside return it too.
		return chainErr
	}

	// Fiber runs the application error handler only after the chain unwinds, so the
	// response is still empty here. Running it now - as Fiber's logger does - makes
	// the status and size below the ones the client sees; its cost is the client's.
	if chainErr != nil {
		errorHandlerStart := time.Now()
		if err := ctx.App().ErrorHandler(ctx, chainErr); err != nil {
			_ = ctx.SendStatus(fiber.StatusInternalServerError) //nolint:errcheck // mirrors Fiber's own fallback
		}
		if m.requestDuration != nil {
			chainTime += time.Since(errorHandlerStart)
		}
	}

	elapsed := chainTime.Seconds()

	// Fiber's timeout middleware answers through TimeoutErrorWithCode, which parks the
	// 408 and copies it over the live response only after the chain unwinds - so
	// ctx.Response() still holds the default 200 the timed-out handler left.
	resp := ctx.Response()
	if timedOut := ctx.RequestCtx().LastTimeoutErrorResponse(); timedOut != nil {
		resp = timedOut
	}

	status := resp.StatusCode()
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
	// Stack-backed for the label counts that fit. The slice provably does not
	// escape - the metric vectors copy the values they are given - so the only
	// reason it reached the heap was its length not being constant.
	var stack [8]string
	var values []string
	if n := 3 + len(m.dynamicLabels); n <= len(stack) {
		values = stack[:n]
	} else {
		values = make([]string, n)
	}
	values[0] = statusLabel(status)
	values[1] = method
	values[2] = routePath
	if !m.resolveDynamicValues(ctx, values) {
		return nil
	}

	if m.requestsTotal != nil {
		m.requestsTotal.WithLabelValues(values...).Inc()
	}

	// Histograms only, and the request-context read is not free: DefaultCtx.Context
	// installs a background context when none was set, costing an extra user value to
	// clear on release. Skip it when every histogram family is disabled.
	if m.observes {
		exemplar := m.exemplarFor(ctx)

		if m.requestDuration != nil {
			observe(m.requestDuration.WithLabelValues(values...), elapsed, exemplar)
		}

		if m.requestSize != nil {
			req := ctx.Request()
			preParsedForm := bytes.HasPrefix(req.Header.ContentType(), multipartFormPrefix)

			// Read per request rather than cached: one handler may be mounted on two
			// apps with different limits. Only the multipart path reaches it, so an
			// ordinary request never pays for App.Config()'s by-value copy.
			bodyLimit := 0
			if preParsedForm {
				bodyLimit = ctx.App().Config().BodyLimit
			}

			if size, known := requestBodySize(req.Header.ContentLength(), bodyLimit, preParsedForm, req); known {
				observe(m.requestSize.WithLabelValues(values...), size, exemplar)
			}
		}

		if m.responseSize != nil {
			// A handler may set SkipBody itself, and fasthttp then drops the buffer
			// however much was written. bodyless covers what fasthttp decides
			// for itself, which it does after this runs.
			if bodyless(method, status) || resp.SkipBody {
				observe(m.responseSize.WithLabelValues(values...), 0, exemplar)
			} else if size, known := responseBodySize(resp.Header.ContentLength(), resp); known {
				observe(m.responseSize.WithLabelValues(values...), size, exemplar)
			}
		}
	}

	if m.requestsByClass != nil {
		values[0] = class
		m.requestsByClass.WithLabelValues(values...).Inc()
	}

	return nil
}

// inFlightMethod bounds the gauge's method label to Fiber's finite set of
// built-in methods. Unlike the route metrics, this gauge is created before
// routing, so retaining an arbitrary request method here would let remote
// clients create an unbounded number of zero-valued time series.
func inFlightMethod(method string) string {
	switch method {
	case fiber.MethodGet, fiber.MethodHead, fiber.MethodPost, fiber.MethodPut,
		fiber.MethodDelete, fiber.MethodConnect, fiber.MethodOptions,
		fiber.MethodTrace, fiber.MethodPatch, fiber.MethodQuery:
		return method
	default:
		return "OTHER"
	}
}

// exemplarFor returns the trace exemplar for this request, or nil when exemplars
// are off or the span is unsampled - one exemplar per bucket is overwritten on
// each observation, so unsampled traces would evict the links that lead somewhere.
func (m *middleware) exemplarFor(ctx fiber.Ctx) prometheus.Labels {
	if !m.exemplars {
		return nil
	}

	spanCtx := trace.SpanContextFromContext(ctx.Context())
	if traceID := spanCtx.TraceID(); traceID.IsValid() && spanCtx.IsSampled() {
		return prometheus.Labels{"traceID": traceID.String()}
	}

	return nil
}

// observe records value, attaching the exemplar when there is one and the
// observer accepts them.
func observe(observer prometheus.Observer, value float64, exemplar prometheus.Labels) {
	if exemplar != nil {
		if exemplarObserver, ok := observer.(prometheus.ExemplarObserver); ok {
			exemplarObserver.ObserveWithExemplar(value, exemplar)
			return
		}
	}

	observer.Observe(value)
}

// resolveDynamicValues fills the dynamic part of the label buffer, reporting
// whether the request can be recorded. These run after the chain unwinds, past any
// recover, so a panicking one drops the sample rather than killing the connection.
func (m *middleware) resolveDynamicValues(ctx fiber.Ctx, values []string) (ok bool) {
	if len(m.dynamicLabels) == 0 {
		return true
	}

	defer func() {
		if r := recover(); r != nil {
			ok = false
			// Reported once, not per request: a bad type assertion panics on every
			// one, and logging each would serialise every connection goroutine
			// through the sink after the response is already generated.
			m.reportLabelPanic.Do(func() {
				if m.errorLog != nil {
					m.errorLog.Println("prometheus middleware: a DynamicLabels function panicked; "+
						"samples are being dropped and this is reported only once:", r)
				}
			})
		}
	}()

	for i, label := range m.dynamicLabels {
		values[3+i] = detachedLabel(label.fn(ctx))
	}

	return true
}

// replacementRune stands in for bytes Prometheus will not accept.
const replacementRune = "�"

// validLabel replaces invalid UTF-8, which Prometheus panics on; the common case is
// a scan and no allocation. Use it for values the middleware owns, such as a route
// pattern; a value read off the request needs detachedLabel instead.
func validLabel(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	return strings.ToValidUTF8(value, replacementRune)
}

// detachedLabel is validLabel for a value that may alias fasthttp's read buffer.
// Without fiber.Config.Immutable those strings point into memory the next request
// reuses, and Prometheus keeps label values for the life of the series.
func detachedLabel(value string) string {
	if utf8.ValidString(value) {
		return strings.Clone(value)
	}
	// ToValidUTF8 allocates whenever it substitutes, so its result is already
	// detached.
	return strings.ToValidUTF8(value, replacementRune)
}

// statusStrings holds the decimal form of every status code, because strconv.Itoa
// allocates above 99 and this is the hottest path in the middleware.
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

// requestBodySize reports the payload received and whether it could be determined.
// The buffer is measured wherever possible; a pre-parsed multipart form falls back
// to the announced length, and a body stream is left unknown. See the README.
func requestBodySize(contentLength, bodyLimit int, preParsedForm bool, p payload) (float64, bool) {
	if p.IsBodyStream() {
		return 0, false
	}

	// The announced length is all a pre-parsed form has: Body() would re-marshal
	// every spilled part just to take its length. Without one - a chunked upload -
	// the size stays unknown rather than paying that, and so does one past the limit.
	if preParsedForm {
		if contentLength <= 0 || (bodyLimit > 0 && contentLength > bodyLimit) {
			return 0, false
		}
		return float64(contentLength), true
	}

	// Buffered, so measurable - but an announced length larger than what is there
	// means it never arrived in full, and recording the zero would drag the
	// percentiles down from a few hundred bytes of traffic.
	buffered := len(p.Body())
	if contentLength > buffered {
		return 0, false
	}

	return float64(buffered), true
}

// multipartFormPrefix is the media type fasthttp parses before the chain runs.
var multipartFormPrefix = []byte(fiber.MIMEMultipartForm)

// responseBodySize reports the payload about to be sent and whether it could be
// determined. The buffer is ground truth, since fasthttp recomputes Content-Length
// from it; a body stream can only use the announced length.
func responseBodySize(contentLength int, p payload) (float64, bool) {
	if p.IsBodyStream() {
		// Zero is announced, not absent: fasthttp uses a negative length for a
		// stream whose size is unknown.
		if contentLength >= 0 {
			return float64(contentLength), true
		}
		return 0, false
	}

	return float64(len(p.Body())), true
}

// bodyless reports whether fasthttp will drop the body the handler generated. It
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

// pathLabel returns the path label and whether the request should be recorded.
// Owning both filters is what keeps the two namespaces apart: SkipURIs holds route
// patterns and never reaches the unmatched label, which is a value.
func (m *middleware) pathLabel(ctx fiber.Ctx) (string, bool) {
	if ctx.Matched() {
		// A registered route always carries a handler. DefaultCtx substitutes a synthetic
		// Route holding the raw path, and a custom Ctx may return nil - taking either at
		// face value would put one series per distinct URL in the registry.
		if route := ctx.Route(); route != nil && len(route.Handlers) > 0 {
			// Registration-time data, so no copy is needed - but a pattern built from
			// external config can still hold raw bytes, and the resulting panic would
			// land on the connection goroutine and take the process with it.
			label := validLabel(normalizePath(route.Path))
			return label, !m.skipped(label)
		}
	}

	return m.unmatchedLabel, m.recordUnmatched
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

// validateLabelName applies client_golang's own rule so the two cannot disagree
// under a stricter validation scheme. client_golang checks names while building a
// descriptor, by which point the collectors are already registered.
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

// validateBuckets rejects bounds client_golang only rejects inside newHistogram,
// which HistogramVec calls lazily on the first observation - too late, from the
// connection goroutine where a recover mounted as prescribed cannot catch it.
func validateBuckets(field string, buckets []float64) {
	// A slice whose only bound is +Inf leaves the histogram with no buckets at
	// all: client_golang substitutes its defaults before dropping a trailing
	// +Inf, not after, so the substitution never runs.
	if len(buckets) == 1 && math.IsInf(buckets[0], 1) {
		panic("prometheus middleware: " + field + " is only +Inf, which leaves no buckets")
	}

	for i, upper := range buckets {
		if math.IsNaN(upper) {
			panic("prometheus middleware: " + field + " contains NaN")
		}
		// -Inf can never be exceeded downwards, so its bucket is permanently
		// zero and only costs a series on every scrape.
		if math.IsInf(upper, -1) {
			panic("prometheus middleware: " + field + " contains -Inf")
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
func registerCollector(registry prometheus.Registerer, collector prometheus.Collector, errorLog promhttp.Logger) {
	if err := registry.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			return
		}

		// Reported rather than fatal: these are conveniences the caller can turn off,
		// and telling a deliberate opt-in from a misconfiguration would mean matching
		// client_golang's unversioned error text, which breaks on any rewording.
		if errorLog != nil {
			errorLog.Println("prometheus middleware: the runtime collector was not registered "+
				"(disable it with DisableGoCollector or DisableProcessCollector):", err)
		}
	}
}
