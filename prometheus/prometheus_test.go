package prometheus

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/basicauth"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
)

// Helper Functions for TestMiddlewareWithExamplar
func otelTracingInit(t *testing.T) {
	// Add trace resource attributes
	res, err := resource.New(
		context.Background(),
		resource.WithTelemetrySDK(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithAttributes(attribute.String("service.name", "fiber")),
	)
	if err != nil {
		t.Errorf("cant create otlp resource: %v", err)
		t.Fail()
	}

	// Create stdout exporter
	traceExporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		t.Errorf("cant create otlp exporter: %v", err)
		t.Fail()
	}

	// Create OTEL trace provider
	tp := trace.NewTracerProvider(
		trace.WithBatcher(traceExporter),
		trace.WithResource(res),
	)

	os.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	os.Setenv("OTEL_TRACES_SAMPLER", "always_on")

	// Set OTLP Provider
	otel.SetTracerProvider(tp)

	// SetTextMapPropagator configures the OpenTelemetry text map propagator
	// using a composite of TraceContext and Baggage propagators.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
}

// Helper Functions for TestMiddlewareWithExamplar
func tracingMiddleware(c *fiber.Ctx) error {
	// Create OTLP tracer
	tracer := otel.Tracer("FOCUZ")

	// Create a new context with cancellation capability from Fiber context
	ctx, cancel := context.WithCancel(c.UserContext())

	// Start a new span with attributes for tracing the current request
	ctx, span := tracer.Start(ctx, c.Route().Name)

	// Ensure the span is ended and context is cancelled when the request completes
	defer span.End()
	defer cancel()

	// Set OTLP context
	c.SetUserContext(ctx)

	// Continue with the next middleware/handler
	return c.Next()
}

func TestMiddleware(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	prometheus := New("test-service")
	prometheus.RegisterAt(app, "/metrics")
	app.Use(prometheus.Middleware)
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})
	app.Get("/error/:type", func(ctx *fiber.Ctx) error {
		switch ctx.Params("type") {
		case "fiber":
			return fiber.ErrBadRequest
		default:
			return fiber.ErrInternalServerError
		}
	})
	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/error/fiber", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/error/unknown", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Check Metrics Response
	want := `http_requests_total{method="GET",path="/",service="test-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/error/:type",service="test-service",status_code="400"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/error/:type",service="test-service",status_code="500"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_request_duration_seconds_count{method="GET",path="/",service="test-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_in_progress_total{method="GET",service="test-service"} 0`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}
}

func TestMiddlewareWithExamplar(t *testing.T) {
	t.Parallel()
	otelTracingInit(t)

	app := fiber.New()
	prometheus := New("test-service")
	prometheus.RegisterAt(app, "/metrics")
	app.Use(tracingMiddleware)
	app.Use(prometheus.Middleware)
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text")
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Check Metrics Response
	want := `http_request_duration_seconds_bucket{method="GET",path="/",service="test-service",status_code="200",le=".*"} 1 # {traceID=".*"} .*`
	re := regexp.MustCompile(want)
	if !re.MatchString(got) {
		t.Errorf("got %s; want pattern %s", got, want)
	}
}

