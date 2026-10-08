package otel

import (
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// config is used to configure the Fiber middleware.
type config struct {
	Next                           func(fiber.Ctx) bool
	TracerProvider                 oteltrace.TracerProvider
	MeterProvider                  otelmetric.MeterProvider
	Port                           *int
	Propagators                    propagation.TextMapPropagator
	TraceResponseHeader            string
	SpanNameFormatter              func(fiber.Ctx) string
	CustomAttributes               func(fiber.Ctx) []attribute.KeyValue
	CustomMetricAttributes         func(fiber.Ctx) []attribute.KeyValue
	CustomResponseAttributes       func(fiber.Ctx) []attribute.KeyValue
	CustomResponseMetricAttributes func(fiber.Ctx) []attribute.KeyValue
	PublicEndpointFn               func(fiber.Ctx) bool
	ResponsePropagators            propagation.TextMapPropagator
	SpanStartOptions               []oteltrace.SpanStartOption
	CapturedRequestHeaders         []string
	CapturedResponseHeaders        []string
	RedactedQueryParams            []string
	clientIP                       bool
	withoutMetrics                 bool
}

// Option specifies instrumentation configuration options.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (o optionFunc) apply(c *config) {
	o(c)
}

// WithNext takes a function that will be called on every
// request, the middleware will be skipped if returning true
func WithNext(f func(ctx fiber.Ctx) bool) Option {
	return optionFunc(func(cfg *config) {
		cfg.Next = f
	})
}

// WithPropagators specifies propagators to use for extracting
// information from the HTTP requests. If none are specified, global
// ones will be used.
func WithPropagators(propagators propagation.TextMapPropagator) Option {
	return optionFunc(func(cfg *config) {
		cfg.Propagators = propagators
	})
}

// WithTraceResponseHeader specifies the response header used to expose
// the current trace ID. If empty, no dedicated trace ID response header is set.
func WithTraceResponseHeader(header string) Option {
	return optionFunc(func(cfg *config) {
		cfg.TraceResponseHeader = header
	})
}

// WithTracerProvider specifies a tracer provider to use for creating a tracer.
// If none is specified, the global provider is used. A noop provider starts no spans.
func WithTracerProvider(provider oteltrace.TracerProvider) Option {
	return optionFunc(func(cfg *config) {
		cfg.TracerProvider = provider
	})
}

// WithMeterProvider specifies a meter provider to use for reporting.
// If none is specified, the global provider is used.
func WithMeterProvider(provider otelmetric.MeterProvider) Option {
	return optionFunc(func(cfg *config) {
		cfg.MeterProvider = provider
	})
}

// WithSpanNameFormatter takes a function that names recording spans once the route is known.
// The default is "{method} {route}", or "{method}" without a route.
func WithSpanNameFormatter(f func(ctx fiber.Ctx) string) Option {
	return optionFunc(func(cfg *config) {
		cfg.SpanNameFormatter = f
	})
}

// WithPort sets server.port on spans and metrics. Without it, spans use the Host header's
// port and metrics carry none, as server.port is opt-in on metrics.
func WithPort(port int) Option {
	return optionFunc(func(cfg *config) {
		cfg.Port = &port
	})
}

// WithCustomAttributes specifies a function that will be called on every
// request and the returned attributes will be added to the span.
func WithCustomAttributes(f func(ctx fiber.Ctx) []attribute.KeyValue) Option {
	return optionFunc(func(cfg *config) {
		cfg.CustomAttributes = f
	})
}

// WithCustomMetricAttributes specifies a function that will be called on every
// request and the returned attributes will be added to the metrics.
func WithCustomMetricAttributes(f func(ctx fiber.Ctx) []attribute.KeyValue) Option {
	return optionFunc(func(cfg *config) {
		cfg.CustomMetricAttributes = f
	})
}

