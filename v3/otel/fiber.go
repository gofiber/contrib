package otel

import (
	"context"
	"encoding/hex"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/otel/internal"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
	otelcontrib "go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const (
	tracerKey           = "gofiber-contrib-tracer-fiber"
	instrumentationName = "github.com/gofiber/contrib/v3/otel"

	MetricNameHTTPServerRequestDuration  = "http.server.request.duration"
	MetricNameHTTPServerRequestBodySize  = "http.server.request.body.size"
	MetricNameHTTPServerResponseBodySize = "http.server.response.body.size"
	MetricNameHTTPServerActiveRequests   = "http.server.active_requests"

	// Unit constants for deprecated metric units
	UnitDimensionless = "1"
	UnitBytes         = "By"
	UnitSeconds       = "s"
	UnitRequest       = "{request}"

	// Deprecated: use MetricNameHTTPServerRequestDuration.
	MetricNameHttpServerDuration = MetricNameHTTPServerRequestDuration
	// Deprecated: use MetricNameHTTPServerRequestBodySize.
	MetricNameHttpServerRequestSize = MetricNameHTTPServerRequestBodySize
	// Deprecated: use MetricNameHTTPServerResponseBodySize.
	MetricNameHttpServerResponseSize = MetricNameHTTPServerResponseBodySize
	// Deprecated: use MetricNameHTTPServerActiveRequests.
	MetricNameHttpServerActiveRequests = MetricNameHTTPServerActiveRequests
	// Deprecated: kept for backward compatibility with legacy millisecond-based metrics.
	// New duration metrics use UnitSeconds.
	UnitMilliseconds = "ms"
)

// httpServerRequestDurationBoundaries are the buckets semconv recommends, in seconds.
var httpServerRequestDurationBoundaries = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

var (
	panicErrorType         = semconv.ErrorTypeKey.String("panic")
	callbackPanicErrorType = semconv.ErrorTypeKey.String("response_callback_panic")
)

// spanEndOptions make span.End record the stack of an in-flight panic.
var spanEndOptions = []oteltrace.SpanEndOption{oteltrace.WithStackTrace(true)}

var serverSpanKind = oteltrace.WithSpanKind(oteltrace.SpanKindServer)

var newRootSpan = oteltrace.WithNewRoot()

// New returns a fiber handler that traces and measures incoming requests.
func New(opts ...Option) fiber.Handler {
	cfg := config{
		clientIP: true,
	}
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	return newMiddleware(cfg).handle
}

// Middleware returns fiber handler which will trace incoming requests.
//
// Deprecated: use New.
func Middleware(opts ...Option) fiber.Handler {
	return New(opts...)
}

type middleware struct {
	config

	tracer oteltrace.Tracer
	// tracing is false for an explicit no-op tracer provider.
	tracing bool
	metrics *serverMetrics // nil with WithoutMetrics

	serverPortAttr attribute.KeyValue

	requestHeaders  []capturedHeader
	responseHeaders []capturedHeader

	settings atomic.Pointer[appSettings]
}

// appSettings caches what requests need from App.Config, which copies the whole config.
type appSettings struct {
	app                      *fiber.App
	disableHeaderNormalizing bool
}

func newMiddleware(cfg config) *middleware {
	m := &middleware{config: cfg}

	if m.TracerProvider == nil {
		m.TracerProvider = otel.GetTracerProvider()
	}
	m.tracer = m.TracerProvider.Tracer(
		instrumentationName,
		oteltrace.WithInstrumentationVersion(otelcontrib.Version()),
		oteltrace.WithSchemaURL(semconv.SchemaURL),
	)
	switch m.TracerProvider.(type) {
	case tracenoop.TracerProvider, *tracenoop.TracerProvider:
	default:
		m.tracing = true
	}

	if !m.withoutMetrics {
		if m.MeterProvider == nil {
			m.MeterProvider = otel.GetMeterProvider()
		}
		m.metrics = newServerMetrics(m.MeterProvider.Meter(
			instrumentationName,
			metric.WithInstrumentationVersion(otelcontrib.Version()),
			metric.WithSchemaURL(semconv.SchemaURL),
		))
		// Without attribute callbacks the attribute sets are bounded, so they can be cached.
		m.metrics.cacheable = m.CustomMetricAttributes == nil && m.CustomResponseMetricAttributes == nil
	}

	if m.Propagators == nil {
		m.Propagators = otel.GetTextMapPropagator()
	}
	if m.ResponsePropagators == nil {
		m.ResponsePropagators = m.Propagators
	}
	m.requestHeaders = capturedHeaders("http.request.header.", m.CapturedRequestHeaders)
	m.responseHeaders = capturedHeaders("http.response.header.", m.CapturedResponseHeaders)
	if m.SpanNameFormatter == nil {
		m.SpanNameFormatter = defaultSpanNameFormatter
	}
	if m.Port != nil {
		m.serverPortAttr = semconv.ServerPort(*m.Port)
	}

	return m
}

