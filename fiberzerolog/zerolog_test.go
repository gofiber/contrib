package fiberzerolog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
)

func Test_GetResBody(t *testing.T) {
	t.Parallel()

	readableResBody := "this is readable response body"

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		GetResBody: func(c fiber.Ctx) []byte {
			return []byte(readableResBody)
		},
		Fields: []string{FieldResBody},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("------this is unreadable resp------")
	})

	_, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, readableResBody, logs[FieldResBody])
}

func Test_SkipBody(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		SkipBody: func(_ fiber.Ctx) bool {
			return true
		},
		Logger: &logger,
		Fields: []string{FieldPID, FieldBody},
	}))

	body := bytes.NewReader([]byte("this is test"))
	resp, err := app.Test(httptest.NewRequest("GET", "/", body))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)
	_, ok := logs[FieldBody]

	utils.AssertEqual(t, false, ok)
}

func Test_SkipResBody(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		SkipResBody: func(_ fiber.Ctx) bool {
			return true
		},
		Logger: &logger,
		Fields: []string{FieldResBody},
	}))

	body := bytes.NewReader([]byte("this is test"))
	resp, err := app.Test(httptest.NewRequest("GET", "/", body))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)
	_, ok := logs[FieldResBody]

	utils.AssertEqual(t, false, ok)
}

func Test_Logger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return errors.New("some random error")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusInternalServerError, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, "some random error", logs[FieldError])
	utils.AssertEqual(t, float64(500), logs[FieldStatus])
}

func Test_Latency(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		time.Sleep(100 * time.Millisecond)
		return c.SendStatus(fiber.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	latencyStr, ok := logs[FieldLatency].(string)
	utils.AssertEqual(t, true, ok)
	utils.AssertEqual(t, true, strings.Contains(latencyStr, "ms"))
	utils.AssertEqual(t, float64(200), logs[FieldStatus])
}

func Test_Logger_Next(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)
}

func Test_Logger_All(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{
			FieldProtocol,
			FieldPID,
			FieldBody,
			FieldIP,
			FieldHost,
			FieldURL,
			FieldLatency,
			FieldRoute,
			FieldMethod,
			FieldResBody,
			FieldQueryParams,
			FieldBytesReceived,
			FieldBytesSent,
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		time.Sleep(100 * time.Millisecond)
		return c.SendStatus(fiber.StatusNotFound)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/?foo=bar", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)

	expected := map[string]interface{}{
		"body":          "",
		"ip":            "0.0.0.0",
		"host":          "example.com",
		"url":           "/?foo=bar",
		"level":         "warn",
		"message":       "Client error",
		"method":        "GET",
		"route":         "/",
		"protocol":      "http",
		"pid":           float64(os.Getpid()),
		"queryParams":   "foo=bar",
		"resBody":       "Not Found",
		"bytesReceived": float64(0),
		"bytesSent":     float64(9),
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	for key, value := range expected {
		utils.AssertEqual(t, value, logs[key])
	}

	latencyStr, ok := logs[FieldLatency].(string)
	utils.AssertEqual(t, true, ok)
	utils.AssertEqual(t, true, strings.Contains(latencyStr, "ms"))
}

func Test_Response_Body(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldResBody},
	}))

	expectedGetResponse := "Sample response body"

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString(expectedGetResponse)
	})

	_, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, expectedGetResponse, logs[FieldResBody])
}

func Test_Request_Id(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldRequestID},
	}))

	requestID := "bf985e8e-6a32-42ec-8e50-05a21db8f0e4"

	app.Get("/", func(c fiber.Ctx) error {
		c.Response().Header.Set(fiber.HeaderXRequestID, requestID)
		return c.SendString("hello")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, requestID, logs[FieldRequestID])
}

func Test_Skip_URIs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger:   &logger,
		SkipURIs: []string{"/ignore_logging"},
	}))

	app.Get("/ignore_logging", func(c fiber.Ctx) error {
		return errors.New("no log")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/ignore_logging", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusInternalServerError, resp.StatusCode)
	utils.AssertEqual(t, 0, buf.Len())
}

func Test_Req_Headers(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldReqHeaders},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add([]string{"foo"}, "bar")
	req.Header.Add([]string{"baz"}, "foo")

	resp, err := app.Test(req)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"Host":    "example.com",
		"Baz":     "foo",
		"Foo":     "bar",
		"level":   "info",
		"message": "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, expected, logs)
}

