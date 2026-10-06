package otel_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fiberotel "github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/utils/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelcontrib "go.opentelemetry.io/contrib"
	b3prop "go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/metric/metricdata/metricdatatest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/gofiber/contrib/v3/otel"

func TestChildSpanFromGlobalTracer(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	app := fiber.New()
	app.Use(fiberotel.New())
	app.Get("/user/:id", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/user/123", nil))
	require.NoError(t, err)
	require.NotNil(t, resp)

	spans := sr.Ended()
	require.Len(t, spans, 1)
}

func TestDeprecatedMiddleware(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	app := fiber.New()
	app.Use(fiberotel.Middleware(fiberotel.WithTracerProvider(provider))) //nolint:staticcheck // the deprecated alias must keep working
	app.Get("/user/:id", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/user/123", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, "GET /user/:id", spans[0].Name())
}

func TestChildSpanFromCustomTracer(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	app := fiber.New()
	app.Use(fiberotel.New(fiberotel.WithTracerProvider(provider)))
	app.Get("/user/:id", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/user/123", nil))
	require.NoError(t, err)
	require.NotNil(t, resp)

	spans := sr.Ended()
	require.Len(t, spans, 1)
}

func TestSkipWithNext(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	app := fiber.New()
	app.Use(fiberotel.New(fiberotel.WithNext(func(c fiber.Ctx) bool {
		return c.Path() == "/health"
	})))

	app.Get("/health", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	require.NoError(t, err)
	require.NotNil(t, resp)

	spans := sr.Ended()
	require.Len(t, spans, 0)
}

func TestTrace200(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	app := fiber.New()
	app.Use(
		fiberotel.New(fiberotel.WithTracerProvider(provider)),
	)
	app.Get("/user/:id", func(ctx fiber.Ctx) error {
		id := ctx.Params("id")
		return ctx.SendString(id)
	})

	r := httptest.NewRequest("GET", "/user/123", nil)
	resp, err := app.Test(r, fiber.TestConfig{Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// do and verify the request
	require.Equal(t, http.StatusOK, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)

	// verify traces look good
	span := spans[0]
	attr := span.Attributes()

	assert.Equal(t, "GET /user/:id", span.Name())
	assert.Equal(t, oteltrace.SpanKindServer, span.SpanKind())
	assert.Equal(t, instrumentation.Scope{
		Name:      instrumentationName,
		Version:   otelcontrib.Version(),
		SchemaURL: semconv.SchemaURL,
	}, span.InstrumentationScope())
	assert.Contains(t, attr, attribute.String("server.address", r.Host))
	assert.Contains(t, attr, attribute.Int("http.response.status_code", http.StatusOK))
	assert.Contains(t, attr, attribute.String("http.request.method", "GET"))
	assert.Contains(t, attr, attribute.String("url.path", "/user/123"))
	assert.Contains(t, attr, attribute.String("http.route", "/user/:id"))
}

// Reporting the Use mount path as http.route would file every 404 under "/".
func TestRouteOnlyForMatchedRequests(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	reader := metric.NewManualReader()

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
	))
	app.Use("/public", func(c fiber.Ctx) error {
		return c.SendString("asset")
	})
	api := app.Group("/api")
	api.Get("/users/:id", func(c fiber.Ctx) error {
		return c.SendString("user")
	})

	testCases := []struct {
		method   string
		path     string
		status   int
		spanName string
		route    string
	}{
		{method: http.MethodGet, path: "/api/users/42", status: http.StatusOK, spanName: "GET /api/users/:id", route: "/api/users/:id"},
		{method: http.MethodGet, path: "/missing", status: http.StatusNotFound, spanName: "GET"},
		{method: http.MethodPost, path: "/api/users/42", status: http.StatusMethodNotAllowed, spanName: "POST"},
		{method: http.MethodGet, path: "/public/app.js", status: http.StatusOK, spanName: "GET"},
	}

	for _, tc := range testCases {
		resp, err := app.Test(httptest.NewRequest(tc.method, tc.path, nil))
		require.NoError(t, err)
		require.Equal(t, tc.status, resp.StatusCode, tc.path)
	}

	spans := sr.Ended()
	require.Len(t, spans, len(testCases))
	for i, tc := range testCases {
		assert.Equal(t, tc.spanName, spans[i].Name(), tc.path)

		attrs := attribute.NewSet(spans[i].Attributes()...)
		route, ok := attrs.Value(semconv.HTTPRouteKey)
		if tc.route == "" {
			assert.False(t, ok, "%s %s has no route, got %q", tc.method, tc.path, route.AsString())
			continue
		}
		require.True(t, ok, tc.path)
		assert.Equal(t, tc.route, route.AsString())
	}

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	for _, set := range metricPoints(t, metrics.ScopeMetrics[0])[fiberotel.MetricNameHTTPServerRequestDuration] {
		route, ok := set.Value(semconv.HTTPRouteKey)
		if !ok {
			continue
		}
		assert.Equal(t, "/api/users/:id", route.AsString())
	}
}

// Only values the request carries are recorded, and a server records no url.full.
func TestRequestAttributes(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
	))
	app.All("/files/:name", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusNoContent)
	})

	download := httptest.NewRequest(http.MethodGet, "/files/report?X-Amz-Signature=c2VjcmV0&download=1", nil)
	// net/http fills in its own User-Agent unless the header is present.
	download.Header["User-Agent"] = []string{""}
	upload := httptest.NewRequest(http.MethodPost, "/files/report", strings.NewReader("hello"))
	upload.Header.Set("User-Agent", "uploader/1.0")

	for _, r := range []*http.Request{download, upload} {
		resp, err := app.Test(r)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	}

	spans := sr.Ended()
	require.Len(t, spans, 2)

	downloadAttrs := attribute.NewSet(spans[0].Attributes()...)
	query, ok := downloadAttrs.Value(semconv.URLQueryKey)
	require.True(t, ok)
	assert.Equal(t, "X-Amz-Signature=REDACTED&download=1", query.AsString())
	assert.Contains(t, spans[0].Attributes(), semconv.URLPath("/files/report"))
	assert.Contains(t, spans[0].Attributes(), semconv.HTTPRequestBodySize(0))
	assert.False(t, downloadAttrs.HasValue(semconv.UserAgentOriginalKey))
	assert.False(t, downloadAttrs.HasValue(semconv.URLFullKey))

	uploadAttrs := attribute.NewSet(spans[1].Attributes()...)
	assert.False(t, uploadAttrs.HasValue(semconv.URLQueryKey))
	assert.Contains(t, spans[1].Attributes(), semconv.UserAgentOriginal("uploader/1.0"))
	assert.Contains(t, spans[1].Attributes(), semconv.HTTPRequestBodySize(len("hello")))
	assert.False(t, uploadAttrs.HasValue(semconv.URLFullKey))
}

