package zerolog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"github.com/gofiber/fiber/v3/middleware/timeout"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
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
	assert.Equal(t, nil, err)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, readableResBody, logs[FieldResBody])
}

func Test_SkipBody(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		SkipField: func(field string, _ fiber.Ctx) bool {
			return field == FieldBody
		},
		Logger: &logger,
		Fields: []string{FieldPID, FieldBody},
	}))

	body := bytes.NewReader([]byte("this is test"))
	resp, err := app.Test(httptest.NewRequest("GET", "/", body))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)
	_, ok := logs[FieldBody]

	assert.Equal(t, false, ok)
}

func Test_SkipResBody(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		SkipField: func(field string, _ fiber.Ctx) bool {
			return field == FieldResBody
		},
		Logger: &logger,
		Fields: []string{FieldResBody},
	}))

	body := bytes.NewReader([]byte("this is test"))
	resp, err := app.Test(httptest.NewRequest("GET", "/", body))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)
	_, ok := logs[FieldResBody]

	assert.Equal(t, false, ok)
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, "some random error", logs[FieldError])
	assert.Equal(t, float64(500), logs[FieldStatus])
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	latencyStr, ok := logs[FieldLatency].(string)
	assert.Equal(t, true, ok)
	assert.Equal(t, true, strings.Contains(latencyStr, "ms"))
	assert.Equal(t, float64(200), logs[FieldStatus])
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func Test_Logger_Skip(t *testing.T) {
	t.Parallel()

	type skipKey struct{}
	skipLog := func(c fiber.Ctx) error {
		c.Locals(skipKey{}, skipKey{})
		return c.Next()
	}

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Skip: func(c fiber.Ctx) bool {
			return c.Locals(skipKey{}) != nil
		},
		Fields: []string{
			FieldResBody,
		},
	}))
	app.Get("/", skipLog, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNotFound)
	})

	body := []byte("HELLO")
	app.Get("/log", func(c fiber.Ctx) error {
		return c.Send(body)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	assert.Equal(t, 0, len(buf.Bytes()))

	resp, err = app.Test(httptest.NewRequest("GET", "/log", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]any{
		FieldResBody: string(body),
	}

	var logs map[string]any
	err = json.Unmarshal(buf.Bytes(), &logs)
	assert.Equal(t, nil, err)

	for key, value := range expected {
		assert.Equal(t, value, logs[key])
	}
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	expected := map[string]interface{}{
		"body":          "",
		"ip":            "0.0.0.0",
		"host":          "example.com",
		"url":           "/?foo=bar",
		"level":         "warn",
		"message":       "Client error",
		"method":        "GET",
		"route":         "/",
		"protocol":      "HTTP/1.1",
		"pid":           float64(os.Getpid()),
		"queryParams":   "foo=bar",
		"resBody":       "Not Found",
		"bytesReceived": float64(0),
		"bytesSent":     float64(9),
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	for key, value := range expected {
		assert.Equal(t, value, logs[key])
	}

	latencyStr, ok := logs[FieldLatency].(string)
	assert.Equal(t, true, ok)
	assert.Equal(t, true, strings.Contains(latencyStr, "ms"))
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
	assert.Equal(t, nil, err)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, expectedGetResponse, logs[FieldResBody])
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, requestID, logs[FieldRequestID])
}

func Test_Skip_URIs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Next: func(c fiber.Ctx) bool {
			return c.Path() == "/ignore_logging"
		},
	}))

	app.Get("/ignore_logging", func(c fiber.Ctx) error {
		return errors.New("no log")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/ignore_logging", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, 0, buf.Len())
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
	req.Header.Add("foo", "bar")
	req.Header.Add("baz", "foo")

	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"Host":    "example.com",
		"Baz":     "foo",
		"Foo":     "bar",
		"level":   "info",
		"message": "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, expected, logs)
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
	req.Header.Add("foo", "bar")
	req.Header.Add("baz", "foo")

	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

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

	assert.Equal(t, expected, logs)
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"Content-Type": "text/plain; charset=utf-8",
		"Baz":          "foo",
		"Foo":          "bar",
		"level":        "info",
		"message":      "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, expected, logs)
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
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

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

	assert.Equal(t, expected, logs)
}

func Test_Res_Headers_RedactsSensitiveValues(t *testing.T) {
	for _, wrapHeaders := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrap=%t", wrapHeaders), func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := zerolog.New(&buf)

			app := fiber.New()
			app.Use(New(Config{
				Logger:      &logger,
				Fields:      []string{FieldResHeaders},
				WrapHeaders: wrapHeaders,
			}))

			app.Get("/", func(c fiber.Ctx) error {
				c.Response().Header.Add(fiber.HeaderSetCookie, "session=secret")
				c.Response().Header.Add(fiber.HeaderSetCookie, "refresh=secret")
				c.Set(fiber.HeaderLocation, "/reset?token=secret")
				c.Set("X-Csrf-Token", "secret")
				c.Set("X-Safe", "visible")
				return c.SendStatus(fiber.StatusNoContent)
			})

			resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
			assert.NoError(t, err)
			assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)

			var logs map[string]any
			assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
			if wrapHeaders {
				logs = logs[FieldResHeaders].(map[string]any)
			}

			// A multi-value sensitive header stays an array of redacted values.
			assert.Equal(t, []any{"[REDACTED]", "[REDACTED]"}, logs["Set-Cookie"])
			assert.Equal(t, "[REDACTED]", logs["Location"])
			assert.Equal(t, "[REDACTED]", logs["X-Csrf-Token"])
			assert.Equal(t, "visible", logs["X-Safe"])
		})
	}
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
	req.Header.Add("X-Request-ID", "uuid")
	req.Header.Add("Baz", "foo")

	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

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

	assert.Equal(t, expected, logs)
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

			assert.Equal(t, nil, err)
			assert.Equal(t, test.Status, resp.StatusCode)

			var logs map[string]any
			_ = json.Unmarshal(buf.Bytes(), &logs)

			assert.Equal(t, test.Level, logs["level"])
			assert.Equal(t, test.Message, logs["message"])
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
	assert.Equal(t, nil, err)

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, "bar", logs["foo"])
}

