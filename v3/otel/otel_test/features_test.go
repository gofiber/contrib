package otel_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	fiberotel "github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// newTracedApp returns an app instrumented with opts and a recorder of its spans.
func newTracedApp(t *testing.T, config fiber.Config, opts ...fiberotel.Option) (*fiber.App, *tracetest.SpanRecorder) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	opts = append([]fiberotel.Option{
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
		fiberotel.WithoutMetrics(true),
	}, opts...)

	app := fiber.New(config)
	app.Use(fiberotel.New(opts...))
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
		opts   []fiberotel.Option
		public bool
	}{
		{name: "default continues the caller's trace"},
		{name: "public endpoint", opts: []fiberotel.Option{fiberotel.WithPublicEndpoint()}, public: true},
		{name: "decided per request", opts: []fiberotel.Option{fiberotel.WithPublicEndpointFn(func(c fiber.Ctx) bool {
			return c.Get("X-Internal") == ""
		})}, public: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, sr := newTracedApp(t, fiber.Config{}, append(tc.opts, fiberotel.WithPropagators(propagation.TraceContext{}))...)

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
			fiberotel.WithPropagators(propagation.TraceContext{}),
			fiberotel.WithPublicEndpointFn(func(c fiber.Ctx) bool {
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

		app, sr := newTracedApp(t, fiber.Config{}, fiberotel.WithPublicEndpoint())
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
	app, sr := newTracedApp(t, fiber.Config{}, fiberotel.WithSpanStartOptions(
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
			app.Use(fiberotel.New(
				fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
				fiberotel.WithoutMetrics(true),
				// Matched case-insensitively, recorded lowercased, captured once.
				fiberotel.WithCapturedRequestHeaders("X-Request-ID", "accept", "X-Missing", "x-request-id"),
				fiberotel.WithCapturedResponseHeaders("x-cache", "Content-Type", "X-Absent"),
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

	app, sr := newTracedApp(t, fiber.Config{}, fiberotel.WithRedactedQueryParams("token", "api_key"))

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

			opts := []fiberotel.Option{
				fiberotel.WithPropagators(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})),
			}
			if tc.propagators != nil {
				opts = append(opts, fiberotel.WithResponsePropagators(tc.propagators))
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
		opts []fiberotel.Option
		port int
	}{
		{name: "named by Host", host: "example.com:8443", port: 8443},
		{name: "IPv6 Host", host: "[::1]:9090", port: 9090},
		{name: "not named by Host", host: "example.com"},
		{name: "configured", host: "example.com:8443", opts: []fiberotel.Option{fiberotel.WithPort(443)}, port: 443},
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
		opts     []fiberotel.Option
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
		{name: "collection disabled", opts: []fiberotel.Option{fiberotel.WithClientIP(false)}},
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
	app.Use(fiberotel.New(
		fiberotel.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))),
		fiberotel.WithMeterProvider(metricnoop.NewMeterProvider()),
		fiberotel.WithCustomResponseAttributes(spanCallback),
		fiberotel.WithSpanNameFormatter(func(c fiber.Ctx) string {
			spanCalls.Add(1)
			return c.Method()
		}),
		fiberotel.WithCustomMetricAttributes(metricCallback),
		fiberotel.WithCustomResponseMetricAttributes(metricCallback),
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
