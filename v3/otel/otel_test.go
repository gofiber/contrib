package otel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/otel/internal"
	"github.com/gofiber/fiber/v3"
	recoverer "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/static"
	"github.com/gofiber/utils/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	otelcontrib "go.opentelemetry.io/contrib"
	b3prop "go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
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

const scopeName = "github.com/gofiber/contrib/v3/otel"

func TestChildSpanFromGlobalTracer(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(provider)

	app := fiber.New()
	app.Use(New())
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
	app.Use(Middleware(WithTracerProvider(provider))) //nolint:staticcheck // the deprecated alias must keep working
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
	app.Use(New(WithTracerProvider(provider)))
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
	app.Use(New(WithNext(func(c fiber.Ctx) bool {
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
		New(WithTracerProvider(provider)),
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
		Name:      scopeName,
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
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
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
	for _, set := range metricPoints(t, metrics.ScopeMetrics[0])[MetricNameHTTPServerRequestDuration] {
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
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
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
			app.Use(New(
				WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
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
	app.Use(New(WithTracerProvider(provider)))
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
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
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
	app.Use(New())
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
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
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
		if name == MetricNameHTTPServerActiveRequests {
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
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
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

	ctx, pspan := provider.Tracer(scopeName).Start(context.Background(), "test")
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(r.Header))

	app := fiber.New()
	app.Use(New(WithTracerProvider(provider)))
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

	ctx, pspan := provider.Tracer(scopeName).Start(context.Background(), "test")
	b3.Inject(ctx, propagation.HeaderCarrier(r.Header))

	app := fiber.New()
	app.Use(New(WithTracerProvider(provider), WithPropagators(b3)))
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
			val, valid := HasBasicAuth(tC.auth)

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
		New(
			WithMeterProvider(provider),
			WithPort(port),
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
	app.Use(New(WithMeterProvider(provider)))
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
		if m.Name != MetricNameHTTPServerRequestBodySize {
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
		Name:      scopeName,
		Version:   otelcontrib.Version(),
		SchemaURL: semconv.SchemaURL,
	}, sm.Scope)

	// Duration value is not predictable.
	m := sm.Metrics[0]
	assert.Equal(t, MetricNameHTTPServerRequestDuration, m.Name)
	assert.Equal(t, UnitSeconds, m.Unit)
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
		Name:        MetricNameHTTPServerRequestBodySize,
		Description: "Size of HTTP server request bodies.",
		Unit:        UnitBytes,
		Data:        getHistogram(0, responseAttrs),
	}
	metricdatatest.AssertEqual(t, want, sm.Metrics[1], metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars())

	// Response size
	want = metricdata.Metrics{
		Name:        MetricNameHTTPServerResponseBodySize,
		Description: "Size of HTTP server response bodies.",
		Unit:        UnitBytes,
		Data:        getHistogram(2, responseAttrs),
	}
	metricdatatest.AssertEqual(t, want, sm.Metrics[2], metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars())

	// Active requests
	want = metricdata.Metrics{
		Name:        MetricNameHTTPServerActiveRequests,
		Description: "Number of active HTTP server requests.",
		Unit:        UnitRequest,
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
		New(
			WithTracerProvider(provider),
			WithCustomAttributes(func(ctx fiber.Ctx) []attribute.KeyValue {
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
		New(
			WithMeterProvider(provider),
			WithPort(port),
			WithCustomMetricAttributes(func(ctx fiber.Ctx) []attribute.KeyValue {
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
			opts := []Option{
				WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
			}
			if optIn {
				// The documented way back, for an application that knows its hosts.
				opts = append(opts, WithCustomMetricAttributes(func(c fiber.Ctx) []attribute.KeyValue {
					return []attribute.KeyValue{semconv.ServerAddress(utils.CopyString(c.Hostname()))}
				}))
			}

			app := fiber.New()
			app.Use(New(opts...))
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
	app.Use(New(
		WithTracerProvider(tracerProvider),
		WithMeterProvider(meterProvider),
		WithCustomResponseAttributes(func(ctx fiber.Ctx) []attribute.KeyValue {
			return []attribute.KeyValue{
				attribute.String("app.trace_outcome", fmt.Sprint(ctx.Locals("outcome"))),
				attribute.Int("app.trace_status", ctx.Response().StatusCode()),
			}
		}),
		WithCustomResponseMetricAttributes(func(ctx fiber.Ctx) []attribute.KeyValue {
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
			options := []Option{
				WithMeterProvider(meterProvider),
			}
			panickingCallback := func(ctx fiber.Ctx) []attribute.KeyValue {
				if ctx.Response().StatusCode() == http.StatusNotFound {
					_ = ctx.Locals("tenant").(string)
				}
				return nil
			}
			if callback == "span" {
				options = append(options, WithCustomResponseAttributes(panickingCallback))
			} else {
				options = append(options, WithCustomResponseMetricAttributes(panickingCallback))
			}

			app := fiber.New()
			app.Use(recoverer.New())
			app.Use(New(options...))
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
				case MetricNameHTTPServerActiveRequests:
					var ok bool
					activeRequests, ok = m.Data.(metricdata.Sum[int64])
					require.True(t, ok)
				case MetricNameHTTPServerRequestDuration:
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
				options := []Option{
					WithTracerProvider(tracerProvider),
					WithMeterProvider(meterProvider),
				}
				panickingCallback := func(ctx fiber.Ctx) []attribute.KeyValue {
					callbackContext = ctx.Context()
					panic(scenario.panicValue)
				}
				if callback == "span" {
					options = append(options, WithCustomResponseAttributes(panickingCallback))
				} else {
					options = append(options, WithCustomResponseMetricAttributes(panickingCallback))
				}
				app := fiber.New(scenario.appConfig)
				app.Use(recoverer.New(recoverer.Config{PanicHandler: func(c fiber.Ctx, value any) error {
					recoveredValue = value
					if scenario.panicError != nil {
						return scenario.panicError
					}
					return recoverer.DefaultPanicHandler(c, value)
				}}))
				app.Use(New(options...))
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
					case MetricNameHTTPServerActiveRequests:
						for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
							assert.Zero(t, point.Value)
						}
					case MetricNameHTTPServerRequestDuration:
						for _, point := range m.Data.(metricdata.Histogram[float64]).DataPoints {
							durationCount += point.Count
							assert.False(t, point.Attributes.HasValue(semconv.HTTPResponseStatusCodeKey))
							errorType, ok := point.Attributes.Value(semconv.ErrorTypeKey)
							require.True(t, ok)
							assert.Equal(t, "response_callback_panic", errorType.AsString())
						}
					case MetricNameHTTPServerRequestBodySize:
						for _, point := range m.Data.(metricdata.Histogram[int64]).DataPoints {
							requestSizeCount += point.Count
						}
					case MetricNameHTTPServerResponseBodySize:
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
	app.Use(recoverer.New(recoverer.Config{PanicHandler: func(c fiber.Ctx, value any) error {
		recovered = value
		return recoverer.DefaultPanicHandler(c, value)
	}}))
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
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
		case MetricNameHTTPServerActiveRequests:
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				assert.Zero(t, point.Value)
			}
		case MetricNameHTTPServerRequestDuration:
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
		case MetricNameHTTPServerRequestBodySize:
			for _, point := range m.Data.(metricdata.Histogram[int64]).DataPoints {
				requestSizeCount += point.Count
				assert.Equal(t, int64(len("order")), point.Sum)
			}
		case MetricNameHTTPServerResponseBodySize:
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
		opts    []Option
	}{
		{name: "handler", handler: panickingHandler},
		{
			name: "response callback",
			handler: func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			},
			opts: []Option{WithCustomResponseAttributes(func(fiber.Ctx) []attribute.KeyValue {
				panic("callback exploded")
			})},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sr := tracetest.NewSpanRecorder()
			app := fiber.New()
			app.Use(recoverer.New())
			app.Use(New(append([]Option{
				WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				WithoutMetrics(true),
				WithPropagators(propagation.TraceContext{}),
				WithTraceResponseHeader("X-Trace-Id"),
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
	app.Use(New(
		WithTracerProvider(tracerProvider),
		WithMeterProvider(meterProvider),
		WithCustomResponseAttributes(func(c fiber.Ctx) []attribute.KeyValue {
			spanStatus = c.Response().StatusCode()
			return []attribute.KeyValue{attribute.Int("app.span_status", spanStatus)}
		}),
		WithCustomResponseMetricAttributes(func(c fiber.Ctx) []attribute.KeyValue {
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
		if m.Name == MetricNameHTTPServerRequestDuration {
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
	app.Use(New(
		WithTracerProvider(tracenoop.NewTracerProvider()),
		WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
		WithPropagators(propagation.TraceContext{}),
		WithTraceResponseHeader("X-Trace-Id"),
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
			app.Use(New(
				WithTracerProvider(provider),
				WithPropagators(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})),
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
	app.Use(New(
		WithTracerProvider(provider),
		WithPropagators(b3prop.New(b3prop.WithInjectEncoding(b3prop.B3MultipleHeader))),
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
	app.Use(New(
		WithTracerProvider(provider),
		WithPropagators(b3prop.New(b3prop.WithInjectEncoding(b3prop.B3MultipleHeader))),
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
	app.Use(New(
		WithTracerProvider(provider),
		WithTraceResponseHeader("X-Trace-Id"),
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
	app.Use(New(
		WithTracerProvider(provider),
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
	ctx, span := provider.Tracer(scopeName).Start(context.Background(), "test")
	defer span.End()
	propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))

	app := fiber.New()
	app.Use(New(
		WithTracerProvider(provider),
		WithPropagators(propagator),
		WithTraceResponseHeader("X-Trace-Id"),
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
		opt  func(bool) Option
	}{
		{name: "WithClientIP", opt: WithClientIP},
		{name: "WithCollectClientIP", opt: WithCollectClientIP},
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
					app.Use(New(
						WithTracerProvider(provider),
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
	app.Use(New(WithTracerProvider(sdktrace.NewTracerProvider())))
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
	app.Use(New(WithTracerProvider(sdktrace.NewTracerProvider())))
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
	app.Use(New(WithTracerProvider(provider)))
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
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithMeterProvider(metric.NewMeterProvider(
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
		New(
			WithMeterProvider(provider),
			WithPort(port),
			WithoutMetrics(true),
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
		New(
			WithMeterProvider(provider),
			WithoutMetrics(true),
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
		New(
			WithMeterProvider(provider),
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
		if m.Name == MetricNameHttpServerResponseSize {
			var ok bool
			got, ok = m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			break
		}
	}

	require.Len(t, got.DataPoints, 1)
	assert.Equal(t, int64(responseBodySize), got.DataPoints[0].Sum)
}

// newTracedApp returns an app instrumented with opts and a recorder of its spans.
func newTracedApp(t *testing.T, config fiber.Config, opts ...Option) (*fiber.App, *tracetest.SpanRecorder) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	opts = append([]Option{
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithoutMetrics(true),
	}, opts...)

	app := fiber.New(config)
	app.Use(New(opts...))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusNoContent)
	})

	return app, sr
}

// callerContext returns a sampled remote span context and its traceparent.
func callerContext() (oteltrace.SpanContext, string) {
	traceID, _ := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	})

	return sc, "00-" + traceID.String() + "-" + spanID.String() + "-01"
}

func spanAttrs(span sdktrace.ReadOnlySpan) attribute.Set {
	return attribute.NewSet(span.Attributes()...)
}

func TestPublicEndpoint(t *testing.T) {
	t.Parallel()

	caller, traceparent := callerContext()

	testCases := []struct {
		name   string
		opts   []Option
		public bool
	}{
		{name: "default continues the caller's trace"},
		{name: "public endpoint", opts: []Option{WithPublicEndpoint()}, public: true},
		{name: "decided per request", opts: []Option{WithPublicEndpointFn(func(c fiber.Ctx) bool {
			return c.Get("X-Internal") == ""
		})}, public: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, sr := newTracedApp(t, fiber.Config{}, append(tc.opts, WithPropagators(propagation.TraceContext{}))...)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("traceparent", traceparent)
			resp, err := app.Test(r)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, resp.StatusCode)

			spans := sr.Ended()
			require.Len(t, spans, 1)
			span := spans[0]

			if !tc.public {
				assert.Equal(t, caller.TraceID(), span.SpanContext().TraceID())
				assert.Equal(t, caller.SpanID(), span.Parent().SpanID())
				assert.Empty(t, span.Links())
				return
			}

			assert.NotEqual(t, caller.TraceID(), span.SpanContext().TraceID(), "a public endpoint starts its own trace")
			assert.False(t, span.Parent().IsValid())
			require.Len(t, span.Links(), 1)
			assert.Equal(t, caller.TraceID(), span.Links()[0].SpanContext.TraceID())
			assert.Equal(t, caller.SpanID(), span.Links()[0].SpanContext.SpanID())
			// The response carries the trace the request was recorded in.
			assert.Contains(t, resp.Header.Get("traceparent"), span.SpanContext().TraceID().String())
		})
	}

	t.Run("trusted request continues the caller's trace", func(t *testing.T) {
		t.Parallel()

		app, sr := newTracedApp(t, fiber.Config{},
			WithPropagators(propagation.TraceContext{}),
			WithPublicEndpointFn(func(c fiber.Ctx) bool {
				return c.Get("X-Internal") == ""
			}),
		)

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("traceparent", traceparent)
		r.Header.Set("X-Internal", "1")
		_, err := app.Test(r)
		require.NoError(t, err)

		spans := sr.Ended()
		require.Len(t, spans, 1)
		assert.Equal(t, caller.TraceID(), spans[0].SpanContext().TraceID())
		assert.Empty(t, spans[0].Links())
	})

	t.Run("without a caller there is nothing to link", func(t *testing.T) {
		t.Parallel()

		app, sr := newTracedApp(t, fiber.Config{}, WithPublicEndpoint())
		_, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
		require.NoError(t, err)

		spans := sr.Ended()
		require.Len(t, spans, 1)
		assert.Empty(t, spans[0].Links())
	})
}

func TestSpanStartOptions(t *testing.T) {
	t.Parallel()

	linked, _ := callerContext()
	app, sr := newTracedApp(t, fiber.Config{}, WithSpanStartOptions(
		oteltrace.WithAttributes(attribute.String("app.tier", "edge")),
		oteltrace.WithLinks(oteltrace.Link{SpanContext: linked}),
	))

	_, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), attribute.String("app.tier", "edge"))
	assert.Equal(t, oteltrace.SpanKindServer, spans[0].SpanKind())
	require.Len(t, spans[0].Links(), 1)
	assert.Equal(t, linked.SpanID(), spans[0].Links()[0].SpanContext.SpanID())
}

func TestCapturedHeaders(t *testing.T) {
	t.Parallel()

	for _, disableNormalizing := range []bool{false, true} {
		t.Run(fmt.Sprintf("DisableHeaderNormalizing=%t", disableNormalizing), func(t *testing.T) {
			t.Parallel()

			sr := tracetest.NewSpanRecorder()
			app := fiber.New(fiber.Config{DisableHeaderNormalizing: disableNormalizing})
			app.Use(New(
				WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				WithoutMetrics(true),
				// Matched case-insensitively, recorded lowercased, captured once.
				WithCapturedRequestHeaders("X-Request-ID", "accept", "X-Missing", "x-request-id"),
				WithCapturedResponseHeaders("x-cache", "Content-Type", "X-Absent"),
			))
			app.Get("/", func(c fiber.Ctx) error {
				c.Set("X-Cache", "HIT")
				return c.JSON(fiber.Map{"ok": true})
			})

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header["x-request-id"] = []string{"first", "second"}
			r.Header["Accept"] = []string{"application/json"}
			resp, err := app.Test(r)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)

			spans := sr.Ended()
			require.Len(t, spans, 1)
			attrs := spanAttrs(spans[0])

			requestID, ok := attrs.Value("http.request.header.x-request-id")
			require.True(t, ok)
			assert.Equal(t, []string{"first", "second"}, requestID.AsStringSlice())
			accept, ok := attrs.Value("http.request.header.accept")
			require.True(t, ok)
			assert.Equal(t, []string{"application/json"}, accept.AsStringSlice())
			assert.False(t, attrs.HasValue("http.request.header.x-missing"))

			cache, ok := attrs.Value("http.response.header.x-cache")
			require.True(t, ok)
			assert.Equal(t, []string{"HIT"}, cache.AsStringSlice())
			contentType, ok := attrs.Value("http.response.header.content-type")
			require.True(t, ok)
			assert.Equal(t, []string{fiber.MIMEApplicationJSONCharsetUTF8}, contentType.AsStringSlice())
			assert.False(t, attrs.HasValue("http.response.header.x-absent"))
		})
	}
}

func TestRedactedQueryParams(t *testing.T) {
	t.Parallel()

	app, sr := newTracedApp(t, fiber.Config{}, WithRedactedQueryParams("token", "api_key"))

	_, err := app.Test(httptest.NewRequest(http.MethodGet, "/?token=s3cr3t&q=otel&sig=abc&api_key=k&Token=kept", nil))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.URLQuery("token=REDACTED&q=otel&sig=REDACTED&api_key=REDACTED&Token=kept"))
}

func TestResponsePropagators(t *testing.T) {
	t.Parallel()

	_, traceparent := callerContext()

	testCases := []struct {
		name        string
		propagators propagation.TextMapPropagator
		traceparent bool
		baggage     bool
	}{
		{name: "default: the extracting propagators", traceparent: true, baggage: true},
		{name: "trace context only", propagators: propagation.TraceContext{}, traceparent: true},
		{name: "none", propagators: propagation.NewCompositeTextMapPropagator()},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := []Option{
				WithPropagators(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})),
			}
			if tc.propagators != nil {
				opts = append(opts, WithResponsePropagators(tc.propagators))
			}
			app, _ := newTracedApp(t, fiber.Config{}, opts...)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("traceparent", traceparent)
			r.Header.Set("baggage", "tenant=acme")
			resp, err := app.Test(r)
			require.NoError(t, err)

			assert.Equal(t, tc.traceparent, resp.Header.Get("traceparent") != "")
			assert.Equal(t, tc.baggage, resp.Header.Get("baggage") == "tenant=acme")
		})
	}
}

// server.port is the port the Host header names, unless WithPort configures one.
func TestServerPort(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		host string
		opts []Option
		port int
	}{
		{name: "named by Host", host: "example.com:8443", port: 8443},
		{name: "IPv6 Host", host: "[::1]:9090", port: 9090},
		{name: "not named by Host", host: "example.com"},
		{name: "configured", host: "example.com:8443", opts: []Option{WithPort(443)}, port: 443},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, sr := newTracedApp(t, fiber.Config{}, tc.opts...)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Host = tc.host
			_, err := app.Test(r)
			require.NoError(t, err)

			spans := sr.Ended()
			require.Len(t, spans, 1)
			attrs := spanAttrs(spans[0])
			port, ok := attrs.Value(semconv.ServerPortKey)
			if tc.port == 0 {
				assert.False(t, ok, "got server.port %d", port.AsInt64())
				return
			}
			require.True(t, ok)
			assert.Equal(t, int64(tc.port), port.AsInt64())
		})
	}
}

// The client is the peer unless a trusted proxy reports another.
func TestNetworkPeerAndClientAddress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		config   fiber.Config
		opts     []Option
		client   string
		withPeer bool
	}{
		{name: "direct", client: "127.0.0.1", withPeer: true},
		{
			name: "behind a trusted proxy",
			config: fiber.Config{
				TrustProxy:       true,
				TrustProxyConfig: fiber.TrustProxyConfig{Loopback: true},
				ProxyHeader:      fiber.HeaderXForwardedFor,
			},
			client:   "203.0.113.7",
			withPeer: true,
		},
		{name: "collection disabled", opts: []Option{WithClientIP(false)}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, sr := newTracedApp(t, tc.config, tc.opts...)

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() {
				served <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
			}()
			t.Cleanup(func() {
				require.NoError(t, app.Shutdown())
				<-served
			})

			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+listener.Addr().String()+"/", nil)
			require.NoError(t, err)
			request.Header.Set(fiber.HeaderXForwardedFor, "203.0.113.7")
			resp, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusNoContent, resp.StatusCode)

			spans := sr.Ended()
			require.Len(t, spans, 1)
			attrs := spanAttrs(spans[0])

			_, listenPort, err := net.SplitHostPort(listener.Addr().String())
			require.NoError(t, err)
			assert.Contains(t, spans[0].Attributes(), semconv.NetworkTransportTCP)
			assert.Contains(t, spans[0].Attributes(), semconv.ServerAddress("127.0.0.1"))
			wantPort, err := strconv.Atoi(listenPort)
			require.NoError(t, err)
			assert.Contains(t, spans[0].Attributes(), semconv.ServerPort(wantPort))

			if !tc.withPeer {
				assert.False(t, attrs.HasValue(semconv.NetworkPeerAddressKey))
				assert.False(t, attrs.HasValue(semconv.NetworkPeerPortKey))
				assert.False(t, attrs.HasValue(semconv.ClientAddressKey))
				return
			}

			assert.Contains(t, spans[0].Attributes(), semconv.NetworkPeerAddress("127.0.0.1"))
			peerPort, ok := attrs.Value(semconv.NetworkPeerPortKey)
			require.True(t, ok)
			assert.Positive(t, peerPort.AsInt64())
			assert.Contains(t, spans[0].Attributes(), semconv.ClientAddress(tc.client))
		})
	}
}

func TestCallbacksRunOnlyWhenRecorded(t *testing.T) {
	t.Parallel()

	var spanCalls, metricCalls atomic.Int32
	spanCallback := func(fiber.Ctx) []attribute.KeyValue {
		spanCalls.Add(1)
		return nil
	}
	metricCallback := func(fiber.Ctx) []attribute.KeyValue {
		metricCalls.Add(1)
		return nil
	}

	app := fiber.New()
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))),
		WithMeterProvider(metricnoop.NewMeterProvider()),
		WithCustomResponseAttributes(spanCallback),
		WithSpanNameFormatter(func(c fiber.Ctx) string {
			spanCalls.Add(1)
			return c.Method()
		}),
		WithCustomMetricAttributes(metricCallback),
		WithCustomResponseMetricAttributes(metricCallback),
	))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	assert.Zero(t, spanCalls.Load(), "the span is not recording")
	assert.Zero(t, metricCalls.Load(), "no metric records")
}

func TestCapturedResponseHeadersSeeTraceHeaders(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	app := fiber.New()
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithoutMetrics(true),
		WithPropagators(propagation.TraceContext{}),
		WithTraceResponseHeader("X-Trace-Id"),
		WithCapturedResponseHeaders("traceparent", "X-Trace-Id"),
	))
	app.Get("/", func(c fiber.Ctx) error {
		c.Set("X-Trace-Id", "stale")
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	attrs := attribute.NewSet(spans[0].Attributes()...)
	for _, name := range []string{"traceparent", "x-trace-id"} {
		value, ok := attrs.Value(attribute.Key("http.response.header." + name))
		require.True(t, ok, name)
		assert.Equal(t, []string{resp.Header.Get(name)}, value.AsStringSlice(), name)
	}
	assert.Equal(t, spans[0].SpanContext().TraceID().String(), resp.Header.Get("X-Trace-Id"))
}

func TestUnixSocketPeer(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		client string
	}{
		{name: "named", client: "client.sock"},
		{name: "unnamed"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			sr := tracetest.NewSpanRecorder()
			app := fiber.New()
			app.Use(New(
				WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				WithoutMetrics(true),
			))
			app.Get("/", func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusNoContent)
			})

			server := &net.UnixAddr{Name: filepath.Join(dir, "server.sock"), Net: "unix"}
			listener, err := net.ListenUnix("unix", server)
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() {
				served <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
			}()
			t.Cleanup(func() {
				require.NoError(t, app.Shutdown())
				<-served
			})

			var local *net.UnixAddr
			if tc.client != "" {
				local = &net.UnixAddr{Name: filepath.Join(dir, tc.client), Net: "unix"}
			}
			conn, err := net.DialUnix("unix", local, server)
			require.NoError(t, err)
			defer conn.Close()
			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
			require.NoError(t, err)
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusNoContent, resp.StatusCode)

			spans := sr.Ended()
			require.Len(t, spans, 1)
			attrs := attribute.NewSet(spans[0].Attributes()...)
			assert.Contains(t, spans[0].Attributes(), semconv.NetworkTransportUnix)
			assert.False(t, attrs.HasValue(semconv.NetworkPeerPortKey))
			if local == nil {
				assert.False(t, attrs.HasValue(semconv.NetworkPeerAddressKey))
				assert.False(t, attrs.HasValue(semconv.ClientAddressKey))
				return
			}
			assert.Contains(t, spans[0].Attributes(), semconv.NetworkPeerAddress(local.Name))
			assert.Contains(t, spans[0].Attributes(), semconv.ClientAddress(local.Name))
		})
	}
}

func TestNoopTracerProviderLeavesOuterSpanAlone(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	var endedEarly bool
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		ctx, span := provider.Tracer("outer").Start(c.Context(), "outer")
		c.SetContext(ctx)
		err := c.Next()
		endedEarly = len(sr.Ended()) > 0
		span.End()
		return err
	})
	app.Use(New(
		WithTracerProvider(tracenoop.NewTracerProvider()),
		WithoutMetrics(true),
	))
	var handlerSpanContext oteltrace.SpanContext
	app.Get("/users/:id", func(c fiber.Ctx) error {
		handlerSpanContext = oteltrace.SpanContextFromContext(c.Context())
		return c.SendStatus(http.StatusInternalServerError)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/users/1", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.False(t, endedEarly, "the outer span was ended before its owner ended it")
	assert.Equal(t, "outer", spans[0].Name())
	assert.Empty(t, spans[0].Attributes())
	assert.Equal(t, codes.Unset, spans[0].Status().Code)
	// The handlers still see the outer span's context.
	assert.Equal(t, spans[0].SpanContext().SpanID(), handlerSpanContext.SpanID())
}

// Without a tracer, a public endpoint hands on no trace.
func TestPublicEndpointWithoutTracing(t *testing.T) {
	t.Parallel()

	_, traceparent := callerContext()

	app := fiber.New()
	app.Use(New(
		WithTracerProvider(tracenoop.NewTracerProvider()),
		WithoutMetrics(true),
		WithPropagators(propagation.TraceContext{}),
		WithPublicEndpoint(),
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

	assert.False(t, handlerSpanContext.IsValid())
	assert.Empty(t, resp.Header.Get("traceparent"))
}

// fasthttp's stamp on a rejected keep-alive request is the previous request's.
func TestRejectedRequestOnKeepAliveConnectionIsNotBackdated(t *testing.T) {
	t.Parallel()

	const idle = 300 * time.Millisecond

	sr := tracetest.NewSpanRecorder()
	app := fiber.New(fiber.Config{BodyLimit: 16})
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithoutMetrics(true),
	))
	app.All("/", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusNoContent)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() {
		served <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() {
		require.NoError(t, app.Shutdown())
		<-served
	})

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	reader := bufio.NewReader(conn)

	_, err = fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	time.Sleep(idle)

	body := strings.Repeat("x", 64)
	_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	require.NoError(t, err)
	resp, err = http.ReadResponse(reader, nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 2)
	assert.Less(t, spans[1].EndTime().Sub(spans[1].StartTime()), idle/2)
}

// A child of a cancelable parent is canceled on return, even for a body stream.
func TestStreamedResponseContextWithCancelableParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "asset.txt"), []byte("asset"), 0o600))

	testCases := []struct {
		name   string
		parent func(c fiber.Ctx) context.Context
	}{
		{name: "long-lived cancelable context", parent: func(fiber.Ctx) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			return ctx
		}},
		{name: "fasthttp request context", parent: func(c fiber.Ctx) context.Context {
			return c.RequestCtx()
		}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				c.SetContext(tc.parent(c))
				return c.Next()
			})
			app.Use(New(
				WithTracerProvider(sdktrace.NewTracerProvider()),
				WithoutMetrics(true),
			))
			handlerContexts := make(chan context.Context, 2)
			app.Get("/events", func(c fiber.Ctx) error {
				handlerContexts <- c.Context()
				return c.SendStreamWriter(func(w *bufio.Writer) {
					_, _ = w.WriteString("data: event\n\n")
				})
			})
			app.Get("/file", func(c fiber.Ctx) error {
				handlerContexts <- c.Context()
				return c.SendFile(filepath.Join(dir, "asset.txt"))
			})

			// fasthttp's request context is cancelable only under a listening server.
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() {
				served <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
			}()
			t.Cleanup(func() {
				require.NoError(t, app.Shutdown())
				<-served
			})

			for _, path := range []string{"/events", "/file"} {
				request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+listener.Addr().String()+path, nil)
				require.NoError(t, err)
				resp, err := http.DefaultClient.Do(request)
				require.NoError(t, err)
				_, err = io.Copy(io.Discard, resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, http.StatusOK, resp.StatusCode, path)
			}

			require.Len(t, handlerContexts, 2)
			for range 2 {
				assert.ErrorIs(t, (<-handlerContexts).Err(), context.Canceled)
			}
		})
	}
}

