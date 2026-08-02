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
	dto "github.com/prometheus/client_model/go"
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

func TestDisabledMetricsRemovesFamilies(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{
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
	app, _ := newAppWithMiddleware(Config{
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
	app, _ := newAppWithMiddleware(Config{
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
	app, _ := newAppWithMiddleware(Config{SkipURIs: []string{"/admin/*"}}, "")
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
	app, _ := newAppWithMiddleware(Config{SkipURIs: []string{"/*"}}, "")
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

func TestIgnoreStatusClasses(t *testing.T) {
	app, _ := newAppWithMiddleware(Config{IgnoreStatusClasses: []string{"4xx"}}, "")
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
	app, _ := newAppWithMiddleware(Config{
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
	app, _ := newAppWithMiddleware(Config{
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
			Service: "svc",
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

	app, _ := newAppWithMiddleware(cfg, "")
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
	app, _ := newAppWithMiddleware(Config{}, "")
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
	app, _ := newAppWithMiddleware(Config{SkipURIs: []string{"/admin"}}, "")
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
	app, _ := newAppWithMiddleware(Config{}, "")
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
	app, _ := newAppWithMiddleware(Config{}, "")
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

	app, _ := newAppWithMiddleware(Config{
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
	app, _ := newAppWithMiddleware(Config{
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
