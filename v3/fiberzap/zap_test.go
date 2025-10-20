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

	"github.com/stretchr/testify/assert"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func setupLogsCapture() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.InfoLevel)
	return zap.New(core), logs
}

func Test_GetResBody(t *testing.T) {
	var readableResBody = "this is readable response body"

	var app = fiber.New()
	var logger, logs = setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		GetResBody: func(c fiber.Ctx) []byte {
			return []byte(readableResBody)
		},
		Fields: []string{"resBody"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("------this is unreadable resp------")
	})

	_, err := app.Test(httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, readableResBody, logs.All()[0].ContextMap()["resBody"])
}

// go test -run Test_SkipBody
func Test_SkipBody(t *testing.T) {
	logger, logs := setupLogsCapture()

	app := fiber.New()
	app.Use(New(Config{
		SkipBody: func(_ fiber.Ctx) bool {
			return true
		},
		Logger: logger,
		Fields: []string{"pid", "body"},
	}))

	body := bytes.NewReader([]byte("this is test"))
	resp, err := app.Test(httptest.NewRequest("GET", "/", body))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	_, ok := logs.All()[0].ContextMap()["body"]
	assert.Equal(t, false, ok)
}

// go test -run Test_SkipResBody
func Test_SkipResBody(t *testing.T) {
	logger, logs := setupLogsCapture()

	app := fiber.New()
	app.Use(New(Config{
		SkipResBody: func(_ fiber.Ctx) bool {
			return true
		},
		Logger: logger,
		Fields: []string{"pid", "resBody"},
	}))

	body := bytes.NewReader([]byte("this is test"))
	resp, err := app.Test(httptest.NewRequest("GET", "/", body))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	_, ok := logs.All()[0].ContextMap()["resBody"]
	assert.Equal(t, false, ok)
}

// go test -run Test_Logger
func Test_Logger(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"pid", "latency", "error"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return errors.New("some random error")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "some random error", logs.All()[0].Context[3].String)
}

// go test -run Test_Logger_Next
func Test_Logger_Next(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	expected := map[string]interface{}{
		"body":          "",
		"ip":            "0.0.0.0",
		"host":          "example.com",
		"url":           "/?foo=bar",
		"method":        "GET",
		"route":         "/",
		"protocol":      "HTTP/1.1",
		"pid":           strconv.Itoa(os.Getpid()),
		"queryParams":   "foo=bar",
		"resBody":       "Not Found",
		"bytesReceived": int64(0),
		"bytesSent":     int64(9),
	}

	assert.Equal(t, expected, logs.All()[0].ContextMap())
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	expected := "foo=bar&baz=moz"
	assert.Equal(t, expected, logs.All()[0].Context[1].String)
}

// go test -run Test_Response_Body
func Test_Response_Body(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"resBody"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Sample response body")
	})

	app.Post("/test", func(c fiber.Ctx) error {
		return c.Send([]byte("Post in test"))
	})

	_, err := app.Test(httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, nil, err)

	expectedGetResponse := "Sample response body"
	assert.Equal(t, expectedGetResponse, logs.All()[0].ContextMap()["resBody"])

	_, err = app.Test(httptest.NewRequest("POST", "/test", nil))
	assert.Equal(t, nil, err)

	expectedPostResponse := "Post in test"
	t.Log(logs.All())
	assert.Equal(t, expectedPostResponse, logs.All()[1].ContextMap()["resBody"])
}

// go test -run Test_Logger_AppendUint
func Test_Logger_AppendUint(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"bytesReceived", "bytesSent", "status"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	output := logs.All()[0].ContextMap()
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, "0 5 200", fmt.Sprintf("%d %d %d", output["bytesReceived"], output["bytesSent"], output["status"]))
}

// go test -run Test_Logger_Data_Race -race
func Test_Logger_Data_Race(t *testing.T) {
	app := fiber.New()
	logger := zap.NewExample()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"bytesReceived", "bytesSent", "status"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
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

	assert.Equal(t, nil, err1)
	assert.Equal(t, fiber.StatusOK, resp1.StatusCode)
	assert.Equal(t, nil, err2)
	assert.Equal(t, fiber.StatusOK, resp2.StatusCode)
}

