package utils

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	v2Fiber "github.com/gofiber/fiber/v2"
	v2Adaptor "github.com/gofiber/fiber/v2/middleware/adaptor"
	v3Fiber "github.com/gofiber/fiber/v3"
	v3Adaptor "github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/stretchr/testify/require"
)

func TestNewWrapFiberContextOfFiverV2(t *testing.T) {
	app := v2Fiber.New()
	reqId := strconv.FormatInt(time.Now().UnixNano(), 16)
	app.Get("/test", func(ctx *v2Fiber.Ctx) error {
		ctx.Response().Header.Set("X-Request-Id", reqId)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			require.Equal(t, reqId, w.Header().Get("X-Request-Id"))
			w.Write([]byte(fmt.Sprintf("reqId: %s", reqId)))
		})
		rawReq, err := v2Adaptor.ConvertRequest(ctx, true)
		require.NoError(t, err)
		handler.ServeHTTP(NewWrapFiberContext(ctx), rawReq)
		return nil
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("reqId: %s", reqId), string(body))
}

func TestNewWrapFiberContextOfFiverV3(t *testing.T) {
	app := v3Fiber.New()
	reqId := strconv.FormatInt(time.Now().UnixNano(), 16)
	app.Get("/test", func(ctx v3Fiber.Ctx) error {
		ctx.Response().Header.Set("X-Request-Id", reqId)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			require.Equal(t, reqId, w.Header().Get("X-Request-Id"))
			w.Write([]byte(fmt.Sprintf("reqId: %s", reqId)))
		})
		rawReq, err := v3Adaptor.ConvertRequest(ctx, true)
		require.NoError(t, err)
		handler.ServeHTTP(NewWrapFiberContext(ctx), rawReq)
		return nil
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("reqId: %s", reqId), string(body))
}
