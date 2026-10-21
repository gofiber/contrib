package pydanticai_test

import (
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
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
// prepare changes the request before it is sent.
func openStream(t *testing.T, url, body string, prepare ...func(*http.Request)) (*http.Response, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*observeTimeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	for _, change := range prepare {
		change(request)
	}
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

	response, _ := openStream(t, listen(t, app)+"/agui", aguiBody, func(request *http.Request) {
		request.Header.Set(fiber.HeaderAcceptEncoding, "gzip") // set by hand, so net/http leaves the body encoded
	})
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

// smallBuffers is a listener whose connections have small send buffers, so that
// a client that does not read blocks the writes of the server soon.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(4 << 10)
	}
	return conn, err
}

// postAndStopReading serves app with small socket buffers, and posts body to path
// on a connection that never reads the response.
func postAndStopReading(t *testing.T, app *fiber.App, path, body string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(smallBuffers{ln}, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = app.ShutdownWithTimeout(observeTimeout) })

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(4<<10))
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		path, len(body), body)
	require.NoError(t, err)
}

// talkativeAgent answers with far more than the socket buffers of a client that
// does not read can hold.
func talkativeAgent() *ai.Agent[struct{}, string] {
	parts := make([]ai.ResponsePart, 400)
	for i := range parts {
		parts[i] = ai.TextPart{Content: strings.Repeat("y", 2000)}
	}
	return ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		context.Context, []ai.ModelMessage, ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		return &ai.ModelResponse{Parts: parts}, nil
	}))
}

func Test_TimeoutEndsAStreamWhoseClientStoppedReading(t *testing.T) {
	pydanticai.SetStreamEndGrace(t, 100*time.Millisecond) // not parallel: the grace is global

	ends, closed := make(chan error, 1), make(chan error, 1)
	app := fiber.New()
	app.Post("/agui", pydanticai.AGUI(talkativeAgent(), pydanticai.AGUIConfig[struct{}]{
		Timeout:  200 * time.Millisecond,
		OnRunEnd: func(_ struct{}, _ *ai.RunInfo, err error) { ends <- err },
		OnClose:  func(_ struct{}, err error) { closed <- err },
	}))

	// The run is stuck in a write to the client, where its context does not
	// reach it. Its connection is closed once the write blocked for the grace.
	postAndStopReading(t, app, "/agui", aguiBody)
	require.Error(t, receive(t, ends, "OnRunEnd"))
	require.Error(t, receive(t, closed, "OnClose"))
}

func Test_TimeoutLeavesTheClientOfARunThatIsSlowToEnd(t *testing.T) {
	pydanticai.SetStreamEndGrace(t, 100*time.Millisecond) // not parallel: the grace is global

	agent := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		context.Context, []ai.ModelMessage, ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		time.Sleep(time.Second) // ignores its context
		return textResponse("late"), nil
	}))
	closed := make(chan error, 1)
	app := newApp("/agui", pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
		Timeout: 200 * time.Millisecond,
		OnClose: func(_ struct{}, err error) { closed <- err },
	}))

	// The client reads, so nothing blocks: the stream is not cut.
	response, _ := openStream(t, listen(t, app)+"/agui", aguiBody)
	events := pydanticaitest.AGUIEvents(t, response.Body)
	last := events[len(events)-1]
	require.Equal(t, agui.EventRunFinished, last.Type)
	require.Nil(t, last.Outcome, "the run timed out")
	require.ErrorIs(t, receive(t, closed, "OnClose"), context.DeadlineExceeded)
}

func Test_AConnectionServesTheNextRequestAfterAStreamThatTimedOut(t *testing.T) {
	pydanticai.SetStreamEndGrace(t, 100*time.Millisecond) // not parallel: the grace is global

	app := newApp("/agui", pydanticai.AGUI(slowAgent(), pydanticai.AGUIConfig[struct{}]{Timeout: 50 * time.Millisecond}))
	url := listen(t, app) + "/agui"
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	// stream posts a request and returns whether it reused a connection.
	stream := func() bool {
		var reused bool
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(context.Background(), trace), 2*observeTimeout)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(aguiBody))
		require.NoError(t, err)
		request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		response, err := client.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		events := pydanticaitest.AGUIEvents(t, response.Body)
		require.Equal(t, agui.EventRunFinished, events[len(events)-1].Type)
		return reused
	}

	stream()
	time.Sleep(300 * time.Millisecond) // past the grace
	require.True(t, stream(), "the connection was closed")
}