// X-Forwarded-Proto is honored only from a trusted proxy.
func TestSchemeFromTrustedProxy(t *testing.T) {
	t.Parallel()

	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%t", trusted), func(t *testing.T) {
			t.Parallel()

			sr := tracetest.NewSpanRecorder()
			reader := metric.NewManualReader()

			config := fiber.Config{}
			want := "http"
			if trusted {
				// app.Test connects from 0.0.0.0.
				config = fiber.Config{TrustProxy: true, TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0"}}}
				want = "https"
			}

			app := fiber.New(config)
			app.Use(fiberotel.New(
				fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				fiberotel.WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
			))
			app.Get("/", func(c fiber.Ctx) error {
				return c.SendStatus(http.StatusOK)
			})

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("X-Forwarded-Proto", "https")
			resp, err := app.Test(r)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)

			spans := sr.Ended()
			require.Len(t, spans, 1)
			assert.Contains(t, spans[0].Attributes(), semconv.URLScheme(want))

			var metrics metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(context.Background(), &metrics))
			require.Len(t, metrics.ScopeMetrics, 1)
			for name, sets := range metricPoints(t, metrics.ScopeMetrics[0]) {
				require.Len(t, sets, 1, name)
				scheme, ok := sets[0].Value(semconv.URLSchemeKey)
				require.True(t, ok, name)
				assert.Equal(t, want, scheme.AsString(), name)
			}
		})
	}
}

func TestError(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	// setup
	app := fiber.New()
	app.Use(fiberotel.New(fiberotel.WithTracerProvider(provider)))
	// configure a handler that returns an error and 5xx status code
	app.Get("/server_err", func(ctx fiber.Ctx) error {
		return errors.New("oh no")
	})
	resp, err := app.Test(httptest.NewRequest("GET", "/server_err", nil))
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// verify the errors and status are correct
	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	attr := span.Attributes()

	assert.Equal(t, "GET /server_err", span.Name())
	assert.Contains(t, attr, attribute.Int("http.response.status_code", http.StatusInternalServerError))
	assert.Contains(t, attr, attribute.String("error.type", "500"))
	assert.Equal(t, attribute.StringValue("oh no"), span.Events()[0].Attributes[1].Value)
	// server errors set the status
	assert.Equal(t, codes.Error, span.Status().Code)
}

// Codes without a reason phrase are valid: 4xx stays unset on server spans, 5xx is an error.
func TestUnnamedStatusCodes(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
	))
	app.Get("/:status", func(c fiber.Ctx) error {
		status := fiber.Params[int](c, "status")
		return c.SendStatus(status)
	})

	for _, status := range []int{299, 499, 520} {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, fmt.Sprintf("/%d", status), nil))
		require.NoError(t, err)
		require.Equal(t, status, resp.StatusCode)
	}

	spans := sr.Ended()
	require.Len(t, spans, 3)

	for _, span := range spans[:2] {
		assert.Equal(t, codes.Unset, span.Status().Code)
		assert.Empty(t, span.Status().Description)
		attrs := attribute.NewSet(span.Attributes()...)
		assert.False(t, attrs.HasValue(semconv.ErrorTypeKey))
	}

	assert.Equal(t, codes.Error, spans[2].Status().Code)
	assert.Empty(t, spans[2].Status().Description)
	assert.Contains(t, spans[2].Attributes(), semconv.ErrorTypeKey.String("520"))
}

func TestErrorOnlyHandledOnce(t *testing.T) {
	timesHandlingError := 0
	app := fiber.New(fiber.Config{
		ErrorHandler: func(ctx fiber.Ctx, err error) error {
			timesHandlingError++
			return fiber.NewError(http.StatusInternalServerError, err.Error())
		},
	})
	app.Use(fiberotel.New())
	app.Get("/", func(ctx fiber.Ctx) error {
		return errors.New("mock error")
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, 1, timesHandlingError)
}

// The middleware must run the error handler Fiber would pick for a mounted app.
func TestErrorUsesMountedAppErrorHandler(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	reader := metric.NewManualReader()

	sub := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			return c.Status(http.StatusTeapot).SendString("handled by sub-app")
		},
	})
	sub.Get("/fail", func(fiber.Ctx) error {
		return errors.New("boom")
	})

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
	))
	app.Use("/sub", sub)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/sub/fail", nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusTeapot, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "handled by sub-app", string(body))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.HTTPResponseStatusCode(http.StatusTeapot))

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	for name, sets := range metricPoints(t, metrics.ScopeMetrics[0]) {
		if name == fiberotel.MetricNameHTTPServerActiveRequests {
			continue
		}
		require.Len(t, sets, 1, name)
		status, ok := sets[0].Value(semconv.HTTPResponseStatusCodeKey)
		require.True(t, ok, name)
		assert.Equal(t, int64(http.StatusTeapot), status.AsInt64(), name)
	}
}

func TestErrorHandlerFailureAnswers500(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()

	app := fiber.New(fiber.Config{
		ErrorHandler: func(_ fiber.Ctx, err error) error {
			return err
		},
	})
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
	))
	app.Get("/", func(fiber.Ctx) error {
		return fiber.ErrTeapot
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.HTTPResponseStatusCode(http.StatusInternalServerError))
	assert.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestGetSpanNotInstrumented(t *testing.T) {
	var gotSpan oteltrace.Span

	app := fiber.New()
	app.Get("/ping", func(ctx fiber.Ctx) error {
		// Assert we don't have a span on the context.
		gotSpan = oteltrace.SpanFromContext(ctx)
		return ctx.SendString("ok")
	})
	resp, err := app.Test(httptest.NewRequest("GET", "/ping", nil))
	require.NoError(t, err)
	require.NotNil(t, resp)
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
	app.Use(fiberotel.New(fiberotel.WithTracerProvider(provider)))
	app.Get("/user/:id", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(r)
	require.NoError(t, err)
	require.NotNil(t, resp)

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
	app.Use(fiberotel.New(fiberotel.WithTracerProvider(provider), fiberotel.WithPropagators(b3)))
	app.Get("/user/:id", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(r)
	require.NoError(t, err)
	require.NotNil(t, resp)

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
			val, valid := fiberotel.HasBasicAuth(tC.auth)

			assert.Equal(t, tC.user, val)
			assert.Equal(t, tC.valid, valid)
		})
	}
}

