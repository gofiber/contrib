package otel

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/static"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// TestMiddleware_HeadStreamedResponseReportsNoBody covers responses whose headers
// describe a body that is never written. A HEAD response keeps the Content-Length
// a GET would have returned, so reading that header as bytes sent would report a
// payload the client never received.
func TestMiddleware_HeadStreamedResponseReportsNoBody(t *testing.T) {
	t.Parallel()

	const payloadSize = 2048

	reader := metric.NewManualReader()

	app := fiber.New()
	app.Use(Middleware(WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))))
	app.All("/stream", func(c fiber.Ctx) error {
		return c.SendStream(bytes.NewReader(make([]byte, payloadSize)), payloadSize)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodHead, "/stream", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, strconv.Itoa(payloadSize), resp.Header.Get("Content-Length"))

	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, readErr)
	require.Empty(t, body, "fasthttp must not write a body for HEAD")

	metrics := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &metrics))

	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricNameHTTPServerResponseBodySize {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			require.Len(t, histogram.DataPoints, 1)
			require.Zero(t, histogram.DataPoints[0].Sum, "HEAD response reported a body it never sent")

			return
		}
	}

	t.Fatal("response body size metric not found")
}

// TestMiddleware_SkipBodyResponseReportsNoBody covers a handler that suppresses
// its own body by setting Response.SkipBody. Neither the method nor the status
// says the body is skipped, so only the flag itself distinguishes the declared
// Content-Length from the bytes actually written.
func TestMiddleware_SkipBodyResponseReportsNoBody(t *testing.T) {
	t.Parallel()

	const payloadSize = 2048

	reader := metric.NewManualReader()

	app := fiber.New()
	app.Use(Middleware(WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))))
	app.Get("/stream", func(c fiber.Ctx) error {
		if err := c.SendStream(bytes.NewReader(make([]byte, payloadSize)), payloadSize); err != nil {
			return err
		}
		c.Response().SkipBody = true

		return nil
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/stream", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, strconv.Itoa(payloadSize), resp.Header.Get("Content-Length"))

	// The handler leaves the declared Content-Length in place, so the client is
	// told to expect a body that never arrives and its read ends short. That is
	// the handler's doing, not the middleware's; what matters here is that no
	// bytes reached the wire.
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Empty(t, body, "fasthttp must not write a body when SkipBody is set")

	metrics := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &metrics))

	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricNameHTTPServerResponseBodySize {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			require.Len(t, histogram.DataPoints, 1)
			require.Zero(t, histogram.DataPoints[0].Sum, "skipped response reported a body it never sent")

			return
		}
	}

	t.Fatal("response body size metric not found")
}

// TestMiddleware_StreamedRequestIgnoresDeclaredLength covers a client that
// declares a large body and sends only the part fasthttp pre-reads. Under
// StreamRequestBody the over-limit request still reaches the handler carrying the
// declared Content-Length, so reading that header would let any client choose the
// value recorded in the request-size histogram.
func TestMiddleware_StreamedRequestIgnoresDeclaredLength(t *testing.T) {
	t.Parallel()

	const (
		declaredSize = 1 << 20
		bodyLimit    = 32 * 1024
		actuallySent = 8 * 1024
	)

	reader := metric.NewManualReader()

	app := fiber.New(fiber.Config{StreamRequestBody: true, BodyLimit: bodyLimit})
	app.Use(Middleware(WithMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))))
	app.Post("/upload", func(c fiber.Ctx) error {
		// Reject without draining, as a handler guarding against oversized
		// uploads would.
		return c.SendStatus(http.StatusRequestEntityTooLarge)
	})

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

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)

	_, err = conn.Write([]byte("POST /upload HTTP/1.1\r\nHost: x\r\nContent-Length: " +
		strconv.Itoa(declaredSize) + "\r\n\r\n" + strings.Repeat("x", actuallySent)))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, statusLine, "413")
	require.NoError(t, conn.Close())

	metrics := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &metrics))

	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricNameHTTPServerRequestBodySize {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			for _, point := range histogram.DataPoints {
				require.Zero(t, point.Sum,
					"recorded a declared body size the client never sent")
			}
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
