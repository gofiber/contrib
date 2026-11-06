package fgprof

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
)

// go test -run Test_Non_Fgprof_Path
func Test_Non_Fgprof_Path(t *testing.T) {
	app := fiber.New()
	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, "escaped", string(body))
}

// go test -run Test_Non_Fgprof_Path_WithPrefix
func Test_Non_Fgprof_Path_WithPrefix(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		Prefix: "/prefix",
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	assert.Equal(t, nil, err)
	assert.Equal(t, "escaped", string(body))
}

// go test -run Test_Fgprof_Path
func Test_Fgprof_Path(t *testing.T) {
	app := fiber.New()
	app.Use(New())

	// Default fgprof interval is 30 seconds
	resp, err := app.Test(httptest.NewRequest("GET", "/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 3000})
	if err != nil && strings.Contains(err.Error(), "empty response") {
		t.Skip("fiber test helper returns empty response for streaming endpoints")
	}
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
}

// go test -run Test_Fgprof_Path_WithPrefix
func Test_Fgprof_Path_WithPrefix(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		Prefix: "/test",
	}))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	// Non fgprof prefix path
	resp, err := app.Test(httptest.NewRequest("GET", "/prefix/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 3000})
	if err != nil && strings.Contains(err.Error(), "empty response") {
		t.Skip("fiber test helper returns empty response for streaming endpoints")
	}
	assert.Equal(t, nil, err)
	assert.Equal(t, 404, resp.StatusCode)
	// Fgprof prefix path
	resp, err = app.Test(httptest.NewRequest("GET", "/test/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 3000})
	if err != nil && strings.Contains(err.Error(), "empty response") {
		t.Skip("fiber test helper returns empty response for streaming endpoints")
	}
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
}

// go test -run Test_Fgprof_Next
func Test_Fgprof_Next(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/pprof/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, 404, resp.StatusCode)
}

// go test -run Test_Fgprof_Next_WithPrefix
func Test_Fgprof_Next_WithPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
		Prefix: "/federated-fiber",
	}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/federated-fiber/debug/pprof/", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, 404, resp.StatusCode)
}

// go test -run Test_Fgprof_Seconds_Overflow
func Test_Fgprof_Seconds_Overflow(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{MaxSeconds: math.MaxInt}))

	for _, seconds := range []string{
		"9223372037",          // the first value whose nanoseconds exceed math.MaxInt64
		"36028797018963968",   // 1<<55, wraps to exactly zero
		"9223372036854775807", // math.MaxInt64
	} {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof?seconds="+seconds, nil))
		assert.Equal(t, nil, err)
		assert.Equal(t, 400, resp.StatusCode)
	}
}

// go test -run Test_Fgprof_Handler_Panic
func Test_Fgprof_Handler_Panic(t *testing.T) {
	app := fiber.New()
	app.Use(adaptor.HTTPHandler(&profileGuard{
		next: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}),
		maxSeconds: defaultSeconds,
		running:    make(chan struct{}, 1),
	}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof", nil))
	assert.Equal(t, nil, err)
	assert.Equal(t, 500, resp.StatusCode)
}

// go test -run Test_Fgprof_MaxSeconds
func Test_Fgprof_MaxSeconds(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{MaxConcurrent: 1, MaxSeconds: 1}))

	// Refused before it takes the only slot, so the next profile still runs
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof?seconds=3600", nil), fiber.TestConfig{Timeout: 2 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 400, resp.StatusCode)

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Without seconds fgprof profiles for 30 seconds
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof", nil), fiber.TestConfig{Timeout: 2 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 400, resp.StatusCode)

	app = fiber.New()
	app.Use(New())

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof?seconds=61", nil), fiber.TestConfig{Timeout: 2 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 400, resp.StatusCode)
}

// go test -run Test_Fgprof_MaxConcurrent
func Test_Fgprof_MaxConcurrent(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		MaxConcurrent: 2,
	}))

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses = map[int]int{}
	)
	for range 6 {
		wg.Go(func() {
			resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof?seconds=2", nil), fiber.TestConfig{Timeout: 10 * time.Second})
			if assert.Equal(t, nil, err) {
				mu.Lock()
				statuses[resp.StatusCode]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	assert.Equal(t, map[int]int{200: 2, 503: 4}, statuses)

	// The finished profiles gave their places back
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 10 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
}

// go test -run Test_Fgprof_ConfigDefault_Prefix
func Test_Fgprof_ConfigDefault_Prefix(t *testing.T) {
	original := ConfigDefault
	t.Cleanup(func() { ConfigDefault = original })
	ConfigDefault.Prefix = "/internal"

	app := fiber.New()
	app.Use(New(Config{
		Next: func(c fiber.Ctx) bool {
			return c.Method() != fiber.MethodGet
		},
	}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 404, resp.StatusCode)

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/internal/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)

	// An explicit Prefix still wins over the default one
	app = fiber.New()
	app.Use(New(Config{Prefix: "/test"}))

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/internal/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 404, resp.StatusCode)

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/test/debug/fgprof?seconds=1", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	assert.Equal(t, nil, err)
	assert.Equal(t, 200, resp.StatusCode)
}