func TestMetric(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	port := 8080
	route := "/foo"

	app := fiber.New()
	app.Use(
		fiberotel.New(
			fiberotel.WithMeterProvider(provider),
			fiberotel.WithPort(port),
		),
	)
	app.Get(route, func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, route, nil)
	resp, err := app.Test(r)
	require.NoError(t, err)
	require.NotNil(t, resp)

	metrics := metricdata.ResourceMetrics{}
	err = reader.Collect(context.Background(), &metrics)
	assert.NoError(t, err)
	assert.Len(t, metrics.ScopeMetrics, 1)

	requestAttrs := []attribute.KeyValue{
		semconv.URLScheme("http"),
		semconv.HTTPRequestMethodKey.String(http.MethodGet),
		semconv.ServerPort(port),
	}
	responseAttrs := []attribute.KeyValue{
		semconv.NetworkProtocolName("http"),
		semconv.NetworkProtocolVersion(fmt.Sprintf("1.%d", r.ProtoMinor)),
		semconv.HTTPResponseStatusCode(200),
		semconv.HTTPRouteKey.String(route),
	}

	assertScopeMetrics(t, metrics.ScopeMetrics[0], route, requestAttrs, append(requestAttrs, responseAttrs...))
}

func TestRequestBodySizeUsesContentLength(t *testing.T) {
	const contentLength = 1024

	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		// Simulate a pre-parsed body whose in-memory representation differs from
		// the size declared by the request, as can happen for multipart forms.
		c.Request().Header.SetContentLength(contentLength)
		return c.Next()
	})
	app.Use(fiberotel.New(fiberotel.WithMeterProvider(provider)))
	app.Post("/upload", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/upload", bytes.NewBufferString("body")))
	require.NoError(t, err)
	require.NotNil(t, resp)

	metrics := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)

	for _, m := range metrics.ScopeMetrics[0].Metrics {
		if m.Name != fiberotel.MetricNameHTTPServerRequestBodySize {
			continue
		}

		histogram, ok := m.Data.(metricdata.Histogram[int64])
		require.True(t, ok)
		require.Len(t, histogram.DataPoints, 1)
		assert.Equal(t, int64(contentLength), histogram.DataPoints[0].Sum)
		return
	}

	t.Fatal("request body size metric not found")
}