func TestResponseCallbackPanicKeepsRoute(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	app := fiber.New()
	app.Use(recoverer.New())
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithoutMetrics(true),
		WithCustomResponseAttributes(func(fiber.Ctx) []attribute.KeyValue {
			panic("callback failed")
		}),
	))
	app.Get("/users/:id", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/users/1", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET /users/:id", spans[0].Name())
	assert.Contains(t, spans[0].Attributes(), semconv.HTTPRoute("/users/:id"))
	assert.Contains(t, spans[0].Attributes(), semconv.ErrorTypeKey.String("response_callback_panic"))
}

// A panicking formatter is reported, and the default one names the span.
func TestSpanNameFormatterPanic(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	reader := metric.NewManualReader()
	app := fiber.New()
	app.Use(recoverer.New())
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
		WithSpanNameFormatter(func(fiber.Ctx) string {
			panic("formatter failed")
		}),
	))
	app.Get("/users/:id", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/users/1", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET /users/:id", spans[0].Name())
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.Contains(t, spans[0].Attributes(), semconv.ErrorTypeKey.String("response_callback_panic"))

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	durations := metricPoints(t, metrics.ScopeMetrics[0])[MetricNameHTTPServerRequestDuration]
	require.Len(t, durations, 1)
	errorType, ok := durations[0].Value(semconv.ErrorTypeKey)
	require.True(t, ok)
	assert.Equal(t, "response_callback_panic", errorType.AsString())
}

