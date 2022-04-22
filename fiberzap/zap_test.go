package fiberzap

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func setupLogsCapture() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.InfoLevel)
	return zap.New(core), logs
}

// go test -run Test_SkipBody
func Test_SkipBody(t *testing.T) {
	logger, logs := setupLogsCapture()

	app := fiber.New()
	app.Use(New(Config{
		SkipBody: func(_ *fiber.Ctx) bool {
			return true
		},
		Logger: logger,
		Fields: []string{"pid", "body"},
	}))

	body := bytes.NewReader([]byte("this is test"))
	resp, err := app.Test(httptest.NewRequest("GET", "/", body))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)

	_, ok := logs.All()[0].ContextMap()["body"]
	utils.AssertEqual(t, false, ok)
}

// go test -run Test_SkipResBody
func Test_SkipResBody(t *testing.T) {
	logger, logs := setupLogsCapture()

	app := fiber.New()
	app.Use(New(Config{
		SkipResBody: func(_ *fiber.Ctx) bool {
			return true
		},
		Logger: logger,
		Fields: []string{"pid", "resBody"},
	}))

	body := bytes.NewReader([]byte("this is test"))
	resp, err := app.Test(httptest.NewRequest("GET", "/", body))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)

	_, ok := logs.All()[0].ContextMap()["resBody"]
	utils.AssertEqual(t, false, ok)
}

// go test -run Test_Logger
func Test_Logger(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"pid", "latency", "error"},
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		return errors.New("some random error")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusInternalServerError, resp.StatusCode)
	utils.AssertEqual(t, "some random error", logs.All()[0].Context[3].String)
}

// go test -run Test_Logger_Next
func Test_Logger_Next(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		Next: func(_ *fiber.Ctx) bool {
			return true
		},
	}))

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)
}

// go test -run Test_Logger_All
func Test_Logger_All(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"protocol", "pid", "body", "ip", "host", "url", "route", "method", "resBody", "queryParams", "bytesReceived", "bytesSent"},
	}))

	resp, err := app.Test(httptest.NewRequest("GET", "/?foo=bar", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)

	expected := map[string]interface{}{
		"body":          "",
		"ip":            "0.0.0.0",
		"host":          "example.com",
		"url":           "/?foo=bar",
		"method":        "GET",
		"route":         "/",
		"protocol":      "http",
		"pid":           strconv.Itoa(os.Getpid()),
		"queryParams":   "foo=bar",
		"resBody":       "Cannot GET /",
		"bytesReceived": int64(0),
		"bytesSent":     int64(12),
	}

	utils.AssertEqual(t, expected, logs.All()[0].ContextMap())
}

// go test -run Test_Query_Params
func Test_Query_Params(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"queryParams"},
	}))

	resp, err := app.Test(httptest.NewRequest("GET", "/?foo=bar&baz=moz", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)

	expected := "foo=bar&baz=moz"
	utils.AssertEqual(t, expected, logs.All()[0].Context[1].String)
}

// go test -run Test_Response_Body
func Test_Response_Body(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"resBody"},
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Sample response body")
	})

	app.Post("/test", func(c *fiber.Ctx) error {
		return c.Send([]byte("Post in test"))
	})

	_, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)

	expectedGetResponse := "Sample response body"
	utils.AssertEqual(t, expectedGetResponse, logs.All()[0].ContextMap()["resBody"])

	_, err = app.Test(httptest.NewRequest("POST", "/test", nil))
	utils.AssertEqual(t, nil, err)

	expectedPostResponse := "Post in test"
	t.Log(logs.All())
	utils.AssertEqual(t, expectedPostResponse, logs.All()[1].ContextMap()["resBody"])
}

// go test -run Test_Logger_AppendUint
func Test_Logger_AppendUint(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"bytesReceived", "bytesSent", "status"},
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("hello")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	output := logs.All()[0].ContextMap()
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)
	utils.AssertEqual(t, "0 5 200", fmt.Sprintf("%d %d %d", output["bytesReceived"], output["bytesSent"], output["status"]))
}

// go test -run Test_Logger_Data_Race -race
func Test_Logger_Data_Race(t *testing.T) {
	app := fiber.New()
	logger := zap.NewExample()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"bytesReceived", "bytesSent", "status"},
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("hello")
	})

	var (
		resp1, resp2 *http.Response
		err1, err2   error
	)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		resp1, err1 = app.Test(httptest.NewRequest("GET", "/", nil))
		wg.Done()
	}()
	resp2, err2 = app.Test(httptest.NewRequest("GET", "/", nil))
	wg.Wait()

	utils.AssertEqual(t, nil, err1)
	utils.AssertEqual(t, fiber.StatusOK, resp1.StatusCode)
	utils.AssertEqual(t, nil, err2)
	utils.AssertEqual(t, fiber.StatusOK, resp2.StatusCode)
}

// go test -v -run=^$ -bench=Benchmark_Logger -benchmem -count=4
func Benchmark_Logger(b *testing.B) {
	app := fiber.New()

	app.Use(New())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod("GET")
	fctx.Request.SetRequestURI("/")

	b.ReportAllocs()
	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		h(fctx)
	}

	utils.AssertEqual(b, 200, fctx.Response.Header.StatusCode())
}

// go test -run Test_Request_Id
func Test_Request_Id(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"requestId"},
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		c.Response().Header.Add(fiber.HeaderXRequestID, "bf985e8e-6a32-42ec-8e50-05a21db8f0e4")
		return c.SendString("hello")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)
	utils.AssertEqual(t, "bf985e8e-6a32-42ec-8e50-05a21db8f0e4", logs.All()[0].Context[0].String)
}
