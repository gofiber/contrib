package otelfiber

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	otelcontrib "go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/global"
	"go.opentelemetry.io/otel/metric/instrument"
	"go.opentelemetry.io/otel/metric/unit"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerKey           = "gofiber-contrib-tracer-fiber"
	instrumentationName = "github.com/gofiber/contrib/otelfiber"
	defaultServiceName  = "fiber-server"

	metricNameHttpServerDuration       = "http.server.duration"
	metricNameHttpServerRequestSize    = "http.server.request.size"
	metricNameHttpServerResponseSize   = "http.server.response.size"
	metricNameHttpServerActiveRequests = "http.server.active_requests"
)

// Middleware returns fiber handler which will trace incoming requests.
func Middleware(service string, opts ...Option) fiber.Handler {
	if service == "" {
		service = defaultServiceName
	}
	cfg := config{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		instrumentationName,
		oteltrace.WithInstrumentationVersion(otelcontrib.SemVersion()),
	)
	if cfg.MeterProvider == nil {
		cfg.MeterProvider = global.MeterProvider()
	}
	meter := cfg.MeterProvider.Meter(
		instrumentationName,
		metric.WithInstrumentationVersion(otelcontrib.SemVersion()),
	)
	httpServerDuration, err := meter.SyncFloat64().Histogram(metricNameHttpServerDuration, instrument.WithUnit(unit.Milliseconds), instrument.WithDescription("measures the duration inbound HTTP requests"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerRequestSize, err := meter.SyncInt64().Histogram(metricNameHttpServerRequestSize, instrument.WithUnit(unit.Bytes), instrument.WithDescription("measures the size of HTTP request messages"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerResponseSize, err := meter.SyncInt64().Histogram(metricNameHttpServerResponseSize, instrument.WithUnit(unit.Bytes), instrument.WithDescription("measures the size of HTTP response messages"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerActiveRequests, err := meter.SyncInt64().UpDownCounter(metricNameHttpServerActiveRequests, instrument.WithUnit(unit.Dimensionless), instrument.WithDescription("measures the number of concurrent HTTP requests that are currently in-flight"))
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
		c.Locals(tracerKey, tracer)
		savedCtx, cancel := context.WithCancel(c.UserContext())

		start := time.Now()

		requestMetricsAttrs := httpServerMetricAttributesFromRequest(c, service)
		httpServerActiveRequests.Add(savedCtx, 1, requestMetricsAttrs...)

		responseMetricAttrs := make([]attribute.KeyValue, len(requestMetricsAttrs))
		copy(responseMetricAttrs, requestMetricsAttrs)

		reqHeader := make(http.Header)
		c.Request().Header.VisitAll(func(k, v []byte) {
			reqHeader.Add(string(k), string(v))
		})

		ctx := cfg.Propagators.Extract(savedCtx, propagation.HeaderCarrier(reqHeader))
		opts := []oteltrace.SpanStartOption{
			oteltrace.WithAttributes(
				// utils.CopyString: we need to copy the string as fasthttp strings are by default
				// mutable so it will be unsafe to use in this middleware as it might be used after
				// the handler returns.
				semconv.HTTPServerNameKey.String(service),
				semconv.HTTPMethodKey.String(utils.CopyString(c.Method())),
				semconv.HTTPTargetKey.String(string(utils.CopyBytes(c.Request().RequestURI()))),
				semconv.HTTPURLKey.String(utils.CopyString(c.OriginalURL())),
				semconv.NetHostIPKey.String(utils.CopyString(c.IP())),
				semconv.NetHostNameKey.String(utils.CopyString(c.Hostname())),
				semconv.HTTPUserAgentKey.String(string(utils.CopyBytes(c.Request().Header.UserAgent()))),
				semconv.HTTPRequestContentLengthKey.Int(c.Request().Header.ContentLength()),
				semconv.HTTPSchemeKey.String(utils.CopyString(c.Protocol())),
				semconv.NetTransportTCP),
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		}
		if username, ok := hasBasicAuth(c.Get(fiber.HeaderAuthorization)); ok {
			opts = append(opts, oteltrace.WithAttributes(semconv.EnduserIDKey.String(utils.CopyString(username))))
		}
		if len(c.IPs()) > 0 {
			opts = append(opts, oteltrace.WithAttributes(semconv.HTTPClientIPKey.String(utils.CopyString(c.IPs()[0]))))
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

		responseAttrs := append(
			semconv.HTTPAttributesFromHTTPStatusCode(c.Response().StatusCode()),
			semconv.HTTPRouteKey.String(c.Route().Path), // no need to copy c.Route().Path: route strings should be immutable across app lifecycle
		)

		responseMetricAttrs = append(
			responseMetricAttrs,
			responseAttrs...)
		requestSize := int64(len(c.Request().Body()))
		responseSize := int64(len(c.Response().Body()))

		defer func() {
			httpServerDuration.Record(savedCtx, float64(time.Since(start).Microseconds())/1000, responseMetricAttrs...)
			httpServerRequestSize.Record(savedCtx, requestSize, responseMetricAttrs...)
			httpServerResponseSize.Record(savedCtx, responseSize, responseMetricAttrs...)
			httpServerActiveRequests.Add(savedCtx, -1, requestMetricsAttrs...)
			c.SetUserContext(savedCtx)
			cancel()
		}()

		span.SetAttributes(responseAttrs...)
		span.SetName(cfg.SpanNameFormatter(c))

		spanStatus, spanMessage := semconv.SpanStatusFromHTTPStatusCodeAndSpanKind(c.Response().StatusCode(), oteltrace.SpanKindServer)
		span.SetStatus(spanStatus, spanMessage)

		return nil
	}
}

// defaultSpanNameFormatter is the default formatter for spans created with the fiber
// integration. Returns the route pathRaw
func defaultSpanNameFormatter(ctx *fiber.Ctx) string {
	return ctx.Route().Path
}

func hasBasicAuth(auth string) (string, bool) {
	if auth == "" {
		return "", false
	}

	// Check if the Authorization header is Basic
	if !strings.HasPrefix(auth, "Basic ") {
		return "", false
	}

	// Decode the header contents
	raw, err := base64.StdEncoding.DecodeString(auth[6:])
	if err != nil {
		return "", false
	}

	// Get the credentials
	creds := utils.UnsafeString(raw)

	// Check if the credentials are in the correct form
	// which is "username:password".
	index := strings.Index(creds, ":")
	if index == -1 {
		return "", false
	}

	// Get the username
	return creds[:index], true
}