// A panicking metric attribute callback still ends the request's telemetry.
func TestMetricAttributesCallbackPanic(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}

	sr := tracetest.NewSpanRecorder()
	reader := metric.NewManualReader()

	var outerBefore, outerAfter context.Context
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxKey{}, "outer"))
		outerBefore = c.Context()
		err := c.Next()
		outerAfter = c.Context()
		return err
	})
	app.Use(recoverer.New())
	app.Use(New(
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
		WithCustomMetricAttributes(func(fiber.Ctx) []attribute.KeyValue {
			panic("callback failed")
		}),
	))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	assert.Same(t, outerBefore, outerAfter)
	require.NoError(t, outerAfter.Err())

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.Contains(t, spans[0].Attributes(), semconv.ErrorTypeKey.String("panic"))

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, point := range sum.DataPoints {
					assert.Zero(t, point.Value, m.Name)
				}
			}
		}
	}
}

func TestMiddleware_StoreTracerInContextWithPassLocalsToContext(t *testing.T) {
	app := fiber.New(fiber.Config{PassLocalsToContext: true})
	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		tracerFromContext, ok := fiber.ValueFromContext[oteltrace.Tracer](c.Context(), tracerKey)
		require.True(t, ok)
		require.NotNil(t, tracerFromContext)
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMiddleware_StaticAssetsDoNotHang(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Clean(filepath.Join(dir, "repro.css")), []byte("body{font-family:sans-serif;}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Clean(filepath.Join(dir, "repro.js")), []byte("console.log('ok');"), 0o644))

	app := fiber.New()
	app.Use(New())
	app.Use("/public", static.New(dir))

	testCases := []struct {
		path        string
		contentType string
		body        string
	}{
		{path: "/public/repro.css", contentType: "text/css", body: "body{font-family:sans-serif;}"},
		{path: "/public/repro.js", contentType: "javascript", body: "console.log('ok');"},
	}

	for i := 0; i < 25; i++ {
		for _, tc := range testCases {
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tc.path, nil))
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)

			body, readErr := io.ReadAll(resp.Body)
			require.NoError(t, resp.Body.Close())
			require.NoError(t, readErr)
			require.Equal(t, tc.body, string(body))
			require.Contains(t, resp.Header.Get("Content-Type"), tc.contentType)
		}
	}
}

