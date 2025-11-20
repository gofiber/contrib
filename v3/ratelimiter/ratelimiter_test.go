package ratelimiter

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/ratelimiter/storage"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_RateLimit_Success(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Max:        2,
		Expiration: 2 * time.Second,
		Storage:    storage.NewMemory(),
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	// First request should pass
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Second request should pass
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Third request should be rate limited
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 429, resp.StatusCode)
}

func Test_RateLimit_CustomKeyFunc(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Max:        1,
		Expiration: 2 * time.Second,
		Storage:    storage.NewMemory(),
		KeyFunc: func(c fiber.Ctx) string {
			return c.Get("X-API-Key")
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	// Request with API key 1 should pass
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "key1")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Request with API key 2 should pass (different key)
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "key2")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Second request with API key 1 should be rate limited
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "key1")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 429, resp.StatusCode)
}

func Test_RateLimit_Next(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Max:        1,
		Expiration: 2 * time.Second,
		Storage:    storage.NewMemory(),
		Next: func(c fiber.Ctx) bool {
			return c.Get("X-Skip") == "true"
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	// First request should pass
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Second request should be rate limited
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 429, resp.StatusCode)

	// Request with skip header should pass
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Skip", "true")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func Test_RateLimit_Headers(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Max:             3,
		Expiration:      2 * time.Second,
		Storage:         storage.NewMemory(),
		EnableDraftSpec: true,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	// First request
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "3", resp.Header.Get("X-RateLimit-Limit"))
	assert.Equal(t, "2", resp.Header.Get("X-RateLimit-Remaining"))
	assert.NotEmpty(t, resp.Header.Get("X-RateLimit-Reset"))
}

func Test_RateLimit_Expiration(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Max:        1,
		Expiration: 500 * time.Millisecond,
		Storage:    storage.NewMemory(),
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	// First request should pass
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Second request should be rate limited
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 429, resp.StatusCode)

	// Wait for expiration
	time.Sleep(600 * time.Millisecond)

	// Third request should pass after expiration
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func Test_Storage_Memory(t *testing.T) {
	store := storage.NewMemory()
	defer store.Close()

	ctx := context.Background()

	// Test Get on non-existent key
	count, err := store.Get(ctx, "test")
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// Test Increment
	count, isNew, err := store.Increment(ctx, "test", time.Second)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.True(t, isNew)

	// Test Increment again
	count, isNew, err = store.Increment(ctx, "test", time.Second)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.False(t, isNew)

	// Test Get
	count, err = store.Get(ctx, "test")
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	// Test Reset
	err = store.Reset(ctx, "test")
	require.NoError(t, err)

	// Test Get after reset
	count, err = store.Get(ctx, "test")
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func Test_ConfigDefault(t *testing.T) {
	cfg := configDefault(Config{})

	assert.Equal(t, 10, cfg.Max)
	assert.Equal(t, time.Minute, cfg.Expiration)
	assert.NotNil(t, cfg.KeyFunc)
	assert.NotNil(t, cfg.LimitReached)
	assert.NotNil(t, cfg.Storage)
	assert.False(t, cfg.SkipFailedRequests)
	assert.False(t, cfg.SkipSuccessfulRequests)
	assert.False(t, cfg.EnableDraftSpec)

	// Clean up
	defer cfg.Storage.Close()
}

func Test_ConfigDefault_Override(t *testing.T) {
	customStore := storage.NewMemory()
	defer customStore.Close()

	cfg := configDefault(Config{
		Max:        5,
		Expiration: 30 * time.Second,
		Storage:    customStore,
	})

	assert.Equal(t, 5, cfg.Max)
	assert.Equal(t, 30*time.Second, cfg.Expiration)
	assert.Equal(t, customStore, cfg.Storage)
}

func Test_CustomLimitReached(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Max:        1,
		Expiration: time.Minute,
		Storage:    storage.NewMemory(),
		LimitReached: func(c fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error":   "Custom rate limit exceeded",
				"retryAt": time.Now().Add(time.Minute).Unix(),
			})
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	// First request should pass
	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	// Second request should be rate limited with custom response
	req = httptest.NewRequest("GET", "/", nil)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 429, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "Custom rate limit exceeded")
}