// requestPhase tells the deferred end what a panic interrupted.
type requestPhase uint8

const (
	phaseChain     requestPhase = iota // handler chain and error handler
	phaseCallbacks                     // response attribute callbacks
	phaseDone
)

// request is the state of one request, kept on the stack.
type request struct {
	ctx    context.Context
	parent context.Context // restored when the request ends
	cancel context.CancelFunc
	span   oteltrace.Span
	start  time.Time
	phase  requestPhase

	requestSize       int64
	responseSize      int64
	requestSizeKnown  bool
	responseSizeKnown bool

	status int
	route  string
	naming bool // set while SpanNameFormatter runs, so one that panics is not called again

	// The fields below are set only when metrics is true.
	metrics           bool
	method            string
	scheme            string
	metricAttrs       []attribute.KeyValue // for uncached attribute sets
	activeOpts        []metric.AddOption   // nil when active_requests was not incremented
	customMetricAttrs []attribute.KeyValue
}

func (m *middleware) handle(c fiber.Ctx) error {
	// Don't execute middleware if Next returns true
	if m.Next != nil && m.Next(c) {
		return c.Next()
	}

	fiber.StoreInContext(c, tracerKey, m.tracer)

	r := request{start: time.Now()}
	// Handlers get a context canceled when the request ends; outer middleware gets its own back.
	r.parent = c.Context()
	base, cancel := context.WithCancel(r.parent)
	r.cancel = cancel
	r.requestSize, r.requestSizeKnown = requestBodySize(c)

	settings := m.appSettings(c.App())
	ctx := m.Propagators.Extract(base, requestCarrierFor(&c.Request().Header, settings))
	if m.tracing {
		opts := make([]oteltrace.SpanStartOption, 0, 5+len(m.SpanStartOptions))
		opts = append(opts,
			oteltrace.WithAttributes(m.startAttributes(c, &r, settings)...),
			serverSpanKind,
			oteltrace.WithTimestamp(r.start),
		)
		if m.PublicEndpointFn != nil && m.PublicEndpointFn(c) {
			// Untrusted callers get a new trace, linked to theirs.
			opts = append(opts, newRootSpan)
			if caller := oteltrace.SpanContextFromContext(ctx); caller.IsValid() && caller.IsRemote() {
				opts = append(opts, oteltrace.WithLinks(oteltrace.Link{SpanContext: caller}))
			}
		}
		opts = append(opts, m.SpanStartOptions...)

		// Named after the method until the route is known.
		ctx, r.span = m.tracer.Start(ctx, c.Method(), opts...)
	} else {
		// Hand on the caller's span context, as a no-op tracer would.
		r.span = oteltrace.SpanFromContext(ctx)
		switch {
		case r.span.IsRecording():
			// Another component's span: wrapped so nothing here ends or annotates it.
			ctx, r.span = m.tracer.Start(ctx, "")
		case r.span.SpanContext().IsRemote() && m.PublicEndpointFn != nil && m.PublicEndpointFn(c):
			ctx = oteltrace.ContextWithSpanContext(ctx, oteltrace.SpanContext{})
			r.span = oteltrace.SpanFromContext(ctx)
		}
	}
	r.ctx = ctx
	// Deferred directly so the SDK records an in-flight panic.
	defer r.span.End(spanEndOptions...)

	// pass the span through userContext
	c.SetContext(ctx)

	// Deferred before any callback or handler runs, so a panic still ends the telemetry.
	defer m.end(c, &r)

	if m.metrics != nil && m.metrics.enabled(ctx) {
		m.startMetrics(c, &r)
	}

	// serve the request to the next middleware
	if err := c.Next(); err != nil {
		r.span.RecordError(err)
		// Run the error handler now, as Fiber's logger does, so the recorded status is the one sent.
		if handlerErr := c.App().ErrorHandler(c, err); handlerErr != nil {
			_ = c.SendStatus(fiber.StatusInternalServerError) //nolint:errcheck // mirrors Fiber's own fallback
		}
	}
	r.phase = phaseCallbacks

	r.status = c.Response().StatusCode()
	r.route = routePattern(c)
	r.responseSize, r.responseSizeKnown = responseBodySize(c)

	if r.metrics && m.CustomResponseMetricAttributes != nil {
		r.customMetricAttrs = m.CustomResponseMetricAttributes(c)
	}
	recording := r.span.IsRecording()
	var (
		customSpanAttrs []attribute.KeyValue
		spanName        string
	)
	if recording {
		if m.CustomResponseAttributes != nil {
			customSpanAttrs = m.CustomResponseAttributes(c)
		}
		r.naming = true
		spanName = m.SpanNameFormatter(c)
		r.naming = false
	}
	r.phase = phaseDone
	m.setTraceHeaders(c, c.Context(), &r)

	if recording {
		spanStatus, spanMessage := internal.SpanStatusFromHTTPStatusCodeAndSpanKind(r.status, oteltrace.SpanKindServer)

		attrs := make([]attribute.KeyValue, 0, 4+len(m.responseHeaders)+len(customSpanAttrs))
		attrs = append(attrs, semconv.HTTPResponseStatusCode(r.status))
		if r.route != "" {
			attrs = append(attrs, semconv.HTTPRoute(r.route))
		}
		if spanStatus == codes.Error {
			attrs = append(attrs, statusErrorType(r.status))
		}
		if r.responseSizeKnown {
			attrs = append(attrs, semconv.HTTPResponseBodySize(int(r.responseSize)))
		}
		for _, header := range m.responseHeaders {
			if headerValues := responseHeaderValues(&c.Response().Header, header.name, settings.disableHeaderNormalizing); len(headerValues) > 0 {
				attrs = append(attrs, header.key.StringSlice(headerValues))
			}
		}
		r.span.SetAttributes(append(attrs, customSpanAttrs...)...)
		r.span.SetName(spanName)
		r.span.SetStatus(spanStatus, spanMessage)
	}

	return nil
}