// Replacing a chunked upload's stream recycles it, leaving the handler a cleared
// reader. A real listener is needed because app.Test cannot send chunked bodies.
func TestMiddleware_StreamedChunkedUploadIsNotRecycled(t *testing.T) {
	t.Parallel()

	const uploadSize = 4096

	var (
		mu         sync.Mutex
		bytesRead  int
		streamNil  bool
		panicValue any
	)

	app := fiber.New(fiber.Config{StreamRequestBody: true})
	app.Use(New())
	app.Post("/upload", func(c fiber.Ctx) error {
		defer func() {
			if recovered := recover(); recovered != nil {
				mu.Lock()
				panicValue = recovered
				mu.Unlock()
			}
		}()

		stream := c.Request().BodyStream()
		if stream == nil {
			mu.Lock()
			streamNil = true
			mu.Unlock()

			return c.SendStatus(http.StatusNoContent)
		}

		body, err := io.ReadAll(stream)
		if err != nil {
			return err
		}

		mu.Lock()
		bytesRead = len(body)
		mu.Unlock()

		return c.SendStatus(http.StatusNoContent)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	served := make(chan error, 1)
	go func() {
		served <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() {
		require.NoError(t, app.Shutdown())
		<-served
	})

	// Unknown length makes net/http use chunked encoding, so no Content-Length.
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/upload", io.NopCloser(bytes.NewReader(make([]byte, uploadSize))))
	require.NoError(t, err)
	request.ContentLength = -1

	resp, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	_, copyErr := io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, copyErr)

	mu.Lock()
	defer mu.Unlock()
	require.Nil(t, panicValue, "handler panicked reading a recycled request stream")
	require.False(t, streamNil, "request body stream was replaced or dropped")
	require.Equal(t, uploadSize, bytesRead)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// Pins down which readers may be measured without being read.
func TestBodyStreamSize(t *testing.T) {
	t.Parallel()

	payload := make([]byte, 2048)

	// Peek fills 64 of the 2048 bytes, so Len understates the body by 1984.
	buffered := bufio.NewReaderSize(bytes.NewReader(payload), 64)
	_, err := buffered.Peek(64)
	require.NoError(t, err)
	require.Equal(t, 64, buffered.Buffered())

	tests := []struct {
		name   string
		stream io.Reader
		size   int64
		known  bool
	}{
		{name: "nil", stream: nil},
		{name: "bytes.Reader", stream: bytes.NewReader(payload), size: 2048, known: true},
		{name: "bytes.Buffer", stream: bytes.NewBuffer(payload), size: 2048, known: true},
		{name: "strings.Reader", stream: strings.NewReader(string(payload)), size: 2048, known: true},
		{name: "io.LimitedReader", stream: &io.LimitedReader{R: bytes.NewReader(payload), N: 512}, size: 512, known: true},
		{name: "io.LimitedReader with negative N", stream: &io.LimitedReader{R: bytes.NewReader(payload), N: -1}},
		{name: "bufio.Reader", stream: buffered},
		{name: "opaque reader", stream: io.NopCloser(bytes.NewReader(payload))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			size, known := bodyStreamSize(tt.stream)
			require.Equal(t, tt.known, known)
			require.Equal(t, tt.size, size)
		})
	}
}

// A HEAD response keeps the Content-Length a GET would return, but sends no bytes.
func TestMiddleware_HeadStreamedResponseReportsNoBody(t *testing.T) {
	t.Parallel()

	const payloadSize = 2048

	reader := metric.NewManualReader()

	app := fiber.New()
	app.Use(New(WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))))
	app.All("/stream", func(c fiber.Ctx) error {
		return c.SendStream(bytes.NewReader(make([]byte, payloadSize)), payloadSize)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodHead, "/stream", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, strconv.Itoa(payloadSize), resp.Header.Get("Content-Length"))

	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, readErr)
	require.Empty(t, body, "fasthttp must not write a body for HEAD")

	metrics := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &metrics))

	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricNameHTTPServerResponseBodySize {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			require.Len(t, histogram.DataPoints, 1)
			require.Zero(t, histogram.DataPoints[0].Sum, "HEAD response reported a body it never sent")

			return
		}
	}

	t.Fatal("response body size metric not found")
}