// go test -v -run=^$ -bench=Benchmark_Logger -benchmem -count=4
func Benchmark_Logger(b *testing.B) {
	app := fiber.New()

	app.Use(New())
	app.Get("/", func(c fiber.Ctx) error {
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

	assert.Equal(b, 200, fctx.Response.Header.StatusCode())
}

// go test -run Test_Request_Id
func Test_Request_Id(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"requestId"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Response().Header.Add(fiber.HeaderXRequestID, "bf985e8e-6a32-42ec-8e50-05a21db8f0e4")
		return c.SendString("hello")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, "bf985e8e-6a32-42ec-8e50-05a21db8f0e4", logs.All()[0].Context[1].String)
}

// go test -run Test_Skip_URIs
func Test_Skip_URIs(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger:   logger,
		SkipURIs: []string{"/ignore_logging"},
	}))

	app.Get("/ignore_logging", func(c fiber.Ctx) error {
		return errors.New("no log")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/ignore_logging", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, 0, len(logs.All()))
}

// go test -run Test_Req_Headers
func Test_Req_Headers(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"reqHeaders"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	expected := map[string]interface{}{
		"Host": "example.com",
		"Baz":  "foo",
		"Foo":  "bar",
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("foo", "bar")
	req.Header.Add("baz", "foo")
	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, expected, logs.All()[0].ContextMap())
}

// go test -run Test_LoggerLevelsAndMessages
func Test_LoggerLevelsAndMessages(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	levels := []zapcore.Level{zapcore.ErrorLevel, zapcore.WarnLevel, zapcore.InfoLevel}
	messages := []string{"server error", "client error", "success"}
	app.Use(New(Config{
		Logger:   logger,
		Messages: messages,
		Levels:   levels,
	}))

	app.Get("/200", func(c fiber.Ctx) error {
		c.Status(fiber.StatusOK)
		return nil
	})
	app.Get("/400", func(c fiber.Ctx) error {
		c.Status(fiber.StatusBadRequest)
		return nil
	})
	app.Get("/500", func(c fiber.Ctx) error {
		c.Status(fiber.StatusInternalServerError)
		return nil
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/500", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, levels[0], logs.All()[0].Level)
	assert.Equal(t, messages[0], logs.All()[0].Message)
	resp, err = app.Test(httptest.NewRequest("GET", "/400", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, levels[1], logs.All()[1].Level)
	assert.Equal(t, messages[1], logs.All()[1].Message)
	resp, err = app.Test(httptest.NewRequest("GET", "/200", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, levels[2], logs.All()[2].Level)
	assert.Equal(t, messages[2], logs.All()[2].Message)
}

// go test -run Test_LoggerLevelsAndMessagesSingle
func Test_LoggerLevelsAndMessagesSingle(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	levels := []zapcore.Level{zapcore.ErrorLevel}
	messages := []string{"server error"}
	app.Use(New(Config{
		Logger:   logger,
		Messages: messages,
		Levels:   levels,
	}))

	app.Get("/200", func(c fiber.Ctx) error {
		c.Status(fiber.StatusOK)
		return nil
	})
	app.Get("/400", func(c fiber.Ctx) error {
		c.Status(fiber.StatusBadRequest)
		return nil
	})
	app.Get("/500", func(c fiber.Ctx) error {
		c.Status(fiber.StatusInternalServerError)
		return nil
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/500", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, levels[0], logs.All()[0].Level)
	assert.Equal(t, messages[0], logs.All()[0].Message)
	resp, err = app.Test(httptest.NewRequest("GET", "/400", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, levels[0], logs.All()[1].Level)
	assert.Equal(t, messages[0], logs.All()[1].Message)
	resp, err = app.Test(httptest.NewRequest("GET", "/200", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, levels[0], logs.All()[2].Level)
	assert.Equal(t, messages[0], logs.All()[2].Message)
}

// go test -run Test_Fields_Func
func Test_Fields_Func(t *testing.T) {
	app := fiber.New()
	logger, logs := setupLogsCapture()

	app.Use(New(Config{
		Logger: logger,
		Fields: []string{"protocol", "pid", "body", "ip", "host", "url", "route", "method", "resBody", "queryParams", "bytesReceived", "bytesSent"},
		FieldsFunc: func(c fiber.Ctx) []zap.Field {
			return []zap.Field{zap.String("test.custom.field", "test")}
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))

	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"body":              "",
		"ip":                "0.0.0.0",
		"host":              "example.com",
		"url":               "/",
		"method":            "GET",
		"route":             "/",
		"protocol":          "HTTP/1.1",
		"pid":               strconv.Itoa(os.Getpid()),
		"queryParams":       "",
		"resBody":           "hello",
		"bytesReceived":     int64(0),
		"bytesSent":         int64(5),
		"test.custom.field": "test",
	}

	assert.Equal(t, expected, logs.All()[0].ContextMap())
}
