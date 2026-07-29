package prometheus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
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

func newAppWithMiddleware(cfg Config, metricsPath string) (*fiber.App, fiber.Handler) {
	if metricsPath != "" {
		cfg.MetricsPath = metricsPath
	}

	app := fiber.New()
	handler := New(cfg)
	app.Use(handler)

	return app, handler
}

func TestMiddlewareRecordsMetrics(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{Service: "test-service"}, "")
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
	app, _ := newAppWithMiddleware(Config{}, "")

	metrics := getMetrics(t, app, "/metrics")

	if !strings.Contains(metrics, "go_goroutines") {
		t.Fatalf("expected Go collector metrics, got %q", metrics)
	}

	if !strings.Contains(metrics, "process_cpu_seconds_total") {
		t.Fatalf("expected process collector metrics, got %q", metrics)
	}
}

func TestSkipURIs(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{SkipURIs: []string{"/skip"}}, "")
	app.Get("/skip", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/skip", nil), noTimeoutConfig); err != nil {
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
	if strings.Contains(metrics, "path=\"/skip\"") {
		t.Fatalf("expected skip path to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_request_size_bytes_sum{status_code=\"200\",method=\"GET\",path=\"/skip\"}") {
		t.Fatalf("expected skip path request size metric to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_response_size_bytes_sum{status_code=\"200\",method=\"GET\",path=\"/skip\"}") {
		t.Fatalf("expected skip path response size metric to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_requests_status_class_total{status_class=\"2xx\",method=\"GET\",path=\"/skip\"}") {
		t.Fatalf("expected skip path status class metric to be excluded")
	}
}

func TestIgnoreStatusCodes(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{IgnoreStatusCodes: []int{fiber.StatusUnauthorized}}, "")
	app.Get("/deny", func(c fiber.Ctx) error {
		return fiber.ErrUnauthorized
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/deny", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
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
	if strings.Contains(metrics, "status_code=\"401\"") {
		t.Fatalf("expected status code 401 to be ignored, got %q", metrics)
	}
	if strings.Contains(metrics, "http_request_size_bytes_sum{status_code=\"401\",method=\"GET\",path=\"/deny\"}") {
		t.Fatalf("expected ignored status code request size metric to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_response_size_bytes_sum{status_code=\"401\",method=\"GET\",path=\"/deny\"}") {
		t.Fatalf("expected ignored status code response size metric to be excluded, got %q", metrics)
	}
	if strings.Contains(metrics, "http_requests_status_class_total{status_class=\"4xx\",method=\"GET\",path=\"/deny\"}") {
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
	app, _ := newAppWithMiddleware(Config{
		IgnoreStatusCodes: []int{fiber.StatusUnauthorized},
		SkipURIs:          []string{"/skip"},
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
	app, _ := newAppWithMiddleware(cfg, "")
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
	app, _ := newAppWithMiddleware(Config{}, "")

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
	app, _ := newAppWithMiddleware(Config{TrackUnmatchedRequests: true}, "")

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
	app, _ := newAppWithMiddleware(Config{}, "")

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
	app, _ := newAppWithMiddleware(Config{
		Next: func(c fiber.Ctx) bool {
			return c.Path() == "/healthz"
		},
	}, "")
	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/healthz", nil), noTimeoutConfig); err != nil {
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
	if strings.Contains(metrics, "path=\"/healthz\"") {
		t.Fatalf("expected next-skipped path to be excluded, got %q", metrics)
	}
}

func TestCustomMetricsPath(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{}, "/internal/metrics")
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
	app, _ := newAppWithMiddleware(Config{}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("fetching metrics with HEAD: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestMetricsEndpointRejectsOtherMethods(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{}, "")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/metrics", nil), noTimeoutConfig)
	if err != nil {
		t.Fatalf("posting metrics: %v", err)
	}
	if resp.StatusCode != fiber.StatusMethodNotAllowed {
		t.Fatalf("expected status 405, got %d", resp.StatusCode)
	}
}

func TestHeadRequestsMatchGetRoutes(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{}, "")

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
	app, _ := newAppWithMiddleware(Config{Registerer: registry}, "")
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
	app, _ := newAppWithMiddleware(Config{EnableOpenMetrics: true}, "")
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
	app, _ := newAppWithMiddleware(Config{
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
	app, _ := newAppWithMiddleware(Config{DisableCompression: true}, "")
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
	app, _ := newAppWithMiddleware(Config{DisableGoCollector: true}, "")

	metrics := getMetrics(t, app, "")

	if strings.Contains(metrics, "go_goroutines") {
		t.Fatalf("expected Go collector metrics to be disabled, got %q", metrics)
	}

	if !strings.Contains(metrics, "process_cpu_seconds_total") {
		t.Fatalf("expected process collector metrics to remain enabled, got %q", metrics)
	}
}

func TestDisableProcessCollector(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{DisableProcessCollector: true}, "")

	metrics := getMetrics(t, app, "")

	if strings.Contains(metrics, "process_cpu_seconds_total") {
		t.Fatalf("expected process collector metrics to be disabled, got %q", metrics)
	}

	if !strings.Contains(metrics, "go_goroutines") {
		t.Fatalf("expected Go collector metrics to remain enabled, got %q", metrics)
	}
}

func TestStatusClassMetrics(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{}, "")

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
	app, _ := newAppWithMiddleware(Config{}, "")
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
	app, _ := newAppWithMiddleware(Config{}, "")
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
	app, _ := newAppWithMiddleware(Config{}, "")
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

func TestWrappedFiberErrorRespectsIgnoreStatusCodes(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{IgnoreStatusCodes: []int{fiber.StatusNotFound}}, "")
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
	app, _ := newAppWithMiddleware(Config{
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
	app, _ := newAppWithMiddleware(Config{MetricsPath: "internal/metrics"}, "")
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
	app, _ := newAppWithMiddleware(Config{TrackUnmatchedRequests: true}, "")

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
	app, _ := newAppWithMiddleware(Config{}, "")
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
	app, _ := newAppWithMiddleware(Config{}, "")
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
