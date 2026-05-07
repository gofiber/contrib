package otel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/static"
	"github.com/stretchr/testify/require"
)

func TestMiddleware_StaticAssetsDoNotHang(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Clean(filepath.Join(dir, "repro.css")), []byte("body{font-family:sans-serif;}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Clean(filepath.Join(dir, "repro.js")), []byte("console.log('ok');"), 0o644))

	app := fiber.New()
	app.Use(Middleware())
	app.Use("/public", static.New(dir))

	testCases := []struct {
		path        string
		contentType string
		body        string
	}{
		{path: "/public/repro.css", contentType: "text/css", body: "body{font-family:sans-serif;}"},
		{path: "/public/repro.js", contentType: "javascript", body: "console.log('ok');"},
	}

	for i := 0; i < 25; i++ {
		for _, tc := range testCases {
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tc.path, nil))
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)

			body, readErr := io.ReadAll(resp.Body)
			require.NoError(t, resp.Body.Close())
			require.NoError(t, readErr)
			require.Equal(t, tc.body, string(body))
			require.Contains(t, resp.Header.Get("Content-Type"), tc.contentType)
		}
	}
}

func TestMiddleware_NotFoundPathDoesNotHang(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(Middleware())

	for i := 0; i < 25; i++ {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/.well-known/appspecific/com.chrome.devtools.json", nil))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)

		_, readErr := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, readErr)
	}
}
