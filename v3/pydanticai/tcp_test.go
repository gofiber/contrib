package pydanticai_test

import (
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/compress"
	"github.com/stretchr/testify/require"
)

// The tests in this file talk to a server on a real socket. app.Test hands the
// whole body over once the handler is done, so it cannot show that events are
// flushed as they are written or that a closed socket stops the run.

// listen serves app on a free local port and returns its base URL.
func listen(t *testing.T, app *fiber.App) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() {
		served <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() {
		require.NoError(t, app.ShutdownWithTimeout(observeTimeout))
		require.NoError(t, <-served)
	})
	return "http://" + ln.Addr().String()
}

// openStream posts body to url and returns the response, whose body can be read
// while the server is still working, and a function that closes the connection.
func openStream(t *testing.T, url, body string) (*http.Response, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*observeTimeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		_ = response.Body.Close()
	})
	return response, cancel
}

func Test_AGUI_FlushesEventsWhileTheRunIsStillWorking(t *testing.T) {
	t.Parallel()

	model, release := gatedModel("late answer")
	url := listen(t, newApp("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](model)))) + "/agui"

	response, _ := openStream(t, url, aguiBody)
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	require.Contains(t, response.TransferEncoding, "chunked")
	scanner := pydanticaitest.NewReader(response.Body)

	// The start of the run reaches the client while the model has not answered.
	record, ok := scanner.NextData()
	require.True(t, ok)
	require.Contains(t, record, `"type":"RUN_STARTED"`)

	close(release)
	var rest strings.Builder
	for {
		record, ok := scanner.NextData()
		if !ok {
			break
		}
		rest.WriteString(record)
	}
	require.Contains(t, rest.String(), "late answer")
	require.Contains(t, rest.String(), `"type":"RUN_FINISHED"`)
}

func Test_AGUI_StreamsThroughCompress(t *testing.T) {
	t.Parallel()

	model, release := gatedModel("late answer")
	closed := make(chan error, 1)
	app := fiber.New()
	app.Use(compress.New())
	app.Post("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		OnClose: func(_ struct{}, err error) { closed <- err },
	}))

	request, err := http.NewRequest(http.MethodPost, listen(t, app)+"/agui", strings.NewReader(aguiBody))
	require.NoError(t, err)
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	request.Header.Set(fiber.HeaderAcceptEncoding, "gzip") // set by hand, so net/http leaves the body encoded
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, "gzip", response.Header.Get(fiber.HeaderContentEncoding))
	reader, err := gzip.NewReader(response.Body)
	require.NoError(t, err)
	scanner := pydanticaitest.NewReader(reader)

	// The start of the run gets through the compression while the model has not answered.
	record, ok := scanner.NextData()
	require.True(t, ok)
	require.Contains(t, record, `"type":"RUN_STARTED"`)

	close(release)
	var rest strings.Builder
	for {
		record, ok := scanner.NextData()
		if !ok {
			break
		}
		rest.WriteString(record)
	}
	require.Contains(t, rest.String(), `"type":"RUN_FINISHED"`)
	// Closing the compressed body closes the body of the handler, which ends the hold.
	require.NoError(t, receive(t, closed, "OnClose"))
}

func Test_AGUI_ClosedSocketCancelsRun(t *testing.T) {
	t.Parallel()

	model, started, canceled := blockingModel()
	closed := make(chan error, 1)
	handler := pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond,
		OnClose:           func(_ struct{}, err error) { closed <- err },
	})
	url := listen(t, newApp("/agui", handler)) + "/agui"

	response, closeConnection := openStream(t, url, aguiBody)
	record, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	require.Contains(t, record, `"type":"RUN_STARTED"`)
	receive(t, started, "the model call")

	closeConnection()

	require.ErrorIs(t, receive(t, canceled, "the cancellation of the model call"), context.Canceled)
	require.Error(t, receive(t, closed, "OnClose"))
}

func Test_Vercel_ClosedSocketCancelsRun(t *testing.T) {
	t.Parallel()

	model, started, canceled := blockingModel()
	closed := make(chan error, 1)
	handler := pydanticai.Vercel(ai.NewAgent[struct{}, string](model), pydanticai.VercelConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond,
		OnClose:           func(_ struct{}, err error) { closed <- err },
	})
	url := listen(t, newApp("/chat", handler)) + "/chat"

	response, closeConnection := openStream(t, url, vercelBody)
	require.Equal(t, "v1", response.Header.Get("x-vercel-ai-ui-message-stream"))
	record, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	require.Contains(t, record, `"type":"start"`)
	receive(t, started, "the model call")

	closeConnection()

	require.ErrorIs(t, receive(t, canceled, "the cancellation of the model call"), context.Canceled)
	require.Error(t, receive(t, closed, "OnClose"))
}
