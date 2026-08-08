package otel

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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

// TestMiddleware_StreamedChunkedUploadIsNotRecycled guards the request side of the
// same stream lifecycle. With StreamRequestBody a chunked upload arrives as a
// *fasthttp.requestStream, and replacing it to measure its size hands it to
// closeBodyStream, which returns it to fasthttp's pool with its reader cleared.
// Downstream handlers then read an already recycled stream and panic.
//
// A real listener is needed because chunked framing has to survive an actual
// connection; app.Test cannot express a body of unknown length.
func TestMiddleware_StreamedChunkedUploadIsNotRecycled(t *testing.T) {
	t.Parallel()

	const uploadSize = 4096

	var (
		mu         sync.Mutex
		bytesRead  int
		streamNil  bool
		panicValue any
	)

	app := fiber.New(fiber.Config{StreamRequestBody: true})
	app.Use(Middleware())
	app.Post("/upload", func(c fiber.Ctx) error {
		defer func() {
			if recovered := recover(); recovered != nil {
				mu.Lock()
				panicValue = recovered
				mu.Unlock()
			}
		}()

		stream := c.Request().BodyStream()
		if stream == nil {
			mu.Lock()
			streamNil = true
			mu.Unlock()

			return c.SendStatus(http.StatusNoContent)
		}

		body, err := io.ReadAll(stream)
		if err != nil {
			return err
		}

		mu.Lock()
		bytesRead = len(body)
		mu.Unlock()

		return c.SendStatus(http.StatusNoContent)
	})

	// The socket is listening from here on, so requests queue in the accept
	// backlog until the server goroutine picks them up.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	served := make(chan error, 1)
	go func() {
		served <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() {
		require.NoError(t, app.Shutdown())
		<-served
	})

	// A body of unknown length makes net/http use chunked transfer encoding, so
	// the request carries no Content-Length for the middleware to read.
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/upload", io.NopCloser(bytes.NewReader(make([]byte, uploadSize))))
	require.NoError(t, err)
	request.ContentLength = -1

	resp, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	_, copyErr := io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, copyErr)

	mu.Lock()
	defer mu.Unlock()
	require.Nil(t, panicValue, "handler panicked reading a recycled request stream")
	require.False(t, streamNil, "request body stream was replaced or dropped")
	require.Equal(t, uploadSize, bytesRead)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
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
