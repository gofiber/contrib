package otelfiber_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/contrib/otelfiber/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelcontrib "go.opentelemetry.io/contrib"
	b3prop "go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/metric/metricdata/metricdatatest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/gofiber/contrib/otelfiber"

func TestChildSpanFromGlobalTracer(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	app := fiber.New()
	app.Use(otelfiber.Middleware())
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/user/123", nil))

	spans := sr.Ended()
	require.Len(t, spans, 1)
}

func TestChildSpanFromCustomTracer(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	app := fiber.New()
	app.Use(otelfiber.Middleware(otelfiber.WithTracerProvider(provider)))
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/user/123", nil))

	spans := sr.Ended()
	require.Len(t, spans, 1)
}

func TestSkipWithNext(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	app := fiber.New()
	app.Use(otelfiber.Middleware(otelfiber.WithNext(func(c *fiber.Ctx) bool {
		return c.Path() == "/health"
	})))

	app.Get("/health", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/health", nil))

	spans := sr.Ended()
	require.Len(t, spans, 0)
}

func TestTrace200(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)
	serviceName := "foobar"

	app := fiber.New()
	app.Use(
		otelfiber.Middleware(
			otelfiber.WithTracerProvider(provider),
			otelfiber.WithServiceName(serviceName),
		),
	)
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		id := ctx.Params("id")
		return ctx.SendString(id)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/user/123?query=123", nil), 3000)

	// do and verify the request
	require.Equal(t, http.StatusOK, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)

	// verify traces look good
	span := spans[0]
	attr := span.Attributes()

	assert.Equal(t, "/user/:id", span.Name())
	assert.Equal(t, oteltrace.SpanKindServer, span.SpanKind())
	assert.Contains(t, attr, attribute.String("service.name", serviceName))
	assert.Contains(t, attr, attribute.Int("http.response.status_code", http.StatusOK))
	assert.Contains(t, attr, attribute.String("http.request.method", "GET"))
	assert.Contains(t, attr, attribute.String("url.original", "/user/123?query=123"))
	assert.Contains(t, attr, attribute.String("url.path", "/user/123"))
	assert.Contains(t, attr, attribute.String("url.query", "query=123"))
	assert.Contains(t, attr, attribute.String("http.route", "/user/:id"))
}

func TestError(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	// setup
	app := fiber.New()
	app.Use(otelfiber.Middleware(otelfiber.WithTracerProvider(provider)))
	// configure a handler that returns an error and 5xx status code
	app.Get("/server_err", func(ctx *fiber.Ctx) error {
		return errors.New("oh no")
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/server_err", nil))
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// verify the errors and status are correct
	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	attr := span.Attributes()

	assert.Equal(t, "/server_err", span.Name())
	assert.Contains(t, attr, attribute.Int("http.response.status_code", http.StatusInternalServerError))
	assert.Equal(t, attribute.StringValue("oh no"), span.Events()[0].Attributes[1].Value)
	// server errors set the status
	assert.Equal(t, codes.Error, span.Status().Code)
}

func TestErrorOnlyHandledOnce(t *testing.T) {
	timesHandlingError := 0
	app := fiber.New(fiber.Config{
		ErrorHandler: func(ctx *fiber.Ctx, err error) error {
			timesHandlingError++
			return fiber.NewError(http.StatusInternalServerError, err.Error())
		},
	})
	app.Use(otelfiber.Middleware())
	app.Get("/", func(ctx *fiber.Ctx) error {
		return errors.New("mock error")
	})
	_, _ = app.Test(httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, 1, timesHandlingError)
}

func TestGetSpanNotInstrumented(t *testing.T) {
	var gotSpan oteltrace.Span

	app := fiber.New()
	app.Get("/ping", func(ctx *fiber.Ctx) error {
		// Assert we don't have a span on the context.
		gotSpan = oteltrace.SpanFromContext(ctx.UserContext())
		return ctx.SendString("ok")
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/ping", nil))
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	ok := !gotSpan.SpanContext().IsValid()
	assert.True(t, ok)
}

func TestPropagationWithGlobalPropagators(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())

	r := httptest.NewRequest("GET", "/user/123", nil)

	ctx, pspan := provider.Tracer(instrumentationName).Start(context.Background(), "test")
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(r.Header))

	app := fiber.New()
	app.Use(otelfiber.Middleware(otelfiber.WithTracerProvider(provider)))
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(r)

	spans := sr.Ended()
	require.Len(t, spans, 1)

	// verify traces look good
	span := spans[0]
	assert.Equal(t, pspan.SpanContext().TraceID(), span.SpanContext().TraceID())
	assert.Equal(t, pspan.SpanContext().SpanID(), span.Parent().SpanID())
}

func TestPropagationWithCustomPropagators(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	b3 := b3prop.New()

	r := httptest.NewRequest("GET", "/user/123", nil)

	ctx, pspan := provider.Tracer(instrumentationName).Start(context.Background(), "test")
	b3.Inject(ctx, propagation.HeaderCarrier(r.Header))

	app := fiber.New()
	app.Use(otelfiber.Middleware(otelfiber.WithTracerProvider(provider), otelfiber.WithPropagators(b3)))
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(r)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	mspan := spans[0]
	assert.Equal(t, pspan.SpanContext().TraceID(), mspan.SpanContext().TraceID())
	assert.Equal(t, pspan.SpanContext().SpanID(), mspan.Parent().SpanID())
}

func TestHasBasicAuth(t *testing.T) {
	testCases := []struct {
		desc  string
		auth  string
		user  string
		valid bool
	}{
		{
			desc:  "valid header",
			auth:  "Basic dXNlcjpwYXNzd29yZA==",
			user:  "user",
			valid: true,
		},
		{
			desc: "invalid header",
			auth: "Bas",
		},
		{
			desc: "invalid basic header",
			auth: "Basic 12345",
		},
		{
			desc: "no header",
		},
	}

	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			val, valid := otelfiber.HasBasicAuth(tC.auth)

			assert.Equal(t, tC.user, val)
			assert.Equal(t, tC.valid, valid)
		})
	}
}

