package stats

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

var (
	benchmarkStatus   atomic.Int64
	benchmarkSnapshot snapshot
)

func BenchmarkFiberBaseline(b *testing.B) {
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	benchmarkFiberApp(b, app)
}

func BenchmarkFiberWithStats(b *testing.B) {
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	benchmarkFiberApp(b, app)
}

func BenchmarkFiberBaselineParallel(b *testing.B) {
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	benchmarkFiberAppParallel(b, app)
}

func BenchmarkFiberWithStatsParallel(b *testing.B) {
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	benchmarkFiberAppParallel(b, app)
}

func BenchmarkSnapshotCacheHit(b *testing.B) {
	m, err := newMiddleware()
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	m.cache.Store(&cacheEntry{
		snapshot: snapshot{CollectedAt: now},
		cachedAt: now,
	})

	b.ReportAllocs()
	b.ResetTimer()
	var current snapshot
	for range b.N {
		current = m.currentSnapshot(now)
	}
	benchmarkSnapshot = current
}

func BenchmarkRuntimeCollection(b *testing.B) {
	benchmarkRuntimeCollection(b, false)
}

func BenchmarkRuntimeCollectionWithGCPauseMetrics(b *testing.B) {
	benchmarkRuntimeCollection(b, true)
}

func BenchmarkSnapshotColdCollection(b *testing.B) {
	benchmarkSnapshotCollection(b, false)
}

func BenchmarkSnapshotColdCollectionWithGCPauseMetrics(b *testing.B) {
	benchmarkSnapshotCollection(b, true)
}

func benchmarkFiberApp(b *testing.B, app *fiber.App) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	status := 0
	for range b.N {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
		if err != nil {
			b.Fatal(err)
		}
		status = response.StatusCode
		if err := response.Body.Close(); err != nil {
			b.Fatal(err)
		}
	}
	benchmarkStatus.Store(int64(status))
}

func benchmarkFiberAppParallel(b *testing.B, app *fiber.App) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		status := 0
		for pb.Next() {
			response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
			if err != nil {
				b.Error(err)
				return
			}
			status = response.StatusCode
			if err := response.Body.Close(); err != nil {
				b.Error(err)
				return
			}
		}
		benchmarkStatus.Store(int64(status))
	})
}

func benchmarkRuntimeCollection(b *testing.B, enableGCPauseMetrics bool) {
	b.Helper()
	current := newCollector(time.Now(), enableGCPauseMetrics)
	b.ReportAllocs()
	b.ResetTimer()
	var stats runtimeStats
	for range b.N {
		stats = current.collectRuntime()
	}
	benchmarkSnapshot.Runtime = stats
}

func benchmarkSnapshotCollection(b *testing.B, enableGCPauseMetrics bool) {
	b.Helper()
	m, err := newMiddleware(Config{EnableGCPauseMetrics: enableGCPauseMetrics})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var current snapshot
	for range b.N {
		current = m.collectSnapshot(m.now())
	}
	benchmarkSnapshot = current
}
