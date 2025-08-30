package monitor

import (
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"

	"github.com/valyala/fasthttp"
)

func Test_Monitor_405(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use("/", New())

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, 405, resp.StatusCode)
}

func Test_Monitor_Html(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	// defaults
	app.Get("/", New())
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))

	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, fiber.MIMETextHTMLCharsetUTF8,
		resp.Header.Get(fiber.HeaderContentType))
	buf, err := io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, true, bytes.Contains(buf, []byte("<title>"+defaultTitle+"</title>")))
	timeoutLine := fmt.Sprintf("setTimeout(fetchJSON, %d)",
		defaultRefresh.Milliseconds()-timeoutDiff)
	assert.Equal(t, true, bytes.Contains(buf, []byte(timeoutLine)))

	// custom config
	conf := Config{Title: "New " + defaultTitle, Refresh: defaultRefresh + time.Second}
	app.Get("/custom", New(conf))
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/custom", nil))

	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, fiber.MIMETextHTMLCharsetUTF8,
		resp.Header.Get(fiber.HeaderContentType))
	buf, err = io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, true, bytes.Contains(buf, []byte("<title>"+conf.Title+"</title>")))
	timeoutLine = fmt.Sprintf("setTimeout(fetchJSON, %d)",
		conf.Refresh.Milliseconds()-timeoutDiff)
	assert.Equal(t, true, bytes.Contains(buf, []byte(timeoutLine)))
}

func Test_Monitor_Html_CustomCodes(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	// defaults
	app.Get("/", New())
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))

	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, fiber.MIMETextHTMLCharsetUTF8,
		resp.Header.Get(fiber.HeaderContentType))
	buf, err := io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, true, bytes.Contains(buf, []byte("<title>"+defaultTitle+"</title>")))
	timeoutLine := fmt.Sprintf("setTimeout(fetchJSON, %d)",
		defaultRefresh.Milliseconds()-timeoutDiff)
	assert.Equal(t, true, bytes.Contains(buf, []byte(timeoutLine)))

	// custom config
	conf := Config{
		Title:      "New " + defaultTitle,
		Refresh:    defaultRefresh + time.Second,
		ChartJSURL: "https://cdnjs.com/libraries/Chart.js",
		FontURL:    "/public/my-font.css",
		CustomHead: `<style>body{background:#fff}</style>`,
	}
	app.Get("/custom", New(conf))
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/custom", nil))

	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, fiber.MIMETextHTMLCharsetUTF8,
		resp.Header.Get(fiber.HeaderContentType))
	buf, err = io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, true, bytes.Contains(buf, []byte("<title>"+conf.Title+"</title>")))
	assert.Equal(t, true, bytes.Contains(buf, []byte("https://cdnjs.com/libraries/Chart.js")))
	assert.Equal(t, true, bytes.Contains(buf, []byte("/public/my-font.css")))
	assert.Equal(t, true, bytes.Contains(buf, []byte(conf.CustomHead)))

	timeoutLine = fmt.Sprintf("setTimeout(fetchJSON, %d)",
		conf.Refresh.Milliseconds()-timeoutDiff)
	assert.Equal(t, true, bytes.Contains(buf, []byte(timeoutLine)))
}

// go test -run Test_Monitor_JSON -race
func Test_Monitor_JSON(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Get("/", New())

	req := httptest.NewRequest(fiber.MethodGet, "/", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, fiber.MIMEApplicationJSON, resp.Header.Get(fiber.HeaderContentType))

	b, err := io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, true, bytes.Contains(b, []byte("pid")))
	assert.Equal(t, true, bytes.Contains(b, []byte("os")))
}

// go test -v -run=^$ -bench=Benchmark_Monitor -benchmem -count=4
func Benchmark_Monitor(b *testing.B) {
	app := fiber.New()

	app.Get("/", New())

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI("/")
	fctx.Request.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h(fctx)
		}
	})

	assert.Equal(b, 200, fctx.Response.Header.StatusCode())
	assert.Equal(b,
		fiber.MIMEApplicationJSON,
		string(fctx.Response.Header.Peek(fiber.HeaderContentType)))
}

// go test -run Test_Monitor_Next
func Test_Monitor_Next(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use("/", New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, 404, resp.StatusCode)
}

// go test -run Test_Monitor_APIOnly -race
func Test_Monitor_APIOnly(t *testing.T) {
	app := fiber.New()

	app.Get("/", New(Config{
		APIOnly: true,
	}))

	req := httptest.NewRequest(fiber.MethodGet, "/", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, fiber.MIMEApplicationJSON, resp.Header.Get(fiber.HeaderContentType))

	b, err := io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, true, bytes.Contains(b, []byte("pid")))
	assert.Equal(t, true, bytes.Contains(b, []byte("os")))
}