func Test_Logger_WhitelistHeaders(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldReqHeaders},
		SkipHeader: func(header string, _ fiber.Ctx) bool {
			switch header {
			case "Foo", "Host", "Bar":
				return false
			default:
				return true
			}
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("foo", "bar")
	req.Header.Add("baz", "foo")

	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"Host":    "example.com",
		"Foo":     "bar",
		"level":   "info",
		"message": "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, expected, logs)

	app.Get("/res-headers", func(c fiber.Ctx) error {
		c.Set("test", "skip")
		c.Set("bar", "bar")
		return c.SendString("hello")
	})
	req = httptest.NewRequest("GET", "/res-headers", nil)
	req.Header.Add("foo", "bar")
	req.Header.Add("baz", "foo")

	resp, err = app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected = map[string]interface{}{
		"Bar":     "bar",
		"level":   "info",
		"message": "Success",
	}
}

func Test_WhitelistHeaders_Resp_Headers(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldResHeaders},
		SkipHeader: func(header string, _ fiber.Ctx) bool {
			return header != "Bar"
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Set("test", "skip")
		c.Set("bar", "bar")
		return c.SendString("hello")
	})
	req := httptest.NewRequest("GET", "/", nil)

	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"Bar":     "bar",
		"level":   "info",
		"message": "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, expected, logs)
}

func Test_Logger_BlacklistHeaders(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldReqHeaders},
		SkipHeader: func(header string, _ fiber.Ctx) bool {
			return header == "Foo"
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("foo", "bar")
	req.Header.Add("baz", "foo")

	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"Host":    "example.com",
		"Baz":     "foo",
		"level":   "info",
		"message": "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, expected, logs)
}

func Test_BlacklistHeaders_Resp_Headers(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldResHeaders},
		SkipHeader: func(header string, _ fiber.Ctx) bool {
			return header == "Test"
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Set("test", "skip")
		c.Set("bar", "bar")
		return c.SendString("hello")
	})
	req := httptest.NewRequest("GET", "/", nil)

	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	expected := map[string]interface{}{
		"Bar":          "bar",
		"Content-Type": "text/plain; charset=utf-8",
		"level":        "info",
		"message":      "Success",
	}

	var logs map[string]any
	_ = json.Unmarshal(buf.Bytes(), &logs)

	assert.Equal(t, expected, logs)
}