func assertScopeMetrics(t *testing.T, sm metricdata.ScopeMetrics, route string, requestAttrs []attribute.KeyValue, responseAttrs []attribute.KeyValue) {
	assert.Equal(t, instrumentation.Scope{
		Name:      instrumentationName,
		Version:   otelcontrib.Version(),
		SchemaURL: semconv.SchemaURL,
	}, sm.Scope)

	// Duration value is not predictable.
	m := sm.Metrics[0]
	assert.Equal(t, fiberotel.MetricNameHTTPServerRequestDuration, m.Name)
	assert.Equal(t, fiberotel.UnitSeconds, m.Unit)
	require.IsType(t, m.Data, metricdata.Histogram[float64]{})
	hist := m.Data.(metricdata.Histogram[float64])
	assert.Equal(t, metricdata.CumulativeTemporality, hist.Temporality)
	require.Len(t, hist.DataPoints, 1)
	dp := hist.DataPoints[0]
	assert.Equal(t, attribute.NewSet(responseAttrs...), dp.Attributes, "attributes")
	// Buckets advised by the semantic conventions, in seconds.
	assert.Equal(t, []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}, dp.Bounds, "bounds")
	assert.Equal(t, uint64(1), dp.Count, "count")
	assert.Less(t, dp.Sum, 0.01) // test shouldn't take longer than 10 milliseconds (0.01 seconds)

	// Request size
	want := metricdata.Metrics{
		Name:        fiberotel.MetricNameHTTPServerRequestBodySize,
		Description: "Size of HTTP server request bodies.",
		Unit:        fiberotel.UnitBytes,
		Data:        getHistogram(0, responseAttrs),
	}
	metricdatatest.AssertEqual(t, want, sm.Metrics[1], metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars())

	// Response size
	want = metricdata.Metrics{
		Name:        fiberotel.MetricNameHTTPServerResponseBodySize,
		Description: "Size of HTTP server response bodies.",
		Unit:        fiberotel.UnitBytes,
		Data:        getHistogram(2, responseAttrs),
	}
	metricdatatest.AssertEqual(t, want, sm.Metrics[2], metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars())

	// Active requests
	want = metricdata.Metrics{
		Name:        fiberotel.MetricNameHTTPServerActiveRequests,
		Description: "Number of active HTTP server requests.",
		Unit:        fiberotel.UnitRequest,
		Data: metricdata.Sum[int64]{
			DataPoints: []metricdata.DataPoint[int64]{
				{Attributes: attribute.NewSet(requestAttrs...), Value: 0},
			},
			Temporality: metricdata.CumulativeTemporality,
		},
	}
	metricdatatest.AssertEqual(t, want, sm.Metrics[3], metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars())
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
		fiberotel.New(
			fiberotel.WithTracerProvider(provider),
			fiberotel.WithCustomAttributes(func(ctx fiber.Ctx) []attribute.KeyValue {
				return []attribute.KeyValue{
					attribute.Key("http.query_params").String(ctx.Request().URI().QueryArgs().String()),
				}
			}),
		),
	)

	app.Get("/user/:id", func(ctx fiber.Ctx) error {
		id := ctx.Params("id")
		return ctx.SendString(id)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/user/123?foo=bar", nil), fiber.TestConfig{Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// do and verify the request
	require.Equal(t, http.StatusOK, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)

	// verify traces look good
	span := spans[0]
	attr := span.Attributes()

	assert.Equal(t, "GET /user/:id", span.Name())
	assert.Equal(t, oteltrace.SpanKindServer, span.SpanKind())
	assert.Contains(t, attr, attribute.Int("http.response.status_code", http.StatusOK))
	assert.Contains(t, attr, attribute.String("http.request.method", "GET"))
	assert.Contains(t, attr, attribute.String("url.path", "/user/123"))
	assert.Contains(t, attr, attribute.String("http.route", "/user/:id"))
	assert.Contains(t, attr, semconv.URLQuery("foo=bar"))
}

func TestCustomMetricAttributes(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	port := 8080
	route := "/foo"

	app := fiber.New()
	app.Use(
		fiberotel.New(
			fiberotel.WithMeterProvider(provider),
			fiberotel.WithPort(port),
			fiberotel.WithCustomMetricAttributes(func(ctx fiber.Ctx) []attribute.KeyValue {
				return []attribute.KeyValue{semconv.URLQuery(ctx.Request().URI().QueryArgs().String())}
			}),
		),
	)

	app.Get(route, func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/foo?foo=bar", nil)
	resp, err := app.Test(r)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// do and verify the request
	require.Equal(t, http.StatusOK, resp.StatusCode)

	metrics := metricdata.ResourceMetrics{}
	err = reader.Collect(context.Background(), &metrics)
	assert.NoError(t, err)
	assert.Len(t, metrics.ScopeMetrics, 1)

	requestAttrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(http.MethodGet),
		semconv.URLSchemeKey.String("http"),
		semconv.ServerPort(port),
		semconv.URLQuery("foo=bar"),
	}
	responseAttrs := []attribute.KeyValue{
		semconv.NetworkProtocolName("http"),
		semconv.NetworkProtocolVersion(fmt.Sprintf("1.%d", r.ProtoMinor)),
		semconv.HTTPResponseStatusCode(200),
		semconv.HTTPRouteKey.String(route),
	}

	assertScopeMetrics(t, metrics.ScopeMetrics[0], route, requestAttrs, append(requestAttrs, responseAttrs...))
}

func metricPoints(t *testing.T, sm metricdata.ScopeMetrics) map[string][]attribute.Set {
	t.Helper()

	points := make(map[string][]attribute.Set)
	for _, m := range sm.Metrics {
		switch data := m.Data.(type) {
		case metricdata.Histogram[float64]:
			for _, point := range data.DataPoints {
				points[m.Name] = append(points[m.Name], point.Attributes)
			}
		case metricdata.Histogram[int64]:
			for _, point := range data.DataPoints {
				points[m.Name] = append(points[m.Name], point.Attributes)
			}
		case metricdata.Sum[int64]:
			for _, point := range data.DataPoints {
				points[m.Name] = append(points[m.Name], point.Attributes)
			}
		default:
			t.Fatalf("unexpected data %T for %s", m.Data, m.Name)
		}
	}

	return points
}

// server.address comes from the client-controlled Host header, so it is opt-in on metrics.
func TestMetricsLeaveOutServerAddress(t *testing.T) {
	t.Parallel()

	for _, optIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("optIn=%t", optIn), func(t *testing.T) {
			t.Parallel()

			sr := tracetest.NewSpanRecorder()
			reader := metric.NewManualReader()
			opts := []fiberotel.Option{
				fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				fiberotel.WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
			}
			if optIn {
				// The documented way back, for an application that knows its hosts.
				opts = append(opts, fiberotel.WithCustomMetricAttributes(func(c fiber.Ctx) []attribute.KeyValue {
					return []attribute.KeyValue{semconv.ServerAddress(utils.CopyString(c.Hostname()))}
				}))
			}

			app := fiber.New()
			app.Use(fiberotel.New(opts...))
			app.Get("/", func(c fiber.Ctx) error {
				return c.SendStatus(http.StatusOK)
			})

			hosts := []string{"a.example.com", "b.example.com", "c.example.com"}
			for _, host := range hosts {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Host = host
				resp, err := app.Test(r)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)
			}

			spans := sr.Ended()
			require.Len(t, spans, len(hosts))
			for i, span := range spans {
				assert.Contains(t, span.Attributes(), semconv.ServerAddress(hosts[i]))
			}

			var metrics metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(context.Background(), &metrics))
			require.Len(t, metrics.ScopeMetrics, 1)

			points := metricPoints(t, metrics.ScopeMetrics[0])
			require.Len(t, points, 4)
			for name, sets := range points {
				if !optIn {
					require.Len(t, sets, 1, name)
					assert.False(t, sets[0].HasValue(semconv.ServerAddressKey), name)
					continue
				}

				require.Len(t, sets, len(hosts), name)
				for _, set := range sets {
					assert.True(t, set.HasValue(semconv.ServerAddressKey), name)
				}
			}
		})
	}
}

func TestCustomResponseAttributes(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := metric.NewManualReader()
	meterProvider := metric.NewMeterProvider(metric.WithReader(reader))

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(tracerProvider),
		fiberotel.WithMeterProvider(meterProvider),
		fiberotel.WithCustomResponseAttributes(func(ctx fiber.Ctx) []attribute.KeyValue {
			return []attribute.KeyValue{
				attribute.String("app.trace_outcome", fmt.Sprint(ctx.Locals("outcome"))),
				attribute.Int("app.trace_status", ctx.Response().StatusCode()),
			}
		}),
		fiberotel.WithCustomResponseMetricAttributes(func(ctx fiber.Ctx) []attribute.KeyValue {
			return []attribute.KeyValue{
				attribute.String("app.metric_outcome", fmt.Sprint(ctx.Locals("outcome"))),
				attribute.String("app.metric_route", ctx.Route().Path),
			}
		}),
	))
	app.Get("/orders/:id", func(ctx fiber.Ctx) error {
		ctx.Locals("outcome", "accepted")
		return ctx.Status(http.StatusAccepted).SendString("ok")
	})

	r := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
	resp, err := app.Test(r)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), attribute.String("app.trace_outcome", "accepted"))
	assert.Contains(t, spans[0].Attributes(), attribute.Int("app.trace_status", http.StatusAccepted))
	assert.NotContains(t, spans[0].Attributes(), attribute.String("app.metric_outcome", "accepted"))

	metrics := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	requestAttrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(http.MethodGet),
		semconv.URLSchemeKey.String("http"),
	}
	responseAttrs := []attribute.KeyValue{
		semconv.NetworkProtocolName("http"),
		semconv.NetworkProtocolVersion(fmt.Sprintf("1.%d", r.ProtoMinor)),
		semconv.HTTPResponseStatusCode(http.StatusAccepted),
		semconv.HTTPRouteKey.String("/orders/:id"),
		attribute.String("app.metric_outcome", "accepted"),
		attribute.String("app.metric_route", "/orders/:id"),
	}
	assertScopeMetrics(t, metrics.ScopeMetrics[0], "/orders/:id", requestAttrs, append(requestAttrs, responseAttrs...))
}

