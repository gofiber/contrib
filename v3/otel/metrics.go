package otel

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/otel/internal"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// serverMetrics are the HTTP server instruments.
type serverMetrics struct {
	duration       metric.Float64Histogram
	requestSize    metric.Int64Histogram
	responseSize   metric.Int64Histogram
	activeRequests metric.Int64UpDownCounter

	// cacheable is set when no callback adds attributes, so attribute sets can be shared.
	cacheable  bool
	activeSets optionCache[activeKey, []metric.AddOption]
	recordSets optionCache[recordKey, []metric.RecordOption]
}

type activeKey struct {
	method string
	scheme string
}

type recordKey struct {
	method string
	scheme string
	route  string
	status int // 0 for a request that panicked
	http11 bool
	phase  requestPhase
}

// newServerMetrics replaces an instrument that fails to be created with a no-op.
func newServerMetrics(meter metric.Meter) *serverMetrics {
	s := &serverMetrics{}

	var err error
	if s.duration, err = meter.Float64Histogram(
		MetricNameHTTPServerRequestDuration,
		metric.WithUnit(UnitSeconds),
		metric.WithDescription("Duration of HTTP server requests."),
		metric.WithExplicitBucketBoundaries(httpServerRequestDurationBoundaries...),
	); err != nil {
		otel.Handle(err)
	}
	if s.duration == nil {
		s.duration = noop.Float64Histogram{}
	}

	if s.requestSize, err = meter.Int64Histogram(
		MetricNameHTTPServerRequestBodySize,
		metric.WithUnit(UnitBytes),
		metric.WithDescription("Size of HTTP server request bodies."),
	); err != nil {
		otel.Handle(err)
	}
	if s.requestSize == nil {
		s.requestSize = noop.Int64Histogram{}
	}

	if s.responseSize, err = meter.Int64Histogram(
		MetricNameHTTPServerResponseBodySize,
		metric.WithUnit(UnitBytes),
		metric.WithDescription("Size of HTTP server response bodies."),
	); err != nil {
		otel.Handle(err)
	}
	if s.responseSize == nil {
		s.responseSize = noop.Int64Histogram{}
	}

	if s.activeRequests, err = meter.Int64UpDownCounter(
		MetricNameHTTPServerActiveRequests,
		metric.WithUnit(UnitRequest),
		metric.WithDescription("Number of active HTTP server requests."),
	); err != nil {
		otel.Handle(err)
	}
	if s.activeRequests == nil {
		s.activeRequests = noop.Int64UpDownCounter{}
	}

	return s
}

// enabled reports whether any instrument records, so requests can skip building attributes.
func (s *serverMetrics) enabled(ctx context.Context) bool {
	return s.duration.Enabled(ctx) || s.requestSize.Enabled(ctx) ||
		s.responseSize.Enabled(ctx) || s.activeRequests.Enabled(ctx)
}

// startMetrics resolves the shared metric attributes and counts the request as active.
func (m *middleware) startMetrics(c fiber.Ctx, r *request) {
	r.method = c.Method()
	r.scheme = c.Scheme()

	if !m.metrics.cacheable {
		var custom []attribute.KeyValue
		if m.CustomMetricAttributes != nil {
			custom = m.CustomMetricAttributes(c)
		}
		// Room for the attributes the histograms add.
		r.metricAttrs = m.appendRequestMetricAttributes(make([]attribute.KeyValue, 0, 3+len(custom)+6), r)
		r.metricAttrs = append(r.metricAttrs, custom...)
	}

	// Set last: if the callback panics, there are no attributes to record with.
	r.metrics = true

	if m.metrics.activeRequests.Enabled(r.ctx) {
		r.activeOpts = m.activeRequestOptions(r)
		m.metrics.activeRequests.Add(r.ctx, 1, r.activeOpts...)
	}
}

// appendRequestMetricAttributes appends the attributes every metric carries.
func (m *middleware) appendRequestMetricAttributes(attrs []attribute.KeyValue, r *request) []attribute.KeyValue {
	attrs = append(attrs,
		semconv.HTTPRequestMethodKey.String(r.method),
		semconv.URLScheme(r.scheme),
	)
	if m.Port != nil {
		attrs = append(attrs, m.serverPortAttr)
	}

	return attrs
}