// A handler sets SkipBody itself, which neither method nor status reveals.
func TestMiddleware_SkipBodyResponseReportsNoBody(t *testing.T) {
	t.Parallel()

	const payloadSize = 2048

	reader := metric.NewManualReader()

	app := fiber.New()
	app.Use(New(WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))))
	app.Get("/stream", func(c fiber.Ctx) error {
		if err := c.SendStream(bytes.NewReader(make([]byte, payloadSize)), payloadSize); err != nil {
			return err
		}
		c.Response().SkipBody = true

		return nil
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/stream", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, strconv.Itoa(payloadSize), resp.Header.Get("Content-Length"))

	// The client's read ends short; what matters is that no bytes reached the wire.
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Empty(t, body, "fasthttp must not write a body when SkipBody is set")

	metrics := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &metrics))

	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricNameHTTPServerResponseBodySize {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			require.Len(t, histogram.DataPoints, 1)
			require.Zero(t, histogram.DataPoints[0].Sum, "skipped response reported a body it never sent")

			return
		}
	}

	t.Fatal("response body size metric not found")
}

// A client declares a large body and sends only the part fasthttp pre-reads.
func TestMiddleware_StreamedRequestIgnoresDeclaredLength(t *testing.T) {
	t.Parallel()

	const (
		declaredSize = 1 << 20
		bodyLimit    = 32 * 1024
		actuallySent = 8 * 1024
	)

	reader := metric.NewManualReader()

	app := fiber.New(fiber.Config{StreamRequestBody: true, BodyLimit: bodyLimit})
	app.Use(New(WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))))
	app.Post("/upload", func(c fiber.Ctx) error {
		// Reject without draining, as an upload guard would.
		return c.SendStatus(http.StatusRequestEntityTooLarge)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	served := make(chan error, 1)
	go func() {
		served <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() {
		require.NoError(t, app.Shutdown())
		<-served
	})

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)

	_, err = conn.Write([]byte("POST /upload HTTP/1.1\r\nHost: x\r\nContent-Length: " +
		strconv.Itoa(declaredSize) + "\r\n\r\n" + strings.Repeat("x", actuallySent)))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, statusLine, "413")
	require.NoError(t, conn.Close())

	metrics := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &metrics))

	// Count observations, not sums: a recorded zero is as wrong as a megabyte.
	// Scoping by route excludes fasthttp's error-path replay.
	observations := uint64(0)
	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricNameHTTPServerRequestBodySize {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			for _, point := range histogram.DataPoints {
				if route, found := point.Attributes.Value(semconv.HTTPRouteKey); found && route.AsString() == "/upload" {
					observations += point.Count
				}
			}
		}
	}

	require.Zero(t, observations,
		"streamed request reported a body size; the declared length is not a measurement")
}

