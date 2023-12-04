package fgprof

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"io"
	"net/http/httptest"
	"testing"
)

// go test -run Test_Non_Fgprof_Path
func Test_Non_Fgprof_Path(t *testing.T) {
	app := fiber.New()
	app.Use(New())

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("escaped")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, 200, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, "escaped", string(body))
}

// go test -run Test_Non_Fgprof_Path_WithPrefix
func Test_Non_Fgprof_Path_WithPrefix(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		Prefix: "/prefix",
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("escaped")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, 200, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, "escaped", string(body))
}

// go test -run Test_Fgprof_Path
func Test_Fgprof_Path(t *testing.T) {
	app := fiber.New()
	app.Use(New())

	// Default fgprof interval is 30 seconds
	resp, err := app.Test(httptest.NewRequest("GET", "/debug/fgprof?seconds=1", nil), 1500)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, 200, resp.StatusCode)
}

// go test -run Test_Fgprof_Path_WithPrefix
func Test_Fgprof_Path_WithPrefix(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		Prefix: "/test",
	}))
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("escaped")
	})

	// Non fgprof prefix path
	resp, err := app.Test(httptest.NewRequest("GET", "/prefix/debug/fgprof?seconds=1", nil), 1500)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, 404, resp.StatusCode)
	// Fgprof prefix path
	resp, err = app.Test(httptest.NewRequest("GET", "/test/debug/fgprof?seconds=1", nil), 1500)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, 200, resp.StatusCode)
}

// go test -run Test_Fgprof_Next
func Test_Fgprof_Next(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Next: func(_ *fiber.Ctx) bool {
			return true
		},
	}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/pprof/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, 404, resp.StatusCode)
}

// go test -run Test_Fgprof_Next_WithPrefix
func Test_Fgprof_Next_WithPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Next: func(_ *fiber.Ctx) bool {
			return true
		},
		Prefix: "/federated-fiber",
	}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/federated-fiber/debug/pprof/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, 404, resp.StatusCode)
}