func TestCustomResponseAttributeCallbacksPanicCleanup(t *testing.T) {
	t.Parallel()

	for _, callback := range []string{"span", "metric"} {
		t.Run(callback, func(t *testing.T) {
			t.Parallel()

			reader := metric.NewManualReader()
			meterProvider := metric.NewMeterProvider(metric.WithReader(reader))
			options := []fiberotel.Option{
				fiberotel.WithMeterProvider(meterProvider),
			}
			panickingCallback := func(ctx fiber.Ctx) []attribute.KeyValue {
				if ctx.Response().StatusCode() == http.StatusNotFound {
					_ = ctx.Locals("tenant").(string)
				}
				return nil
			}
			if callback == "span" {
				options = append(options, fiberotel.WithCustomResponseAttributes(panickingCallback))
			} else {
				options = append(options, fiberotel.WithCustomResponseMetricAttributes(panickingCallback))
			}

			app := fiber.New()
			app.Use(recover.New())
			app.Use(fiberotel.New(options...))
			app.Get("/orders/:id", func(ctx fiber.Ctx) error {
				ctx.Locals("tenant", "shopper")
				return ctx.SendStatus(http.StatusOK)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/missing", nil))
			require.NoError(t, err)
			require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
			resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/orders/42", nil))
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)

			metrics := metricdata.ResourceMetrics{}
			require.NoError(t, reader.Collect(context.Background(), &metrics))
			require.Len(t, metrics.ScopeMetrics, 1)
			var activeRequests metricdata.Sum[int64]
			var durationCount uint64
			for _, m := range metrics.ScopeMetrics[0].Metrics {
				switch m.Name {
				case fiberotel.MetricNameHTTPServerActiveRequests:
					var ok bool
					activeRequests, ok = m.Data.(metricdata.Sum[int64])
					require.True(t, ok)
				case fiberotel.MetricNameHTTPServerRequestDuration:
					histogram, ok := m.Data.(metricdata.Histogram[float64])
					require.True(t, ok)
					for _, point := range histogram.DataPoints {
						durationCount += point.Count
					}
				}
			}
			require.NotEmpty(t, activeRequests.DataPoints)
			for _, point := range activeRequests.DataPoints {
				assert.Zero(t, point.Value)
			}
			assert.Equal(t, uint64(2), durationCount)
		})
	}
}

func TestCustomResponseCallbackPanicTelemetry(t *testing.T) {
	for _, callback := range []string{"span", "metric"} {
		for _, scenario := range []struct {
			name       string
			panicValue error
			status     int
			appConfig  fiber.Config
			panicError error
		}{
			{name: "default", panicValue: errors.New("callback failed"), status: http.StatusInternalServerError},
			{name: "fiber error", panicValue: fiber.ErrForbidden, status: http.StatusForbidden},
			{name: "custom error handler", panicValue: errors.New("callback failed"), status: http.StatusServiceUnavailable,
				appConfig: fiber.Config{ErrorHandler: func(c fiber.Ctx, _ error) error {
					return c.Status(http.StatusServiceUnavailable).SendString("custom response")
				}}},
			{name: "custom panic handler", panicValue: errors.New("callback failed"), status: http.StatusBadGateway, panicError: fiber.ErrBadGateway},
		} {
			t.Run(callback+"/"+scenario.name, func(t *testing.T) {
				sr := tracetest.NewSpanRecorder()
				tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
				reader := metric.NewManualReader()
				meterProvider := metric.NewMeterProvider(metric.WithReader(reader))
				var recoveredValue any
				var callbackContext context.Context
				options := []fiberotel.Option{
					fiberotel.WithTracerProvider(tracerProvider),
					fiberotel.WithMeterProvider(meterProvider),
				}
				panickingCallback := func(ctx fiber.Ctx) []attribute.KeyValue {
					callbackContext = ctx.Context()
					panic(scenario.panicValue)
				}
				if callback == "span" {
					options = append(options, fiberotel.WithCustomResponseAttributes(panickingCallback))
				} else {
					options = append(options, fiberotel.WithCustomResponseMetricAttributes(panickingCallback))
				}
				app := fiber.New(scenario.appConfig)
				app.Use(recover.New(recover.Config{PanicHandler: func(c fiber.Ctx, value any) error {
					recoveredValue = value
					if scenario.panicError != nil {
						return scenario.panicError
					}
					return recover.DefaultPanicHandler(c, value)
				}}))
				app.Use(fiberotel.New(options...))
				resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/missing", nil))
				require.NoError(t, err)
				assert.Equal(t, scenario.status, resp.StatusCode)
				assert.Same(t, scenario.panicValue, recoveredValue)
				assert.ErrorIs(t, callbackContext.Err(), context.Canceled)

				spans := sr.Ended()
				require.Len(t, spans, 1)
				assert.Equal(t, codes.Error, spans[0].Status().Code)
				spanAttrs := attribute.NewSet(spans[0].Attributes()...)
				assert.False(t, spanAttrs.HasValue(semconv.HTTPResponseStatusCodeKey))
				assert.False(t, spanAttrs.HasValue(semconv.HTTPResponseBodySizeKey))
				errorType, ok := spanAttrs.Value(semconv.ErrorTypeKey)
				require.True(t, ok)
				assert.Equal(t, "response_callback_panic", errorType.AsString())

				var metrics metricdata.ResourceMetrics
				require.NoError(t, reader.Collect(context.Background(), &metrics))
				require.Len(t, metrics.ScopeMetrics, 1)
				var durationCount, requestSizeCount uint64
				for _, m := range metrics.ScopeMetrics[0].Metrics {
					switch m.Name {
					case fiberotel.MetricNameHTTPServerActiveRequests:
						for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
							assert.Zero(t, point.Value)
						}
					case fiberotel.MetricNameHTTPServerRequestDuration:
						for _, point := range m.Data.(metricdata.Histogram[float64]).DataPoints {
							durationCount += point.Count
							assert.False(t, point.Attributes.HasValue(semconv.HTTPResponseStatusCodeKey))
							errorType, ok := point.Attributes.Value(semconv.ErrorTypeKey)
							require.True(t, ok)
							assert.Equal(t, "response_callback_panic", errorType.AsString())
						}
					case fiberotel.MetricNameHTTPServerRequestBodySize:
						for _, point := range m.Data.(metricdata.Histogram[int64]).DataPoints {
							requestSizeCount += point.Count
						}
					case fiberotel.MetricNameHTTPServerResponseBodySize:
						t.Error("response size is unknown until the outer error handler runs")
					}
				}
				assert.Equal(t, uint64(1), durationCount)
				assert.Equal(t, uint64(1), requestSizeCount)
			})
		}
	}
}