func TestMiddleware_NotFoundPathDoesNotHang(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())

	for i := 0; i < 25; i++ {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/.well-known/appspecific/com.chrome.devtools.json", nil))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)

		_, readErr := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, readErr)
	}
}

func newRequestHeader(t *testing.T, normalize bool, headers ...[2]string) *fasthttp.RequestHeader {
	t.Helper()

	header := &fasthttp.RequestHeader{}
	if !normalize {
		header.DisableNormalizing()
	}
	for _, kv := range headers {
		header.Add(kv[0], kv[1])
	}

	return header
}

func TestRequestCarrier(t *testing.T) {
	t.Parallel()

	header := newRequestHeader(t, true,
		[2]string{"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		[2]string{"Baggage", "a=1"},
		[2]string{"baggage", "b=2"},
	)
	carrier := (*requestCarrier)(header)

	// Names are normalized on both sides, so any spelling finds the header.
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("traceparent"))
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("TraceParent"))
	require.Empty(t, carrier.Get("tracestate"))
	require.Equal(t, []string{"a=1", "b=2"}, carrier.Values("baggage"))
	require.Nil(t, carrier.Values("tracestate"))
	require.ElementsMatch(t, []string{"Traceparent", "Baggage", "Baggage"}, carrier.Keys())

	carrier.Set("traceparent", "overwritten")
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("traceparent"))
}

