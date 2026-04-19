package monitor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
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
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, fiber.MIMEApplicationJSONCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))

	var result struct {
		PID struct {
			CPU        float64 `json:"cpu"`
			RAM        uint64  `json:"ram"`
			Conns      int     `json:"conns"`
			Goroutines int     `json:"goroutines"`
			Requests   string  `json:"requests"`
			Uptime     float64 `json:"uptime"`
		} `json:"pid"`
		OS struct {
			CPU      float64 `json:"cpu"`
			RAM      uint64  `json:"ram"`
			TotalRAM uint64  `json:"total_ram"`
			LoadAvg  float64 `json:"load_avg"`
			Conns    int     `json:"conns"`
		} `json:"os"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	assert.Equal(t, nil, err)

	// Validate new field types and expected value ranges.
	_, parseErr := strconv.ParseUint(result.PID.Requests, 10, 64)
	assert.NoError(t, parseErr, "pid.requests must be a string containing a non-negative integer")
	assert.GreaterOrEqual(t, result.PID.Uptime, float64(0), "pid.uptime must be >= 0")
	assert.Greater(t, result.PID.Goroutines, 0, "pid.goroutines must be > 0")
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
		fiber.MIMEApplicationJSONCharsetUTF8,
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
	assert.Equal(t, fiber.MIMEApplicationJSONCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))

	b, err := io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, true, bytes.Contains(b, []byte("pid")))
	assert.Equal(t, true, bytes.Contains(b, []byte("os")))
}

// go test -run Test_Monitor_Requests -race
func Test_Monitor_Requests(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Get("/metrics", New(Config{
		APIOnly: true,
	}))

	const nRequests = 5

	// Make several requests
	for i := 0; i < nRequests; i++ {
		req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
		req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
		resp, err := app.Test(req)
		assert.Equal(t, nil, err)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NoError(t, resp.Body.Close())
	}

	// Decode the final response and verify the counter reflects actual traffic.
	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
	defer resp.Body.Close() //nolint:errcheck

	var result struct {
		PID struct {
			Requests string `json:"requests"`
		} `json:"pid"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	assert.Equal(t, nil, err)

	count, err := strconv.ParseUint(result.PID.Requests, 10, 64)
	assert.Equal(t, nil, err)
	// The counter is a global atomic shared across all parallel tests; assert >= the
	// minimum number of requests we know we made (nRequests + this final request).
	assert.GreaterOrEqual(t, count, uint64(nRequests+1))
}
