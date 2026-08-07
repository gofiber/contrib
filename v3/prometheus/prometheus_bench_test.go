package prometheus

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
)

// newBenchmarkApp builds an app with the routes the benchmarks exercise. A nil
// cfg leaves the middleware out entirely, which is the baseline every other
// benchmark is measured against.
func newBenchmarkApp(b *testing.B, cfg *Config) *fiber.App {
	b.Helper()

	app := fiber.New()

	if cfg != nil {
		// A private registry per app keeps repeated benchmark runs independent.
		registry := prometheus.NewRegistry()
		local := *cfg
		local.Registerer = registry
		local.Gatherer = registry
		local.DisableGoCollector = true
		local.DisableProcessCollector = true
		app.Use(New(local))
	}

	app.Get("/user/:id", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})
	app.Get("/skip", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	return app
}

// benchmarkRequests drives the app and fails the benchmark if a response ever
// deviates from the expected status, so a benchmark cannot silently measure an
// error path.
func benchmarkRequests(b *testing.B, app *fiber.App, path string, wantStatus int) {
	b.Helper()
	b.ReportAllocs()

	// b.Loop manages the timer itself and keeps the compiler from optimising
	// the body away, which a plain counted loop does not guarantee.
	for b.Loop() {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), noTimeoutConfig)
		if err != nil {
			b.Fatalf("unexpected request error: %v", err)
		}
		if resp.StatusCode != wantStatus {
			b.Fatalf("expected status %d, got %d", wantStatus, resp.StatusCode)
		}
	}
}

// BenchmarkBaseline measures the app without the middleware.
func BenchmarkBaseline(b *testing.B) {
	benchmarkRequests(b, newBenchmarkApp(b, nil), "/user/42", http.StatusOK)
}

// BenchmarkInstrumented measures the default configuration: six metric
// families, no dynamic labels.
func BenchmarkInstrumented(b *testing.B) {
	benchmarkRequests(b, newBenchmarkApp(b, &Config{}), "/user/42", http.StatusOK)
}

// BenchmarkInstrumentedWithService adds a constant label, which widens every
// label set by one pair.
func BenchmarkInstrumentedWithService(b *testing.B) {
	benchmarkRequests(b, newBenchmarkApp(b, &Config{ServiceName: "bench"}), "/user/42", http.StatusOK)
}

// BenchmarkInstrumentedWithDynamicLabels measures the per-request cost of
// computing two extra label values.
func BenchmarkInstrumentedWithDynamicLabels(b *testing.B) {
	cfg := Config{
		DynamicLabels: map[string]func(fiber.Ctx) string{
			"tenant": func(c fiber.Ctx) string { return c.Get("X-Tenant", "none") },
			"zone":   func(fiber.Ctx) string { return "eu" },
		},
	}
	benchmarkRequests(b, newBenchmarkApp(b, &cfg), "/user/42", http.StatusOK)
}

// BenchmarkInstrumentedCountersOnly measures the configuration a
// cardinality-conscious deployment is likely to run: the two size histograms
// dropped.
func BenchmarkInstrumentedCountersOnly(b *testing.B) {
	cfg := Config{DisabledMetrics: []Metric{MetricRequestSize, MetricResponseSize}}
	benchmarkRequests(b, newBenchmarkApp(b, &cfg), "/user/42", http.StatusOK)
}

// BenchmarkSkippedURI measures a route excluded from instrumentation, which
// still pays for the in-flight gauge and the route lookup.
func BenchmarkSkippedURI(b *testing.B) {
	cfg := Config{SkipURIs: []string{"/skip"}}
	benchmarkRequests(b, newBenchmarkApp(b, &cfg), "/skip", http.StatusOK)
}

// BenchmarkSkippedURIPrefix measures the linear scan a "*" entry adds.
func BenchmarkSkippedURIPrefix(b *testing.B) {
	cfg := Config{SkipURIs: []string{"/a/*", "/b/*", "/skip/*"}}
	benchmarkRequests(b, newBenchmarkApp(b, &cfg), "/skip", http.StatusOK)
}

// BenchmarkUnmatchedRoute measures a request that resolves to no route, the
// path a 404 flood would take.
func BenchmarkUnmatchedRoute(b *testing.B) {
	benchmarkRequests(b, newBenchmarkApp(b, &Config{}), "/nothing/here", http.StatusNotFound)
}

// BenchmarkMetricsEndpoint measures serving a scrape.
func BenchmarkMetricsEndpoint(b *testing.B) {
	app := newBenchmarkApp(b, &Config{})

	// Populate a handful of series so the scrape has something to encode.
	for _, id := range []string{"1", "2", "3"} {
		if _, err := app.Test(httptest.NewRequest(http.MethodGet, "/user/"+id, nil), noTimeoutConfig); err != nil {
			b.Fatalf("unexpected request error: %v", err)
		}
	}

	benchmarkRequests(b, app, "/metrics", http.StatusOK)
}
