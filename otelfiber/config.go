package otelfiber

import (
	"github.com/gofiber/fiber/v2"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// config is used to configure the Fiber middleware.
type config struct {
	Next              func(*fiber.Ctx) bool
	TracerProvider    oteltrace.TracerProvider
	MeterProvider     otelmetric.MeterProvider
	Port              *int
	Propagators       propagation.TextMapPropagator
	ServerName        *string
	SpanNameFormatter func(*fiber.Ctx) string
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
func WithNext(f func(ctx *fiber.Ctx) bool) Option {
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

// WithTracerProvider specifies a tracer provider to use for creating a tracer.
// If none is specified, the global provider is used.
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

// WithSpanNameFormatter takes a function that will be called on every
// request and the returned string will become the Span Name
func WithSpanNameFormatter(f func(ctx *fiber.Ctx) string) Option {
	return optionFunc(func(cfg *config) {
		cfg.SpanNameFormatter = f
	})
}

// WithServerName specifies the value to use when setting the `http.server_name`
// attribute on metrics/spans.
func WithServerName(serverName string) Option {
	return optionFunc(func(cfg *config) {
		cfg.ServerName = &serverName
	})
}

// WithPort specifies the value to use when setting the `net.host.port`
// attribute on metrics/spans. Attribute is "Conditionally Required: If not
// default (`80` for `http`, `443` for `https`).
func WithPort(port int) Option {
	return optionFunc(func(cfg *config) {
		cfg.Port = &port
	})
}

