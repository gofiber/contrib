package otelfiber

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	otelcontrib "go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerKey           = "gofiber-contrib-tracer-fiber"
	instrumentationName = "github.com/gofiber/contrib/otelfiber"

	AttributeNameHTTPRequestHeaderContentLength = "http.request.header.content-length"
)

// http.server.request.duration SHOULD be specified with ExplicitBucketBoundaries of [ 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10 ].
var ExplicitBucketBoundaries = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// Middleware returns fiber handler which will trace incoming requests.
func Middleware(opts ...Option) fiber.Handler {
	cfg := config{
		collectClientIP: true,
	}
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		instrumentationName,
		oteltrace.WithInstrumentationVersion(otelcontrib.Version()),
	)

	if cfg.MeterProvider == nil {
		cfg.MeterProvider = otel.GetMeterProvider()
	}
	meter := cfg.MeterProvider.Meter(
		instrumentationName,
		metric.WithInstrumentationVersion(otelcontrib.Version()),
	)

	httpServerDuration, err := meter.Float64Histogram(semconv.HTTPServerRequestDurationName, metric.WithUnit(semconv.HTTPServerRequestDurationUnit), metric.WithDescription(semconv.HTTPServerRequestDurationDescription), metric.WithExplicitBucketBoundaries(ExplicitBucketBoundaries...))
	if err != nil {
		otel.Handle(err)
	}
	httpServerRequestSize, err := meter.Int64Histogram(semconv.HTTPServerRequestBodySizeName, metric.WithUnit(semconv.HTTPServerRequestBodySizeUnit), metric.WithDescription(semconv.HTTPServerRequestBodySizeDescription))
	if err != nil {
		otel.Handle(err)
	}
	httpServerResponseSize, err := meter.Int64Histogram(semconv.HTTPServerResponseBodySizeName, metric.WithUnit(semconv.HTTPServerResponseBodySizeUnit), metric.WithDescription(semconv.HTTPServerResponseBodySizeDescription))
	if err != nil {
		otel.Handle(err)
	}
	httpServerActiveRequests, err := meter.Int64UpDownCounter(semconv.HTTPServerActiveRequestsName, metric.WithUnit(semconv.HTTPServerActiveRequestsUnit), metric.WithDescription(semconv.HTTPServerActiveRequestsDescription))
	if err != nil {
		otel.Handle(err)
	}

	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	if cfg.SpanNameFormatter == nil {
		cfg.SpanNameFormatter = defaultSpanNameFormatter
	}

	return func(c *fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		c.Locals(tracerKey, tracer)
		savedCtx, cancel := context.WithCancel(c.UserContext())

		start := time.Now()

		requestMetricsAttrs := httpServerMetricAttributesFromRequest(c, cfg)
		httpServerActiveRequests.Add(savedCtx, 1, metric.WithAttributes(requestMetricsAttrs...))

		responseMetricAttrs := make([]attribute.KeyValue, len(requestMetricsAttrs))
		copy(responseMetricAttrs, requestMetricsAttrs)

		reqHeader := make(http.Header)
		c.Request().Header.VisitAll(func(k, v []byte) {
			reqHeader.Add(string(k), string(v))
		})

		ctx := cfg.Propagators.Extract(savedCtx, propagation.HeaderCarrier(reqHeader))

		opts := []oteltrace.SpanStartOption{
			oteltrace.WithAttributes(httpServerTraceAttributesFromRequest(c, cfg)...),
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		}

		// temporary set to c.Path() first
		// update with c.Route().Path after c.Next() is called
		// to get pathRaw
		spanName := utils.CopyString(c.Path())
		ctx, span := tracer.Start(ctx, spanName, opts...)
		defer span.End()

		// pass the span through userContext
		c.SetUserContext(ctx)

		// serve the request to the next middleware
		if err := c.Next(); err != nil {
			span.RecordError(err)
			// invokes the registered HTTP error handler
			// to get the correct response status code
			_ = c.App().Config().ErrorHandler(c, err)
		}

		// extract common attributes from response
		responseAttrs := []attribute.KeyValue{
			semconv.HTTPResponseStatusCode(c.Response().StatusCode()),
			semconv.HTTPRouteKey.String(c.Route().Path), // no need to copy c.Route().Path: route strings should be immutable across app lifecycle
		}

		var responseSize int64
		requestSize := int64(len(c.Request().Body()))
		if c.GetRespHeader("Content-Type") != "text/event-stream" {
			responseSize = int64(len(c.Response().Body()))
		}

		defer func() {
			responseMetricAttrs = append(
				responseMetricAttrs,
				responseAttrs...)

			httpServerActiveRequests.Add(savedCtx, -1, metric.WithAttributes(requestMetricsAttrs...))
			httpServerRequestSize.Record(savedCtx, requestSize, metric.WithAttributes(responseMetricAttrs...))
			httpServerResponseSize.Record(savedCtx, responseSize, metric.WithAttributes(responseMetricAttrs...))
			httpServerDuration.Record(savedCtx, time.Since(start).Seconds(), metric.WithAttributes(responseMetricAttrs...))

			c.SetUserContext(savedCtx)
			cancel()
		}()

		span.SetAttributes(
			append(
				responseAttrs,
				semconv.HTTPResponseBodySize(int(responseSize)),
			)...)
		span.SetName(cfg.SpanNameFormatter(c))

		span.SetStatus(Status(c.Response().StatusCode()))

		//Propagate tracing context as headers in outbound response
		tracingHeaders := make(propagation.HeaderCarrier)
		cfg.Propagators.Inject(c.UserContext(), tracingHeaders)
		for _, headerKey := range tracingHeaders.Keys() {
			c.Set(headerKey, tracingHeaders.Get(headerKey))
		}

		return nil
	}
}

// defaultSpanNameFormatter is the default formatter for spans created with the fiber
// integration. Returns the route pathRaw
func defaultSpanNameFormatter(ctx *fiber.Ctx) string {
	return ctx.Route().Path
}

// Status returns a span status code and message for an HTTP status code
// value returned by a server. Status codes in the 400-499 range are not
// returned as errors.
func Status(code int) (codes.Code, string) {
	if code < 100 || code >= 600 {
		return codes.Error, fmt.Sprintf("Invalid HTTP status code %d", code)
	}
	if code >= 500 {
		return codes.Error, ""
	}
	return codes.Unset, ""
}