// WithCustomResponseAttributes specifies a function called after the handler
// chain has run; its attributes are added to the server span. The callback can
// inspect the matched route, response, and values set by handlers. Spans may be
// exported after the request ends, so copy strings read from fiber.Ctx methods
// such as Params, Get, and GetRespHeader with utils.CopyString unless
// fiber.Config.Immutable is enabled.
// If a response attribute callback panics, telemetry records an error with
// error.type=response_callback_panic and omits the final HTTP status and response
// size, which depend on outer recovery. The original panic propagates normally.
func WithCustomResponseAttributes(f func(ctx fiber.Ctx) []attribute.KeyValue) Option {
	return optionFunc(func(cfg *config) {
		cfg.CustomResponseAttributes = f
	})
}

// WithCustomResponseMetricAttributes specifies a function called after the
// handler chain has run; its attributes are added to the request duration and
// body size metrics. Active request metrics retain request-time attributes.
// Copy strings read from fiber.Ctx as described for WithCustomResponseAttributes,
// and keep metric attributes low-cardinality because each distinct attribute
// set creates a new metric series.
// Callback panics are recorded as described for WithCustomResponseAttributes.
func WithCustomResponseMetricAttributes(f func(ctx fiber.Ctx) []attribute.KeyValue) Option {
	return optionFunc(func(cfg *config) {
		cfg.CustomResponseMetricAttributes = f
	})
}

// WithPublicEndpoint starts a new trace for every request, linked to the caller's, so
// untrusted clients cannot choose the trace or its sampling.
func WithPublicEndpoint() Option {
	return WithPublicEndpointFn(func(fiber.Ctx) bool { return true })
}

// WithPublicEndpointFn applies WithPublicEndpoint to the requests f returns true for.
func WithPublicEndpointFn(f func(ctx fiber.Ctx) bool) Option {
	return optionFunc(func(cfg *config) {
		cfg.PublicEndpointFn = f
	})
}

// WithSpanStartOptions adds options to every server span, applied after the middleware's own.
func WithSpanStartOptions(opts ...oteltrace.SpanStartOption) Option {
	return optionFunc(func(cfg *config) {
		cfg.SpanStartOptions = append(cfg.SpanStartOptions, opts...)
	})
}

// WithResponsePropagators sets the propagators that inject the trace context into responses.
// They default to the extraction propagators.
func WithResponsePropagators(propagators propagation.TextMapPropagator) Option {
	return optionFunc(func(cfg *config) {
		cfg.ResponsePropagators = propagators
	})
}

// WithCapturedRequestHeaders records the named request headers as http.request.header.<name>
// span attributes. Names are matched case-insensitively.
func WithCapturedRequestHeaders(headers ...string) Option {
	return optionFunc(func(cfg *config) {
		cfg.CapturedRequestHeaders = append(cfg.CapturedRequestHeaders, headers...)
	})
}

// WithCapturedResponseHeaders records the named response headers as http.response.header.<name>.
func WithCapturedResponseHeaders(headers ...string) Option {
	return optionFunc(func(cfg *config) {
		cfg.CapturedResponseHeaders = append(cfg.CapturedResponseHeaders, headers...)
	})
}

// WithRedactedQueryParams redacts these query parameters in url.query, in addition to the
// ones semconv names. Names are matched case-sensitively.
func WithRedactedQueryParams(params ...string) Option {
	return optionFunc(func(cfg *config) {
		cfg.RedactedQueryParams = append(cfg.RedactedQueryParams, params...)
	})
}

// WithClientIP specifies whether to record client.address and the peer's address and port.
// This is enabled by default.
func WithClientIP(collect bool) Option {
	return optionFunc(func(cfg *config) {
		cfg.clientIP = collect
	})
}

// WithCollectClientIP is kept for backwards compatibility.
//
// Deprecated: use WithClientIP instead.
func WithCollectClientIP(collect bool) Option {
	return WithClientIP(collect)
}

// WithoutMetrics disables metrics collection when set to true
func WithoutMetrics(withoutMetrics bool) Option {
	return optionFunc(func(cfg *config) {
		cfg.withoutMetrics = withoutMetrics
	})
}