func Test_StreamedRequest_NotBuffered(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	unread := strings.NewReader("streamed")
	left := -1

	app := fiber.New(fiber.Config{StreamRequestBody: true})
	app.Use(func(c fiber.Ctx) error {
		c.Request().SetBodyStream(unread, unread.Len())
		err := c.Next()
		left = unread.Len()
		return err
	})
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldStatus, FieldBytesReceived, FieldBody},
	}))
	app.Post("/", func(_ fiber.Ctx) error {
		return fiber.ErrUnauthorized
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/", nil))
	assert.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	// the stream the handler left alone is not read for a log field
	assert.Equal(t, len("streamed"), left)

	var logs map[string]any
	assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
	assert.EqualValues(t, fiber.StatusUnauthorized, logs[FieldStatus])
	assert.EqualValues(t, len("streamed"), logs[FieldBytesReceived])
	assert.Equal(t, "", logs[FieldBody])
}

func Test_StreamedMultipartForm_NotMarshaled(t *testing.T) {
	t.Parallel()

	const body = "--b\r\nContent-Disposition: form-data; name=\"field\"\r\n\r\nvalue\r\n--b--\r\n"

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	app := fiber.New(fiber.Config{StreamRequestBody: true, BodyLimit: 8})
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldStatus, FieldBytesReceived, FieldBody},
	}))
	app.Post("/", func(_ fiber.Ctx) error {
		return fiber.ErrUnauthorized
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, "multipart/form-data; boundary=b")

	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	var logs map[string]any
	assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
	assert.EqualValues(t, fiber.StatusUnauthorized, logs[FieldStatus])
	assert.EqualValues(t, len(body), logs[FieldBytesReceived])
	assert.Equal(t, "", logs[FieldBody])
}

func Test_StreamedResponse_NotBuffered(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		size int
		sent int
	}{
		{name: "known length", size: len("streamed"), sent: len("streamed")},
		{name: "unknown length", size: -1, sent: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := zerolog.New(&buf)

			unread := strings.NewReader("streamed")
			left := -1

			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				err := c.Next()
				left = unread.Len()
				return err
			})
			app.Use(New(Config{
				Logger: &logger,
				Fields: []string{FieldStatus, FieldBytesSent, FieldResBody},
			}))
			app.Get("/", func(c fiber.Ctx) error {
				return c.SendStream(unread, tc.size)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
			assert.NoError(t, err)
			assert.Equal(t, fiber.StatusOK, resp.StatusCode)
			// the stream is for the client, not the log
			assert.Equal(t, len("streamed"), left)

			var logs map[string]any
			assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
			assert.EqualValues(t, fiber.StatusOK, logs[FieldStatus])
			assert.EqualValues(t, tc.sent, logs[FieldBytesSent])
			assert.Equal(t, "", logs[FieldResBody])
		})
	}
}

func Test_Body_LoggedAsReceived(t *testing.T) {
	t.Parallel()

	gzipped := func(b []byte) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, err := w.Write(b)
		assert.NoError(t, err)
		assert.NoError(t, w.Close())
		return buf.Bytes()
	}

	for _, tc := range []struct {
		name     string
		encoding string
		body     []byte
	}{
		{name: "unknown coding", encoding: "unknown", body: []byte("raw")},
		{name: "compress", encoding: "compress", body: []byte("raw")},
		{name: "gzip", encoding: "gzip", body: gzipped([]byte("decoded"))},
		{name: "gzip past BodyLimit", encoding: "gzip", body: gzipped(make([]byte, 4096))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := zerolog.New(&buf)

			app := fiber.New(fiber.Config{BodyLimit: 1024})
			app.Use(New(Config{
				Logger: &logger,
				Fields: []string{FieldStatus, FieldBody},
			}))
			app.Post("/", func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(tc.body))
			req.Header.Set(fiber.HeaderContentEncoding, tc.encoding)
			resp, err := app.Test(req)
			assert.NoError(t, err)
			assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)

			// zerolog replaces invalid UTF-8 as encoding/json does
			want, err := json.Marshal(string(tc.body))
			assert.NoError(t, err)
			var wantBody string
			assert.NoError(t, json.Unmarshal(want, &wantBody))

			var logs map[string]any
			assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
			assert.EqualValues(t, fiber.StatusNoContent, logs[FieldStatus])
			assert.Equal(t, wantBody, logs[FieldBody])
		})
	}
}