func (m *middleware) activeRequestOptions(r *request) []metric.AddOption {
	if !m.metrics.cacheable {
		// A copy: NewSet sorts its input, and metricAttrs is extended later.
		set := attribute.NewSet(append([]attribute.KeyValue(nil), r.metricAttrs...)...)
		return []metric.AddOption{metric.WithAttributeSet(set)}
	}

	key := activeKey{method: r.method, scheme: r.scheme}
	if opts, ok := m.metrics.activeSets.load(key); ok {
		return opts
	}

	set := attribute.NewSet(m.appendRequestMetricAttributes(make([]attribute.KeyValue, 0, 3), r)...)
	opts := []metric.AddOption{metric.WithAttributeSet(set)}
	m.metrics.activeSets.store(key, opts)

	return opts
}

func (m *middleware) recordMetrics(c fiber.Ctx, r *request) {
	if r.activeOpts != nil {
		m.metrics.activeRequests.Add(r.ctx, -1, r.activeOpts...)
	}

	recordDuration := m.metrics.duration.Enabled(r.ctx)
	recordRequestSize := r.requestSizeKnown && m.metrics.requestSize.Enabled(r.ctx)
	recordResponseSize := r.phase == phaseDone && r.responseSizeKnown && m.metrics.responseSize.Enabled(r.ctx)
	if !recordDuration && !recordRequestSize && !recordResponseSize {
		return
	}

	opts := m.recordOptions(c, r)
	if recordDuration {
		m.metrics.duration.Record(r.ctx, time.Since(r.start).Seconds(), opts...)
	}
	if recordRequestSize {
		m.metrics.requestSize.Record(r.ctx, r.requestSize, opts...)
	}
	if recordResponseSize {
		m.metrics.responseSize.Record(r.ctx, r.responseSize, opts...)
	}
}

func (m *middleware) recordOptions(c fiber.Ctx, r *request) []metric.RecordOption {
	http11 := c.Request().Header.IsHTTP11()

	if !m.metrics.cacheable {
		attrs := m.appendResponseMetricAttributes(r.metricAttrs, r, http11)
		return []metric.RecordOption{metric.WithAttributeSet(attribute.NewSet(attrs...))}
	}

	key := recordKey{method: r.method, scheme: r.scheme, route: r.route, http11: http11, phase: r.phase}
	if r.phase == phaseDone {
		key.status = r.status
	}
	if opts, ok := m.metrics.recordSets.load(key); ok {
		return opts
	}

	attrs := m.appendRequestMetricAttributes(make([]attribute.KeyValue, 0, 9), r)
	attrs = m.appendResponseMetricAttributes(attrs, r, http11)
	opts := []metric.RecordOption{metric.WithAttributeSet(attribute.NewSet(attrs...))}
	m.metrics.recordSets.store(key, opts)

	return opts
}

// appendResponseMetricAttributes appends the protocol and the request's outcome.
func (*middleware) appendResponseMetricAttributes(attrs []attribute.KeyValue, r *request, http11 bool) []attribute.KeyValue {
	attrs = append(attrs, httpProtocolNameAttr, http10VersionAttr)
	if http11 {
		attrs[len(attrs)-1] = http11VersionAttr
	}
	if r.route != "" {
		attrs = append(attrs, semconv.HTTPRoute(r.route))
	}

	switch r.phase {
	case phaseDone:
		attrs = append(attrs, semconv.HTTPResponseStatusCode(r.status))
		if status, _ := internal.SpanStatusFromHTTPStatusCodeAndSpanKind(r.status, oteltrace.SpanKindServer); status == codes.Error {
			attrs = append(attrs, statusErrorType(r.status))
		}
		attrs = append(attrs, r.customMetricAttrs...)
	case phaseCallbacks:
		// The recovery middleware decides the status later.
		attrs = append(attrs, r.customMetricAttrs...)
		attrs = append(attrs, callbackPanicErrorType)
	case phaseChain:
		attrs = append(attrs, panicErrorType)
	}

	return attrs
}

// maxCachedSets bounds each cache; past it, attribute sets are built per request.
const maxCachedSets = 1024

// optionCache shares the metric options of recurring attribute sets.
type optionCache[K comparable, V any] struct {
	entries sync.Map
	size    atomic.Int32
}

func (c *optionCache[K, V]) load(key K) (V, bool) {
	if value, ok := c.entries.Load(key); ok {
		return value.(V), true //nolint:errcheck,forcetypeassert // only V is stored
	}

	var zero V
	return zero, false
}

func (c *optionCache[K, V]) store(key K, value V) {
	if c.size.Load() >= maxCachedSets {
		return
	}
	if _, loaded := c.entries.LoadOrStore(key, value); !loaded {
		c.size.Add(1)
	}
}
