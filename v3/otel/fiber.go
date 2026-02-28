package otel

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/otel/internal"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	otelcontrib "go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	oteltrace "go.opentelemetry.io/otel/trace"
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

type bodyStreamSizeReader struct {
	reader io.Reader
	onEOF  func(read int64)
	read   int64
	eof    sync.Once
}

func (b *bodyStreamSizeReader) Read(p []byte) (n int, err error) {
	n, err = b.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(&b.read, int64(n))
	}
	if err == io.EOF && b.onEOF != nil {
		read := atomic.LoadInt64(&b.read)
		b.eof.Do(func() {
			b.onEOF(read)
		})
	}

	return n, err
}

func (b *bodyStreamSizeReader) Close() error {
	closer, ok := b.reader.(io.Closer)
	if !ok {
		return nil
	}

	return closer.Close()
}

func detachedMetricContext(ctx context.Context) context.Context {
	detached := context.Background()

	if spanContext := oteltrace.SpanContextFromContext(ctx); spanContext.IsValid() {
		detached = oteltrace.ContextWithSpanContext(detached, spanContext)
	}

	if bg := baggage.FromContext(ctx); bg.Len() > 0 {
		detached = baggage.ContextWithBaggage(detached, bg)
	}

	return detached
}

// Middleware returns fiber handler which will trace incoming requests.
func Middleware(opts ...Option) fiber.Handler {
	cfg := config{
		clientIP: true,
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

	var httpServerDuration metric.Float64Histogram
	var httpServerRequestSize metric.Int64Histogram
	var httpServerResponseSize metric.Int64Histogram
	var httpServerActiveRequests metric.Int64UpDownCounter

	if !cfg.withoutMetrics {
		if cfg.MeterProvider == nil {
			cfg.MeterProvider = otel.GetMeterProvider()
		}
		meter := cfg.MeterProvider.Meter(
			instrumentationName,
			metric.WithInstrumentationVersion(otelcontrib.Version()),
		)

		var err error
		httpServerDuration, err = meter.Float64Histogram(MetricNameHTTPServerRequestDuration, metric.WithUnit(UnitSeconds), metric.WithDescription("Duration of HTTP server requests."))
		if err != nil {
			otel.Handle(err)
		}
		httpServerRequestSize, err = meter.Int64Histogram(MetricNameHTTPServerRequestBodySize, metric.WithUnit(UnitBytes), metric.WithDescription("Size of HTTP server request bodies."))
		if err != nil {
			otel.Handle(err)
		}
		httpServerResponseSize, err = meter.Int64Histogram(MetricNameHTTPServerResponseBodySize, metric.WithUnit(UnitBytes), metric.WithDescription("Size of HTTP server response bodies."))
		if err != nil {
			otel.Handle(err)
		}
		httpServerActiveRequests, err = meter.Int64UpDownCounter(MetricNameHTTPServerActiveRequests, metric.WithUnit(UnitDimensionless), metric.WithDescription("Number of active HTTP server requests."))
		if err != nil {
			otel.Handle(err)
		}
	}

	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	if cfg.SpanNameFormatter == nil {
		cfg.SpanNameFormatter = defaultSpanNameFormatter
	}

	return func(c fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		fiber.StoreInContext(c, tracerKey, tracer)
		savedCtx, cancel := context.WithCancel(c)

		start := time.Now()

		requestMetricsAttrs := httpServerMetricAttributesFromRequest(c, cfg)
		if !cfg.withoutMetrics {
			httpServerActiveRequests.Add(savedCtx, 1, metric.WithAttributes(requestMetricsAttrs...))
		}

		responseMetricAttrs := make([]attribute.KeyValue, len(requestMetricsAttrs))
		copy(responseMetricAttrs, requestMetricsAttrs)

		request := c.Request()
		isRequestBodyStream := request.IsBodyStream()
		requestSize := int64(0)
		var requestBodyStreamSizeReader *bodyStreamSizeReader
		if isRequestBodyStream && !cfg.withoutMetrics {
			requestBodyStream := request.BodyStream()
			if requestBodyStream != nil {
				requestBodyStreamSizeReader = &bodyStreamSizeReader{reader: requestBodyStream}
				request.SetBodyStream(requestBodyStreamSizeReader, -1)
			}
		} else {
			requestSize = int64(len(request.Body()))
		}

		reqHeader := make(http.Header)
		for header, values := range c.GetReqHeaders() {
			for _, value := range values {
				reqHeader.Add(header, value)
			}
		}

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
		c.SetContext(ctx)

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

		response := c.Response()
		isSSE := c.GetRespHeader("Content-Type") == "text/event-stream"
		responseSize := int64(0)
		isResponseBodyStream := response.IsBodyStream()
		if !isResponseBodyStream && !isSSE {
			responseSize = int64(len(response.Body()))
		}

		if isResponseBodyStream && !isSSE && !cfg.withoutMetrics {
			responseBodyStream := response.BodyStream()
			if responseBodyStream != nil {
				responseMetricAttrsWithResponse := append(responseMetricAttrs, responseAttrs...)
				responseMetricsCtx := detachedMetricContext(savedCtx)
				responseBodyStreamReader := &bodyStreamSizeReader{
					reader: responseBodyStream,
					onEOF: func(read int64) {
						httpServerResponseSize.Record(responseMetricsCtx, read, metric.WithAttributes(responseMetricAttrsWithResponse...))
					},
				}
				response.SetBodyStream(responseBodyStreamReader, -1)
			} else {
				isResponseBodyStream = false
			}
		}

		defer func() {
			responseMetricAttrs = append(responseMetricAttrs, responseAttrs...)
			if requestBodyStreamSizeReader != nil {
				requestSize = atomic.LoadInt64(&requestBodyStreamSizeReader.read)
			}

			if !cfg.withoutMetrics {
				httpServerActiveRequests.Add(savedCtx, -1, metric.WithAttributes(requestMetricsAttrs...))
				httpServerDuration.Record(savedCtx, time.Since(start).Seconds(), metric.WithAttributes(responseMetricAttrs...))
				httpServerRequestSize.Record(savedCtx, requestSize, metric.WithAttributes(responseMetricAttrs...))
				if !isResponseBodyStream {
					httpServerResponseSize.Record(savedCtx, responseSize, metric.WithAttributes(responseMetricAttrs...))
				}
			}

			c.SetContext(savedCtx)
			cancel()
		}()

		if !isResponseBodyStream {
			span.SetAttributes(append(responseAttrs, semconv.HTTPResponseBodySizeKey.Int64(responseSize))...)
		} else {
			span.SetAttributes(responseAttrs...)
		}
		span.SetName(cfg.SpanNameFormatter(c))

		spanStatus, spanMessage := internal.SpanStatusFromHTTPStatusCodeAndSpanKind(c.Response().StatusCode(), oteltrace.SpanKindServer)
		span.SetStatus(spanStatus, spanMessage)

		//Propagate tracing context as headers in outbound response
		tracingHeaders := make(propagation.HeaderCarrier)
		cfg.Propagators.Inject(c.Context(), tracingHeaders)
		for _, headerKey := range tracingHeaders.Keys() {
			c.Set(headerKey, tracingHeaders.Get(headerKey))
		}

		return nil
	}
}

// defaultSpanNameFormatter is the default formatter for spans created with the fiber
// integration. Returns the route pathRaw
func defaultSpanNameFormatter(ctx fiber.Ctx) string {
	return ctx.Route().Path
}