func Test_Logger_TimedOutHandler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	wrote := make(chan struct{})

	app := fiber.New()
	app.Use(New(Config{
		Logger:      &logger,
		Fields:      []string{FieldStatus, FieldResBody, FieldBytesSent, FieldRequestID, FieldResHeaders},
		WrapHeaders: true,
	}))
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		<-wrote
		return err
	})
	app.Get("/", timeout.New(func(c fiber.Ctx) error {
		<-c.Context().Done()
		c.Set(fiber.HeaderXRequestID, "late")
		err := c.Status(fiber.StatusAccepted).SendString("late")
		close(wrote)
		// Still writing its answer while the entry is assembled.
		_ = c.SendStatus(fiber.StatusAccepted)
		return err
	}, timeout.Config{Timeout: 10 * time.Millisecond}))

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	assert.NoError(t, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	var logs map[string]any
	assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
	assert.Equal(t, map[string]any{
		"level":         "error",
		"message":       "Server error",
		FieldStatus:     float64(fiber.StatusServiceUnavailable),
		FieldResBody:    fiber.ErrServiceUnavailable.Message,
		FieldBytesSent:  float64(len(fiber.ErrServiceUnavailable.Message)),
		FieldRequestID:  "",
		FieldResHeaders: map[string]any{fiber.HeaderContentType: fiber.MIMETextPlainCharsetUTF8},
	}, logs)
}

func Test_Logger_TimedOutHandler_GetResBody(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	wrote := make(chan struct{})

	app := fiber.New()
	app.Use(New(Config{
		Logger: &logger,
		Fields: []string{FieldResBody},
		GetResBody: func(c fiber.Ctx) []byte {
			return c.Response().Body()
		},
	}))
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		<-wrote
		return err
	})
	app.Get("/", timeout.New(func(c fiber.Ctx) error {
		<-c.Context().Done()
		err := c.SendString("late")
		close(wrote)
		return err
	}, timeout.Config{Timeout: 10 * time.Millisecond}))

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	assert.NoError(t, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	var logs map[string]any
	assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
	assert.Equal(t, fiber.ErrServiceUnavailable.Message, logs[FieldResBody])
}

func Test_Logger_TimedOutHandler_ReturnsError(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]Config{
		"logged":   {},
		"skipped":  {Skip: func(fiber.Ctx) bool { return true }},
		"disabled": {Levels: []zerolog.Level{zerolog.Disabled}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			logger := zerolog.New(io.Discard)
			cfg.Logger = &logger
			errs := make(chan error, 1)

			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				err := c.Next()
				errs <- err
				return err
			})
			app.Use(New(cfg))
			app.Get("/slow", timeout.New(func(c fiber.Ctx) error {
				<-c.Context().Done()
				return c.SendString("late")
			}, timeout.Config{Timeout: 10 * time.Millisecond}))
			app.Get("/bad", func(fiber.Ctx) error { return fiber.ErrBadRequest })

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/slow", nil))
			assert.NoError(t, err)
			assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
			assert.ErrorIs(t, <-errs, fiber.ErrServiceUnavailable)

			resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/bad", nil))
			assert.NoError(t, err)
			assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
			assert.NoError(t, <-errs)
		})
	}
}

func Test_Logger_TimedOutHandler_Request(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		config fiber.Config
		want   map[string]any
	}{
		"direct": {
			want: map[string]any{
				"level":            "error",
				"message":          "Server error",
				FieldStatus:        float64(fiber.StatusServiceUnavailable),
				FieldIP:            "0.0.0.0",
				FieldHost:          "example.com",
				FieldPath:          "/",
				FieldBytesReceived: float64(0),
			},
		},
		"trusted proxy": {
			config: fiber.Config{
				TrustProxy:       true,
				TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0"}},
				ProxyHeader:      fiber.HeaderXForwardedFor,
			},
			want: map[string]any{
				"level":            "error",
				"message":          "Server error",
				FieldStatus:        float64(fiber.StatusServiceUnavailable),
				FieldPath:          "/",
				FieldBytesReceived: float64(0),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := zerolog.New(&buf)
			finished := make(chan struct{})

			app := fiber.New(tc.config)
			app.Use(New(Config{
				Logger: &logger,
				Fields: []string{
					FieldStatus, FieldIP, FieldHost, FieldPath, FieldReferer, FieldIPs, FieldUserAgent,
					FieldQueryParams, FieldBody, FieldReqHeaders, FieldBytesReceived,
				},
				WrapHeaders: true,
			}))
			app.Post("/", timeout.New(func(c fiber.Ctx) error {
				defer close(finished)
				<-c.Context().Done()
				_, _, _, _ = c.IP(), c.Query("a"), c.Cookies("b"), c.GetReqHeaders()
				return nil
			}, timeout.Config{Timeout: 10 * time.Millisecond}))

			req := httptest.NewRequest(http.MethodPost, "/?a=1", strings.NewReader("body"))
			req.TransferEncoding = []string{"chunked"}
			req.Header.Set(fiber.HeaderCookie, "b=2")
			req.Header.Set(fiber.HeaderReferer, "https://example.com/")
			req.Header.Set(fiber.HeaderUserAgent, "test")
			req.Header.Set(fiber.HeaderXForwardedFor, "203.0.113.7")
			resp, err := app.Test(req)
			assert.NoError(t, err)
			assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
			<-finished

			var logs map[string]any
			assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
			assert.Equal(t, tc.want, logs)
		})
	}
}

