package otel

import (
	"context"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// discardSpanProcessor ends spans without exporting them.
type discardSpanProcessor struct{}

func (discardSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (discardSpanProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (discardSpanProcessor) Shutdown(context.Context) error                  { return nil }
func (discardSpanProcessor) ForceFlush(context.Context) error                { return nil }

const benchmarkTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// benchmarkProviders returns the options for a scenario; unused SDKs are left to no-op providers.
func benchmarkOptions(sampler sdktrace.Sampler, metrics bool) []Option {
	opts := []Option{
		WithPropagators(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})),
	}

	if sampler == nil {
		opts = append(opts, WithTracerProvider(tracenoop.NewTracerProvider()))
	} else {
		opts = append(opts, WithTracerProvider(sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sampler),
			sdktrace.WithSpanProcessor(discardSpanProcessor{}),
		)))
	}

	if metrics {
		opts = append(opts, WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))))
	} else {
		opts = append(opts, WithMeterProvider(noop.NewMeterProvider()))
	}

	return opts
}

// newBenchmarkApp builds the benchmark app; nil opts leave the middleware out, as a baseline.
func newBenchmarkApp(opts []Option) *fiber.App {
	app := fiber.New()
	if opts != nil {
		app.Use(New(opts...))
	}
	app.Get("/users/:id", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	return app
}

// benchmarkHandler drives the fasthttp handler directly with one reused request context.
func benchmarkHandler(b *testing.B, app *fiber.App, traceparent string) {
	b.Helper()
	b.ReportAllocs()

	handler := app.Handler()

	var req fasthttp.Request
	req.Header.SetMethod(http.MethodGet)
	req.SetRequestURI("/users/123?expand=orders")
	req.Header.SetHost("example.com")
	req.Header.SetUserAgent("benchmark/1.0")
	if traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}

	var fctx fasthttp.RequestCtx
	for b.Loop() {
		fctx.Init(&req, nil, nil)
		fctx.Response.Reset()
		handler(&fctx)
		if status := fctx.Response.StatusCode(); status != http.StatusOK {
			b.Fatalf("expected status %d, got %d", http.StatusOK, status)
		}
	}
}

func BenchmarkMiddleware(b *testing.B) {
	scenarios := []struct {
		name    string
		sampler sdktrace.Sampler
		metrics bool
	}{
		{name: "Noop"},
		{name: "MetricsOnly", metrics: true},
		{name: "TracesOnly", sampler: sdktrace.AlwaysSample()},
		{name: "Sampled", sampler: sdktrace.AlwaysSample(), metrics: true},
		{name: "Unsampled", sampler: sdktrace.NeverSample(), metrics: true},
	}

	b.Run("Baseline", func(b *testing.B) {
		benchmarkHandler(b, newBenchmarkApp(nil), "")
	})

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			benchmarkHandler(b, newBenchmarkApp(benchmarkOptions(scenario.sampler, scenario.metrics)), "")
		})
		b.Run(scenario.name+"/Traceparent", func(b *testing.B) {
			benchmarkHandler(b, newBenchmarkApp(benchmarkOptions(scenario.sampler, scenario.metrics)), benchmarkTraceparent)
		})
	}
}
