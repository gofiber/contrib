package circuitbreaker_test

import (
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"

	"github.com/gofiber/contrib/v3/circuitbreaker"
)

// The benchmarks below drive a one-route Fiber app through its fasthttp handler,
// so a result is the cost of a whole request: routing, the middleware, and a
// handler that only sets a status. They run in parallel against a single
// breaker, which is the shape that matters - every request shares that state, so
// the admission and outcome paths decide whether one breaker in front of a busy
// route scales or serializes. Benchmark_CircuitBreaker_Baseline is the same app
// without the middleware, and subtracting it leaves the breaker's own cost.
//
// go test -run=^$ -bench=Benchmark_CircuitBreaker -benchmem -count=6

// benchHandler returns the app's fasthttp handler, guarded by cb unless it is nil.
func benchHandler(b *testing.B, cb *circuitbreaker.CircuitBreaker) fasthttp.RequestHandler {
	b.Helper()

	app := fiber.New()
	if cb != nil {
		app.Use(circuitbreaker.Middleware(cb))
	}
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	return app.Handler()
}

// benchParallel replays one request per iteration from every goroutine, each with
// its own context since a fasthttp request cannot be shared.
func benchParallel(b *testing.B, handler fasthttp.RequestHandler) {
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		fctx := &fasthttp.RequestCtx{}
		fctx.Request.Header.SetMethod(fiber.MethodGet)
		fctx.Request.SetRequestURI("/")
		for pb.Next() {
			handler(fctx)
		}
	})
}

// Benchmark_CircuitBreaker_Baseline is the route with no breaker in front of it.
func Benchmark_CircuitBreaker_Baseline(b *testing.B) {
	benchParallel(b, benchHandler(b, nil))
}

// Benchmark_CircuitBreaker_Closed is the healthy path, where essentially all
// traffic goes: a closed circuit admits a request and takes its success without
// changing anything, so concurrent requests should not serialize on each other.
func Benchmark_CircuitBreaker_Closed(b *testing.B) {
	benchParallel(b, benchHandler(b, circuitbreaker.New(circuitbreaker.Config{})))
}

// Benchmark_CircuitBreaker_Open is the refusal path, which does serialize. That
// is the deliberate half of the trade: a tripped circuit is answering from the
// middleware instead of the service it is protecting.
func Benchmark_CircuitBreaker_Open(b *testing.B) {
	cb := circuitbreaker.New(circuitbreaker.Config{})
	cb.ForceOpen()

	benchParallel(b, benchHandler(b, cb))
}