// Not parallel, and measured against GetRespHeaders so an upgrade does not break it.
func Test_Res_Headers_Allocs(t *testing.T) {
	app := fiber.New()
	c := app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(c)
	for i := range 10 {
		c.Set("X-Header-"+strconv.Itoa(i), "value")
	}

	want := testing.AllocsPerRun(100, func() { _ = c.GetRespHeaders() })
	got := testing.AllocsPerRun(100, func() { _ = responseHeaders(app, c.Response()) })
	assert.Equal(t, want, got)
}

type ctxValueKey struct{}

func Test_Logger_HookReadsUserContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf).Hook(zerolog.HookFunc(func(e *zerolog.Event, _ zerolog.Level, _ string) {
		if v, ok := e.GetCtx().Value(ctxValueKey{}).(string); ok {
			e.Str("ctx_value", v)
		}
		if id := requestid.FromContext(e.GetCtx()); id != "" {
			e.Str("hook_request_id", id)
		}
	}))

	app := fiber.New()
	app.Use(requestid.New())
	app.Use(New(Config{Logger: &logger}))
	app.Get("/", func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxValueKey{}, "from-user-context"))
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(fiber.MethodGet, "/", nil)
	req.Header.Set(fiber.HeaderXRequestID, "rid-123")
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	var logs map[string]any
	assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
	assert.Equal(t, "from-user-context", logs["ctx_value"])
	assert.Equal(t, "rid-123", logs["hook_request_id"])
}

func Test_Logger_TimedOutHandler_HookContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf).Hook(zerolog.HookFunc(func(e *zerolog.Event, _ zerolog.Level, _ string) {
		if v, ok := e.GetCtx().Value(ctxValueKey{}).(string); ok {
			e.Str("ctx_value", v)
		}
		e.Str("hook_request_id", requestid.FromContext(e.GetCtx()))
	}))
	wrote := make(chan struct{})

	app := fiber.New()
	app.Use(requestid.New())
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxValueKey{}, "from-user-context"))
		return c.Next()
	})
	app.Use(New(Config{Logger: &logger}))
	app.Get("/", timeout.New(func(c fiber.Ctx) error {
		<-c.Context().Done()
		c.Locals("late", true)
		close(wrote)
		return nil
	}, timeout.Config{Timeout: 10 * time.Millisecond}))

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	assert.NoError(t, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	<-wrote

	var logs map[string]any
	assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
	assert.Equal(t, "from-user-context", logs["ctx_value"])
	assert.Empty(t, logs["hook_request_id"])
}

func Test_Logger_SSE_HookReadsLocals(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf).Hook(zerolog.HookFunc(func(e *zerolog.Event, _ zerolog.Level, _ string) {
		e.Str("hook_request_id", requestid.FromContext(e.GetCtx()))
	}))

	app := fiber.New()
	app.Use(requestid.New())
	app.Use(New(Config{Logger: &logger}))
	app.Get("/", sse.New(sse.Config{Handler: func(c fiber.Ctx, _ *sse.Stream) error {
		c.Locals("late", true)
		return nil
	}}))

	req := httptest.NewRequest(fiber.MethodGet, "/", nil)
	req.Header.Set(fiber.HeaderXRequestID, "rid-123")
	resp, err := app.Test(req)
	assert.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	assert.NoError(t, err)

	var logs map[string]any
	assert.NoError(t, json.Unmarshal(buf.Bytes(), &logs))
	assert.Equal(t, "rid-123", logs["hook_request_id"])
}