func Test_Req_Headers_WrapHeaders(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger:      &logger,
		Fields:      []string{FieldReqHeaders},
		WrapHeaders: true,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add([]string{"foo"}, "bar")
	req.Header.Add([]string{"baz"}, "foo")

	resp, err := app.Test(req)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"reqHeaders": map[string]interface{}{
			"Host": "example.com",
			"Baz":  "foo",
			"Foo":  "bar",
		},
		"level":   "info",
		"message": "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, expected, logs)
}

func Test_Res_Headers(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldResHeaders},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Set("foo", "bar")
		c.Set("baz", "foo")
		return c.SendString("hello")
	})

	req := httptest.NewRequest("GET", "/", nil)

	resp, err := app.Test(req)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"Content-Type": "text/plain; charset=utf-8",
		"Baz":          "foo",
		"Foo":          "bar",
		"level":        "info",
		"message":      "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, expected, logs)
}

func Test_Res_Headers_WrapHeaders(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger:      &logger,
		Fields:      []string{FieldResHeaders},
		WrapHeaders: true,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Set("foo", "bar")
		c.Set("baz", "foo")
		return c.SendString("hello")
	})

	req := httptest.NewRequest("GET", "/", nil)

	resp, err := app.Test(req)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"resHeaders": map[string]interface{}{
			"Content-Type": "text/plain; charset=utf-8",
			"Baz":          "foo",
			"Foo":          "bar",
		},
		"level":   "info",
		"message": "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, expected, logs)
}

func Test_FieldsSnakeCase(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(requestid.New())
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{
			FieldResBody,
			FieldQueryParams,
			FieldBytesReceived,
			FieldBytesSent,
			FieldRequestID,
			FieldResHeaders,
			FieldReqHeaders,
		},
		FieldsSnakeCase: true,
		WrapHeaders:     true,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Set("Foo", "bar")
		return c.SendString("hello")
	})

	req := httptest.NewRequest("GET", "/?param=value", nil)
	req.Header.Add([]string{"X-Request-ID"}, "uuid")
	req.Header.Add([]string{"Baz"}, "foo")

	resp, err := app.Test(req)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"bytes_received": float64(0),
		"bytes_sent":     float64(5),
		"query_params":   "param=value",
		"req_headers": map[string]interface{}{
			"Host":         "example.com",
			"Baz":          "foo",
			"X-Request-Id": "uuid",
		},
		"res_headers": map[string]interface{}{
			"Content-Type": "text/plain; charset=utf-8",
			"Foo":          "bar",
			"X-Request-Id": "uuid",
		},
		"request_id": "uuid",
		"res_body":   "hello",
		"level":      "info",
		"message":    "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, expected, logs)
}

func Test_LoggerLevelsAndMessages(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	levels := []zerolog.Level{zerolog.ErrorLevel, zerolog.WarnLevel, zerolog.InfoLevel}
	messages := []string{"server error", "client error", "success"}

	app := fiber.New()
	app.Use(New(Config{
		Logger:   &logger,
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

	tests := []struct {
		Req     *http.Request
		Status  int
		Level   string
		Message string
	}{
		{
			Req:     httptest.NewRequest("GET", "/500", nil),
			Status:  fiber.StatusInternalServerError,
			Level:   levels[0].String(),
			Message: messages[0],
		},
		{
			Req:     httptest.NewRequest("GET", "/400", nil),
			Status:  fiber.StatusBadRequest,
			Level:   levels[1].String(),
			Message: messages[1],
		},
		{
			Req:     httptest.NewRequest("GET", "/200", nil),
			Status:  fiber.StatusOK,
			Level:   levels[2].String(),
			Message: messages[2],
		},
	}

	for _, test := range tests {
		name := fmt.Sprintf("%s %s", test.Req.Method, test.Req.URL)

		t.Run(name, func(t *testing.T) {
			buf.Reset()
			resp, err := app.Test(test.Req)

			utils.AssertEqual(t, nil, err)
			utils.AssertEqual(t, test.Status, resp.StatusCode)

			var logs map[string]any
			_ = json.Unmarshal(buf.Bytes(), &logs)

			utils.AssertEqual(t, test.Level, logs["level"])
			utils.AssertEqual(t, test.Message, logs["message"])
		})
	}
}

func Test_Logger_FromContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	app := fiber.New()
	app.Use(New(Config{
		GetLogger: func(c fiber.Ctx) zerolog.Logger {
			return zerolog.New(&buf).
				With().
				Str("foo", "bar").
				Logger()
		},
	}))

	_, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	utils.AssertEqual(t, "bar", logs["foo"])
}
