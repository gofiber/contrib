package pydanticai_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/fiber/v3"
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

// nextRecord reads the next server-sent events record that carries data and
// skips heartbeat comments. It returns false when the stream has ended.
func nextRecord(t *testing.T, scanner *bufio.Scanner) (string, bool) {
	t.Helper()

	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "" && len(data) > 0:
			return strings.Join(data, "\n"), true
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
	require.NoError(t, scanner.Err())
	return "", false
}

func Test_AGUI_FlushesEventsWhileTheRunIsStillWorking(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	model := fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return textResponse("late answer"), nil
	})
	app := aguiApp(pydanticai.AGUI(ai.NewAgent[struct{}, string](model)))
	url := listen(t, app) + "/agui"

	response, _ := openStream(t, url, aguiBody)
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	require.Contains(t, response.TransferEncoding, "chunked")
	scanner := bufio.NewScanner(response.Body)

	// The start of the run reaches the client while the model has not answered.
	record, ok := nextRecord(t, scanner)
	require.True(t, ok)
	require.Contains(t, record, `"type":"RUN_STARTED"`)

	close(release)
	var rest strings.Builder
	for {
		record, ok := nextRecord(t, scanner)
		if !ok {
			break
		}
		rest.WriteString(record)
	}
	require.Contains(t, rest.String(), "late answer")
	require.Contains(t, rest.String(), `"type":"RUN_FINISHED"`)
}

func Test_AGUI_ClosedSocketCancelsRun(t *testing.T) {
	t.Parallel()

	model, started, canceled := blockingModel()
	closed := make(chan error, 1)
	app := aguiApp(pydanticai.AGUI(ai.NewAgent[struct{}, string](model), pydanticai.AGUIConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond,
		OnClose:           func(_ fiber.Ctx, err error) { closed <- err },
	}))
	url := listen(t, app) + "/agui"

	response, closeConnection := openStream(t, url, aguiBody)
	record, ok := nextRecord(t, bufio.NewScanner(response.Body))
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
	app := vercelApp(pydanticai.Vercel(ai.NewAgent[struct{}, string](model), pydanticai.VercelConfig[struct{}]{
		HeartbeatInterval: 20 * time.Millisecond,
		OnClose:           func(_ fiber.Ctx, err error) { closed <- err },
	}))
	url := listen(t, app) + "/chat"

	response, closeConnection := openStream(t, url, vercelBody)
	require.Equal(t, "v1", response.Header.Get("x-vercel-ai-ui-message-stream"))
	record, ok := nextRecord(t, bufio.NewScanner(response.Body))
	require.True(t, ok)
	require.Contains(t, record, `"type":"start"`)
	receive(t, started, "the model call")

	closeConnection()

	require.ErrorIs(t, receive(t, canceled, "the cancellation of the model call"), context.Canceled)
	require.Error(t, receive(t, closed, "OnClose"))
}

func Test_AGUI_FullRunOverTCPMatchesTheAdapter(t *testing.T) {
	t.Parallel()

	app := aguiApp(pydanticai.AGUI(weatherAgent(), pydanticai.AGUIConfig[struct{}]{
		Adapter: agui.Config{MaxRequestBytes: 1 << 10},
	}))
	url := listen(t, app) + "/agui"

	response, _ := openStream(t, url, aguiBody)
	scanner := bufio.NewScanner(response.Body)
	var types []string
	for {
		record, ok := nextRecord(t, scanner)
		if !ok {
			break
		}
		var event agui.Event
		require.NoError(t, jsonUnmarshal(record, &event))
		types = append(types, string(event.Type))
	}
	require.Equal(t, "RUN_STARTED", types[0])
	require.Equal(t, "RUN_FINISHED", types[len(types)-1])
	require.Contains(t, types, "TOOL_CALL_RESULT")
}
