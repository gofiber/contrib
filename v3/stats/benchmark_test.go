package stats

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

var benchmarkStatus int

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

func benchmarkFiberApp(b *testing.B, app *fiber.App) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
		if err != nil {
			b.Fatal(err)
		}
		benchmarkStatus = response.StatusCode
		if err := response.Body.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