// panickingHandler is named so the recorded stack trace can be checked for it.
func panickingHandler(fiber.Ctx) error {
	panic("handler exploded")
}

// A handler panic still ends the telemetry and reaches the recovery middleware unchanged.
func TestHandlerPanic(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	reader := metric.NewManualReader()

	var recovered any
	var handlerContext context.Context

	app := fiber.New()
	app.Use(recover.New(recover.Config{PanicHandler: func(c fiber.Ctx, value any) error {
		recovered = value
		return recover.DefaultPanicHandler(c, value)
	}}))
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
	))
	app.Post("/orders/:id", func(c fiber.Ctx) error {
		handlerContext = c.Context()
		return panickingHandler(c)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/orders/42", strings.NewReader("order")))
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "handler exploded", recovered)
	require.NotNil(t, handlerContext)
	assert.ErrorIs(t, handlerContext.Err(), context.Canceled)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "POST /orders/:id", span.Name())
	assert.Equal(t, codes.Error, span.Status().Code)

	attrs := attribute.NewSet(span.Attributes()...)
	assert.Contains(t, span.Attributes(), semconv.ErrorTypeKey.String("panic"))
	assert.Contains(t, span.Attributes(), semconv.HTTPRoute("/orders/:id"))
	// The status is decided by the recovery middleware, after this one returns.
	assert.False(t, attrs.HasValue(semconv.HTTPResponseStatusCodeKey))
	assert.False(t, attrs.HasValue(semconv.HTTPResponseBodySizeKey))

	require.Len(t, span.Events(), 1)
	event := attribute.NewSet(span.Events()[0].Attributes...)
	message, ok := event.Value(semconv.ExceptionMessageKey)
	require.True(t, ok)
	assert.Equal(t, "handler exploded", message.AsString())
	stack, ok := event.Value(semconv.ExceptionStacktraceKey)
	require.True(t, ok)
	assert.Contains(t, stack.AsString(), "panickingHandler")

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	var durationCount, requestSizeCount uint64
	for _, m := range metrics.ScopeMetrics[0].Metrics {
		switch m.Name {
		case fiberotel.MetricNameHTTPServerActiveRequests:
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				assert.Zero(t, point.Value)
			}
		case fiberotel.MetricNameHTTPServerRequestDuration:
			for _, point := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				durationCount += point.Count
				assert.False(t, point.Attributes.HasValue(semconv.HTTPResponseStatusCodeKey))
				errorType, ok := point.Attributes.Value(semconv.ErrorTypeKey)
				require.True(t, ok)
				assert.Equal(t, "panic", errorType.AsString())
				route, ok := point.Attributes.Value(semconv.HTTPRouteKey)
				require.True(t, ok)
				assert.Equal(t, "/orders/:id", route.AsString())
			}
		case fiberotel.MetricNameHTTPServerRequestBodySize:
			for _, point := range m.Data.(metricdata.Histogram[int64]).DataPoints {
				requestSizeCount += point.Count
				assert.Equal(t, int64(len("order")), point.Sum)
			}
		case fiberotel.MetricNameHTTPServerResponseBodySize:
			t.Error("the response size is unknown until the recovery middleware answers")
		}
	}
	assert.Equal(t, uint64(1), durationCount)
	assert.Equal(t, uint64(1), requestSizeCount)
}

func TestTraceHeadersOnPanic(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		handler fiber.Handler
		opts    []fiberotel.Option
	}{
		{name: "handler", handler: panickingHandler},
		{
			name: "response callback",
			handler: func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			},
			opts: []fiberotel.Option{fiberotel.WithCustomResponseAttributes(func(fiber.Ctx) []attribute.KeyValue {
				panic("callback exploded")
			})},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sr := tracetest.NewSpanRecorder()
			app := fiber.New()
			app.Use(recover.New())
			app.Use(fiberotel.New(append([]fiberotel.Option{
				fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				fiberotel.WithoutMetrics(true),
				fiberotel.WithPropagators(propagation.TraceContext{}),
				fiberotel.WithTraceResponseHeader("X-Trace-Id"),
			}, tc.opts...)...))
			app.Get("/", tc.handler)

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
			require.NoError(t, err)
			require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

			spans := sr.Ended()
			require.Len(t, spans, 1)
			traceID := spans[0].SpanContext().TraceID().String()
			assert.Equal(t, traceID, resp.Header.Get("X-Trace-Id"))
			assert.Contains(t, resp.Header.Get("Traceparent"), traceID)
		})
	}
}

func TestCustomResponseAttributesAfterHandlerError(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := metric.NewManualReader()
	meterProvider := metric.NewMeterProvider(metric.WithReader(reader))
	var spanStatus, metricStatus int
	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(tracerProvider),
		fiberotel.WithMeterProvider(meterProvider),
		fiberotel.WithCustomResponseAttributes(func(c fiber.Ctx) []attribute.KeyValue {
			spanStatus = c.Response().StatusCode()
			return []attribute.KeyValue{attribute.Int("app.span_status", spanStatus)}
		}),
		fiberotel.WithCustomResponseMetricAttributes(func(c fiber.Ctx) []attribute.KeyValue {
			metricStatus = c.Response().StatusCode()
			return []attribute.KeyValue{attribute.Int("app.metric_status", metricStatus)}
		}),
	))
	app.Get("/forbidden", func(fiber.Ctx) error { return fiber.ErrForbidden })
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/forbidden", nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, http.StatusForbidden, spanStatus)
	assert.Equal(t, http.StatusForbidden, metricStatus)
	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.HTTPResponseStatusCode(http.StatusForbidden))
	assert.Contains(t, spans[0].Attributes(), attribute.Int("app.span_status", http.StatusForbidden))
	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	for _, m := range metrics.ScopeMetrics[0].Metrics {
		if m.Name == fiberotel.MetricNameHTTPServerRequestDuration {
			points := m.Data.(metricdata.Histogram[float64]).DataPoints
			require.Len(t, points, 1)
			assert.Contains(t, points[0].Attributes.ToSlice(), semconv.HTTPResponseStatusCode(http.StatusForbidden))
			assert.Contains(t, points[0].Attributes.ToSlice(), attribute.Int("app.metric_status", http.StatusForbidden))
			return
		}
	}
	t.Fatal("request duration metric not found")
}