func TestMetric(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	serviceName := "foobar"
	port := 8080
	route := "/foo"

	app := fiber.New()
	app.Use(
		otelfiber.Middleware(
			otelfiber.WithMeterProvider(provider),
			otelfiber.WithPort(port),
			otelfiber.WithServiceName(serviceName),
		),
	)
	app.Get(route, func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, route, nil)
	_, _ = app.Test(r)

	metrics := metricdata.ResourceMetrics{}
	err := reader.Collect(context.Background(), &metrics)
	assert.NoError(t, err)
	assert.Len(t, metrics.ScopeMetrics, 1)

	requestAttrs := []attribute.KeyValue{
		semconv.NetworkProtocolVersion(fmt.Sprintf("1.%d", r.ProtoMinor)),
		semconv.HTTPRequestMethodKey.String(http.MethodGet),
		semconv.URLScheme("http"),
		semconv.ServerAddress(r.Host),
		semconv.ServerPort(port),
		semconv.ServiceName(serviceName),
	}
	responseAttrs := []attribute.KeyValue{
		semconv.HTTPResponseStatusCode(200),
		semconv.HTTPRouteKey.String(route),
	}

	assertScopeMetrics(t, metrics.ScopeMetrics[0], route, requestAttrs, append(requestAttrs, responseAttrs...))
}