// Propagators keep values, so they must not alias fasthttp's buffers.
func TestRequestCarrierCopiesValues(t *testing.T) {
	t.Parallel()

	header := newRequestHeader(t, true, [2]string{"tracestate", "vendor=value"})
	carrier := (*requestCarrier)(header)

	value := carrier.Get("tracestate")
	values := carrier.Values("tracestate")
	header.Set("tracestate", "XXXXXX=XXXXX")

	require.Equal(t, "vendor=value", value)
	require.Equal(t, []string{"vendor=value"}, values)
}

// With DisableHeaderNormalizing, names must still match case-insensitively.
func TestFoldingRequestCarrier(t *testing.T) {
	t.Parallel()

	header := newRequestHeader(t, false,
		[2]string{"Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		[2]string{"BAGGAGE", "a=1"},
		[2]string{"baggage", "b=2"},
	)
	require.Empty(t, header.Peek("traceparent"), "the exact match the carrier falls back from")

	carrier := (*foldingRequestCarrier)(header)
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("traceparent"))
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("Traceparent"))
	require.Empty(t, carrier.Get("tracestate"))
	require.Equal(t, []string{"a=1", "b=2"}, carrier.Values("baggage"))
	require.Nil(t, carrier.Values("tracestate"))
	require.ElementsMatch(t, []string{"Traceparent", "BAGGAGE", "baggage"}, carrier.Keys())

	carrier.Set("traceparent", "overwritten")
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("traceparent"))
}

