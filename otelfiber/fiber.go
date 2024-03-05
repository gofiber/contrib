package otelfiber

import (
	"context"
	"github.com/valyala/fasthttp"
	"io"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	otelcontrib "go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerKey           = "gofiber-contrib-tracer-fiber"
	instrumentationName = "github.com/gofiber/contrib/otelfiber"

	MetricNameHttpServerDuration       = "http.server.duration"
	MetricNameHttpServerRequestSize    = "http.server.request.size"
	MetricNameHttpServerResponseSize   = "http.server.response.size"
	MetricNameHttpServerActiveRequests = "http.server.active_requests"

	// Unit constants for deprecated metric units
	UnitDimensionless = "1"
	UnitBytes         = "By"
	UnitMilliseconds  = "ms"
)

var _ io.Reader = (*bodyStreamSizeReader)(nil)

type bodyStreamSizeReader struct {
	impl       io.Reader
	histogram  metric.Int64Histogram
	measureOpt metric.MeasurementOption
	ctx        context.Context
	read       int64
}

func (b *bodyStreamSizeReader) Read(p []byte) (n int, err error) {
	n, err = b.impl.Read(p)
	b.read += int64(n)
	if err == io.EOF {
		b.histogram.Record(b.ctx, b.read, b.measureOpt)
		b.read = 0
	}

	return n, err
}

// Middleware returns fiber handler which will trace incoming requests.
func Middleware(opts ...Option) fiber.Handler {
	cfg := config{}
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

	httpServerDuration, err := meter.Float64Histogram(MetricNameHttpServerDuration, metric.WithUnit(UnitMilliseconds), metric.WithDescription("measures the duration inbound HTTP requests"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerRequestSize, err := meter.Int64Histogram(MetricNameHttpServerRequestSize, metric.WithUnit(UnitBytes), metric.WithDescription("measures the size of HTTP request messages"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerResponseSize, err := meter.Int64Histogram(MetricNameHttpServerResponseSize, metric.WithUnit(UnitBytes), metric.WithDescription("measures the size of HTTP response messages"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerActiveRequests, err := meter.Int64UpDownCounter(MetricNameHttpServerActiveRequests, metric.WithUnit(UnitDimensionless), metric.WithDescription("measures the number of concurrent HTTP requests that are currently in-flight"))
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
		request := c.Request()
		request.Header.VisitAll(func(k, v []byte) {
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
		response := c.Response()
		responseAttrs := append(
			semconv.HTTPAttributesFromHTTPStatusCode(response.StatusCode()),
			semconv.HTTPRouteKey.String(c.Route().Path), // no need to copy c.Route().Path: route strings should be immutable across app lifecycle
		)

		var (
			requestSize  int64
			responseSize int64
		)

		if !request.IsBodyStream() {
			requestSize = int64(len(request.Body()))
		} else {
			// NOTICE: we have to create response copy because underlying steam closed before change
			copyReq := &fasthttp.Request{}
			request.CopyTo(copyReq)
			copyReq.SetBodyStream(&bodyStreamSizeReader{
				impl:      response.BodyStream(),
				histogram: httpServerRequestSize,
				measureOpt: metric.WithAttributes(append(
					responseMetricAttrs,
					responseAttrs...)...),
				ctx:  context.WithoutCancel(savedCtx),
				read: 0,
			}, -1)

			request = copyReq
		}

		if !response.IsBodyStream() {
			responseSize = int64(len(response.Body()))
		} else {
			// NOTICE: we have to create response copy because underlying steam closed before change
			copyResp := &fasthttp.Response{}
			response.CopyTo(copyResp)
			copyResp.SetBodyStream(&bodyStreamSizeReader{
				impl:      response.BodyStream(),
				histogram: httpServerResponseSize,
				measureOpt: metric.WithAttributes(append(
					responseMetricAttrs,
					responseAttrs...)...),
				ctx:  context.WithoutCancel(savedCtx),
				read: 0,
			}, -1)

			response = copyResp
		}

		defer func() {
			responseMetricAttrs = append(
				responseMetricAttrs,
				responseAttrs...)

			httpServerActiveRequests.Add(savedCtx, -1, metric.WithAttributes(requestMetricsAttrs...))
			httpServerDuration.Record(savedCtx, float64(time.Since(start).Microseconds())/1000, metric.WithAttributes(responseMetricAttrs...))
			if !request.IsBodyStream() {
				httpServerRequestSize.Record(savedCtx, requestSize, metric.WithAttributes(responseMetricAttrs...))
			}

			if !response.IsBodyStream() {
				httpServerResponseSize.Record(savedCtx, responseSize, metric.WithAttributes(responseMetricAttrs...))
			}

			c.SetUserContext(savedCtx)
			cancel()
		}()

		span.SetAttributes(
			append(
				responseAttrs,
				semconv.HTTPResponseContentLengthKey.Int64(responseSize),
			)...)
		span.SetName(cfg.SpanNameFormatter(c))

		spanStatus, spanMessage := semconv.SpanStatusFromHTTPStatusCodeAndSpanKind(response.StatusCode(), oteltrace.SpanKindServer)
		span.SetStatus(spanStatus, spanMessage)

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