// setTraceHeaders writes the trace ID header and the response propagators' fields.
func (m *middleware) setTraceHeaders(c fiber.Ctx, ctx context.Context, r *request) {
	if m.TraceResponseHeader != "" {
		if traceID := r.span.SpanContext().TraceID(); traceID.IsValid() {
			var encoded [2 * len(traceID)]byte
			hex.Encode(encoded[:], traceID[:])
			c.Response().Header.SetBytesV(m.TraceResponseHeader, encoded[:])
		}
	}

	// Propagate tracing context as headers in outbound response
	m.ResponsePropagators.Inject(ctx, (*responseCarrier)(&c.Response().Header))
}

// end finishes the request's telemetry, on a panic too, which it does not recover.
func (m *middleware) end(c fiber.Ctx, r *request) {
	switch r.phase {
	case phaseChain:
		// A handler panicked; the recovery middleware decides the status later.
		r.route = routePattern(c)
		if r.route != "" {
			r.span.SetAttributes(semconv.HTTPRoute(r.route))
		}
		r.span.SetAttributes(panicErrorType)
		r.span.SetStatus(codes.Error, "")
	case phaseCallbacks:
		if r.route != "" {
			r.span.SetAttributes(semconv.HTTPRoute(r.route))
		}
		r.span.SetAttributes(callbackPanicErrorType)
		r.span.SetStatus(codes.Error, "")
	}

	if r.metrics {
		m.recordMetrics(c, r)
	}

	var ctx context.Context
	if r.phase != phaseDone {
		ctx = c.Context()
	}
	c.SetContext(r.parent)
	// A body stream is written after return and may watch ctx.Done(), so its context is
	// left uncanceled - unless the parent is cancelable, where that would leak.
	if !c.Response().IsBodyStream() || r.parent.Done() != nil {
		r.cancel()
	}

	if r.phase != phaseDone {
		// Last: the propagators and the formatter are application code and may panic.
		m.setTraceHeaders(c, ctx, r)
		if r.span.IsRecording() {
			format := m.SpanNameFormatter
			if r.naming {
				format = defaultSpanNameFormatter
			}
			r.span.SetName(format(c))
		}
	}
}

func requestCarrierFor(header *fasthttp.RequestHeader, settings *appSettings) propagation.TextMapCarrier {
	if settings.disableHeaderNormalizing {
		return (*foldingRequestCarrier)(header)
	}

	return (*requestCarrier)(header)
}

// appSettings caches the last app's settings: a middleware usually serves one app.
func (m *middleware) appSettings(app *fiber.App) *appSettings {
	if settings := m.settings.Load(); settings != nil && settings.app == app {
		return settings
	}

	settings := &appSettings{
		app:                      app,
		disableHeaderNormalizing: app.Config().DisableHeaderNormalizing,
	}
	m.settings.Store(settings)

	return settings
}

// defaultSpanNameFormatter returns "{method} {route}", or "{method}" without a route.
func defaultSpanNameFormatter(ctx fiber.Ctx) string {
	route := routePattern(ctx)
	if route == "" {
		return ctx.Method()
	}
	return ctx.Method() + " " + route
}