func TestMiddlewareWithSkipPath(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	prometheus := New("test-service")
	prometheus.RegisterAt(app, "/metrics")
	prometheus.SetSkipPaths([]string{"/healthz", "/livez"})
	app.Use(prometheus.Middleware)
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})
	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})
	app.Get("/livez", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	// Make requests
	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/healthz", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/livez", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	// Check Metrics Response
	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	want := `http_requests_total{method="GET",path="/",service="test-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/healthz",service="test-service",status_code="200"} 1`
	if strings.Contains(got, want) {
		t.Errorf("got %s; not want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/metrics",service="test-service",status_code="200"}`
	if strings.Contains(got, want) {
		t.Errorf("got %s; not want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/livez",service="test-service",status_code="200"} 1`
	if strings.Contains(got, want) {
		t.Errorf("got %s; not want %s", got, want)
	}

	want = `http_request_duration_seconds_count{method="GET",path="/",service="test-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; not want %s", got, want)
	}

	want = `http_requests_in_progress_total{method="GET",service="test-service"} 0`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; not want %s", got, want)
	}
}

func TestMiddlewareWithGroup(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	prometheus := New("test-service")
	prometheus.RegisterAt(app, "/metrics")
	app.Use(prometheus.Middleware)

	// Define Group
	public := app.Group("/public")

	// Define Group Routes
	public.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})
	public.Get("/error/:type", func(ctx *fiber.Ctx) error {
		switch ctx.Params("type") {
		case "fiber":
			return fiber.ErrBadRequest
		default:
			return fiber.ErrInternalServerError
		}
	})
	req := httptest.NewRequest("GET", "/public", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/public/error/fiber", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/public/error/unknown", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Check Metrics Response
	want := `http_requests_total{method="GET",path="/public",service="test-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/public/error/:type",service="test-service",status_code="400"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/public/error/:type",service="test-service",status_code="500"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_request_duration_seconds_count{method="GET",path="/public",service="test-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_in_progress_total{method="GET",service="test-service"} 0`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}
}

func TestMiddlewareOnRoute(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	prometheus := New("test-route")
	prefix := "/prefix/path"

	app.Route(prefix, func(route fiber.Router) {
		prometheus.RegisterAt(route, "/metrics")
	}, "Prefixed Route")
	app.Use(prometheus.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	app.Get("/error/:type", func(ctx *fiber.Ctx) error {
		switch ctx.Params("type") {
		case "fiber":
			return fiber.ErrBadRequest
		default:
			return fiber.ErrInternalServerError
		}
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/error/fiber", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/error/unknown", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fail()
	}

	req = httptest.NewRequest("GET", prefix+"/metrics", nil)
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	want := `http_requests_total{method="GET",path="/",service="test-route",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/error/:type",service="test-route",status_code="400"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/error/:type",service="test-route",status_code="500"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_request_duration_seconds_count{method="GET",path="/",service="test-route",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_in_progress_total{method="GET",service="test-route"} 0`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}
}

func TestMiddlewareWithServiceName(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	prometheus := NewWith("unique-service", "my_service_with_name", "http")
	prometheus.RegisterAt(app, "/metrics")
	app.Use(prometheus.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})
	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	want := `my_service_with_name_http_requests_total{method="GET",path="/",service="unique-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `my_service_with_name_http_request_duration_seconds_count{method="GET",path="/",service="unique-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `my_service_with_name_http_requests_in_progress_total{method="GET",service="unique-service"} 0`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}
}

func TestMiddlewareWithLabels(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	constLabels := map[string]string{
		"customkey1": "customvalue1",
		"customkey2": "customvalue2",
	}
	prometheus := NewWithLabels(constLabels, "my_service", "http")
	prometheus.RegisterAt(app, "/metrics")
	app.Use(prometheus.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})
	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	want := `my_service_http_requests_total{customkey1="customvalue1",customkey2="customvalue2",method="GET",path="/",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `my_service_http_request_duration_seconds_count{customkey1="customvalue1",customkey2="customvalue2",method="GET",path="/",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `my_service_http_requests_in_progress_total{customkey1="customvalue1",customkey2="customvalue2",method="GET"} 0`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}
}

func TestMiddlewareWithBasicAuth(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	prometheus := New("basic-auth")
	prometheus.RegisterAt(app, "/metrics", basicauth.New(basicauth.Config{
		Users: map[string]string{
			"prometheus": "password",
		},
	}))

	app.Use(prometheus.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != 401 {
		t.Fail()
	}

	req.SetBasicAuth("prometheus", "password")
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fail()
	}
}

func TestMiddlewareWithCustomRegistry(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	registry := prometheus.NewRegistry()
	srv := httptest.NewServer(promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	t.Cleanup(srv.Close)
	promfiber := NewWithRegistry(registry, "unique-service", "my_service_with_name", "http", nil)
	app.Use(promfiber.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fail()
	}
	if resp.StatusCode != 200 {
		t.Fail()
	}

	resp, err = srv.Client().Get(srv.URL)
	if err != nil {
		t.Fail()
	}
	if resp == nil {
		t.Fatal("response is nil")
	}
	if resp.StatusCode != 200 {
		t.Fail()
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	want := `my_service_with_name_http_requests_total{method="GET",path="/",service="unique-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `my_service_with_name_http_request_duration_seconds_count{method="GET",path="/",service="unique-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `my_service_with_name_http_requests_in_progress_total{method="GET",service="unique-service"} 0`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}
}

func TestCustomRegistryRegisterAt(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	registry := prometheus.NewRegistry()
	registry.Register(collectors.NewGoCollector())
	registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	fpCustom := NewWithRegistry(registry, "custom-registry", "custom_name", "http", nil)
	fpCustom.RegisterAt(app, "/metrics")
	app.Use(fpCustom.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello, world!")
	})
	req := httptest.NewRequest("GET", "/", nil)
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(fmt.Errorf("GET / failed: %w", err))
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(fmt.Errorf("GET /: Status=%d", res.StatusCode))
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	resMetr, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(fmt.Errorf("GET /metrics failed: %W", err))
	}
	defer resMetr.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(fmt.Errorf("GET /metrics: Status=%d", resMetr.StatusCode))
	}
	body, err := io.ReadAll(resMetr.Body)
	if err != nil {
		t.Fatal(fmt.Errorf("GET /metrics: read body: %w", err))
	}
	got := string(body)

	want := `custom_name_http_requests_total{method="GET",path="/",service="custom-registry",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	// Make sure that /metrics was skipped
	want = `custom_name_http_requests_total{method="GET",path="/metrics",service="custom-registry",status_code="200"} 1`
	if strings.Contains(got, want) {
		t.Errorf("got %s; not want %s", got, want)
	}
}

// TestInFlightGauge verifies that the in-flight requests gauge is updated correctly.
func TestInFlightGauge(t *testing.T) {
	app := fiber.New()
	prometheus := New("inflight-service")
	app.Use(prometheus.Middleware)

	// Long-running handler to simulate in-flight requests
	app.Get("/long", func(c *fiber.Ctx) error {
		// Sleep for a short duration
		time.Sleep(100 * time.Millisecond)
		return c.SendString("Long Request")
	})

	// Register metrics endpoint
	prometheus.RegisterAt(app, "/metrics")

	var wg sync.WaitGroup
	requests := 10
	wg.Add(requests)

	// Start multiple concurrent requests
	for i := 0; i < requests; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/long", nil)
			app.Test(req, -1)
		}()
	}

	// Allow some time for requests to start
	time.Sleep(10 * time.Millisecond)
	wg.Wait()

	// After all requests complete, in-flight gauge should be zero
	req := httptest.NewRequest("GET", "/metrics", nil)
	resp, _ := app.Test(req, -1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// The in-flight gauge should be equal to the number of concurrent requests
	// Since requests are sleeping, some may have completed, so we check for at least one
	if !strings.Contains(got, `http_requests_in_progress_total{method="GET",service="inflight-service"} 1`) {
		t.Errorf("Expected in-flight gauge to be at least 1, got %s", got)
	}

	want := `http_requests_total{method="GET",path="/long",service="inflight-service",status_code="200"} 10`
	if !strings.Contains(got, want) {
		t.Errorf("Expected in-flight gauge to be 0, got %s", got)
	}
}

// TestDifferentHTTPMethods verifies that metrics are correctly recorded for various HTTP methods.
func TestDifferentHTTPMethods(t *testing.T) {
	app := fiber.New()
	prometheus := New("methods-service")
	app.Use(prometheus.Middleware)

	// Define handlers for different methods
	app.Get("/resource", func(c *fiber.Ctx) error {
		return c.SendString("GET")
	})
	app.Post("/resource", func(c *fiber.Ctx) error {
		return c.SendString("POST")
	})
	app.Put("/resource", func(c *fiber.Ctx) error {
		return c.SendString("PUT")
	})
	app.Delete("/resource", func(c *fiber.Ctx) error {
		return c.SendString("DELETE")
	})

	// Register metrics endpoint
	prometheus.RegisterAt(app, "/metrics")

	// Make requests with different methods
	methods := []string{"GET", "POST", "PUT", "DELETE"}
	for _, method := range methods {
		req := httptest.NewRequest(method, "/resource", nil)
		resp, _ := app.Test(req, -1)
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200 for %s, got %d", method, resp.StatusCode)
		}
	}

	// Check Metrics
	req := httptest.NewRequest("GET", "/metrics", nil)
	resp, _ := app.Test(req, -1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, method := range methods {
		want := `http_requests_total{method="` + method + `",path="/resource",service="methods-service",status_code="200"} 1`
		if !strings.Contains(got, want) {
			t.Errorf("Expected metric %s, got %s", want, got)
		}
	}
}

// TestSkipPathsWithTrailingSlash verifies that skip paths are correctly normalized and skipped even with trailing slashes.
func TestSkipPathsWithTrailingSlash(t *testing.T) {
	app := fiber.New()
	prometheus := New("skip-service")
	prometheus.RegisterAt(app, "/metrics")
	prometheus.SetSkipPaths([]string{"/healthz", "/livez"})
	app.Use(prometheus.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})
	app.Get("/healthz/", func(c *fiber.Ctx) error { // Trailing slash
		return c.SendString("Healthz")
	})
	app.Get("/livez/", func(c *fiber.Ctx) error { // Trailing slash
		return c.SendString("Livez")
	})

	// Make requests
	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	req = httptest.NewRequest("GET", "/healthz/", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	req = httptest.NewRequest("GET", "/livez/", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	// Check Metrics
	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Only the root path should be recorded
	want := `http_requests_total{method="GET",path="/",service="skip-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("Expected metric %s, got %s", want, got)
	}

	// Ensure skipped paths are not recorded
	skippedPaths := []string{"/healthz", "/livez"}
	for _, path := range skippedPaths {
		want := `http_requests_total{method="GET",path="` + path + `",service="skip-service",status_code="200"} 1`
		if strings.Contains(got, want) {
			t.Errorf("Did not expect metric %s, but found in %s", want, got)
		}
	}
}

// TestMetricsAfterError verifies that metrics are recorded correctly even when handlers return errors.
func TestMetricsAfterError(t *testing.T) {
	app := fiber.New()
	prometheus := New("error-service")
	app.Use(prometheus.Middleware)

	app.Get("/badrequest", func(c *fiber.Ctx) error {
		return fiber.ErrBadRequest
	})
	app.Get("/internalerror", func(c *fiber.Ctx) error {
		return fiber.ErrInternalServerError
	})

	// Register metrics endpoint
	prometheus.RegisterAt(app, "/metrics")

	// Make error requests
	req := httptest.NewRequest("GET", "/badrequest", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("Expected status 400, got %d", resp.StatusCode)
	}

	req = httptest.NewRequest("GET", "/internalerror", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("Expected status 500, got %d", resp.StatusCode)
	}

	// Check Metrics
	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	want400 := `http_requests_total{method="GET",path="/badrequest",service="error-service",status_code="400"} 1`
	if !strings.Contains(got, want400) {
		t.Errorf("Expected metric %s, got %s", want400, got)
	}

	want500 := `http_requests_total{method="GET",path="/internalerror",service="error-service",status_code="500"} 1`
	if !strings.Contains(got, want500) {
		t.Errorf("Expected metric %s, got %s", want500, got)
	}
}

// TestMultipleRegistrations ensures that calling RegisterAt multiple times does not duplicate handlers.
func TestMultipleRegistrations(t *testing.T) {
	app := fiber.New()
	prometheus := New("multi-register-service")
	app.Use(prometheus.Middleware)
	prometheus.RegisterAt(app, "/metrics")
	prometheus.RegisterAt(app, "/metrics") // Register again

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	// Make requests
	req := httptest.NewRequest("GET", "/", nil)
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	// Make a request to /metrics
	req = httptest.NewRequest("GET", "/metrics", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Expect metrics to be registered only once
	want := `http_requests_total{method="GET",path="/",service="multi-register-service",status_code="200"} 1`
	if strings.Count(got, want) != 1 {
		t.Errorf("Expected metric %s to appear once, got %s occurrences", want, got)
	}
}

// TestMetricsHandlerConcurrentAccess verifies that the metrics handler can handle concurrent access without issues.
func TestMetricsHandlerConcurrentAccess(t *testing.T) {
	app := fiber.New()
	prometheus := New("concurrent-service")
	app.Use(prometheus.Middleware)

	app.Get("/resource", func(c *fiber.Ctx) error {
		return c.SendString("Resource")
	})

	// Register metrics endpoint
	prometheus.RegisterAt(app, "/metrics")

	// Make multiple concurrent requests
	var wg sync.WaitGroup
	requests := 100
	wg.Add(requests)

	for i := 0; i < requests; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/resource", nil)
			app.Test(req, -1)
		}()
	}

	wg.Wait()

	// Check Metrics
	req := httptest.NewRequest("GET", "/metrics", nil)
	resp, _ := app.Test(req, -1)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Verify that requests_total is incremented correctly
	wantTotal := `http_requests_total{method="GET",path="/resource",service="concurrent-service",status_code="200"} ` + strconv.Itoa(requests)
	if !strings.Contains(got, wantTotal) {
		t.Errorf("Expected metric %s, got %s", wantTotal, got)
	}
}

func Benchmark_Middleware(b *testing.B) {
	app := fiber.New()

	prometheus := New("test-benchmark")
	prometheus.RegisterAt(app, "/metrics")
	app.Use(prometheus.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	h := app.Handler()
	ctx := &fasthttp.RequestCtx{}

	req := &fasthttp.Request{}
	req.Header.SetMethod(fiber.MethodOptions)
	req.SetRequestURI("/")
	ctx.Init(req, nil, nil)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		h(ctx)
	}
}

func Benchmark_Middleware_Parallel(b *testing.B) {
	app := fiber.New()

	prometheus := New("test-benchmark")
	prometheus.RegisterAt(app, "/metrics")
	app.Use(prometheus.Middleware)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello World")
	})

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		ctx := &fasthttp.RequestCtx{}
		req := &fasthttp.Request{}
		req.Header.SetMethod(fiber.MethodOptions)
		req.SetRequestURI("/metrics")
		ctx.Init(req, nil, nil)

		for pb.Next() {
			h(ctx)
		}
	})
}
