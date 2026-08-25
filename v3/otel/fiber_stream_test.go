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
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
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

// TestMiddleware_StreamedChunkedUploadIsNotRecycled guards the request side:
// replacing a chunked upload's stream recycles it, so handlers read a cleared
// reader and panic. A real listener is needed for chunked framing.
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

	// Listening from here on, so requests queue in the accept backlog.
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

	// Unknown length makes net/http use chunked encoding, so no Content-Length.
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

// TestBodyStreamSize pins down which readers may be measured without being read.
func TestBodyStreamSize(t *testing.T) {
	t.Parallel()

	payload := make([]byte, 2048)

	// Peek fills 64 of the 2048 bytes, so Len understates the body by 1984.
	buffered := bufio.NewReaderSize(bytes.NewReader(payload), 64)
	_, err := buffered.Peek(64)
	require.NoError(t, err)
	require.Equal(t, 64, buffered.Buffered())

	tests := []struct {
		name   string
		stream io.Reader
		size   int64
		known  bool
	}{
		{name: "nil", stream: nil},
		{name: "bytes.Reader", stream: bytes.NewReader(payload), size: 2048, known: true},
		{name: "bytes.Buffer", stream: bytes.NewBuffer(payload), size: 2048, known: true},
		{name: "strings.Reader", stream: strings.NewReader(string(payload)), size: 2048, known: true},
		{name: "io.LimitedReader", stream: &io.LimitedReader{R: bytes.NewReader(payload), N: 512}, size: 512, known: true},
		{name: "io.LimitedReader with negative N", stream: &io.LimitedReader{R: bytes.NewReader(payload), N: -1}},
		{name: "bufio.Reader", stream: buffered},
		{name: "opaque reader", stream: io.NopCloser(bytes.NewReader(payload))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			size, known := bodyStreamSize(tt.stream)
			require.Equal(t, tt.known, known)
			require.Equal(t, tt.size, size)
		})
	}
}

// TestMiddleware_HeadStreamedResponseReportsNoBody: a HEAD response keeps the
// Content-Length a GET would return, but sends no bytes.
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

// TestMiddleware_SkipBodyResponseReportsNoBody: a handler sets SkipBody itself,
// which neither the method nor the status reveals.
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

	// The handler leaves Content-Length in place, so the client's read ends short.
	// What matters here is that no bytes reached the wire.
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

// TestMiddleware_StreamedRequestIgnoresDeclaredLength: a client declares a large
// body and sends only the pre-read part, so its Content-Length must not be used.
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
		// Reject without draining, as an upload guard would.
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

	// Count observations, not sums: a recorded zero is as wrong as the declared
	// megabyte. Scoping by route excludes fasthttp's error-path replay.
	observations := uint64(0)
	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricNameHTTPServerRequestBodySize {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			for _, point := range histogram.DataPoints {
				if route, found := point.Attributes.Value(semconv.HTTPRouteKey); found && route.AsString() == "/upload" {
					observations += point.Count
				}
			}
		}
	}

	require.Zero(t, observations,
		"streamed request reported a body size; the declared length is not a measurement")
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