func TestNoopTracerProviderPropagatesInboundContext(t *testing.T) {
	t.Parallel()

	const (
		traceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
		traceparent = "00-" + traceID + "-00f067aa0ba902b7-01"
	)

	reader := metric.NewManualReader()

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(tracenoop.NewTracerProvider()),
		fiberotel.WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
		fiberotel.WithPropagators(propagation.TraceContext{}),
		fiberotel.WithTraceResponseHeader("X-Trace-Id"),
	))
	var handlerSpanContext oteltrace.SpanContext
	app.Get("/", func(c fiber.Ctx) error {
		handlerSpanContext = oteltrace.SpanContextFromContext(c.Context())
		return c.SendStatus(http.StatusNoContent)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("traceparent", traceparent)
	resp, err := app.Test(r)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	assert.Equal(t, traceID, handlerSpanContext.TraceID().String())
	assert.True(t, handlerSpanContext.IsRemote())
	assert.Equal(t, traceID, resp.Header.Get("X-Trace-Id"))
	assert.Equal(t, traceparent, resp.Header.Get("traceparent"))

	// Without an inbound context there is nothing to hand on.
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	assert.Empty(t, resp.Header.Get("traceparent"))
	assert.Empty(t, resp.Header.Get("X-Trace-Id"))

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	assert.Len(t, metrics.ScopeMetrics[0].Metrics, 4)
}

// Also with DisableHeaderNormalizing, where fasthttp keeps names as received.
func TestPropagationHeaderNamesAreCaseInsensitive(t *testing.T) {
	t.Parallel()

	for _, disableNormalizing := range []bool{false, true} {
		t.Run(fmt.Sprintf("DisableHeaderNormalizing=%t", disableNormalizing), func(t *testing.T) {
			t.Parallel()

			sr := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

			app := fiber.New(fiber.Config{DisableHeaderNormalizing: disableNormalizing})
			app.Use(fiberotel.New(
				fiberotel.WithTracerProvider(provider),
				fiberotel.WithPropagators(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})),
			))
			var members []string
			app.Get("/", func(c fiber.Ctx) error {
				for _, member := range baggage.FromContext(c.Context()).Members() {
					members = append(members, member.Key()+"="+member.Value())
				}
				return c.SendStatus(http.StatusNoContent)
			})

			parentCtx, parent := provider.Tracer("test").Start(context.Background(), "parent")
			parent.End()

			// Set through the map, so the names are sent as spelled.
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header["TRACEPARENT"] = []string{"00-" + parent.SpanContext().TraceID().String() + "-" + parent.SpanContext().SpanID().String() + "-01"}
			r.Header["baggage"] = []string{"tenant=acme"}
			r.Header["Baggage"] = []string{"region=eu"}
			resp, err := app.Test(r)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, resp.StatusCode)

			spans := sr.Ended()
			require.Len(t, spans, 2)
			server := spans[1]
			assert.Equal(t, oteltrace.SpanContextFromContext(parentCtx).TraceID(), server.SpanContext().TraceID())
			assert.Equal(t, parent.SpanContext().SpanID(), server.Parent().SpanID())
			assert.ElementsMatch(t, []string{"tenant=acme", "region=eu"}, members)
		})
	}
}

func TestOutboundTracingPropagation(t *testing.T) {
	sr := new(tracetest.SpanRecorder)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(provider),
		fiberotel.WithPropagators(b3prop.New(b3prop.WithInjectEncoding(b3prop.B3MultipleHeader))),
	))
	app.Get("/foo", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/foo", nil), fiber.TestConfig{Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.NotNil(t, resp)

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
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(provider),
		fiberotel.WithPropagators(b3prop.New(b3prop.WithInjectEncoding(b3prop.B3MultipleHeader))),
	))
	app.Get("/foo", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	req := httptest.NewRequest("GET", "/foo", nil)

	req.Header.Set("X-B3-SpanId", spanId)
	req.Header.Set("X-B3-TraceId", traceId)
	req.Header.Set("X-B3-Sampled", "1")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.NotEmpty(t, resp.Header.Get("X-B3-SpanId"))
	assert.Equal(t, traceId, resp.Header.Get("X-B3-TraceId"))
	assert.Equal(t, "1", resp.Header.Get("X-B3-Sampled"))
}

func TestTraceResponseHeader(t *testing.T) {
	sr := new(tracetest.SpanRecorder)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(provider),
		fiberotel.WithTraceResponseHeader("X-Trace-Id"),
	))
	app.Get("/foo", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/foo", nil), fiber.TestConfig{Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.NotNil(t, resp)

	spans := sr.Ended()
	require.Len(t, spans, 1)

	assert.Equal(t, spans[0].SpanContext().TraceID().String(), resp.Header.Get("X-Trace-Id"))
}

func TestTraceResponseHeaderDisabledByDefault(t *testing.T) {
	sr := new(tracetest.SpanRecorder)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(provider),
	))
	app.Get("/foo", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/foo", nil), fiber.TestConfig{Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Empty(t, resp.Header.Get("X-Trace-Id"))
}

func TestTraceResponseHeaderUsesInboundTraceID(t *testing.T) {
	sr := new(tracetest.SpanRecorder)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	propagator := propagation.TraceContext{}

	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	ctx, span := provider.Tracer(instrumentationName).Start(context.Background(), "test")
	defer span.End()
	propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(provider),
		fiberotel.WithPropagators(propagator),
		fiberotel.WithTraceResponseHeader("X-Trace-Id"),
	))
	app.Get("/foo", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, span.SpanContext().TraceID().String(), resp.Header.Get("X-Trace-Id"))
}

func TestCollectClientIP(t *testing.T) {
	t.Parallel()

	optFactories := []struct {
		name string
		opt  func(bool) fiberotel.Option
	}{
		{name: "WithClientIP", opt: fiberotel.WithClientIP},
		{name: "WithCollectClientIP", opt: fiberotel.WithCollectClientIP},
	}

	for _, factory := range optFactories {
		factory := factory
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()

			for _, enabled := range []bool{true, false} {
				enabled := enabled
				t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
					t.Parallel()

					sr := tracetest.NewSpanRecorder()
					provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

					app := fiber.New()
					app.Use(fiberotel.New(
						fiberotel.WithTracerProvider(provider),
						factory.opt(enabled),
					))
					app.Get("/foo", func(ctx fiber.Ctx) error {
						return ctx.SendStatus(http.StatusNoContent)
					})

					req := httptest.NewRequest("GET", "/foo", nil)
					resp, err := app.Test(req)
					require.NoError(t, err)
					require.NotNil(t, resp)

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
		})
	}
}