func assertScopeMetrics(t *testing.T, sm metricdata.ScopeMetrics, route string, requestAttrs []attribute.KeyValue, responseAttrs []attribute.KeyValue) {
	assert.Equal(t, instrumentation.Scope{
		Name:    instrumentationName,
		Version: otelcontrib.Version(),
	}, sm.Scope)

	// Duration value is not predictable.
	m := sm.Metrics[0]
	assert.Equal(t, semconv.HTTPServerRequestDurationName, m.Name)
	require.IsType(t, m.Data, metricdata.Histogram[float64]{})
	hist := m.Data.(metricdata.Histogram[float64])
	assert.Equal(t, metricdata.CumulativeTemporality, hist.Temporality)
	require.Len(t, hist.DataPoints, 1)
	dp := hist.DataPoints[0]
	assert.Equal(t, attribute.NewSet(responseAttrs...), dp.Attributes, "attributes")
	assert.Equal(t, uint64(1), dp.Count, "count")
	assert.Less(t, dp.Sum, float64(10)) // test shouldn't take longer than 10 milliseconds

	// Request size
	want := metricdata.Metrics{
		Name:        semconv.HTTPServerRequestBodySizeName,
		Description: semconv.HTTPServerRequestBodySizeDescription,
		Unit:        semconv.HTTPServerRequestBodySizeUnit,
		Data:        getHistogram(0, responseAttrs),
	}
	metricdatatest.AssertEqual(t, want, sm.Metrics[1], metricdatatest.IgnoreTimestamp())

	// Response size
	want = metricdata.Metrics{
		Name:        semconv.HTTPServerResponseBodySizeName,
		Description: semconv.HTTPServerResponseBodySizeDescription,
		Unit:        semconv.HTTPServerResponseBodySizeUnit,
		Data:        getHistogram(2, responseAttrs),
	}
	metricdatatest.AssertEqual(t, want, sm.Metrics[2], metricdatatest.IgnoreTimestamp())

	// Active requests
	want = metricdata.Metrics{
		Name:        semconv.HTTPServerActiveRequestsName,
		Description: semconv.HTTPServerActiveRequestsDescription,
		Unit:        semconv.HTTPServerActiveRequestsUnit,
		Data: metricdata.Sum[int64]{
			DataPoints: []metricdata.DataPoint[int64]{
				{Attributes: attribute.NewSet(requestAttrs...), Value: 0},
			},
			Temporality: metricdata.CumulativeTemporality,
		},
	}
	metricdatatest.AssertEqual(t, want, sm.Metrics[3], metricdatatest.IgnoreTimestamp())
}

func getHistogram(value float64, attrs []attribute.KeyValue) metricdata.Histogram[int64] {
	bounds := []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}
	bucketCounts := make([]uint64, len(bounds)+1)

	for i, v := range bounds {
		if value <= v {
			bucketCounts[i]++
			break
		}

		if i == len(bounds)-1 {
			bounds[i+1]++
			break
		}
	}

	extremaValue := metricdata.NewExtrema[int64](int64(value))

	return metricdata.Histogram[int64]{
		DataPoints: []metricdata.HistogramDataPoint[int64]{
			{
				Attributes:   attribute.NewSet(attrs...),
				Bounds:       bounds,
				BucketCounts: bucketCounts,
				Count:        1,
				Min:          extremaValue,
				Max:          extremaValue,
				Sum:          int64(value),
			},
		},
		Temporality: metricdata.CumulativeTemporality,
	}
}

func TestCustomAttributes(t *testing.T) {
	sr := new(tracetest.SpanRecorder)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	app := fiber.New()
	app.Use(
		otelfiber.Middleware(
			otelfiber.WithTracerProvider(provider),
			otelfiber.WithCustomAttributes(func(ctx *fiber.Ctx) []attribute.KeyValue {
				return []attribute.KeyValue{
					attribute.Key("http.query_params").String(ctx.Request().URI().QueryArgs().String()),
				}
			}),
		),
	)

	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		id := ctx.Params("id")
		return ctx.SendString(id)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/user/123?foo=bar", nil), 3000)

	// do and verify the request
	require.Equal(t, http.StatusOK, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)

	// verify traces look good
	span := spans[0]
	attr := span.Attributes()

	assert.Equal(t, "/user/:id", span.Name())
	assert.Equal(t, oteltrace.SpanKindServer, span.SpanKind())
	assert.Contains(t, attr, attribute.Int("http.response.status_code", http.StatusOK))
	assert.Contains(t, attr, attribute.String("http.request.method", "GET"))
	assert.Contains(t, attr, attribute.String("url.original", "/user/123?foo=bar"))
	assert.Contains(t, attr, attribute.String("url.path", "/user/123"))
	assert.Contains(t, attr, attribute.String("url.query", "foo=bar"))
	assert.Contains(t, attr, attribute.String("http.route", "/user/:id"))
}

