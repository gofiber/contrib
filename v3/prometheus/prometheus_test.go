package prometheus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/gofiber/fiber/v3"
	recoverer "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/model"
	"go.opentelemetry.io/otel"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
)

var noTimeoutConfig = fiber.TestConfig{Timeout: 0}

func getMetrics(t *testing.T, app *fiber.App, path string) string {
	t.Helper()

	if path == "" {
		path = "/metrics"
	}

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}

	return string(body)
}

func newAppWithMiddleware(cfg Config, metricsPath string) *fiber.App {
	if metricsPath != "" {
		cfg.MetricsPath = metricsPath
	}

	app := fiber.New()
	app.Use(New(cfg))

	return app
}

func TestMiddlewareRecordsMetrics(t *testing.T) {
	app := newAppWithMiddleware(Config{ServiceName: "test-service"}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Post("/payload", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	payloadReq := httptest.NewRequest(fiber.MethodPost, "/payload", strings.NewReader("hello world"))
	payloadReq.Header.Set("Content-Type", "text/plain")
	if _, err := app.Test(payloadReq, noTimeoutConfig); err != nil {
		t.Fatalf("unexpected payload request error: %v", err)
	}

	metricsResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if metricsResp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", metricsResp.StatusCode)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}
	metrics := string(body)
	if !strings.Contains(metrics, "http_requests_total") {
		t.Fatalf("expected metrics to contain request counter, got %q", metrics)
	}
	if !strings.Contains(metrics, "path=\"/hello\"") {
		t.Fatalf("expected metrics to contain path label, got %q", metrics)
	}
	if !strings.Contains(metrics, "service=\"test-service\"") {
		t.Fatalf("expected metrics to include service label, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_request_size_bytes_sum") {
		t.Fatalf("expected metrics to contain request size histogram, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_response_size_bytes_sum") {
		t.Fatalf("expected metrics to contain response size histogram, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_requests_status_class_total") {
		t.Fatalf("expected metrics to contain status class counter")
	}
	if !strings.Contains(metrics, "http_requests_in_progress{method=\"GET\",service=\"test-service\"}") {
		t.Fatalf("expected in-flight gauge to include the method label, got %q", metrics)
	}
}

func TestDefaultRuntimeCollectorsEnabled(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")

	metrics := getMetrics(t, app, "/metrics")

	if !strings.Contains(metrics, "go_goroutines") {
		t.Fatalf("expected Go collector metrics, got %q", metrics)
	}

	if !strings.Contains(metrics, "process_cpu_seconds_total") {
		t.Fatalf("expected process collector metrics, got %q", metrics)
	}
}

func TestSkipURIs(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/skip"}}, "")
	app.Get("/skip", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/kept", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/skip", "/kept"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	// getMetrics fails the test on a non-200, which matters here: every
	// assertion below is a negative, so a scrape that errored out would satisfy
	// all of them. The positive control on /kept closes the other half of the
	// gap - a middleware recording nothing at all would pass too.
	metrics := getMetrics(t, app, "")

	if !strings.Contains(metrics, "path=\"/kept\"") {
		t.Fatalf("expected the unfiltered route to be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, "path=\"/skip\"") {
		t.Fatalf("expected skip path to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_request_size_bytes_sum{method=\"GET\",path=\"/skip\",status_code=\"200\"}") {
		t.Fatalf("expected skip path request size metric to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_response_size_bytes_sum{method=\"GET\",path=\"/skip\",status_code=\"200\"}") {
		t.Fatalf("expected skip path response size metric to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_requests_status_class_total{method=\"GET\",path=\"/skip\",status_class=\"2xx\"}") {
		t.Fatalf("expected skip path status class metric to be excluded")
	}
}

func TestSkipStatusCodes(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipStatusCodes: []int{fiber.StatusUnauthorized}}, "")
	app.Get("/deny", func(c fiber.Ctx) error {
		return fiber.ErrUnauthorized
	})
	app.Get("/allow", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/deny", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/allow", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	// Scrape through getMetrics and assert a positive, as in TestSkipURIs.
	metrics := getMetrics(t, app, "")

	if !strings.Contains(metrics, "status_code=\"200\"") {
		t.Fatalf("expected the unfiltered status to be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, "status_code=\"401\"") {
		t.Fatalf("expected status code 401 to be ignored, got %q", metrics)
	}
	if strings.Contains(metrics, "http_request_size_bytes_sum{method=\"GET\",path=\"/deny\",status_code=\"401\"}") {
		t.Fatalf("expected ignored status code request size metric to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_response_size_bytes_sum{method=\"GET\",path=\"/deny\",status_code=\"401\"}") {
		t.Fatalf("expected ignored status code response size metric to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_requests_status_class_total{method=\"GET\",path=\"/deny\",status_class=\"4xx\"}") {
		t.Fatalf("expected ignored status code status class metric to be excluded")
	}
}

// gaugeValue returns the sample value of the given fully-qualified series,
// failing the test when the series is missing.
func gaugeValue(t *testing.T, metrics, series string) float64 {
	t.Helper()

	for _, line := range strings.Split(metrics, "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found || name != series {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("parsing value of %s: %v", series, err)
		}
		return parsed
	}

	t.Fatalf("expected series %s to be present, got %q", series, metrics)
	return 0
}

// TestInFlightGaugeIsBalanced covers requests that are dropped from
// instrumentation for every reason the middleware supports. Each of them still
// has to leave the in-flight gauge where it found it.
func TestInFlightGaugeIsBalanced(t *testing.T) {
	app := newAppWithMiddleware(Config{
		SkipStatusCodes: []int{fiber.StatusUnauthorized},
		SkipURIs:        []string{"/skip"},
	}, "")
	app.Get("/deny", func(c fiber.Ctx) error {
		return fiber.ErrUnauthorized
	})
	app.Get("/skip", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/deny", "/skip", "/ok", "/unmatched", "/deny", "/skip"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error for %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if value := gaugeValue(t, metrics, "http_requests_in_progress{method=\"GET\"}"); value != 0 {
		t.Fatalf("expected in-flight gauge to settle back to 0, got %v", value)
	}
}

func TestCustomHistogramBuckets(t *testing.T) {
	cfg := Config{
		RequestDurationBuckets: []float64{0.1, 0.2},
		RequestSizeBuckets:     []float64{111, 222},
		ResponseSizeBuckets:    []float64{333, 444},
	}
	app := newAppWithMiddleware(cfg, "")
	app.Post("/bucket", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req := httptest.NewRequest(fiber.MethodPost, "/bucket", strings.NewReader(strings.Repeat("a", 150)))
	req.Header.Set("Content-Type", "text/plain")
	if _, err := app.Test(req, noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")

	if strings.Contains(metrics, "le=\"0.005\"") {
		t.Fatalf("expected default duration buckets to be replaced, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_request_duration_seconds_bucket{method=\"POST\",path=\"/bucket\",status_code=\"200\",le=\"0.2\"}") {
		t.Fatalf("expected custom duration buckets in metrics, got %q", metrics)
	}

	if strings.Contains(metrics, "le=\"5242880\"") {
		t.Fatalf("expected default size buckets to be replaced, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_request_size_bytes_bucket{method=\"POST\",path=\"/bucket\",status_code=\"200\",le=\"111\"}") {
		t.Fatalf("expected custom request size buckets in metrics, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_response_size_bytes_bucket{method=\"POST\",path=\"/bucket\",status_code=\"200\",le=\"333\"}") {
		t.Fatalf("expected custom response size buckets in metrics, got %q", metrics)
	}
}

func TestTrackUnmatchedRequestsDisabled(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/unmatched", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected status 404, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "/metrics")
	if strings.Contains(metrics, "path=\"/__unmatched__\"") {
		t.Fatalf("expected unmatched routes to be excluded when tracking disabled, got %q", metrics)
	}
}

func TestTrackUnmatchedRequestsEnabled(t *testing.T) {
	app := newAppWithMiddleware(Config{TrackUnmatchedRequests: true}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/unmatched", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected status 404, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "/metrics")
	if !strings.Contains(metrics, "http_requests_total{method=\"GET\",path=\"/__unmatched__\",status_code=\"404\"") {
		t.Fatalf("expected unmatched route request counter to include fallback label, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_requests_status_class_total{method=\"GET\",path=\"/__unmatched__\",status_class=\"4xx\"}") {
		t.Fatalf("expected unmatched route status class counter to include fallback label, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_request_duration_seconds_sum{method=\"GET\",path=\"/__unmatched__\",status_code=\"404\"") {
		t.Fatalf("expected unmatched route duration histogram to include fallback label, got %q", metrics)
	}
}

func TestRoutesRefreshAfterInitialRequest(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/late", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected status 404 for late route before registration, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "path=\"/late\"") {
		t.Fatalf("expected metrics to exclude late route before registration, got %q", metrics)
	}

	app.Get("/late", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/late", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error after registering route: %v", err)
	}

	metrics = getMetrics(t, app, "")
	if !strings.Contains(metrics, "http_requests_total{method=\"GET\",path=\"/late\",status_code=\"200\"}") {
		t.Fatalf("expected metrics to include late-registered route, got %q", metrics)
	}
}

func TestNextSkipsInstrumentation(t *testing.T) {
	app := newAppWithMiddleware(Config{
		Next: func(c fiber.Ctx) bool {
			return c.Path() == "/healthz"
		},
	}, "")
	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/kept", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/healthz", "/kept"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	// Scrape through getMetrics and assert a positive, as in TestSkipURIs.
	metrics := getMetrics(t, app, "")

	if !strings.Contains(metrics, "path=\"/kept\"") {
		t.Fatalf("expected the unfiltered route to be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, "path=\"/healthz\"") {
		t.Fatalf("expected next-skipped path to be excluded, got %q", metrics)
	}
}

func TestCustomMetricsPath(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "/internal/metrics")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	req := httptest.NewRequest(fiber.MethodGet, "/internal/metrics", nil)
	resp, err := app.Test(req, noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}
	metrics := string(body)
	if !strings.Contains(metrics, "path=\"/hello\"") {
		t.Fatalf("expected request metrics to be recorded, got %q", metrics)
	}
}

func TestMetricsEndpointAllowsHead(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics with HEAD: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestMetricsEndpointRejectsOtherMethods(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("posting metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusMethodNotAllowed {
		t.Fatalf("expected status 405, got %d", resp.StatusCode)
	}
	// RFC 9110 requires Allow on a 405, and setting it is the only reason
	// serveMetrics does not simply return the error.
	if allow := resp.Header.Get(fiber.HeaderAllow); allow != "GET, HEAD" {
		t.Fatalf("expected Allow header %q, got %q", "GET, HEAD", allow)
	}
}

func TestHeadRequestsMatchGetRoutes(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")

	app.Get("/head-get", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/head-get", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected HEAD request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")

	if !strings.Contains(metrics, "http_requests_total{method=\"HEAD\",path=\"/head-get\",status_code=\"200\"}") {
		t.Fatalf("expected HEAD request counter to be emitted, got %q", metrics)
	}

	if !strings.Contains(metrics, "http_request_duration_seconds_count{method=\"HEAD\",path=\"/head-get\",status_code=\"200\"}") {
		t.Fatalf("expected HEAD request duration histogram to be emitted, got %q", metrics)
	}

	if !strings.Contains(metrics, "http_requests_in_progress{method=\"HEAD\"}") {
		t.Fatalf("expected HEAD request in-flight gauge to be emitted, got %q", metrics)
	}
}

func TestCustomRegistry(t *testing.T) {
	registry := prometheus.NewRegistry()
	app := newAppWithMiddleware(Config{Registerer: registry}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metricsResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}
	metrics := string(body)
	if !strings.Contains(metrics, "http_requests_total") {
		t.Fatalf("expected metrics to be produced, got %q", metrics)
	}

	// Reading the endpoint alone would pass even if Registerer were ignored and
	// a private registry used instead, so assert against the supplied registry.
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering from the supplied registry: %v", err)
	}
	var found bool
	for _, family := range families {
		if family.GetName() == "http_requests_total" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected the supplied Registerer to hold the metrics")
	}
}

func TestRegistererWithoutGathererPanics(t *testing.T) {
	baseRegistry := prometheus.NewRegistry()
	registerer := prometheus.WrapRegistererWithPrefix("custom_", baseRegistry)

	defer func() {
		if r := recover(); r != nil {
			message := fmt.Sprint(r)
			if !strings.Contains(message, "Registerer does not implement prometheus.Gatherer") {
				t.Fatalf("expected panic about missing Gatherer, got %q", message)
			}
			return
		}
		t.Fatal("expected panic when Registerer does not implement Gatherer")
	}()

	_ = New(Config{Registerer: registerer})
}

func TestEnableOpenMetricsNegotiation(t *testing.T) {
	app := newAppWithMiddleware(Config{EnableOpenMetrics: true}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")

	resp, err := app.Test(req, noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/openmetrics-text") {
		t.Fatalf("expected OpenMetrics content type, got %q", contentType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}
	metrics := string(body)
	if !strings.Contains(metrics, "# EOF") {
		t.Fatalf("expected OpenMetrics EOF marker, got %q", metrics)
	}
}

func TestEnableOpenMetricsTextCreatedSamples(t *testing.T) {
	app := newAppWithMiddleware(Config{
		EnableOpenMetrics:                   true,
		EnableOpenMetricsTextCreatedSamples: true,
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")

	resp, err := app.Test(req, noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}
	metrics := string(body)
	if !strings.Contains(metrics, "_created") {
		t.Fatalf("expected created samples in OpenMetrics output, got %q", metrics)
	}
}

func TestDisableCompression(t *testing.T) {
	app := newAppWithMiddleware(Config{DisableCompression: true}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := app.Test(req, noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" {
		t.Fatalf("expected compression to be disabled, got %q", encoding)
	}
}

func TestDisableGoCollector(t *testing.T) {
	app := newAppWithMiddleware(Config{DisableGoCollector: true}, "")

	metrics := getMetrics(t, app, "")

	if strings.Contains(metrics, "go_goroutines") {
		t.Fatalf("expected Go collector metrics to be disabled, got %q", metrics)
	}

	if !strings.Contains(metrics, "process_cpu_seconds_total") {
		t.Fatalf("expected process collector metrics to remain enabled, got %q", metrics)
	}
}

func TestDisableProcessCollector(t *testing.T) {
	app := newAppWithMiddleware(Config{DisableProcessCollector: true}, "")

	metrics := getMetrics(t, app, "")

	if strings.Contains(metrics, "process_cpu_seconds_total") {
		t.Fatalf("expected process collector metrics to be disabled, got %q", metrics)
	}

	if !strings.Contains(metrics, "go_goroutines") {
		t.Fatalf("expected Go collector metrics to remain enabled, got %q", metrics)
	}
}

func TestStatusClassMetrics(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")

	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	app.Get("/bad", func(c fiber.Ctx) error {
		return fiber.ErrBadRequest
	})

	app.Get("/boom", func(c fiber.Ctx) error {
		return fiber.ErrInternalServerError
	})

	requests := []*http.Request{
		httptest.NewRequest(fiber.MethodGet, "/ok", nil),
		httptest.NewRequest(fiber.MethodGet, "/bad", nil),
		httptest.NewRequest(fiber.MethodGet, "/boom", nil),
	}

	for _, req := range requests {
		if _, err := app.Test(req, noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error: %v", err)
		}
	}

	metrics := getMetrics(t, app, "")
	found := map[string]bool{}
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.Contains(line, "http_requests_status_class_total") {
			continue
		}

		switch {
		case strings.Contains(line, "status_class=\"2xx\"") && strings.Contains(line, "path=\"/ok\"") && strings.Contains(line, "method=\"GET\""):
			found["2xx"] = true
		case strings.Contains(line, "status_class=\"4xx\"") && strings.Contains(line, "path=\"/bad\"") && strings.Contains(line, "method=\"GET\""):
			found["4xx"] = true
		case strings.Contains(line, "status_class=\"5xx\"") && strings.Contains(line, "path=\"/boom\"") && strings.Contains(line, "method=\"GET\""):
			found["5xx"] = true
		}
	}

	for _, class := range []string{"2xx", "4xx", "5xx"} {
		if !found[class] {
			t.Fatalf("expected status class %s metric to be present", class)
		}
	}
}

func TestSizeHistogramsIncludeTraceExemplars(t *testing.T) {
	prev := otel.GetTracerProvider()
	tp := tracesdk.NewTracerProvider(tracesdk.WithSampler(tracesdk.AlwaysSample()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			t.Fatalf("shutting down tracer provider: %v", err)
		}
	})

	tracer := otel.Tracer("test")

	app := fiber.New()
	handler := New(Config{EnableOpenMetrics: true})

	app.Use(func(c fiber.Ctx) error {
		ctxWithSpan, span := tracer.Start(c.Context(), "test-request")
		defer span.End()
		c.SetContext(ctxWithSpan)
		return handler(c)
	})

	app.Post("/upload", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	payload := httptest.NewRequest(fiber.MethodPost, "/upload", strings.NewReader("payload"))
	payload.Header.Set("Content-Type", "text/plain")
	if _, err := app.Test(payload, noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metricsReq := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	metricsReq.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	metricsResp, err := app.Test(metricsReq, noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if metricsResp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", metricsResp.StatusCode)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}

	metrics := string(body)

	requestLineWithExemplar := false
	responseLineWithExemplar := false
	for _, line := range strings.Split(metrics, "\n") {
		if strings.Contains(line, "http_request_size_bytes_bucket{") && strings.Contains(line, "status_code=\"200\"") && strings.Contains(line, "method=\"POST\"") && strings.Contains(line, "path=\"/upload\"") && strings.Contains(line, "le=\"256.0\"") && strings.Contains(line, "# {traceID=\"") {
			requestLineWithExemplar = true
		}
		if strings.Contains(line, "http_response_size_bytes_bucket{") && strings.Contains(line, "status_code=\"200\"") && strings.Contains(line, "method=\"POST\"") && strings.Contains(line, "path=\"/upload\"") && strings.Contains(line, "le=\"256.0\"") && strings.Contains(line, "# {traceID=\"") {
			responseLineWithExemplar = true
		}
	}

	if !requestLineWithExemplar {
		t.Fatalf("expected request size histogram to include a trace exemplar, got %q", metrics)
	}
	if !responseLineWithExemplar {
		t.Fatalf("expected response size histogram to include a trace exemplar, got %q", metrics)
	}
}

func TestMiddlewareDoesNotHijackRootRoute(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != "Hello World" {
		t.Fatalf("expected the application handler to serve /, got %q", body)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "http_requests_total{method=\"GET\",path=\"/\",status_code=\"200\"}") {
		t.Fatalf("expected the root route to be instrumented, got %q", metrics)
	}
}

func TestParameterizedRoutesUseRoutePattern(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/user/:id", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/files/*", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/user/42", "/user/1337", "/files/a/b.txt"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error for %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "http_requests_total{method=\"GET\",path=\"/user/:id\",status_code=\"200\"} 2") {
		t.Fatalf("expected parameterized requests to collapse onto the route pattern, got %q", metrics)
	}
	if !strings.Contains(metrics, "http_requests_total{method=\"GET\",path=\"/files/*\",status_code=\"200\"}") {
		t.Fatalf("expected wildcard requests to use the route pattern, got %q", metrics)
	}
	if strings.Contains(metrics, "path=\"/user/42\"") {
		t.Fatalf("expected the request path to stay out of the labels, got %q", metrics)
	}
}

func TestCustomErrorHandlerStatusAndResponseSize(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			return c.Status(fiber.StatusTeapot).SendString("short and stout")
		},
	})
	app.Use(New(Config{}))
	app.Get("/boom", func(_ fiber.Ctx) error {
		return errors.New("boom")
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/boom", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusTeapot {
		t.Fatalf("expected status 418, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != "short and stout" {
		t.Fatalf("expected the custom error handler body, got %q", body)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "http_requests_total{method=\"GET\",path=\"/boom\",status_code=\"418\"}") {
		t.Fatalf("expected the status written by the error handler to be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, "status_code=\"500\"") {
		t.Fatalf("expected no 500 to be recorded, got %q", metrics)
	}

	size := gaugeValue(t, metrics, "http_response_size_bytes_sum{method=\"GET\",path=\"/boom\",status_code=\"418\"}")
	if size != float64(len("short and stout")) {
		t.Fatalf("expected the error response body to be measured, got %v", size)
	}
}

func TestWrappedFiberErrorUsesWrappedStatus(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/missing", func(_ fiber.Ctx) error {
		return fmt.Errorf("looking up record: %w", fiber.ErrNotFound)
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/missing", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected status 404, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "http_requests_total{method=\"GET\",path=\"/missing\",status_code=\"404\"}") {
		t.Fatalf("expected the wrapped fiber.Error status to be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, "status_code=\"500\"") {
		t.Fatalf("expected no 500 to be recorded for a wrapped fiber.Error, got %q", metrics)
	}
}

func TestWrappedFiberErrorRespectsSkipStatusCodes(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipStatusCodes: []int{fiber.StatusNotFound}}, "")
	app.Get("/missing", func(_ fiber.Ctx) error {
		return fmt.Errorf("looking up record: %w", fiber.ErrNotFound)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/missing", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "path=\"/missing\"") {
		t.Fatalf("expected the ignored status code to be excluded, got %q", metrics)
	}
}

func TestNextSkipsMetricsEndpoint(t *testing.T) {
	app := newAppWithMiddleware(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected Next to gate the metrics endpoint, got status %d", resp.StatusCode)
	}
}

func TestMetricsPathConfig(t *testing.T) {
	app := newAppWithMiddleware(Config{MetricsPath: "internal/metrics"}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected the default metrics path to be unused, got status %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "/internal/metrics")
	if !strings.Contains(metrics, "path=\"/hello\"") {
		t.Fatalf("expected request metrics to be recorded, got %q", metrics)
	}
}

// uncomparableRegistry is a Registerer/Gatherer whose dynamic type is not
// comparable because of the map field. Comparing two of them through an
// interface panics at run time, so the middleware must not do that.
type uncomparableRegistry struct {
	*prometheus.Registry

	tags map[string]string
}

func TestUncomparableRegistryDoesNotPanic(t *testing.T) {
	registry := uncomparableRegistry{Registry: prometheus.NewRegistry(), tags: map[string]string{"a": "b"}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("expected no panic for an uncomparable registry, got %v", r)
		}
	}()

	_ = New(Config{Registerer: registry, Gatherer: registry})
}

func TestMismatchedRegistryPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			message := fmt.Sprint(r)
			if !strings.Contains(message, "must reference the same metrics source") {
				t.Fatalf("expected panic about mismatched sources, got %q", message)
			}
			return
		}
		t.Fatal("expected panic when Registerer and Gatherer differ")
	}()

	_ = New(Config{Registerer: prometheus.NewRegistry(), Gatherer: prometheus.NewRegistry()})
}

func TestMetricsEndpointIsNotInstrumented(t *testing.T) {
	app := newAppWithMiddleware(Config{TrackUnmatchedRequests: true}, "")

	getMetrics(t, app, "")
	metrics := getMetrics(t, app, "")

	if strings.Contains(metrics, "path=\"/metrics\"") {
		t.Fatalf("expected scrapes to stay out of the metrics, got %q", metrics)
	}
	if strings.Contains(metrics, "path=\"/__unmatched__\"") {
		t.Fatalf("expected scrapes not to be counted as unmatched, got %q", metrics)
	}
}

func TestPayloadSizesAreRecorded(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Post("/echo", func(c fiber.Ctx) error {
		return c.SendString(strings.Repeat("b", 654))
	})

	req := httptest.NewRequest(fiber.MethodPost, "/echo", strings.NewReader(strings.Repeat("a", 321)))
	req.Header.Set("Content-Type", "text/plain")
	if _, err := app.Test(req, noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")

	if size := gaugeValue(t, metrics, "http_request_size_bytes_sum{method=\"POST\",path=\"/echo\",status_code=\"200\"}"); size != 321 {
		t.Fatalf("expected request size 321, got %v", size)
	}
	if size := gaugeValue(t, metrics, "http_response_size_bytes_sum{method=\"POST\",path=\"/echo\",status_code=\"200\"}"); size != 654 {
		t.Fatalf("expected response size 654, got %v", size)
	}
}

// TestMiddlewareRoutesAreNotTreatedAsEndpoints guards against counting the
// middleware's own `use` route as a registered endpoint, which would make any
// method on "/" look matched.
func TestMiddlewareRoutesAreNotTreatedAsEndpoints(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusMethodNotAllowed {
		t.Fatalf("expected status 405, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "http_requests_total") {
		t.Fatalf("expected the unregistered method to stay uninstrumented, got %q", metrics)
	}
}

func TestDisabledMetricsRemovesFamilies(t *testing.T) {
	app := newAppWithMiddleware(Config{
		DisabledMetrics: []Metric{MetricRequestSize, MetricResponseSize},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	for _, name := range []string{"http_request_size_bytes", "http_response_size_bytes"} {
		if strings.Contains(metrics, name) {
			t.Fatalf("expected %s to be absent, got %q", name, metrics)
		}
	}
	for _, name := range []string{"http_requests_total", "http_request_duration_seconds", "http_requests_in_progress"} {
		if !strings.Contains(metrics, name) {
			t.Fatalf("expected %s to still be exposed, got %q", name, metrics)
		}
	}
}

func TestDisabledInProgressGauge(t *testing.T) {
	app := newAppWithMiddleware(Config{
		DisabledMetrics: []Metric{MetricRequestsInProgress},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "http_requests_in_progress") {
		t.Fatalf("expected in-progress gauge to be absent, got %q", metrics)
	}
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/hello",status_code="200"}`) {
		t.Fatalf("expected requests to still be counted, got %q", metrics)
	}
}

func TestAllRecordingMetricsDisabled(t *testing.T) {
	app := newAppWithMiddleware(Config{
		DisabledMetrics: []Metric{
			MetricRequestsTotal,
			MetricRequestsStatusClassTotal,
			MetricRequestDuration,
			MetricRequestSize,
			MetricResponseSize,
		},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected the handler to still run, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "http_requests_in_progress") {
		t.Fatalf("expected the in-progress gauge to remain, got %q", metrics)
	}
	if strings.Contains(metrics, "http_requests_total") {
		t.Fatalf("expected no request counter, got %q", metrics)
	}
}

func TestSkipURIsPrefixWildcard(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/admin/*"}}, "")
	for _, path := range []string{"/admin", "/admin/users", "/administration"} {
		app.Get(path, func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		})
	}

	for _, path := range []string{"/admin", "/admin/users", "/administration"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error for %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	for _, path := range []string{`path="/admin"`, `path="/admin/users"`} {
		if strings.Contains(metrics, path) {
			t.Fatalf("expected %s to be skipped, got %q", path, metrics)
		}
	}
	// The prefix must stop at a path segment boundary.
	if !strings.Contains(metrics, `path="/administration"`) {
		t.Fatalf("expected /administration to be recorded, got %q", metrics)
	}
}

func TestSkipURIsWildcardMatchesEverything(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/*"}}, "")
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/deep/route", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/", "/deep/route"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error for %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "http_requests_total{") {
		t.Fatalf("expected every route to be skipped, got %q", metrics)
	}
}

func TestSkipStatusClasses(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipStatusClasses: []string{"4xx"}}, "")
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/denied", func(c fiber.Ctx) error {
		return fiber.ErrUnauthorized
	})
	app.Get("/gone", func(c fiber.Ctx) error {
		return fiber.ErrNotFound
	})

	for _, path := range []string{"/ok", "/denied", "/gone"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error for %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="/ok"`) {
		t.Fatalf("expected the 2xx route to be recorded, got %q", metrics)
	}
	for _, path := range []string{`path="/denied"`, `path="/gone"`} {
		if strings.Contains(metrics, path) {
			t.Fatalf("expected %s to be ignored as 4xx, got %q", path, metrics)
		}
	}
}

func TestDynamicLabels(t *testing.T) {
	app := newAppWithMiddleware(Config{
		DynamicLabels: map[string]func(fiber.Ctx) string{
			"tenant": func(c fiber.Ctx) string { return c.Get("X-Tenant") },
		},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	req := httptest.NewRequest(fiber.MethodGet, "/hello", nil)
	req.Header.Set("X-Tenant", "acme")
	if _, err := app.Test(req, noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/hello",status_code="200",tenant="acme"}`) {
		t.Fatalf("expected the dynamic label on the counter, got %q", metrics)
	}
	if !strings.Contains(metrics, `http_requests_status_class_total{method="GET",path="/hello",status_class="2xx",tenant="acme"}`) {
		t.Fatalf("expected the dynamic label on the status class counter, got %q", metrics)
	}

	// The in-flight gauge is incremented before routing, so it cannot carry
	// labels derived from the request outcome.
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "http_requests_in_progress{") && strings.Contains(line, "tenant=") {
			t.Fatalf("expected no dynamic label on the in-progress gauge, got %q", line)
		}
	}
}

// Prometheus emits label pairs alphabetically, so this asserts the whole series
// to prove each dynamic name is paired with its own function's value rather than
// a neighbour's.
func TestDynamicLabelsPairNamesWithValues(t *testing.T) {
	app := newAppWithMiddleware(Config{
		DynamicLabels: map[string]func(fiber.Ctx) string{
			"zone":   func(fiber.Ctx) string { return "eu" },
			"tenant": func(fiber.Ctx) string { return "acme" },
			"apex":   func(fiber.Ctx) string { return "one" },
		},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	want := `http_requests_total{apex="one",method="GET",path="/hello",status_code="200",tenant="acme",zone="eu"}`
	if !strings.Contains(metrics, want) {
		t.Fatalf("expected %q, got %q", want, metrics)
	}
}

func TestDynamicLabelCollisionPanics(t *testing.T) {
	for name, cfg := range map[string]Config{
		"built-in": {
			DynamicLabels: map[string]func(fiber.Ctx) string{
				"path": func(fiber.Ctx) string { return "x" },
			},
		},
		"constant": {
			ServiceName: "svc",
			DynamicLabels: map[string]func(fiber.Ctx) string{
				"service": func(fiber.Ctx) string { return "x" },
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					if !strings.Contains(fmt.Sprint(r), "collides with") {
						t.Fatalf("expected a collision panic, got %v", r)
					}
					return
				}
				t.Fatal("expected a panic for a colliding dynamic label")
			}()

			_ = New(cfg)
		})
	}
}

func TestDynamicLabelWithoutFunctionPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			if !strings.Contains(fmt.Sprint(r), "has no function") {
				t.Fatalf("expected a panic about the missing function, got %v", r)
			}
			return
		}
		t.Fatal("expected a panic for a nil dynamic label function")
	}()

	_ = New(Config{DynamicLabels: map[string]func(fiber.Ctx) string{"tenant": nil}})
}

// durationHistogram gathers the request duration histogram straight from the
// registry, because native histograms are only carried by the protobuf
// exposition format and never appear in the text one.
func durationHistogram(t *testing.T, registry *prometheus.Registry) *dto.Histogram {
	t.Helper()

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != "http_request_duration_seconds" {
			continue
		}
		if len(family.Metric) == 0 {
			t.Fatal("expected at least one duration sample")
		}
		return family.Metric[0].Histogram
	}

	t.Fatal("expected the duration histogram to be registered")
	return nil
}

func newAppWithRegistry(t *testing.T, cfg Config) (*fiber.App, *prometheus.Registry) {
	t.Helper()

	registry := prometheus.NewRegistry()
	cfg.Registerer = registry
	cfg.Gatherer = registry

	app := newAppWithMiddleware(cfg, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	return app, registry
}

func TestNativeHistogramsDisabledByDefault(t *testing.T) {
	_, registry := newAppWithRegistry(t, Config{})

	histogram := durationHistogram(t, registry)
	if histogram.Schema != nil {
		t.Fatalf("expected no native histogram schema, got %d", histogram.GetSchema())
	}
	if len(histogram.Bucket) == 0 {
		t.Fatal("expected the default classic buckets")
	}
}

func TestNativeHistogramBucketFactor(t *testing.T) {
	_, registry := newAppWithRegistry(t, Config{NativeHistogramBucketFactor: 1.1})

	histogram := durationHistogram(t, registry)
	if histogram.Schema == nil {
		t.Fatal("expected native histograms to be enabled")
	}
	// Both representations are emitted unless the classic buckets are dropped.
	if len(histogram.Bucket) == 0 {
		t.Fatal("expected the classic buckets to remain alongside the native ones")
	}
}

func TestNativeHistogramsWithoutClassicBuckets(t *testing.T) {
	_, registry := newAppWithRegistry(t, Config{
		NativeHistogramBucketFactor: 1.1,
		// An empty non-nil slice drops the classic buckets, unlike nil which
		// selects the defaults.
		RequestDurationBuckets: []float64{},
	})

	histogram := durationHistogram(t, registry)
	if histogram.Schema == nil {
		t.Fatal("expected native histograms to be enabled")
	}
	if len(histogram.Bucket) != 0 {
		t.Fatalf("expected no classic buckets, got %d", len(histogram.Bucket))
	}
	if histogram.GetSampleCount() != 1 {
		t.Fatalf("expected the sample to still be counted, got %d", histogram.GetSampleCount())
	}
}

// TestShortCircuitRouteGuardKeepsRoutePattern pins the case a ctx.IsMiddleware()
// based fallback got wrong: Fiber reports IsMiddleware for any route whose
// handler chain stopped early, which is exactly what a per-route guard that
// rejects a request leaves behind. Those requests must keep their own route
// pattern, or every 401 from a guard collapses into one series and SkipURIs
// entries for the route stop matching.
func TestShortCircuitRouteGuardKeepsRoutePattern(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/admin", func(c fiber.Ctx) error {
		return fiber.ErrUnauthorized
	}, func(c fiber.Ctx) error {
		return c.SendString("admin")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/admin", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/admin",status_code="401"}`) {
		t.Fatalf("expected the guarded route to keep its pattern, got %q", metrics)
	}
}

// TestSkipURIsAppliesToShortCircuitRouteGuard is the second half of the same
// regression: skipped() is handed the route label, so a wrong label silently
// disables SkipURIs for guarded routes.
func TestSkipURIsAppliesToShortCircuitRouteGuard(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/admin"}}, "")
	app.Get("/admin", func(c fiber.Ctx) error {
		return fiber.ErrUnauthorized
	}, func(c fiber.Ctx) error {
		return c.SendString("admin")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/admin", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "http_requests_total{") {
		t.Fatalf("expected the guarded route to be skipped, got %q", metrics)
	}
}

// TestFallThroughToTrailingMiddlewareUsesMountPath documents a known
// limitation rather than desired behaviour. Once a handler delegates onwards
// with c.Next() and a trailing Use middleware runs last, Fiber has replaced the
// route on the context with that middleware's mount path and the endpoint
// pattern is gone; Fiber exposes nothing that distinguishes a Use route, so the
// case cannot be detected either.
func TestFallThroughToTrailingMiddlewareUsesMountPath(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/user/:id", func(c fiber.Ctx) error {
		return c.Next()
	})
	app.Use(func(c fiber.Ctx) error {
		return c.SendString("trailing")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/user/42", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="/"`) {
		t.Fatalf("expected the documented mount-path attribution, got %q", metrics)
	}
}

// TestRouteLevelMiddlewareKeepsRoutePattern pins the common case the fallback
// must not disturb: extra handlers on the route itself still resolve to the
// route pattern.
func TestRouteLevelMiddlewareKeepsRoutePattern(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/user/:id", func(c fiber.Ctx) error {
		return c.Next()
	}, func(c fiber.Ctx) error {
		return c.SendString("handled")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/user/42", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="/user/:id"`) {
		t.Fatalf("expected the route pattern to be preserved, got %q", metrics)
	}
}

// TestWrappingRegistererWithMatchingGatherer covers pairing a Registerer that
// is not itself a Gatherer with the registry it wraps, which is the reason to
// supply both.
func TestWrappingRegistererWithMatchingGatherer(t *testing.T) {
	registry := prometheus.NewRegistry()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("expected a wrapping Registerer paired with its registry to be accepted, got %v", r)
		}
	}()

	app := newAppWithMiddleware(Config{
		Registerer: prometheus.WrapRegistererWithPrefix("app_", registry),
		Gatherer:   registry,
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "app_http_requests_total") {
		t.Fatalf("expected the prefixed metric to be gathered, got %q", metrics)
	}
}

// TestEmptyBucketsWithoutNativeHistogramsKeepsDefaults pins the documented
// caveat: client_golang substitutes its own defaults rather than leave a
// histogram with no buckets, so an empty slice alone does not drop them.
func TestEmptyBucketsWithoutNativeHistogramsKeepsDefaults(t *testing.T) {
	_, registry := newAppWithRegistry(t, Config{RequestDurationBuckets: []float64{}})

	histogram := durationHistogram(t, registry)
	if len(histogram.Bucket) == 0 {
		t.Fatal("expected client_golang to substitute its default buckets")
	}
	if histogram.Schema != nil {
		t.Fatal("expected native histograms to stay disabled")
	}
}

// TestDynamicLabelValuesAreCopied guards the corruption that follows from
// storing Fiber's zero-copy strings. A label function reading a header returns
// a string aliasing the connection read buffer; Prometheus keeps label values
// for the lifetime of the series, so without a copy every series rewrites
// itself to whatever request arrived last, collapsing them onto one label set
// and leaving the registry unable to gather at all.
func TestDynamicLabelValuesAreCopied(t *testing.T) {
	app := newAppWithMiddleware(Config{
		DynamicLabels: map[string]func(fiber.Ctx) string{
			"tenant": func(c fiber.Ctx) string { return c.Get("X-Tenant", "unknown") },
		},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	const tenants = 25
	for i := range tenants {
		req := httptest.NewRequest(fiber.MethodGet, "/hello", nil)
		req.Header.Set("X-Tenant", fmt.Sprintf("tenant-%d", i))
		if _, err := app.Test(req, noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error: %v", err)
		}
	}

	// A corrupted registry fails to gather, which getMetrics reports as a
	// non-200 scrape.
	metrics := getMetrics(t, app, "")

	seen := 0
	for i := range tenants {
		if strings.Contains(metrics, fmt.Sprintf(`tenant="tenant-%d"`, i)) {
			seen++
		}
	}
	if seen != tenants {
		t.Fatalf("expected all %d tenant label values to survive, found %d: %q", tenants, seen, metrics)
	}
}

// TestSkipURIsMatchesWildcardRoutePattern covers the overload in "*": it is
// both the prefix syntax for SkipURIs and a legitimate character in a Fiber
// route pattern. An entry ending in "*" therefore has to match the route
// literally named that, not only the paths beneath its prefix.
func TestSkipURIsMatchesWildcardRoutePattern(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/static*", "/files/*"}}, "")
	app.Get("/static*", func(c fiber.Ctx) error {
		return c.SendString("static")
	})
	app.Get("/files/*", func(c fiber.Ctx) error {
		return c.SendString("files")
	})
	app.Get("/keep", func(c fiber.Ctx) error {
		return c.SendString("keep")
	})

	for _, path := range []string{"/staticfoo", "/files/a.txt", "/keep"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error for %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	for _, skipped := range []string{`path="/static*"`, `path="/files/*"`} {
		if strings.Contains(metrics, skipped) {
			t.Fatalf("expected %s to be skipped, got %q", skipped, metrics)
		}
	}
	if !strings.Contains(metrics, `path="/keep"`) {
		t.Fatalf("expected the unlisted route to still be recorded, got %q", metrics)
	}
}

// TestDynamicLabelValuesSurviveInvalidUTF8 guards a process-killer: Prometheus
// rejects label values that are not valid UTF-8 by panicking inside
// WithLabelValues, and the documented way to write a label function is to
// return a request value directly. A single header of raw bytes would take the
// server down.
func TestDynamicLabelValuesSurviveInvalidUTF8(t *testing.T) {
	app := newAppWithMiddleware(Config{
		DynamicLabels: map[string]func(fiber.Ctx) string{
			"tenant": func(c fiber.Ctx) string { return c.Get("X-Tenant") },
		},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	req := httptest.NewRequest(fiber.MethodGet, "/hello", nil)
	req.Header.Set("X-Tenant", "\xff\xfe")
	resp, err := app.Test(req, noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected the request to be served, got %d", resp.StatusCode)
	}

	// A corrupted or unreadable registry shows up as a non-200 scrape.
	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "http_requests_total{") {
		t.Fatalf("expected the request to be recorded, got %q", metrics)
	}
	if !strings.Contains(metrics, "�") {
		t.Fatalf("expected invalid bytes to be replaced, got %q", metrics)
	}
}

// TestSkipURIsIgnoresTrailingSlashAfterWildcard pins that "/admin/*/" behaves
// as "/admin/*", since trailing slashes are documented as ignored.
func TestSkipURIsIgnoresTrailingSlashAfterWildcard(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/admin/*/"}}, "")
	app.Get("/admin/users", func(c fiber.Ctx) error {
		return c.SendString("users")
	})
	app.Get("/keep", func(c fiber.Ctx) error {
		return c.SendString("keep")
	})

	for _, path := range []string{"/admin/users", "/keep"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error for %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, `path="/admin/users"`) {
		t.Fatalf("expected the wildcard prefix to still apply, got %q", metrics)
	}
	if !strings.Contains(metrics, `path="/keep"`) {
		t.Fatalf("expected the unlisted route to be recorded, got %q", metrics)
	}
}

func TestLabelsAreAttachedToEveryMetric(t *testing.T) {
	app := newAppWithMiddleware(Config{
		Labels: prometheus.Labels{"env": "staging", "region": "eu"},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	for _, family := range []string{
		"http_requests_total{",
		"http_requests_status_class_total{",
		"http_request_duration_seconds_sum{",
		"http_request_size_bytes_sum{",
		"http_response_size_bytes_sum{",
		"http_requests_in_progress{",
	} {
		found := false
		for _, line := range strings.Split(metrics, "\n") {
			if strings.HasPrefix(line, family) &&
				strings.Contains(line, `env="staging"`) && strings.Contains(line, `region="eu"`) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected constant labels on %s, got %q", family, metrics)
		}
	}
}

// TestServiceOverridesLabelsCollision pins the precedence when Labels carries a
// "service" key and Service is also set: Service wins, because it is applied
// after the configured labels are copied.
func TestServiceOverridesLabelsCollision(t *testing.T) {
	app := newAppWithMiddleware(Config{
		ServiceName: "from-service",
		Labels:      prometheus.Labels{"service": "from-labels"},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `service="from-service"`) {
		t.Fatalf("expected Service to win over a colliding Labels entry, got %q", metrics)
	}
	if strings.Contains(metrics, `service="from-labels"`) {
		t.Fatalf("expected the colliding Labels entry to be overridden, got %q", metrics)
	}
}

func TestNamespaceAndSubsystemPrefixMetricNames(t *testing.T) {
	app := newAppWithMiddleware(Config{Namespace: "svc", Subsystem: "api"}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	for _, name := range []string{
		"svc_api_requests_total",
		"svc_api_requests_status_class_total",
		"svc_api_request_duration_seconds",
		"svc_api_request_size_bytes",
		"svc_api_response_size_bytes",
		"svc_api_requests_in_progress",
	} {
		if !strings.Contains(metrics, name) {
			t.Fatalf("expected %s to be exposed, got %q", name, metrics)
		}
	}
	if strings.Contains(metrics, "http_requests_total") {
		t.Fatalf("expected the default namespace to be replaced, got %q", metrics)
	}
}

// TestGathererOnlyResolvesRegisterer covers the branch where only a Gatherer is
// supplied and it also implements Registerer.
func TestGathererOnlyResolvesRegisterer(t *testing.T) {
	registry := prometheus.NewRegistry()

	app := newAppWithMiddleware(Config{Gatherer: registry}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("expected the supplied Gatherer to have been registered into")
	}
}

func TestGathererWithoutRegistererPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			if !strings.Contains(fmt.Sprint(r), "does not implement prometheus.Registerer") {
				t.Fatalf("expected a panic about the missing Registerer, got %v", r)
			}
			return
		}
		t.Fatal("expected a panic when the Gatherer cannot register")
	}()

	_ = New(Config{Gatherer: prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		return nil, nil
	})})
}

// TestCollectorsRegisteredIntoSuppliedRegistry pins that the runtime collectors
// land in the configured registry, and that registering them twice into the
// same one is tolerated rather than fatal.
func TestCollectorsRegisteredIntoSuppliedRegistry(t *testing.T) {
	registry := prometheus.NewRegistry()

	_ = New(Config{Registerer: registry, Gatherer: registry})
	// A second middleware on the same registry would hit AlreadyRegistered for
	// the collectors; only the metric families conflict, so vary the namespace.
	_ = New(Config{Registerer: registry, Gatherer: registry, Namespace: "other"})

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}

	var sawGo, sawProcess bool
	for _, family := range families {
		switch family.GetName() {
		case "go_goroutines":
			sawGo = true
		case "process_cpu_seconds_total":
			sawProcess = true
		}
	}
	if !sawGo || !sawProcess {
		t.Fatalf("expected both runtime collectors, go=%v process=%v", sawGo, sawProcess)
	}
}

func TestStatusClassMapping(t *testing.T) {
	for status, want := range map[int]string{
		100: "1xx",
		204: "2xx",
		301: "3xx",
		404: "4xx",
		503: "5xx",
		999: "unknown",
	} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

// TestErrorHandlerFailureFallsBackTo500 covers the branch where the application
// error handler itself fails and the middleware has to produce a status.
func TestErrorHandlerFailureFallsBackTo500(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(fiber.Ctx, error) error {
			return errors.New("error handler failed")
		},
	})
	app.Use(New(Config{}))
	app.Get("/boom", func(fiber.Ctx) error {
		return errors.New("boom")
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/boom", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/boom",status_code="500"}`) {
		t.Fatalf("expected the fallback status to be recorded, got %q", metrics)
	}
}

// TestUnknownResponseSizeIsNotObserved covers a streamed response of
// unannounced length. Observing it as zero bytes would leave a file-serving or
// SSE app reporting a median response size of nothing, so the sample is left
// out of the histogram entirely while the request still counts.
func TestUnknownResponseSizeIsNotObserved(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/stream", func(c fiber.Ctx) error {
		return c.SendStream(strings.NewReader(strings.Repeat("s", 5000)))
	})
	app.Get("/sized", func(c fiber.Ctx) error {
		return c.SendString(strings.Repeat("s", 5000))
	})

	for _, path := range []string{"/stream", "/sized"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")

	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/stream",status_code="200"}`) {
		t.Fatalf("expected the streamed request to be counted, got %q", metrics)
	}
	if strings.Contains(metrics, `http_response_size_bytes_sum{method="GET",path="/stream",status_code="200"}`) {
		t.Fatalf("expected no response size series for a stream of unknown length, got %q", metrics)
	}
	if size := gaugeValue(t, metrics, `http_response_size_bytes_sum{method="GET",path="/sized",status_code="200"}`); size != 5000 {
		t.Fatalf("expected response size 5000 for the buffered body, got %v", size)
	}
}

// fakePayload stands in for *fasthttp.Request and *fasthttp.Response so
// the size helpers can be exercised on both sides without a live connection.
type fakePayload struct {
	stream bool
	body   []byte
}

func (p fakePayload) IsBodyStream() bool { return p.stream }
func (p fakePayload) Body() []byte       { return p.body }

// TestRequestBodySize covers the received side, where Content-Length is what
// fasthttp parsed off the wire and the buffer may not hold the body at all.
func TestRequestBodySize(t *testing.T) {
	tests := []struct {
		name          string
		contentLength int
		bodyLimit     int
		preParsedForm bool
		payload       fakePayload
		want          float64
		wantKnown     bool
	}{
		// The buffer holds what arrived, so an announced length a client made
		// up cannot reach the histogram - and since more was announced than
		// arrived, the request was refused unread and has no honest size at all.
		{name: "inflated content length is undetermined", contentLength: 500000000, payload: fakePayload{}, wantKnown: false},
		{name: "announced length matching the buffer is measured", contentLength: 5, payload: fakePayload{body: []byte("hello")}, want: 5, wantKnown: true},
		{name: "buffered body is measured", contentLength: 5, payload: fakePayload{body: []byte("hello")}, want: 5, wantKnown: true},
		{name: "chunked body is measured", contentLength: -1, payload: fakePayload{body: []byte("hello")}, want: 5, wantKnown: true},
		// A pre-parsed form is the one case where the buffer is not the body.
		{name: "form takes the announced length", contentLength: 2048, bodyLimit: 4096, preParsedForm: true, payload: fakePayload{}, want: 2048, wantKnown: true},
		// Beyond the limit the body was never received in full, so there is no
		// honest size to record - fabricating one would be worse than none.
		{name: "form beyond the limit is undetermined", contentLength: 500000000, bodyLimit: 4096, preParsedForm: true, payload: fakePayload{}, wantKnown: false},
		{name: "form without a limit takes the announced length", contentLength: 2048, preParsedForm: true, payload: fakePayload{}, want: 2048, wantKnown: true},
		{name: "stream takes the announced length", contentLength: 42, bodyLimit: 4096, payload: fakePayload{stream: true}, want: 42, wantKnown: true},
		{name: "stream beyond the limit is undetermined", contentLength: 500000000, bodyLimit: 1024, payload: fakePayload{stream: true}, wantKnown: false},
		{name: "stream announcing zero is a known zero", contentLength: 0, payload: fakePayload{stream: true}, want: 0, wantKnown: true},
		{name: "stream of unknown length is undetermined", contentLength: -1, payload: fakePayload{stream: true}, wantKnown: false},
		{name: "identity encoding stream is undetermined", contentLength: -2, payload: fakePayload{stream: true}, wantKnown: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, known := requestBodySize(tt.contentLength, tt.bodyLimit, tt.preParsedForm, tt.payload)
			if known != tt.wantKnown {
				t.Fatalf("expected known=%v, got %v", tt.wantKnown, known)
			}
			if known && size != tt.want {
				t.Fatalf("expected size %v, got %v", tt.want, size)
			}
		})
	}
}

// TestResponseBodySize covers the sent side, where fasthttp recomputes
// Content-Length from the body it is about to write.
func TestResponseBodySize(t *testing.T) {
	tests := []struct {
		name          string
		contentLength int
		payload       fakePayload
		want          float64
		wantKnown     bool
	}{
		{name: "buffered body wins over content length", contentLength: 999999, payload: fakePayload{body: []byte("tiny")}, want: 4, wantKnown: true},
		{name: "content length is all a stream has", contentLength: 42, payload: fakePayload{stream: true}, want: 42, wantKnown: true},
		{name: "buffered body is measured", contentLength: -1, payload: fakePayload{body: []byte("hello")}, want: 5, wantKnown: true},
		{name: "empty body is a known zero", contentLength: 0, payload: fakePayload{}, want: 0, wantKnown: true},
		{name: "stream announcing zero is a known zero", contentLength: 0, payload: fakePayload{stream: true}, want: 0, wantKnown: true},
		{name: "stream of unknown length is undetermined", contentLength: -1, payload: fakePayload{stream: true}, wantKnown: false},
		{name: "identity encoding stream is undetermined", contentLength: -2, payload: fakePayload{stream: true}, wantKnown: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, known := responseBodySize(tt.contentLength, tt.payload)
			if known != tt.wantKnown {
				t.Fatalf("expected known=%v, got %v", tt.wantKnown, known)
			}
			if known && size != tt.want {
				t.Fatalf("expected size %v, got %v", tt.want, size)
			}
		})
	}
}

// TestSkipURIsWithoutLeadingSlash pins that an entry is matched against route
// patterns, which Fiber always registers with a leading slash. Requiring the
// caller to supply one would make "hello" a silent no-op.
func TestSkipURIsWithoutLeadingSlash(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"hello", "admin/*"}}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/admin/users", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/kept", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/hello", "/admin/users", "/kept"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")

	for _, path := range []string{"/hello", "/admin/users"} {
		if strings.Contains(metrics, `path="`+path+`"`) {
			t.Fatalf("expected %s to be skipped, got %q", path, metrics)
		}
	}
	if !strings.Contains(metrics, `path="/kept"`) {
		t.Fatalf("expected /kept to be recorded, got %q", metrics)
	}
}

// TestNewWithoutConfigUsesDefaults exercises the argument-less call, which the
// rest of the suite never takes.
func TestNewWithoutConfigUsesDefaults(t *testing.T) {
	app := fiber.New()
	app.Use(New())
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")

	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/hello",status_code="200"}`) {
		t.Fatalf("expected the default namespace and path label, got %q", metrics)
	}
	if !strings.Contains(metrics, `http_request_duration_seconds_bucket{method="GET",path="/hello",status_code="200",le="0.005"}`) {
		t.Fatalf("expected the default duration buckets, got %q", metrics)
	}
	if !strings.Contains(metrics, "go_goroutines") {
		t.Fatalf("expected the Go collector to be registered by default, got %q", metrics)
	}
}

// nilRouteCtx reports a match without exposing a route, which DefaultCtx never
// does but a Ctx supplied through fiber.NewWithCustomCtx may.
type nilRouteCtx struct {
	*fiber.DefaultCtx
}

func (*nilRouteCtx) Route() *fiber.Route { return nil }

func newNilRouteApp() *fiber.App {
	return fiber.NewWithCustomCtx(func(app *fiber.App) fiber.CustomCtx {
		return &nilRouteCtx{DefaultCtx: fiber.NewDefaultCtx(app)}
	})
}

// TestMatchedRequestWithoutRouteObeysTrackUnmatched pins that the unmatched
// label is only ever emitted when the caller asked for it. A match the Ctx
// cannot name is indistinguishable from no match at all, so it follows the same
// opt-in rather than smuggling a series past it.
func TestMatchedRequestWithoutRouteObeysTrackUnmatched(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(fmt.Sprintf("tracked=%v", tracked), func(t *testing.T) {
			app := newNilRouteApp()
			app.Use(New(Config{TrackUnmatchedRequests: tracked}))
			app.Get("/hello", func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			})

			if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
				t.Fatalf("unexpected request error: %v", err)
			}

			metrics := getMetrics(t, app, "")
			recorded := strings.Contains(metrics, `path="/__unmatched__"`)
			if recorded != tracked {
				t.Fatalf("expected recorded=%v with TrackUnmatchedRequests=%v, got %q", tracked, tracked, metrics)
			}
		})
	}
}

// TestConstantLabelCollisionPanics pins the middleware's own message for a
// Labels key that shadows a variable label. Left unchecked, client_golang
// rejects the descriptor with a panic naming neither Labels nor the key.
func TestConstantLabelCollisionPanics(t *testing.T) {
	for _, name := range []string{"path", "method", "status_code", "status_class"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected a panic for the constant label %q", name)
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "constant label") || !strings.Contains(msg, name) {
					t.Fatalf("expected the panic to name the offending constant label, got %v", r)
				}
			}()

			_ = New(Config{Labels: prometheus.Labels{name: "x"}})
		})
	}
}

// TestInvalidStatusClassPanics covers a typo in SkipStatusClasses, which would
// otherwise sit in the filter matching nothing while the operator believes the
// class is excluded.
func TestInvalidStatusClassPanics(t *testing.T) {
	for _, class := range []string{"4x", "400", "6xx", "xx"} {
		t.Run(class, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected a panic for the status class %q", class)
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "status class") {
					t.Fatalf("expected a status class panic, got %v", r)
				}
			}()

			_ = New(Config{SkipStatusClasses: []string{class}})
		})
	}
}

// TestValidStatusClassesAreAccepted guards the validation against rejecting the
// documented spellings, including the padding and casing it normalizes away.
func TestValidStatusClassesAreAccepted(t *testing.T) {
	_ = New(Config{SkipStatusClasses: []string{"1xx", "2xx", "3xx", " 4XX ", "5Xx"}})
}

// TestBlankSkipURIIsIgnored covers the shape an unset environment variable
// takes after strings.Split: a single empty entry, which would normalize to "/"
// and silently drop the root route from every metric.
func TestBlankSkipURIIsIgnored(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"", "   "}}, "")
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/",status_code="200"}`) {
		t.Fatalf("expected the root route to stay instrumented, got %q", metrics)
	}
}

// TestExplicitRootSkipURI is the counterpart: asking for "/" still works.
func TestExplicitRootSkipURI(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/"}}, "")
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, `path="/"`) {
		t.Fatalf("expected the root route to be skipped, got %q", metrics)
	}
}

// TestMetricsTimeoutServesScrape exercises the promhttp timeout path, which
// wraps the handler in http.TimeoutHandler and so runs it on its own goroutine.
func TestMetricsTimeoutServesScrape(t *testing.T) {
	app := newAppWithMiddleware(Config{MetricsTimeout: 5 * time.Second}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/hello",status_code="200"}`) {
		t.Fatalf("expected the scrape to be served under a timeout, got %q", metrics)
	}
}

// blockingGatherer holds a scrape inside Gather until it is released, so a
// second scrape can be made to arrive while the first is still in flight.
type blockingGatherer struct {
	prometheus.Gatherer
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (g *blockingGatherer) Gather() ([]*dto.MetricFamily, error) {
	g.entered <- struct{}{}
	<-g.release
	families, err := g.Gatherer.Gather()
	if g.finished != nil {
		g.finished <- struct{}{}
	}
	return families, err
}

// TestMetricsMaxRequestsInFlightRejectsExcess pins that the promhttp limiter is
// wired up: with one slot taken, the next scrape is turned away rather than
// queued behind it.
func TestMetricsMaxRequestsInFlightRejectsExcess(t *testing.T) {
	registry := prometheus.NewRegistry()
	// Buffered, and released through Cleanup: should the limiter ever stop
	// reaching promhttp, the second scrape enters Gather too and must not block
	// forever on a send nobody reads - a hung test says nothing, a failing one
	// says everything.
	gatherer := &blockingGatherer{
		Gatherer: registry,
		entered:  make(chan struct{}, 2),
		release:  make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gatherer.release) }) }
	t.Cleanup(release)

	app := newAppWithMiddleware(Config{
		Registerer:                 registry,
		Gatherer:                   gatherer,
		MetricsMaxRequestsInFlight: 1,
	}, "")

	firstDone := make(chan int, 1)
	go func() {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), noTimeoutConfig)
		if err != nil {
			firstDone <- 0
			return
		}
		firstDone <- resp.StatusCode
	}()

	<-gatherer.entered

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("the second scrape was not turned away: %v", err)
	}
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("expected status 503 while the only slot is taken, got %d", resp.StatusCode)
	}

	release()
	if status := <-firstDone; status != fiber.StatusOK {
		t.Fatalf("expected the first scrape to succeed, got %d", status)
	}
}

// TestDetachRequestCopiesHeader covers the wrapper that keeps a promhttp
// goroutine outliving the Fiber handler from reading recycled buffers.
func TestDetachRequestCopiesHeader(t *testing.T) {
	var seen *http.Request
	handler := detachRequest(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r
	}))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", "text/plain")
	req.Header.Add("Accept-Encoding", "gzip")
	req.Header.Add("Accept-Encoding", "zstd")
	original := req.Header

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen == nil {
		t.Fatal("expected the wrapped handler to run")
	}
	if len(seen.Header) != len(original) {
		t.Fatalf("expected %d header names, got %d", len(original), len(seen.Header))
	}
	for name, values := range original {
		got := seen.Header[name]
		if len(got) != len(values) {
			t.Fatalf("expected %d values for %s, got %d", len(values), name, len(got))
		}
		for i, value := range values {
			if got[i] != value {
				t.Fatalf("expected %s[%d] to be %q, got %q", name, i, value, got[i])
			}
			// Equality alone would survive dropping the clones, since the
			// values are copied into a fresh slice either way. The point of the
			// wrapper is that the bytes behind each string are new too, so the
			// backing arrays are what the test compares.
			if unsafe.StringData(got[i]) == unsafe.StringData(value) {
				t.Fatalf("expected %s[%d] to be backed by a fresh allocation", name, i)
			}
		}
	}

	// Names alias too: the adaptor passes a name already in canonical form
	// straight through, so they are cloned on the same grounds as the values.
	for name := range original {
		var detached string
		for candidate := range seen.Header {
			if candidate == name {
				detached = candidate
			}
		}
		if detached == "" {
			t.Fatalf("expected the header name %q to survive", name)
		}
		if unsafe.StringData(detached) == unsafe.StringData(name) {
			t.Fatalf("expected the name %q to be backed by a fresh allocation", name)
		}
	}
}

// TestPanicWithDownstreamRecoverIsRecorded pins the mounting order the New doc
// prescribes: recovering downstream turns the panic into an error return the
// middleware can attribute, so the 500 the client received is not invisible.
func TestPanicWithDownstreamRecoverIsRecorded(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Use(recoverer.New())
	app.Get("/boom", func(fiber.Ctx) error {
		panic("boom")
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/boom", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/boom",status_code="500"}`) {
		t.Fatalf("expected the panic-induced 500 to be recorded, got %q", metrics)
	}
}

// TestBlankSkipStatusClassIsIgnored matches the tolerance SkipURIs has for the
// entry an unset environment variable produces: an empty setting must not stop
// the process from booting.
func TestBlankSkipStatusClassIsIgnored(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipStatusClasses: []string{"", "  "}}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="/hello"`) {
		t.Fatalf("expected a blank class to filter nothing, got %q", metrics)
	}
}

// TestSkipURIsTrimsWhitespace covers the other half of splitting a setting on
// ",": the entries after the first carry a leading space, which would otherwise
// land behind the slash the middleware prepends and match no route.
func TestSkipURIsTrimsWhitespace(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: strings.Split("/health, /ping", ",")}, "")
	for _, path := range []string{"/health", "/ping"} {
		app.Get(path, func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		})
	}

	for _, path := range []string{"/health", "/ping"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "http_requests_total") {
		t.Fatalf("expected both padded entries to be skipped, got %q", metrics)
	}
}

// TestUnknownStatusClassIsFilterable covers a response outside 1xx-5xx, whose
// class statusClass reports as "unknown" - the one class an operator is most
// likely to want gone.
func TestUnknownStatusClassIsFilterable(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprintf("filtered=%v", filtered), func(t *testing.T) {
			cfg := Config{}
			if filtered {
				cfg.SkipStatusClasses = []string{"unknown"}
			}

			app := newAppWithMiddleware(cfg, "")
			app.Get("/odd", func(c fiber.Ctx) error {
				return c.Status(999).SendString("odd")
			})

			if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/odd", nil), noTimeoutConfig); err != nil {
				t.Fatalf("unexpected request error: %v", err)
			}

			metrics := getMetrics(t, app, "")
			recorded := strings.Contains(metrics, `status_class="unknown"`)
			if recorded == filtered {
				t.Fatalf("expected recorded=%v with the class filtered=%v, got %q", !filtered, filtered, metrics)
			}
		})
	}
}

// TestUnknownDisabledMetricPanics covers a mistyped family name, which would
// otherwise disable nothing while the operator waits for a cardinality drop.
func TestUnknownDisabledMetricPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic for an unknown metric name")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "unknown metric") {
			t.Fatalf("expected an unknown metric panic, got %v", r)
		}
	}()

	_ = New(Config{DisabledMetrics: []Metric{"request_duration"}})
}

// TestInvalidSkipStatusCodePanics covers a code that is not three digits, which
// no response could ever carry.
func TestInvalidSkipStatusCodePanics(t *testing.T) {
	for _, code := range []int{0, 99, 4040, -404} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected a panic for the status code %d", code)
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "status code") {
					t.Fatalf("expected a status code panic, got %v", r)
				}
			}()

			_ = New(Config{SkipStatusCodes: []int{code}})
		})
	}
}

// TestRejectedConfigLeavesRegistryClean pins that validation happens before the
// first Register call. Registering half the families and then rejecting the
// config would leave a shared registry unusable, and the corrected retry would
// fail on a duplicate registration instead - a message pointing nowhere near the
// actual mistake.
func TestRejectedConfigLeavesRegistryClean(t *testing.T) {
	registry := prometheus.NewRegistry()

	rejected := []Config{
		{SkipStatusClasses: []string{"4x"}},
		{SkipStatusCodes: []int{4040}},
		{DisabledMetrics: []Metric{"nope"}},
		{Labels: prometheus.Labels{"path": "x"}},
		{DynamicLabels: map[string]func(fiber.Ctx) string{"method": func(fiber.Ctx) string { return "x" }}},
	}

	for i, cfg := range rejected {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected the invalid config to be rejected")
				}
			}()

			cfg.Registerer = registry
			cfg.Gatherer = registry
			_ = New(cfg)
		})
	}

	// Untouched means empty, not merely free of the metric families: the Go and
	// process collectors register through registerCollector, which swallows
	// AlreadyRegisteredError, so a retry would succeed even with them left
	// behind by a config the caller never got to work.
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering after the rejected configs: %v", err)
	}
	if len(families) != 0 {
		names := make([]string, 0, len(families))
		for _, family := range families {
			names = append(names, family.GetName())
		}
		t.Fatalf("expected the registry to be untouched, got %v", names)
	}

	// So the corrected config registers cleanly rather than dying on a duplicate.
	app := fiber.New()
	app.Use(New(Config{Registerer: registry, Gatherer: registry}))
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/hello",status_code="200"}`) {
		t.Fatalf("expected the corrected config to register cleanly, got %q", metrics)
	}
}

// TestMetricsTimeoutBoundsScrape pins that the configured timeout reaches
// promhttp. Without it a gatherer stuck on a slow collector holds the handler -
// and its in-flight slot - indefinitely, which is the pile-up the option exists
// to prevent.
func TestMetricsTimeoutBoundsScrape(t *testing.T) {
	registry := prometheus.NewRegistry()
	// MetricsTimeout runs the gather on its own goroutine and answers 503
	// without it, so the test has to wait for that straggler: it calls
	// registry.Gather, which reads the process-global model.NameValidationScheme
	// that later tests write.
	gatherer := &blockingGatherer{
		Gatherer: registry,
		entered:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		finished: make(chan struct{}, 1),
	}
	t.Cleanup(func() {
		close(gatherer.release)
		<-gatherer.finished
	})

	app := newAppWithMiddleware(Config{
		Registerer:     registry,
		Gatherer:       gatherer,
		MetricsTimeout: 50 * time.Millisecond,
	}, "")

	// A bounded wait, well clear of the 50ms deadline: should the timeout stop
	// reaching promhttp, the gather never returns and this has to fail rather
	// than hang the suite.
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("the scrape was not bounded by MetricsTimeout: %v", err)
	}
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("expected status 503 once the deadline fires, got %d", resp.StatusCode)
	}
}

// TestScrapeHandlerDetachesOnlyWithTimeout pins the wiring, not just the
// wrapper: detachRequest replaces the request's header in place, so whether the
// scrape handler was built with it is visible from the caller's own request.
func TestScrapeHandlerDetachesOnlyWithTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.Header.Set("Accept", "text/plain")
			before := req.Header["Accept"][0]

			newScrapeHandler(Config{MetricsTimeout: timeout}, prometheus.NewRegistry()).
				ServeHTTP(httptest.NewRecorder(), req)

			after := req.Header["Accept"][0]
			detached := unsafe.StringData(after) != unsafe.StringData(before)
			if detached != (timeout > 0) {
				t.Fatalf("expected detached=%v with MetricsTimeout=%s", timeout > 0, timeout)
			}
		})
	}
}

// TestBucketsAreCopiedFromConfig pins the defensive copy: client_golang aliases
// the slice it is handed into the live histogram, so a caller reusing its own
// slice would otherwise reshape an already-registered metric.
func TestBucketsAreCopiedFromConfig(t *testing.T) {
	buckets := []float64{1, 2, 3}

	app := newAppWithMiddleware(Config{
		RequestDurationBuckets: buckets,
		DisabledMetrics:        []Metric{MetricRequestSize, MetricResponseSize},
	}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	// Mutating the caller's slice must not reach the registered histogram.
	buckets[0] = 1000

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_request_duration_seconds_bucket{method="GET",path="/hello",status_code="200",le="1"}`) {
		t.Fatalf("expected the bucket bounds configured at startup, got %q", metrics)
	}
	if strings.Contains(metrics, `le="1000"`) {
		t.Fatalf("expected the caller's mutation not to reshape the histogram, got %q", metrics)
	}
}

// TestUnmatchedRouteLabelIsTakenAsGiven pins the documented contract that,
// unlike MetricsPath, the label is used verbatim - normalizing it would rename a
// live series and break every dashboard keyed on it.
func TestUnmatchedRouteLabelIsTakenAsGiven(t *testing.T) {
	app := newAppWithMiddleware(Config{
		TrackUnmatchedRequests: true,
		UnmatchedRouteLabel:    "unmatched",
	}, "")

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/nothing/here", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="unmatched"`) {
		t.Fatalf("expected the label to be used verbatim, got %q", metrics)
	}
}

// TestMetricsPathIgnoresTrailingSlash covers both sides of the documented
// tolerance: a request with a trailing slash, and a configured path carrying one.
func TestMetricsPathIgnoresTrailingSlash(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		app := newAppWithMiddleware(Config{}, "")
		if body := getMetrics(t, app, "/metrics/"); !strings.Contains(body, "go_goroutines") {
			t.Fatalf("expected /metrics/ to be served, got %q", body)
		}
	})

	t.Run("config", func(t *testing.T) {
		app := newAppWithMiddleware(Config{MetricsPath: "/telemetry/"}, "")
		if body := getMetrics(t, app, "/telemetry"); !strings.Contains(body, "go_goroutines") {
			t.Fatalf("expected /telemetry to be served, got %q", body)
		}
	})
}

// TestDisableExemplarsSkipsContextRead covers the opt-out for applications with
// no tracing middleware, which otherwise pay a context install and clear per
// request for exemplars that can never be produced.
func TestDisableExemplarsSkipsContextRead(t *testing.T) {
	prev := otel.GetTracerProvider()
	tp := tracesdk.NewTracerProvider(tracesdk.WithSampler(tracesdk.AlwaysSample()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			t.Fatalf("shutting down tracer provider: %v", err)
		}
	})

	tracer := otel.Tracer("test")

	app := fiber.New()
	handler := New(Config{DisableExemplars: true, EnableOpenMetrics: true})

	app.Use(func(c fiber.Ctx) error {
		ctxWithSpan, span := tracer.Start(c.Context(), "test-request")
		defer span.End()
		c.SetContext(ctxWithSpan)
		return handler(c)
	})

	app.Get("/traced", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/traced", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metricsReq := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	metricsReq.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	metricsResp, err := app.Test(metricsReq, noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}
	metrics := string(body)
	if !strings.Contains(metrics, `path="/traced"`) {
		t.Fatalf("expected the request to still be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, "traceID=") {
		t.Fatalf("expected no exemplar with DisableExemplars set, got %q", metrics)
	}
}

// TestTypedNilRegistryPanics covers a nil *prometheus.Registry handed over as a
// non-nil interface, which would otherwise die on a nil dereference inside
// client_golang rather than at the boundary that can name the field.
func TestTypedNilRegistryPanics(t *testing.T) {
	var registry *prometheus.Registry

	for name, cfg := range map[string]Config{
		"registerer": {Registerer: registry},
		"gatherer":   {Gatherer: registry},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("expected a panic for a typed nil")
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "typed nil") {
					t.Fatalf("expected a typed nil panic, got %v", r)
				}
			}()

			_ = New(cfg)
		})
	}
}

// TestInvalidBucketsPanicAtStartup covers bounds client_golang rejects. It only
// checks them inside newHistogram, which HistogramVec calls lazily on the first
// observation, so an unvalidated slice lets New return and the app boot, then
// panics on every instrumented request from the connection goroutine - where a
// recover mounted as this package prescribes cannot catch it.
func TestInvalidBucketsPanicAtStartup(t *testing.T) {
	nan := math.NaN()

	for name, cfg := range map[string]Config{
		"duplicate duration bound": {RequestDurationBuckets: []float64{1, 1}},
		"descending request size":  {RequestSizeBuckets: []float64{2, 1}},
		"descending response size": {ResponseSizeBuckets: []float64{100, 50, 200}},
		"nan":                      {RequestDurationBuckets: []float64{1, nan, 3}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("expected the invalid buckets to be rejected by New")
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "prometheus middleware:") {
					t.Fatalf("expected the module's own panic, got %v", r)
				}
			}()

			_ = New(cfg)
		})
	}
}

// TestValidBucketShapesAreAccepted guards the validation against rejecting what
// client_golang allows, including a trailing +Inf and the empty slice that drops
// the classic buckets.
func TestValidBucketShapesAreAccepted(t *testing.T) {
	_ = New(Config{
		RequestDurationBuckets:      []float64{0.5, 1, math.Inf(1)},
		RequestSizeBuckets:          []float64{},
		ResponseSizeBuckets:         []float64{1},
		NativeHistogramBucketFactor: 1.1,
	})
}

// TestReservedHistogramLabelPanics covers "le", which Prometheus keeps for
// bucket bounds. Counters and gauges accept it, and NewHistogramVec only builds
// the Desc, so without the check the rejection surfaces on the first request
// rather than at startup.
func TestReservedHistogramLabelPanics(t *testing.T) {
	for name, cfg := range map[string]Config{
		"constant": {Labels: prometheus.Labels{"le": "x"}},
		"dynamic":  {DynamicLabels: map[string]func(fiber.Ctx) string{"le": func(fiber.Ctx) string { return "x" }}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal(`expected "le" to be rejected by New`)
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "reserved label") {
					t.Fatalf("expected a reserved label panic, got %v", r)
				}
			}()

			_ = New(cfg)
		})
	}
}

// TestEmptyBucketsStayEmpty pins the nil-versus-empty distinction Config makes
// load-bearing: nil selects the defaults, an empty non-nil slice drops the
// classic buckets. A clone that flattened one into the other would leave the
// two spellings meaning the same thing.
func TestEmptyBucketsStayEmpty(t *testing.T) {
	cfg := configDefault(Config{RequestDurationBuckets: []float64{}})
	if cfg.RequestDurationBuckets == nil {
		t.Fatal("expected an empty non-nil slice to survive the copy")
	}
	if len(cfg.RequestDurationBuckets) != 0 {
		t.Fatalf("expected no bounds, got %v", cfg.RequestDurationBuckets)
	}

	if cfg := configDefault(Config{}); len(cfg.RequestDurationBuckets) != len(defaultRequestDurationBuckets) {
		t.Fatalf("expected nil to select the defaults, got %v", cfg.RequestDurationBuckets)
	}
}

// TestInvalidUnmatchedRouteLabelPanics covers the one label value the
// middleware supplies itself that client_golang never sees at startup: const
// labels are validated when the descriptor is built, this one only reaches
// WithLabelValues, on the first unmatched request.
func TestInvalidUnmatchedRouteLabelPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected the invalid label to be rejected by New")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "UnmatchedRouteLabel") {
			t.Fatalf("expected the panic to name the field, got %v", r)
		}
	}()

	_ = New(Config{TrackUnmatchedRequests: true, UnmatchedRouteLabel: "bad-\xff\xfe"})
}

// TestPanickingDynamicLabelDropsSample pins that instrumentation cannot take the
// request down. The functions run after the handler chain has unwound, past any
// recover the application mounted, so an unguarded panic would reach the
// connection goroutine and the client would get nothing at all.
func TestPanickingDynamicLabelDropsSample(t *testing.T) {
	app := newAppWithMiddleware(Config{
		DynamicLabels: map[string]func(fiber.Ctx) string{
			"tenant": func(c fiber.Ctx) string {
				// The shape Config invites: a local the handler may not have set.
				return c.Locals("tenant").(string)
			},
		},
	}, "")
	app.Get("/tenanted", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	app.Get("/set", func(c fiber.Ctx) error {
		c.Locals("tenant", "acme")
		return c.SendString("ok")
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/tenanted", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("the panicking label function reached the connection: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected the response to be unaffected, got %d", resp.StatusCode)
	}

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/set", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, `path="/tenanted"`) {
		t.Fatalf("expected the sample to be dropped, got %q", metrics)
	}
	if !strings.Contains(metrics, `tenant="acme"`) {
		t.Fatalf("expected the working route to still be recorded, got %q", metrics)
	}
}

// TestHeadResponseRecordsNoPayload covers the body Fiber generates for a HEAD by
// running the GET handler and fasthttp then discards: no payload bytes reach the
// client, so counting them would overstate egress for anything polled with HEAD.
func TestHeadResponseRecordsNoPayload(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/big", func(c fiber.Ctx) error {
		return c.SendString(strings.Repeat("b", 4096))
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/big", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if size := gaugeValue(t, metrics, `http_response_size_bytes_sum{method="HEAD",path="/big",status_code="200"}`); size != 0 {
		t.Fatalf("expected a HEAD response to record no payload, got %v", size)
	}
}

// TestSyntheticRouteIsNotARoutePattern covers the Route DefaultCtx substitutes
// when it has none to report: it carries the raw request path, so taking it at
// face value would put one series per distinct URL in the registry.
func TestSyntheticRouteIsNotARoutePattern(t *testing.T) {
	app := fiber.NewWithCustomCtx(func(app *fiber.App) fiber.CustomCtx {
		return &syntheticRouteCtx{DefaultCtx: fiber.NewDefaultCtx(app)}
	})
	app.Use(New(Config{TrackUnmatchedRequests: true}))
	app.Get("/user/:id", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, id := range []string{"42", "1337"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/user/"+id, nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error: %v", err)
		}
	}

	metrics := getMetrics(t, app, "")
	for _, id := range []string{"42", "1337"} {
		if strings.Contains(metrics, `path="/user/`+id+`"`) {
			t.Fatalf("expected no series for the raw request path, got %q", metrics)
		}
	}
	if !strings.Contains(metrics, `path="/__unmatched__"`) {
		t.Fatalf("expected the requests to count as unmatched, got %q", metrics)
	}
}

// syntheticRouteCtx reports the Route that DefaultCtx builds when it has none:
// the raw request path, with no handlers.
type syntheticRouteCtx struct {
	*fiber.DefaultCtx
}

func (c *syntheticRouteCtx) Route() *fiber.Route {
	return &fiber.Route{Path: c.Path(), Method: c.Method()}
}

// TestDisabledFamilyBucketsAreStillValidated pins that bad bounds are rejected
// whatever is disabled. Skipping them would only defer the failure: the
// application boots today and refuses to start the day someone drops that
// family from DisabledMetrics, over a slice they never touched.
func TestDisabledFamilyBucketsAreStillValidated(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected the bounds to be rejected even though the family is off")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "RequestSizeBuckets") {
			t.Fatalf("expected the panic to name the field, got %v", r)
		}
	}()

	_ = New(Config{
		DisabledMetrics:    []Metric{MetricRequestSize, MetricResponseSize},
		RequestSizeBuckets: []float64{2, 1},
	})
}

// failingGatherer stands in for a registry where one collector errors while the
// rest gather fine, which is what separates the two error-handling modes:
// promhttp answers 500 either way when nothing at all was gathered.
type failingGatherer struct {
	prometheus.Gatherer
}

func (g failingGatherer) Gather() ([]*dto.MetricFamily, error) {
	families, _ := g.Gatherer.Gather()
	return families, errors.New("gather exploded")
}

// recordingLogger captures what promhttp reports, which is otherwise silent.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) Println(v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprint(v...))
}

func (l *recordingLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

// TestMetricsErrorLogReceivesGatherErrors pins that the logger reaches promhttp;
// without it a failing collector is silent forever.
func TestMetricsErrorLogReceivesGatherErrors(t *testing.T) {
	logger := &recordingLogger{}
	registry := prometheus.NewRegistry()

	app := newAppWithMiddleware(Config{
		Registerer:      registry,
		Gatherer:        failingGatherer{Gatherer: registry},
		MetricsErrorLog: logger,
	}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected the default HTTPErrorOnError to answer 500, got %d", resp.StatusCode)
	}
	if logger.count() == 0 {
		t.Fatal("expected the gather error to reach MetricsErrorLog")
	}
}

// TestMetricsErrorHandlingContinuesOnError pins the other half: the handling
// mode decides what the scraper sees.
func TestMetricsErrorHandlingContinuesOnError(t *testing.T) {
	registry := prometheus.NewRegistry()

	app := newAppWithMiddleware(Config{
		Registerer:           registry,
		Gatherer:             failingGatherer{Gatherer: registry},
		MetricsErrorHandling: promhttp.ContinueOnError,
	}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected ContinueOnError to still answer 200, got %d", resp.StatusCode)
	}
}

// TestBodylessResponsesRecordNoPayload covers the statuses RFC 9110 forbids a
// body on. Fiber's error handler and any handler are free to write one, but
// fasthttp drops it, so counting those bytes overstates egress for a
// cache-fronted app serving mostly 304s.
func TestBodylessResponsesRecordNoPayload(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	for _, status := range []int{fiber.StatusContinue, fiber.StatusNoContent, fiber.StatusNotModified} {
		app.Get("/"+strconv.Itoa(status), func(c fiber.Ctx) error {
			return c.Status(status).SendString(strings.Repeat("b", 4096))
		})
	}
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendString(strings.Repeat("b", 4096))
	})

	for _, path := range []string{"/100", "/204", "/304", "/ok"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	for _, c := range []struct {
		path   string
		status string
	}{{"/100", "100"}, {"/204", "204"}, {"/304", "304"}} {
		series := `http_response_size_bytes_sum{method="GET",path="` + c.path + `",status_code="` + c.status + `"}`
		if size := gaugeValue(t, metrics, series); size != 0 {
			t.Fatalf("expected %s to record no payload, got %v", c.path, size)
		}
	}

	// The positive control: an ordinary response still records its body.
	if size := gaugeValue(t, metrics, `http_response_size_bytes_sum{method="GET",path="/ok",status_code="200"}`); size != 4096 {
		t.Fatalf("expected the ordinary response to record 4096, got %v", size)
	}
}

// TestStaleContentLengthDoesNotInflateSize covers a header left behind by a
// body-rewriting handler. fasthttp recomputes Content-Length from the body it
// writes, so the buffer is what the client receives.
func TestStaleContentLengthDoesNotInflateSize(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/stale", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderContentLength, "999999")
		return c.SendString("tiny")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/stale", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if size := gaugeValue(t, metrics, `http_response_size_bytes_sum{method="GET",path="/stale",status_code="200"}`); size != 4 {
		t.Fatalf("expected the buffered body to be measured, got %v", size)
	}
}

// TestInvalidConstantLabelValuePanicsBeforeRegistration pins that a bad label
// value is caught at the boundary rather than by client_golang, which would
// only reject it once the first family was built - after the collectors had
// gone into a caller's registry.
func TestInvalidConstantLabelValuePanicsBeforeRegistration(t *testing.T) {
	for name, cfg := range map[string]Config{
		"service name": {ServiceName: "svc-\xff"},
		"labels":       {Labels: prometheus.Labels{"zone": "eu-\xff"}},
	} {
		t.Run(name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			cfg.Registerer = registry
			cfg.Gatherer = registry

			func() {
				defer func() {
					r := recover()
					if r == nil {
						t.Fatal("expected the invalid label value to be rejected")
					}
					if msg := fmt.Sprint(r); !strings.Contains(msg, "not valid UTF-8") {
						t.Fatalf("expected a UTF-8 panic, got %v", r)
					}
				}()

				_ = New(cfg)
			}()

			families, err := registry.Gather()
			if err != nil {
				t.Fatalf("gathering after the rejected config: %v", err)
			}
			if len(families) != 0 {
				t.Fatalf("expected the registry to be untouched, got %d families", len(families))
			}
		})
	}
}

// TestTypedNilErrorLogPanics covers a nil *log.Logger in the interface, which
// promhttp tests against nil, passes, and then dereferences the first time a
// gather fails - long after the process started.
func TestTypedNilErrorLogPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic for a typed nil logger")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "MetricsErrorLog") {
			t.Fatalf("expected the panic to name the field, got %v", r)
		}
	}()

	var logger *log.Logger
	_ = New(Config{MetricsErrorLog: logger})
}

// TestInvalidRoutePatternIsSanitised covers the last label value that reached
// Prometheus unchecked. A pattern is registration-time data, but an application
// registering routes from external config can still put raw bytes in one, and
// the panic would land on the connection goroutine - where neither fasthttp nor
// Fiber installs a recover, so the process would go with it.
func TestInvalidRoutePatternIsSanitised(t *testing.T) {
	// UnescapePath is what turns a percent-escaped request into the raw bytes
	// the pattern was registered with.
	app := fiber.New(fiber.Config{UnescapePath: true})
	app.Use(New(Config{}))
	app.Get("/caf\xe9", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/caf%E9", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("the invalid route pattern reached the connection: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected the response to be unaffected, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "�") {
		t.Fatalf("expected the pattern to be recorded with a replacement rune, got %q", metrics)
	}
}

// TestInvalidNamesPanicBeforeRegistration covers the config shapes client_golang
// rejects while building a descriptor, which is after the runtime collectors
// have gone into a caller-supplied registry.
func TestInvalidNamesPanicBeforeRegistration(t *testing.T) {
	for name, cfg := range map[string]Config{
		"empty label name":     {Labels: prometheus.Labels{"": "x"}},
		"invalid label name":   {Labels: prometheus.Labels{"zone-\xff": "x"}},
		"empty dynamic name":   {DynamicLabels: map[string]func(fiber.Ctx) string{"": func(fiber.Ctx) string { return "x" }}},
		"invalid dynamic name": {DynamicLabels: map[string]func(fiber.Ctx) string{"z-\xff": func(fiber.Ctx) string { return "x" }}},
		"invalid namespace":    {Namespace: "ns-\xff"},
		"invalid subsystem":    {Subsystem: "sub-\xff"},
	} {
		t.Run(name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			cfg.Registerer = registry
			cfg.Gatherer = registry

			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("expected the invalid config to be rejected")
					}
				}()

				_ = New(cfg)
			}()

			families, err := registry.Gather()
			if err != nil {
				t.Fatalf("gathering after the rejected config: %v", err)
			}
			if len(families) != 0 {
				t.Fatalf("expected the registry to be untouched, got %d families", len(families))
			}
		})
	}
}

// TestSkipAllExcludesTheInFlightGauge pins the documented meaning of a "/*"
// entry. The gauge is incremented before routing, so nothing downstream can drop
// it - without this it would be the one family still exporting series for an
// application that asked for no instrumentation at all.
func TestSkipAllExcludesTheInFlightGauge(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/*"}}, "")
	app.Get("/a", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/a", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "http_requests_in_progress") {
		t.Fatalf("expected no in-flight gauge with everything excluded, got %q", metrics)
	}
	if !strings.Contains(metrics, "go_goroutines") {
		t.Fatalf("expected the runtime collectors to still be exposed, got %q", metrics)
	}
}

// TestBlankDisabledMetricIsIgnored matches the tolerance the skip lists have for
// the entry an unset environment variable produces.
func TestBlankDisabledMetricIsIgnored(t *testing.T) {
	app := newAppWithMiddleware(Config{DisabledMetrics: []Metric{"", "  "}}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	for _, name := range []string{"http_requests_total", "http_request_duration_seconds", "http_requests_in_progress"} {
		if !strings.Contains(metrics, name) {
			t.Fatalf("expected %s to still be exposed, got %q", name, metrics)
		}
	}
}

// TestReservedPrefixLabelNamePanics covers the "__" prefix Prometheus keeps for
// itself, which client_golang rejects while building a descriptor - after the
// runtime collectors have gone into a caller's registry.
func TestReservedPrefixLabelNamePanics(t *testing.T) {
	for name, cfg := range map[string]Config{
		"constant": {Labels: prometheus.Labels{"__tenant": "x"}},
		"dynamic":  {DynamicLabels: map[string]func(fiber.Ctx) string{"__tenant": func(fiber.Ctx) string { return "x" }}},
	} {
		t.Run(name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			cfg.Registerer = registry
			cfg.Gatherer = registry

			func() {
				defer func() {
					r := recover()
					if r == nil {
						t.Fatal(`expected the "__" prefix to be rejected`)
					}
					if msg := fmt.Sprint(r); !strings.Contains(msg, "__") {
						t.Fatalf("expected the panic to name the prefix, got %v", r)
					}
				}()

				_ = New(cfg)
			}()

			if families, _ := registry.Gather(); len(families) != 0 {
				t.Fatalf("expected the registry to be untouched, got %d families", len(families))
			}
		})
	}
}

// TestDisabledMetricsTrimsWhitespace covers the padded entries that splitting a
// setting on "," produces, which the skip lists already tolerate.
func TestDisabledMetricsTrimsWhitespace(t *testing.T) {
	disabled := make([]Metric, 0, 2)
	for _, name := range strings.Split("requests_total, request_size_bytes", ",") {
		disabled = append(disabled, Metric(name))
	}

	app := newAppWithMiddleware(Config{DisabledMetrics: disabled}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	for _, name := range []string{"http_requests_total", "http_request_size_bytes"} {
		if strings.Contains(metrics, name) {
			t.Fatalf("expected %s to be disabled, got %q", name, metrics)
		}
	}
	if !strings.Contains(metrics, "http_request_duration_seconds") {
		t.Fatalf("expected the other families to survive, got %q", metrics)
	}
}

// TestSkipURIMatchesSanitisedRoutePattern pins that both sides of the skip
// comparison get the same treatment. The route label is sanitised for
// Prometheus, so an entry kept in its raw bytes would never match it again.
func TestSkipURIMatchesSanitisedRoutePattern(t *testing.T) {
	app := fiber.New(fiber.Config{UnescapePath: true})
	app.Use(New(Config{SkipURIs: []string{"/caf\xe9"}}))
	app.Get("/caf\xe9", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/kept", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/caf%E9", "/kept"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="/kept"`) {
		t.Fatalf("expected the unfiltered route to be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, "caf") {
		t.Fatalf("expected the listed route to be skipped, got %q", metrics)
	}
}

// TestSkipAllRegistersNoFamilies is the general form of the in-flight gauge
// case: with every route excluded, nothing can receive a sample, so claiming the
// metric names in a caller's registry only invites a collision with the next
// middleware configured the same way.
func TestSkipAllRegistersNoFamilies(t *testing.T) {
	registry := prometheus.NewRegistry()

	for range 2 {
		app := fiber.New()
		app.Use(New(Config{
			Registerer:              registry,
			Gatherer:                registry,
			SkipURIs:                []string{"/*"},
			DisableGoCollector:      true,
			DisableProcessCollector: true,
		}))
		app.Get("/a", func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		})

		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/a", nil), noTimeoutConfig); err != nil {
			t.Fatalf("unexpected request error: %v", err)
		}
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	if len(families) != 0 {
		t.Fatalf("expected no families to be registered, got %d", len(families))
	}
}

// TestLegacyNameValidationIsHonoured pins that the middleware applies
// client_golang's own rule rather than approximating it. Under the legacy
// scheme a hyphen makes a name invalid, and client_golang would only say so
// while building the first descriptor - with the runtime collectors already in
// the caller's registry.
func TestLegacyNameValidationIsHonoured(t *testing.T) {
	previous := model.NameValidationScheme
	model.NameValidationScheme = model.LegacyValidation
	t.Cleanup(func() { model.NameValidationScheme = previous })

	for name, cfg := range map[string]Config{
		"namespace":     {Namespace: "my-app"},
		"subsystem":     {Subsystem: "my-sub"},
		"label":         {Labels: prometheus.Labels{"my-label": "x"}},
		"dynamic label": {DynamicLabels: map[string]func(fiber.Ctx) string{"my-label": func(fiber.Ctx) string { return "x" }}},
	} {
		t.Run(name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			cfg.Registerer = registry
			cfg.Gatherer = registry

			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("expected the legacy-invalid name to be rejected")
					}
				}()

				_ = New(cfg)
			}()

			if families, _ := registry.Gather(); len(families) != 0 {
				t.Fatalf("expected the registry to be untouched, got %d families", len(families))
			}
		})
	}
}

// TestInertMiddlewarePassesErrorsThrough pins that a middleware with nothing
// left to record stays out of the way. Taking over the error handler would stop
// errors from reaching middleware mounted before it, and move where the handler
// runs in the stack, for no metric at all.
func TestInertMiddlewarePassesErrorsThrough(t *testing.T) {
	app := fiber.New()

	var seen error
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		seen = err
		return err
	})
	app.Use(New(Config{SkipURIs: []string{"/*"}}))
	app.Get("/boom", func(fiber.Ctx) error {
		return fiber.ErrTeapot
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/boom", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusTeapot {
		t.Fatalf("expected status 418, got %d", resp.StatusCode)
	}
	if seen == nil {
		t.Fatal("expected the error to reach the middleware mounted before this one")
	}
}

// TestGaugeOnlyMiddlewarePassesErrorsThrough is the other half of
// TestInertMiddlewarePassesErrorsThrough: the in-flight gauge is labelled by
// method alone and never reads the status, so a configuration that keeps only
// the gauge has no reason to take over the error handler either.
func TestGaugeOnlyMiddlewarePassesErrorsThrough(t *testing.T) {
	app := fiber.New()

	var seen error
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		seen = err
		return err
	})
	app.Use(New(Config{DisabledMetrics: []Metric{
		MetricRequestsTotal,
		MetricRequestsStatusClassTotal,
		MetricRequestDuration,
		MetricRequestSize,
		MetricResponseSize,
	}}))
	app.Get("/boom", func(fiber.Ctx) error {
		return fiber.ErrTeapot
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/boom", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusTeapot {
		t.Fatalf("expected status 418, got %d", resp.StatusCode)
	}
	if seen == nil {
		t.Fatal("expected the error to reach the middleware mounted before this one")
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "http_requests_in_progress") {
		t.Fatalf("expected the gauge to still be exposed, got %q", metrics)
	}
}

// TestMetricNamesValidatedWhenEverythingIsExcluded pins that a bad Namespace is
// caught whatever is disabled. Gating the check would let it sit unnoticed until
// the day someone turns a family back on and the application stops booting.
func TestMetricNamesValidatedWhenEverythingIsExcluded(t *testing.T) {
	previous := model.NameValidationScheme
	model.NameValidationScheme = model.LegacyValidation
	t.Cleanup(func() { model.NameValidationScheme = previous })

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected the invalid namespace to be rejected")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "valid metric name") {
			t.Fatalf("expected a metric name panic, got %v", r)
		}
	}()

	_ = New(Config{Namespace: "my-app", SkipURIs: []string{"/*"}})
}

// TestInvalidNamespaceNamesTheSameMetricEveryTime pins the deterministic report.
// A startup panic pasted into a bug report has to be reproducible, and iterating
// a map would name whichever family the runtime yielded first.
func TestInvalidNamespaceNamesTheSameMetricEveryTime(t *testing.T) {
	previous := model.NameValidationScheme
	model.NameValidationScheme = model.LegacyValidation
	t.Cleanup(func() { model.NameValidationScheme = previous })

	messages := make(map[string]struct{})
	for range 16 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					messages[fmt.Sprint(r)] = struct{}{}
				}
			}()

			_ = New(Config{Namespace: "my-app"})
		}()
	}

	if len(messages) != 1 {
		t.Fatalf("expected one panic message across runs, got %d: %v", len(messages), messages)
	}
}

// TestDoubledWildcardSkipsEverything covers the glob spelling an operator is
// likely to reach for. Recognising only a single trailing star turned it into a
// near-no-op that excluded nothing and said nothing.
func TestDoubledWildcardSkipsEverything(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/**"}}, "")
	app.Get("/a", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/a", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	for _, name := range []string{"http_requests_total", "http_requests_in_progress"} {
		if strings.Contains(metrics, name) {
			t.Fatalf("expected %s to be absent with everything excluded, got %q", name, metrics)
		}
	}
}

// TestInvalidServiceNamePanicsByName pins that the panic names the field the
// caller set. ServiceName becomes the "service" label, so validating it only
// after the merge would point at a key they never wrote.
func TestInvalidServiceNamePanicsByName(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected the invalid service name to be rejected")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "ServiceName") {
			t.Fatalf("expected the panic to name ServiceName, got %v", r)
		}
	}()

	_ = New(Config{ServiceName: "svc-\xff"})
}

// TestMetricsPathIsTrimmed covers the trailing newline an environment variable
// or a mounted secret picks up, which would otherwise leave the scrape endpoint
// unreachable with no warning at all.
func TestMetricsPathIsTrimmed(t *testing.T) {
	for _, path := range []string{"/telemetry\n", " /telemetry", "/telemetry "} {
		t.Run(strconv.Quote(path), func(t *testing.T) {
			app := newAppWithMiddleware(Config{MetricsPath: path}, "")
			if body := getMetrics(t, app, "/telemetry"); !strings.Contains(body, "go_goroutines") {
				t.Fatalf("expected /telemetry to be served, got %q", body)
			}
		})
	}
}

// TestDisabledDurationHistogram covers the path taken when the only consumer of
// the request clock is gone. It pins the behaviour - everything else is still
// recorded - not the saved clock reads, which produce no observable difference.
func TestDisabledDurationHistogram(t *testing.T) {
	app := newAppWithMiddleware(Config{DisabledMetrics: []Metric{MetricRequestDuration}}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, "http_request_duration_seconds") {
		t.Fatalf("expected the duration histogram to be absent, got %q", metrics)
	}
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/hello",status_code="200"}`) {
		t.Fatalf("expected the request to still be counted, got %q", metrics)
	}
}

// TestDurationExcludesInstrumentationOverhead pins where the stopwatch stops.
// Everything between the chain unwinding and the observation is this
// middleware's own work - the route lookup, the skip scan, the counter
// increment, the context read, and any DynamicLabels function the application
// supplied - and charging that to the request would put instrumentation
// overhead into the metric people alert on.
func TestDurationExcludesInstrumentationOverhead(t *testing.T) {
	const labelCost = 120 * time.Millisecond

	app := newAppWithMiddleware(Config{
		DynamicLabels: map[string]func(fiber.Ctx) string{
			"slow": func(fiber.Ctx) string {
				time.Sleep(labelCost)
				return "x"
			},
		},
	}, "")
	app.Get("/fast", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/fast", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	series := `http_request_duration_seconds_sum{method="GET",path="/fast",slow="x",status_code="200"}`
	if seconds := gaugeValue(t, metrics, series); seconds >= labelCost.Seconds()/2 {
		t.Fatalf("expected the label function's cost to stay out of the request duration, got %vs", seconds)
	}
}

// TestUnusableNativeHistogramFactorPanics covers a factor between 0 and 1 - 0.1
// where 1.1 was meant. client_golang enables nothing below 1, then substitutes
// its latency defaults for any bucket slice deliberately left empty, so a byte
// histogram silently ends up bucketed in seconds.
func TestUnusableNativeHistogramFactorPanics(t *testing.T) {
	for _, factor := range []float64{0.1, 1, math.NaN()} {
		t.Run(fmt.Sprint(factor), func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected factor %v to be rejected", factor)
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "NativeHistogramBucketFactor") {
					t.Fatalf("expected the panic to name the field, got %v", r)
				}
			}()

			_ = New(Config{NativeHistogramBucketFactor: factor})
		})
	}
}

// TestSkipURIsMatchesUnmatchedLabel covers filtering unmatched traffic with the
// label spelled the way Config recommends. Forcing a leading slash onto the
// entry would leave the filter working in the default config and silently
// failing the moment the operator took that advice.
func TestSkipURIsMatchesUnmatchedLabel(t *testing.T) {
	for _, label := range []string{"unmatched", "/__unmatched__"} {
		t.Run(label, func(t *testing.T) {
			app := newAppWithMiddleware(Config{
				TrackUnmatchedRequests: true,
				UnmatchedRouteLabel:    label,
				SkipURIs:               []string{label},
			}, "")
			app.Get("/kept", func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			})

			for _, path := range []string{"/nothing/here", "/kept"} {
				if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
					t.Fatalf("requesting %s: %v", path, err)
				}
			}

			metrics := getMetrics(t, app, "")
			if !strings.Contains(metrics, `path="/kept"`) {
				t.Fatalf("expected the ordinary route to be recorded, got %q", metrics)
			}
			if strings.Contains(metrics, `path="`+label+`"`) {
				t.Fatalf("expected unmatched traffic to be skipped, got %q", metrics)
			}
		})
	}
}

// TestUnmatchedRouteLabelIsTrimmed covers the trailing newline a mounted secret
// picks up, which is valid UTF-8 and would otherwise become part of every
// unmatched series' label.
func TestUnmatchedRouteLabelIsTrimmed(t *testing.T) {
	app := newAppWithMiddleware(Config{
		TrackUnmatchedRequests: true,
		UnmatchedRouteLabel:    "unmatched\n",
	}, "")

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/nothing/here", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="unmatched"`) {
		t.Fatalf("expected the label to be trimmed, got %q", metrics)
	}
}

// TestMultipartUploadIsNotReMarshalled covers the request size of a pre-parsed
// multipart form. fasthttp keeps the parsed parts rather than the raw body -
// spilling large ones to temp files so they never sit in memory - and
// Request.Body() re-marshals the whole form into a fresh buffer, so measuring
// the buffer would double what an upload costs and throw the copy away.
func TestMultipartUploadIsNotReMarshalled(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "payload.bin")
	if err != nil {
		t.Fatalf("creating the form file: %v", err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("u"), 4096)); err != nil {
		t.Fatalf("writing the form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the writer: %v", err)
	}
	announced := float64(body.Len())

	app := newAppWithMiddleware(Config{}, "")
	app.Post("/upload", func(c fiber.Ctx) error {
		if _, err := c.FormFile("file"); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(fiber.MethodPost, "/upload", body)
	req.Header.Set(fiber.HeaderContentType, writer.FormDataContentType())
	resp, err := app.Test(req, noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	metrics := getMetrics(t, app, "")
	series := `http_request_size_bytes_sum{method="POST",path="/upload",status_code="200"}`
	if size := gaugeValue(t, metrics, series); size != announced {
		t.Fatalf("expected the announced %v bytes, got %v", announced, size)
	}
}

// TestUnsampledTraceProducesNoExemplar pins that an exemplar points somewhere.
// Under head-based sampling every request carries a valid trace ID, and
// Prometheus keeps one exemplar per bucket, overwritten on each observation - so
// recording unsampled traces would evict the links that lead anywhere.
func TestUnsampledTraceProducesNoExemplar(t *testing.T) {
	prev := otel.GetTracerProvider()
	tp := tracesdk.NewTracerProvider(tracesdk.WithSampler(tracesdk.NeverSample()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			t.Fatalf("shutting down tracer provider: %v", err)
		}
	})

	tracer := otel.Tracer("test")

	app := fiber.New()
	handler := New(Config{EnableOpenMetrics: true})
	app.Use(func(c fiber.Ctx) error {
		ctxWithSpan, span := tracer.Start(c.Context(), "test-request")
		defer span.End()
		c.SetContext(ctxWithSpan)
		return handler(c)
	})
	app.Get("/traced", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/traced", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metricsReq := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	metricsReq.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	metricsResp, err := app.Test(metricsReq, noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics: %v", err)
	}
	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}
	metrics := string(body)

	if !strings.Contains(metrics, `path="/traced"`) {
		t.Fatalf("expected the request to still be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, "traceID=") {
		t.Fatalf("expected no exemplar for an unsampled trace, got %q", metrics)
	}
}

// TestSkipURIEntryFiltersBothMeanings covers an entry that names the unmatched
// label and a registered route at once: diverting it to the unmatched key alone
// would make the route filter the operator also asked for a silent no-op.
func TestSkipURIEntryFiltersBothMeanings(t *testing.T) {
	app := newAppWithMiddleware(Config{
		TrackUnmatchedRequests: true,
		UnmatchedRouteLabel:    "admin",
		SkipURIs:               []string{"admin"},
	}, "")
	app.Get("/admin", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/kept", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/admin", "/kept", "/nothing/here"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="/kept"`) {
		t.Fatalf("expected the unfiltered route to be recorded, got %q", metrics)
	}
	if strings.Contains(metrics, `path="/admin"`) {
		t.Fatalf("expected the route of that name to be skipped, got %q", metrics)
	}
	if strings.Contains(metrics, `path="admin"`) {
		t.Fatalf("expected unmatched traffic to be skipped, got %q", metrics)
	}
}

// TestNegativeNativeHistogramFactorPanics covers the other side of zero: a
// negative factor is neither 0 nor greater than 1, and client_golang treats it
// exactly as it treats 0.1.
func TestNegativeNativeHistogramFactorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a negative factor to be rejected")
		}
	}()

	_ = New(Config{NativeHistogramBucketFactor: -1})
}

// TestInflatedContentLengthIsNotRecorded covers the shape a client can send for
// free: a body over BodyLimit, which fasthttp rejects before reading a byte of
// it. Fiber then replays the Use chain, and trusting the announced length would
// let a few hundred bytes of traffic bill the histogram for half a gigabyte.
func TestInflatedContentLengthIsNotRecorded(t *testing.T) {
	// Both shapes: an ordinary body, and the multipart one whose announced
	// length the middleware does consult when the form was actually parsed.
	for _, contentType := range []string{"application/octet-stream", "multipart/form-data; boundary=zzz"} {
		t.Run(contentType, func(t *testing.T) {
			testInflatedContentLength(t, contentType)
		})
	}
}

func testInflatedContentLength(t *testing.T, contentType string) {
	t.Helper()

	app := fiber.New(fiber.Config{BodyLimit: 1024})
	app.Use(New(Config{TrackUnmatchedRequests: true}))
	app.Post("/upload", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(fiber.MethodPost, "/upload", strings.NewReader("tiny"))
	req.Header.Set(fiber.HeaderContentType, contentType)
	req.Header.Set(fiber.HeaderContentLength, "500000000")
	req.ContentLength = 500000000
	// fasthttp rejects the request at the connection level, which app.Test
	// surfaces as an error rather than a response - but Fiber has already
	// replayed the Use chain by then, so the middleware ran and may have
	// recorded something.
	_, _ = app.Test(req, noTimeoutConfig)

	metrics := getMetrics(t, app, "")
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, "http_request_size_bytes_sum") {
			continue
		}
		_, value, _ := strings.Cut(line, " ")
		size, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
		if size > 4 {
			t.Fatalf("expected no size beyond what arrived, got %v in %q", size, line)
		}
	}
}

// TestZeroLengthStreamIsRecorded covers c.SendStream(reader, 0): the length is
// announced, not absent, so the observation belongs in the histogram - otherwise
// _count under-reports against http_requests_total.
func TestZeroLengthStreamIsRecorded(t *testing.T) {
	app := newAppWithMiddleware(Config{}, "")
	app.Get("/empty-stream", func(c fiber.Ctx) error {
		return c.SendStream(strings.NewReader(""), 0)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/empty-stream", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	series := `http_response_size_bytes_count{method="GET",path="/empty-stream",status_code="200"}`
	if count := gaugeValue(t, metrics, series); count != 1 {
		t.Fatalf("expected the observation to be recorded, got %v", count)
	}
}

// TestDuplicateWildcardEntriesShareOnePrefix pins that the spellings documented
// as equivalent collapse to a single scan, rather than repeating an identical
// HasPrefix on every instrumented request for the process lifetime.
func TestDuplicateWildcardEntriesShareOnePrefix(t *testing.T) {
	// The prefix set is internal, so drive resolveFilters directly.
	mw := &middleware{
		skipURIs:          make(map[string]struct{}),
		skipStatusCodes:   make(map[int]struct{}),
		skipStatusClasses: make(map[string]struct{}),
		unmatchedLabel:    "/__unmatched__",
	}
	mw.resolveFilters(Config{SkipURIs: []string{"/admin/*", "/admin/**", "/admin/*/"}})

	if len(mw.skipPrefixes) != 1 {
		t.Fatalf("expected one prefix, got %v", mw.skipPrefixes)
	}
	if mw.skipPrefixes[0] != "/admin/" {
		t.Fatalf("expected the prefix to be /admin/, got %q", mw.skipPrefixes[0])
	}
}

// TestSkipURIsWildcardMatchesUnmatchedLabel covers the star spelling against a
// label without a leading slash - the one Config recommends. The comparison has
// to happen before the entry is given a slash it never wears itself.
func TestSkipURIsWildcardMatchesUnmatchedLabel(t *testing.T) {
	for _, label := range []string{"unmatched", "/__unmatched__"} {
		t.Run(label, func(t *testing.T) {
			app := newAppWithMiddleware(Config{
				TrackUnmatchedRequests: true,
				UnmatchedRouteLabel:    label,
				SkipURIs:               []string{label + "*"},
			}, "")
			app.Get("/kept", func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			})

			for _, path := range []string{"/nothing/here", "/kept"} {
				if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
					t.Fatalf("requesting %s: %v", path, err)
				}
			}

			metrics := getMetrics(t, app, "")
			if !strings.Contains(metrics, `path="/kept"`) {
				t.Fatalf("expected the ordinary route to be recorded, got %q", metrics)
			}
			if strings.Contains(metrics, `path="`+label+`"`) {
				t.Fatalf("expected unmatched traffic to be skipped, got %q", metrics)
			}
		})
	}
}

// TestPrefixSkipDoesNotSwallowUnmatchedLabel pins that a filter written for real
// routes cannot take 404 monitoring with it. The label is a value, not a path,
// so no prefix rule applies to it however it is spelled.
func TestPrefixSkipDoesNotSwallowUnmatchedLabel(t *testing.T) {
	app := newAppWithMiddleware(Config{
		TrackUnmatchedRequests: true,
		UnmatchedRouteLabel:    "/api/unmatched",
		SkipURIs:               []string{"/api/*"},
	}, "")
	app.Get("/api/users", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/api/users", "/nothing/here"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, `path="/api/users"`) {
		t.Fatalf("expected the API route to be skipped, got %q", metrics)
	}
	if !strings.Contains(metrics, `path="/api/unmatched"`) {
		t.Fatalf("expected unmatched traffic to survive a prefix filter, got %q", metrics)
	}
}

// TestServiceNameIsTrimmed covers the trailing newline an environment variable
// picks up. It is valid UTF-8, so nothing panics - the whole application's
// metrics simply stop matching any dashboard selecting on the service label.
func TestServiceNameIsTrimmed(t *testing.T) {
	app := newAppWithMiddleware(Config{ServiceName: "svc\n"}, "")
	app.Get("/kept", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/kept", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `service="svc"`) {
		t.Fatalf("expected the service label to be trimmed, got %q", metrics)
	}
}

// TestConfigDefaultBucketsAreNotShared pins that ConfigDefault is a value a
// caller may copy and adjust in place. Aliasing the package defaults would let
// one bound edited on that copy reshape every later New that supplies none.
func TestConfigDefaultBucketsAreNotShared(t *testing.T) {
	// Captured by value first: were the slices shared, the write below would
	// corrupt the very array an assertion against it would read back.
	want := defaultRequestDurationBuckets[0]

	cfg := ConfigDefault
	cfg.RequestDurationBuckets[0] = 30
	t.Cleanup(func() {
		cfg.RequestDurationBuckets[0] = want
		defaultRequestDurationBuckets[0] = want
	})

	resolved := configDefault(Config{})
	if resolved.RequestDurationBuckets[0] != want {
		t.Fatalf("expected the defaults to survive, got %v", resolved.RequestDurationBuckets[0])
	}
}

// TestRoutePatternTrailingSlashIsTrimmed pins the trimming that lets a SkipURIs
// entry match a pattern however it was spelled, and that keeps "/x" and "/x/"
// from becoming two series for one endpoint.
func TestRoutePatternTrailingSlashIsTrimmed(t *testing.T) {
	app := newAppWithMiddleware(Config{SkipURIs: []string{"/skipped"}}, "")
	app.Get("/trailing/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/skipped/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/trailing/", "/skipped/"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `path="/trailing"`) {
		t.Fatalf("expected the pattern to be recorded without its trailing slash, got %q", metrics)
	}
	if strings.Contains(metrics, `path="/trailing/"`) {
		t.Fatalf("expected no series carrying the trailing slash, got %q", metrics)
	}
	// The entry was spelled without the slash the route carries, which only
	// matches because both sides are trimmed.
	if strings.Contains(metrics, "skipped") {
		t.Fatalf("expected the skip entry to match the pattern, got %q", metrics)
	}
}

// TestDynamicLabelsAreSortedDeterministically pins the ordering the label buffer
// depends on. Prometheus sorts label pairs alphabetically for the exposition, so
// map iteration order is invisible there - what it would change is the order the
// names are declared in each Desc, which has to stay the same across restarts.
func TestDynamicLabelsAreSortedDeterministically(t *testing.T) {
	fn := func(fiber.Ctx) string { return "x" }
	cfg := configDefault(Config{DynamicLabels: map[string]func(fiber.Ctx) string{
		"zone": fn, "tenant": fn, "region": fn, "cluster": fn, "shard": fn,
	}})

	// Repeated, because Go randomizes map iteration per range: one unsorted
	// build would be indistinguishable from a sorted one by luck.
	for range 20 {
		resolved := resolveDynamicLabels(cfg)
		names := make([]string, len(resolved))
		for i, label := range resolved {
			names[i] = label.name
		}
		if !slices.IsSorted(names) {
			t.Fatalf("expected the dynamic labels to be sorted, got %v", names)
		}
	}
}

// TestWildcardPrefixDoesNotSwallowUnmatchedLabel covers the exact key a wildcard
// entry leaves behind. "/api/*" registers "/api" so that the route of that name
// is skipped too - but when the unmatched label is "/api", that byproduct would
// silently take every 404 with it.
func TestWildcardPrefixDoesNotSwallowUnmatchedLabel(t *testing.T) {
	app := newAppWithMiddleware(Config{
		TrackUnmatchedRequests: true,
		UnmatchedRouteLabel:    "/api",
		SkipURIs:               []string{"/api/*"},
	}, "")
	app.Get("/api/users", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/kept", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/api/users", "/kept", "/nothing/here"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, `path="/api/users"`) {
		t.Fatalf("expected the wildcard to still exclude routes below it, got %q", metrics)
	}
	if !strings.Contains(metrics, `path="/api"`) {
		t.Fatalf("expected unmatched traffic to survive the wildcard, got %q", metrics)
	}
	if !strings.Contains(metrics, `path="/kept"`) {
		t.Fatalf("expected the unfiltered route to be recorded, got %q", metrics)
	}
}

// TestExplicitEntryStillSkipsUnmatchedLabel is the counterpart: naming the label
// deliberately still works, wildcard or not.
func TestExplicitEntryStillSkipsUnmatchedLabel(t *testing.T) {
	app := newAppWithMiddleware(Config{
		TrackUnmatchedRequests: true,
		UnmatchedRouteLabel:    "/api",
		SkipURIs:               []string{"/api", "/api/*"},
	}, "")
	app.Get("/kept", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/kept", "/nothing/here"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, `path="/api"`) {
		t.Fatalf("expected the explicit entry to skip unmatched traffic, got %q", metrics)
	}
	if !strings.Contains(metrics, `path="/kept"`) {
		t.Fatalf("expected the unfiltered route to be recorded, got %q", metrics)
	}
}

// TestRealRouteAtUnmatchedLabelObeysPrefixRules pins that the two namespaces are
// separate. SkipURIs holds route patterns; UnmatchedRouteLabel is a value. A
// route spelled the same as the label must still obey "/admin/*", and unmatched
// traffic must still survive it.
func TestRealRouteAtUnmatchedLabelObeysPrefixRules(t *testing.T) {
	app := newAppWithMiddleware(Config{
		TrackUnmatchedRequests: true,
		UnmatchedRouteLabel:    "/admin",
		SkipURIs:               []string{"/admin/*"},
	}, "")
	app.Get("/admin", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/admin/users", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for _, path := range []string{"/admin", "/admin/users", "/nothing/here"} {
		if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), noTimeoutConfig); err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
	}

	metrics := getMetrics(t, app, "")
	if strings.Contains(metrics, `path="/admin",status_code="200"`) {
		t.Fatalf("expected the wildcard to exclude the route of that name, got %q", metrics)
	}
	if strings.Contains(metrics, `path="/admin/users"`) {
		t.Fatalf("expected the wildcard to exclude routes below it, got %q", metrics)
	}
	if !strings.Contains(metrics, `path="/admin",status_code="404"`) {
		t.Fatalf("expected unmatched traffic to survive a route filter, got %q", metrics)
	}
}

// TestUnmatchedLabelEntryIgnoresTrailingSlash covers the spellings Config
// documents as equivalent: the slash is trimmed before the star, so the
// separator still distinguishes a label entry from a prefix rule.
func TestUnmatchedLabelEntryIgnoresTrailingSlash(t *testing.T) {
	for _, entry := range []string{"/api", "/api*", "/api*/", "/api/"} {
		t.Run(entry, func(t *testing.T) {
			app := newAppWithMiddleware(Config{
				TrackUnmatchedRequests: true,
				UnmatchedRouteLabel:    "/api",
				SkipURIs:               []string{entry},
			}, "")

			if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/nothing/here", nil), noTimeoutConfig); err != nil {
				t.Fatalf("unexpected request error: %v", err)
			}

			if metrics := getMetrics(t, app, ""); strings.Contains(metrics, `path="/api"`) {
				t.Fatalf("expected %q to filter unmatched traffic, got %q", entry, metrics)
			}
		})
	}
}

// TestPrefixEntryDoesNotFilterUnmatchedLabel is the counterpart: a rule for the
// routes below "/api" says nothing about a label spelled "/api".
func TestPrefixEntryDoesNotFilterUnmatchedLabel(t *testing.T) {
	app := newAppWithMiddleware(Config{
		TrackUnmatchedRequests: true,
		UnmatchedRouteLabel:    "/api",
		SkipURIs:               []string{"/api/*"},
	}, "")

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/nothing/here", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	if metrics := getMetrics(t, app, ""); !strings.Contains(metrics, `path="/api"`) {
		t.Fatalf("expected unmatched traffic to be recorded, got %q", metrics)
	}
}

// TestNamespaceAndSubsystemAreTrimmed covers the newline an environment variable
// carries. The default validation scheme accepts it, so it renames every family
// instead of failing.
func TestNamespaceAndSubsystemAreTrimmed(t *testing.T) {
	app := newAppWithMiddleware(Config{Namespace: "svc\n", Subsystem: " api "}, "")
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, "svc_api_requests_total") {
		t.Fatalf("expected the prefixes to be trimmed, got %q", metrics)
	}
}

// TestPanickingDynamicLabelIsReported pins that the drop is visible. Silently
// recording nothing looks exactly like no traffic at all.
func TestPanickingDynamicLabelIsReported(t *testing.T) {
	logger := &recordingLogger{}

	app := newAppWithMiddleware(Config{
		MetricsErrorLog: logger,
		DynamicLabels: map[string]func(fiber.Ctx) string{
			"tenant": func(c fiber.Ctx) string { return c.Locals("tenant").(string) },
		},
	}, "")
	app.Get("/tenanted", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/tenanted", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	if logger.count() == 0 {
		t.Fatal("expected the dropped sample to reach MetricsErrorLog")
	}
}

// TestInvalidErrorHandlingPanics covers a value outside promhttp's three: its
// switch has no default, so the failure is served as if healthy.
func TestInvalidErrorHandlingPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected an unknown error-handling mode to be rejected")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "MetricsErrorHandling") {
			t.Fatalf("expected the panic to name the field, got %v", r)
		}
	}()

	_ = New(Config{MetricsErrorHandling: promhttp.HandlerErrorHandling(9)})
}

// TestDegenerateBucketsPanic covers bounds that leave a histogram with nothing
// useful: a lone +Inf (client_golang substitutes defaults before dropping it, so
// the substitution never runs) and -Inf (a permanently empty bucket per scrape).
func TestDegenerateBucketsPanic(t *testing.T) {
	for name, cfg := range map[string]Config{
		"only +Inf": {RequestDurationBuckets: []float64{math.Inf(1)}},
		"-Inf":      {RequestSizeBuckets: []float64{math.Inf(-1), 1, 2}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected the degenerate bounds to be rejected")
				}
			}()

			_ = New(cfg)
		})
	}
}

// TestExtendedGoCollectorIsTolerated covers a caller-supplied registry that
// already holds its own Go collector. That collides per descriptor rather than
// as AlreadyRegistered, and it is a deliberate opt-in, not a misconfiguration.
func TestExtendedGoCollectorIsTolerated(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(
		collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsScheduler),
	))

	app := fiber.New()
	app.Use(New(Config{Registerer: registry, Gatherer: registry}))
	app.Get("/hello", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hello", nil), noTimeoutConfig); err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	metrics := getMetrics(t, app, "")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/hello",status_code="200"}`) {
		t.Fatalf("expected the middleware to register alongside the extended collector, got %q", metrics)
	}
}
