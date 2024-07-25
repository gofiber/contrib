package prometheus

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/basicauth"
	"github.com/gofiber/fiber/v2/middleware/cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp"
)

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
	want := `http_requests_total{method="GET",path="/",service="test-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/error/fiber",service="test-service",status_code="400"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/error/unknown",service="test-service",status_code="500"} 1`
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
	want := `http_requests_total{method="GET",path="/public",service="test-service",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/public/error/fiber",service="test-service",status_code="400"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/public/error/unknown",service="test-service",status_code="500"} 1`
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

	want = `http_requests_total{method="GET",path="/error/fiber",service="test-route",status_code="400"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `http_requests_total{method="GET",path="/error/unknown",service="test-route",status_code="500"} 1`
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
}

func TestWithCacheMiddleware(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	registry := prometheus.NewRegistry()
	registry.Register(collectors.NewGoCollector())
	registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	fpCustom := NewWithRegistry(registry, "custom-registry", "custom_name", "http", nil)
	fpCustom.RegisterAt(app, "/metrics")

	app.Use(fpCustom.Middleware)
	app.Use(cache.New())

	app.Get("/myPath", func(c *fiber.Ctx) error {
		return c.SendString("Hello, world!")
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/myPath", nil)
		res, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(fmt.Errorf("GET / failed: %w", err))
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(fmt.Errorf("GET /: Status=%d", res.StatusCode))
		}
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(fmt.Errorf("GET /metrics failed: %W", err))
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(fmt.Errorf("GET /metrics: Status=%d", res.StatusCode))
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(fmt.Errorf("GET /metrics: read body: %w", err))
	}
	got := string(body)
	want := `custom_name_http_requests_total{method="GET",path="/myPath",service="custom-registry",status_code="200"} 2`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `custom_name_http_cache_results{cache_result="hit",method="GET",path="/myPath",service="custom-registry",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `custom_name_http_cache_results{cache_result="miss",method="GET",path="/myPath",service="custom-registry",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}
}

func TestWithCacheMiddlewareWithCustomKey(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	registry := prometheus.NewRegistry()
	registry.Register(collectors.NewGoCollector())
	registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	fpCustom := NewWithRegistry(registry, "custom-registry", "custom_name", "http", nil)
	fpCustom.RegisterAt(app, "/metrics")
	fpCustom.CustomCacheKey("my-custom-cache-header")

	app.Use(fpCustom.Middleware)
	app.Use(cache.New(
		cache.Config{
			CacheHeader: "my-custom-cache-header",
		},
	))

	app.Get("/myPath", func(c *fiber.Ctx) error {
		return c.SendString("Hello, world!")
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/myPath", nil)
		res, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(fmt.Errorf("GET / failed: %w", err))
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(fmt.Errorf("GET /: Status=%d", res.StatusCode))
		}
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(fmt.Errorf("GET /metrics failed: %W", err))
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(fmt.Errorf("GET /metrics: Status=%d", res.StatusCode))
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(fmt.Errorf("GET /metrics: read body: %w", err))
	}
	got := string(body)
	want := `custom_name_http_requests_total{method="GET",path="/myPath",service="custom-registry",status_code="200"} 2`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `custom_name_http_cache_results{cache_result="hit",method="GET",path="/myPath",service="custom-registry",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
	}

	want = `custom_name_http_cache_results{cache_result="miss",method="GET",path="/myPath",service="custom-registry",status_code="200"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("got %s; want %s", got, want)
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