func TestCustomMetricAttributes(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	serviceName := "foobar"
	port := 8080
	route := "/foo"

	app := fiber.New()
	app.Use(
		otelfiber.Middleware(
			otelfiber.WithMeterProvider(provider),
			otelfiber.WithPort(port),
			otelfiber.WithServiceName(serviceName),
			otelfiber.WithCustomMetricAttributes(func(ctx *fiber.Ctx) []attribute.KeyValue {
				return []attribute.KeyValue{
					attribute.Key("http.query_params").String(ctx.Request().URI().QueryArgs().String()),
				}
			}),
		),
	)

	app.Get(route, func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/foo?foo=bar", nil)
	resp, _ := app.Test(r)

	// do and verify the request
	require.Equal(t, http.StatusOK, resp.StatusCode)

	metrics := metricdata.ResourceMetrics{}
	err := reader.Collect(context.Background(), &metrics)
	assert.NoError(t, err)
	assert.Len(t, metrics.ScopeMetrics, 1)

	requestAttrs := []attribute.KeyValue{
		semconv.NetworkProtocolVersion(fmt.Sprintf("1.%d", r.ProtoMinor)),
		semconv.HTTPRequestMethodKey.String(http.MethodGet),
		semconv.URLScheme("http"),
		semconv.ServerAddress(r.Host),
		semconv.ServerPort(port),
		semconv.ServiceName(serviceName),
		attribute.String("http.query_params", "foo=bar"),
	}
	responseAttrs := []attribute.KeyValue{
		semconv.HTTPResponseStatusCode(200),
		semconv.HTTPRouteKey.String(route),
	}

	assertScopeMetrics(t, metrics.ScopeMetrics[0], route, requestAttrs, append(requestAttrs, responseAttrs...))
}

func TestOutboundTracingPropagation(t *testing.T) {
	sr := new(tracetest.SpanRecorder)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	app := fiber.New()
	app.Use(otelfiber.Middleware(
		otelfiber.WithTracerProvider(provider),
		otelfiber.WithPropagators(b3prop.New(b3prop.WithInjectEncoding(b3prop.B3MultipleHeader))),
	))
	app.Get("/foo", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/foo", nil), 3000)

	assert.Equal(t, "1", resp.Header.Get("X-B3-Sampled"))
	assert.NotEmpty(t, resp.Header.Get("X-B3-SpanId"))
	assert.NotEmpty(t, resp.Header.Get("X-B3-TraceId"))

}

func TestOutboundTracingPropagationWithInboundContext(t *testing.T) {
	const spanId = "619907d88b766fb8"
	const traceId = "813dd2766ff711bf02b60e9883014964"

	sr := new(tracetest.SpanRecorder)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	app := fiber.New()
	app.Use(otelfiber.Middleware(
		otelfiber.WithTracerProvider(provider),
		otelfiber.WithPropagators(b3prop.New(b3prop.WithInjectEncoding(b3prop.B3MultipleHeader))),
	))
	app.Get("/foo", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	req := httptest.NewRequest("GET", "/foo", nil)

	req.Header.Set("X-B3-SpanId", spanId)
	req.Header.Set("X-B3-TraceId", traceId)
	req.Header.Set("X-B3-Sampled", "1")

	resp, _ := app.Test(req, 3000)

	assert.NotEmpty(t, resp.Header.Get("X-B3-SpanId"))
	assert.Equal(t, traceId, resp.Header.Get("X-B3-TraceId"))
	assert.Equal(t, "1", resp.Header.Get("X-B3-Sampled"))
}

func TestCollectClientIP(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{true, false} {
		enabled := enabled
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			t.Parallel()

			sr := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
			otel.SetTracerProvider(provider)

			app := fiber.New()
			app.Use(otelfiber.Middleware(
				otelfiber.WithTracerProvider(provider),
				otelfiber.WithCollectClientIP(enabled),
			))
			app.Get("/foo", func(ctx *fiber.Ctx) error {
				return ctx.SendStatus(http.StatusNoContent)
			})

			req := httptest.NewRequest("GET", "/foo", nil)
			_, _ = app.Test(req)

			spans := sr.Ended()
			require.Len(t, spans, 1)

			span := spans[0]
			attrs := span.Attributes()
			if enabled {
				assert.Contains(t, attrs, attribute.String("client.address", "0.0.0.0"))
			} else {
				assert.NotContains(t, attrs, attribute.String("client.address", "0.0.0.0"))
			}
		})
	}
}