// Handlers get a context canceled when the request ends; outer middleware gets its own back.
func TestMiddlewareRestoresOuterContext(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}

	var outerBefore, outerAfter, handlerContext context.Context

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxKey{}, "outer"))
		outerBefore = c.Context()
		err := c.Next()
		outerAfter = c.Context()
		return err
	})
	app.Use(fiberotel.New(fiberotel.WithTracerProvider(sdktrace.NewTracerProvider())))
	app.Get("/", func(c fiber.Ctx) error {
		handlerContext = c.Context()
		return c.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	assert.Same(t, outerBefore, outerAfter)
	require.NoError(t, outerAfter.Err())
	assert.ErrorIs(t, handlerContext.Err(), context.Canceled)
	assert.Equal(t, "outer", handlerContext.Value(ctxKey{}))
}

// An SSE writer watching ctx.Done() runs after the middleware has returned.
func TestStreamWriterContextOutlivesMiddleware(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(fiberotel.New(fiberotel.WithTracerProvider(sdktrace.NewTracerProvider())))
	app.Get("/events", func(c fiber.Ctx) error {
		ctx := c.Context()
		c.Set(fiber.HeaderContentType, "text/event-stream")
		return c.SendStreamWriter(func(w *bufio.Writer) {
			if ctx.Err() != nil {
				_, _ = w.WriteString("data: canceled\n\n")
				return
			}
			_, _ = w.WriteString("data: live\n\n")
		})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/events", nil))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "data: live\n\n", string(body))
}

func TestMiddlewarePreservesUserContext(t *testing.T) {
	type ctxKey string
	const requestIDKey ctxKey = "request_id"
	const expectedID = 1234

	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	app := fiber.New()
	// Middleware that injects a value into the context before otel
	app.Use(func(c fiber.Ctx) error {
		ctx := context.WithValue(c.Context(), requestIDKey, expectedID)
		c.SetContext(ctx)
		return c.Next()
	})
	app.Use(fiberotel.New(fiberotel.WithTracerProvider(provider)))
	app.Get("/", func(c fiber.Ctx) error {
		val := c.Context().Value(requestIDKey)
		if val == nil {
			return c.SendString("request_id NOT found in context")
		}
		return c.SendString(fmt.Sprintf("request_id from context: %d", val.(int)))
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("request_id from context: %d", expectedID), string(body))
}

func TestMetricExemplarsReferenceRequestSpan(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	reader := metric.NewManualReader()

	app := fiber.New()
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithMeterProvider(metric.NewMeterProvider(
			metric.WithReader(reader),
			metric.WithExemplarFilter(exemplar.TraceBasedFilter),
		)),
	))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	traceID := spans[0].SpanContext().TraceID()
	spanID := spans[0].SpanContext().SpanID()

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)

	checked := 0
	for _, m := range metrics.ScopeMetrics[0].Metrics {
		var exemplars [][2][]byte
		switch data := m.Data.(type) {
		case metricdata.Histogram[float64]:
			for _, point := range data.DataPoints {
				for _, e := range point.Exemplars {
					exemplars = append(exemplars, [2][]byte{e.TraceID, e.SpanID})
				}
			}
		case metricdata.Histogram[int64]:
			for _, point := range data.DataPoints {
				for _, e := range point.Exemplars {
					exemplars = append(exemplars, [2][]byte{e.TraceID, e.SpanID})
				}
			}
		default:
			continue
		}

		require.Len(t, exemplars, 1, m.Name)
		assert.Equal(t, traceID[:], exemplars[0][0], m.Name)
		assert.Equal(t, spanID[:], exemplars[0][1], m.Name)
		checked++
	}
	assert.Equal(t, 3, checked, "every histogram carries an exemplar")
}

func TestWithoutMetrics(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	port := 8080
	route := "/foo"

	app := fiber.New()
	app.Use(
		fiberotel.New(
			fiberotel.WithMeterProvider(provider),
			fiberotel.WithPort(port),
			fiberotel.WithoutMetrics(true),
		),
	)
	app.Get(route, func(ctx fiber.Ctx) error {
		return ctx.SendStatus(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, route, nil)
	resp, err := app.Test(r)
	require.NoError(t, err)
	require.NotNil(t, resp)

	metrics := metricdata.ResourceMetrics{}
	err = reader.Collect(context.Background(), &metrics)
	assert.NoError(t, err)
	assert.Len(t, metrics.ScopeMetrics, 0, "No metrics should be collected when metrics are disabled")
}

func TestWithoutMetricsWithStreamResponse(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	app := fiber.New()
	app.Use(
		fiberotel.New(
			fiberotel.WithMeterProvider(provider),
			fiberotel.WithoutMetrics(true),
		),
	)

	app.Get("/stream", func(ctx fiber.Ctx) error {
		return ctx.SendStream(bytes.NewReader(make([]byte, 2048)))
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/stream", nil))
	require.NoError(t, err)
	require.NotNil(t, resp)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Len(t, body, 2048)

	metrics := metricdata.ResourceMetrics{}
	err = reader.Collect(context.Background(), &metrics)
	assert.NoError(t, err)
	assert.Len(t, metrics.ScopeMetrics, 0, "No metrics should be collected when metrics are disabled")
}

func TestResponseBodySizeWithStream(t *testing.T) {
	const responseBodySize = 8192

	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	app := fiber.New()
	app.Use(
		fiberotel.New(
			fiberotel.WithMeterProvider(provider),
		),
	)

	app.Get("/stream", func(ctx fiber.Ctx) error {
		payload := make([]byte, responseBodySize)
		return ctx.SendStream(bytes.NewReader(payload))
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/stream", nil))
	require.NoError(t, err)
	require.NotNil(t, resp)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Len(t, body, responseBodySize)

	metrics := metricdata.ResourceMetrics{}
	err = reader.Collect(context.Background(), &metrics)
	require.NoError(t, err)
	require.Len(t, metrics.ScopeMetrics, 1)

	var got metricdata.Histogram[int64]
	for _, m := range metrics.ScopeMetrics[0].Metrics {
		if m.Name == fiberotel.MetricNameHttpServerResponseSize {
			var ok bool
			got, ok = m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			break
		}
	}

	require.Len(t, got.DataPoints, 1)
	assert.Equal(t, int64(responseBodySize), got.DataPoints[0].Sum)
}
