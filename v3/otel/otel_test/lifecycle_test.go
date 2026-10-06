package otel_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fiberotel "github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

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
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(tracenoop.NewTracerProvider()),
		fiberotel.WithoutMetrics(true),
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
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(tracenoop.NewTracerProvider()),
		fiberotel.WithoutMetrics(true),
		fiberotel.WithPropagators(propagation.TraceContext{}),
		fiberotel.WithPublicEndpoint(),
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
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithoutMetrics(true),
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
			app.Use(fiberotel.New(
				fiberotel.WithTracerProvider(sdktrace.NewTracerProvider()),
				fiberotel.WithoutMetrics(true),
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
	app.Use(recover.New())
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithoutMetrics(true),
		fiberotel.WithCustomResponseAttributes(func(fiber.Ctx) []attribute.KeyValue {
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
	app.Use(recover.New())
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
		fiberotel.WithSpanNameFormatter(func(fiber.Ctx) string {
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
	durations := metricPoints(t, metrics.ScopeMetrics[0])[fiberotel.MetricNameHTTPServerRequestDuration]
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
	app.Use(recover.New())
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader))),
		fiberotel.WithCustomMetricAttributes(func(fiber.Ctx) []attribute.KeyValue {
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