func TestResponseCarrier(t *testing.T) {
	t.Parallel()

	header := &fasthttp.ResponseHeader{}
	header.Set("Traceparent", "stale")
	carrier := (*responseCarrier)(header)

	propagation.TraceContext{}.Inject(t.Context(), carrier)
	require.Equal(t, "stale", carrier.Get("traceparent"), "an invalid span context injects nothing")

	carrier.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	carrier.Set("tracestate", "vendor=value")
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", string(header.Peek("Traceparent")))
	require.Equal(t, "vendor=value", carrier.Get("Tracestate"))
	require.Subset(t, carrier.Keys(), []string{"Traceparent", "Tracestate"})
}

func TestAppendRedactedQuery(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		query string
		extra []string
		want  string
	}{
		{name: "plain", query: "q=otel&page=2", want: "q=otel&page=2"},
		{name: "single sensitive", query: "sig=abc", want: "sig=REDACTED"},
		{
			name:  "pre-signed S3",
			query: "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA%2F20260101&X-Amz-Signature=deadbeef&X-Amz-Security-Token=tok",
			want:  "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=REDACTED&X-Amz-Signature=REDACTED&X-Amz-Security-Token=REDACTED",
		},
		{name: "legacy S3", query: "AWSAccessKeyId=AKIA&Signature=c2ln&Expires=1", want: "AWSAccessKeyId=REDACTED&Signature=REDACTED&Expires=1"},
		{name: "GCS", query: "X-Goog-Signature=abc&alt=media", want: "X-Goog-Signature=REDACTED&alt=media"},
		{name: "repeated key", query: "sig=a&sig=b", want: "sig=REDACTED&sig=REDACTED"},
		// Matching is case-sensitive, as the semantic conventions specify.
		{name: "other case", query: "SIG=abc&signature=def", want: "SIG=abc&signature=def"},
		{name: "empty value kept", query: "sig=&q=1", want: "sig=&q=1"},
		{name: "key without value", query: "sig&q=1", want: "sig&q=1"},
		{name: "value holding equals", query: "sig=a=b&q=1", want: "sig=REDACTED&q=1"},
		{name: "empty pairs kept", query: "&&sig=a&", want: "&&sig=REDACTED&"},
		{name: "key as value", query: "q=sig", want: "q=sig"},
		{name: "encoded name", query: "s%69g=abc&q=1", want: "s%69g=REDACTED&q=1"},
		{name: "invalid escape kept", query: "s%zzg=abc", want: "s%zzg=abc"},
		{name: "extra name", query: "token=abc&q=1", extra: []string{"token"}, want: "token=REDACTED&q=1"},
		{name: "encoded extra name", query: "to%6Ben=abc", extra: []string{"token"}, want: "to%6Ben=REDACTED"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, string(appendRedactedQuery(nil, []byte(tc.query), tc.extra)))
			// Appending to a buffer keeps what was already there.
			require.Equal(t, "x"+tc.want, string(appendRedactedQuery([]byte("x"), []byte(tc.query), tc.extra)))
		})
	}
}

func TestOptionCacheIsBounded(t *testing.T) {
	t.Parallel()

	var cache optionCache[int, string]
	for i := range maxCachedSets + 10 {
		cache.store(i, "set")
	}
	require.Equal(t, int32(maxCachedSets), cache.size.Load())

	value, ok := cache.load(0)
	require.True(t, ok)
	require.Equal(t, "set", value)

	_, ok = cache.load(maxCachedSets + 5)
	require.False(t, ok, "a key past the bound is built per request instead")
}

// A key is stored once: a racing store neither replaces it nor counts twice.
func TestOptionCacheKeepsFirstValue(t *testing.T) {
	t.Parallel()

	var cache optionCache[string, int]

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			cache.store("GET /users/:id 200", i)
			_, ok := cache.load("GET /users/:id 200")
			assert.True(t, ok)
		})
	}
	wg.Wait()

	require.Equal(t, int32(1), cache.size.Load())
}

func TestSpanStatusFromHTTPStatusCodeAndSpanKind(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		code    int
		kind    oteltrace.SpanKind
		status  codes.Code
		message string
	}{
		{name: "below range", code: 99, kind: oteltrace.SpanKindServer, status: codes.Error, message: "Invalid HTTP status code 99"},
		{name: "zero", code: 0, kind: oteltrace.SpanKindServer, status: codes.Error, message: "Invalid HTTP status code 0"},
		{name: "above range", code: 600, kind: oteltrace.SpanKindClient, status: codes.Error, message: "Invalid HTTP status code 600"},
		{name: "informational", code: http.StatusSwitchingProtocols, kind: oteltrace.SpanKindServer, status: codes.Unset},
		{name: "ok", code: http.StatusOK, kind: oteltrace.SpanKindClient, status: codes.Unset},
		// Valid codes without a registered reason phrase are statuses all the same.
		{name: "unnamed 2xx", code: 299, kind: oteltrace.SpanKindServer, status: codes.Unset},
		{name: "unused 306", code: 306, kind: oteltrace.SpanKindClient, status: codes.Unset},
		{name: "server 4xx", code: http.StatusBadRequest, kind: oteltrace.SpanKindServer, status: codes.Unset},
		{name: "server unnamed 4xx", code: 499, kind: oteltrace.SpanKindServer, status: codes.Unset},
		{name: "client 4xx", code: http.StatusNotFound, kind: oteltrace.SpanKindClient, status: codes.Error},
		{name: "server 5xx", code: http.StatusInternalServerError, kind: oteltrace.SpanKindServer, status: codes.Error},
		{name: "server unnamed 5xx", code: 520, kind: oteltrace.SpanKindServer, status: codes.Error},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status, message := internal.SpanStatusFromHTTPStatusCodeAndSpanKind(tc.code, tc.kind)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.message, message)
		})
	}
}

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
		opts = append(opts, WithMeterProvider(metric.NewMeterProvider(metric.WithReader(metric.NewManualReader()))))
	} else {
		opts = append(opts, WithMeterProvider(metricnoop.NewMeterProvider()))
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
