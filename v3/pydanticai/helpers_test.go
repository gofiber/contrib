package pydanticai_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

const (
	aguiBody   = `{"threadId":"thread-1","runId":"run-1","messages":[{"id":"user-1","role":"user","content":"Hello"}]}`
	vercelBody = `{"trigger":"submit-message","id":"chat-1","messages":[{"id":"user-1","role":"user","parts":[{"type":"text","text":"Hello"}]}]}`
	runBody    = `{"prompt":"Hello"}`

	// observeTimeout bounds how long a test waits for something to happen in the background.
	observeTimeout = 5 * time.Second
)

// userDeps are the dependencies of the agents that see a per-request user.
type userDeps struct {
	User string
}

// userFromQuery resolves the dependencies from the user query parameter.
func userFromQuery(c fiber.Ctx) (userDeps, error) {
	return userDeps{User: c.Query("user")}, nil
}

type ctxKey struct{}

// closeEvent is what an OnClose callback received.
type closeEvent struct {
	deps userDeps
	err  error
}

// unencodable is a payload that fails to encode as JSON.
type unencodable struct{}

func (unencodable) MarshalJSON() ([]byte, error) { return nil, errors.New("cannot encode the payload") }

// progressAgent has a tool that emits a custom event no protocol can encode.
func progressAgent() *ai.Agent[struct{}, string] {
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddTool(agent, "progress", func(_ context.Context, rc *ai.RunContext[struct{}], _ struct{}) (string, error) {
		return "ok", rc.Emit(ai.NewCustomEvent("progress", unencodable{}))
	})
	return agent
}

// panicAgent has a tool that panics.
func panicAgent() *ai.Agent[struct{}, string] {
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "explode", func(context.Context, struct{}) (string, error) {
		panic("kaboom")
	})
	return agent
}

// weatherAgent has a tool, so its runs contain the tool call events as well.
func weatherAgent() *ai.Agent[struct{}, string] {
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "weather", func(context.Context, struct{}) (map[string]int, error) {
		return map[string]int{"temperature": 20}, nil
	})
	return agent
}

// textResponse is a model response that only contains text.
func textResponse(text string) *ai.ModelResponse {
	return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: text}}}
}

// echoModel answers with the instructions of the request, which shows what the
// agent was configured with for this request, and with the value stored under
// ctxKey in the context of the request.
func echoModel() *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(
		ctx context.Context, messages []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		var text string
		for _, message := range slices.Backward(messages) {
			if request, ok := message.(ai.ModelRequest); ok {
				text = request.Instructions
				break
			}
		}
		if value, ok := ctx.Value(ctxKey{}).(string); ok {
			text += "|" + value
		}
		return textResponse(text), nil
	})
}

// userAgent is an agent whose instructions name the user in its dependencies.
func userAgent() *ai.Agent[userDeps, string] {
	agent := ai.NewAgent[userDeps, string](echoModel())
	agent.AddInstructionsFunc(func(_ context.Context, rc *ai.RunContext[userDeps]) (string, error) {
		return "user=" + rc.Deps.User, nil
	})
	return agent
}

// failingModel always fails with err.
func failingModel(err error) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(
		context.Context, []ai.ModelMessage, ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		return nil, err
	})
}

// unreachableModel fails the test when it is called. It reports whether it was.
func unreachableModel() (model *fakes.FunctionModel, called *atomic.Bool) {
	called = new(atomic.Bool)
	model = fakes.NewFunctionModel(func(
		context.Context, []ai.ModelMessage, ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		called.Store(true)
		return textResponse("unreachable"), nil
	})
	return model, called
}

// blockingModel waits until its context is canceled. started is closed when the
// model is called and canceled receives the cause of the cancellation.
func blockingModel() (model *fakes.FunctionModel, started <-chan struct{}, canceled <-chan error) {
	startedCh := make(chan struct{})
	canceledCh := make(chan error, 1)
	model = fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		close(startedCh)
		<-ctx.Done()
		canceledCh <- context.Cause(ctx)
		return nil, ctx.Err()
	})
	return model, startedCh, canceledCh
}

// gatedModel answers once release is closed, or when its context is canceled.
func gatedModel(answer string) (model *fakes.FunctionModel, release chan<- struct{}) {
	gate := make(chan struct{})
	model = fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return textResponse(answer), nil
	})
	return model, gate
}

// slowAgent needs longer to answer than the heartbeat intervals of the tests.
func slowAgent() *ai.Agent[struct{}, string] {
	return ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
		}
		return textResponse("done"), nil
	}))
}

// errorLog records the errors that reach the error handler of an app.
type errorLog struct {
	mu   sync.Mutex
	last error
}

// config is a fiber.Config with the default error handler, wrapped to record the error.
func (l *errorLog) config() fiber.Config {
	return fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error {
		l.mu.Lock()
		l.last = err
		l.mu.Unlock()
		return fiber.DefaultErrorHandler(c, err)
	}}
}

// err returns the error that was recorded last.
func (l *errorLog) err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// newApp serves handler for POST requests to path.
func newApp(path string, handler fiber.Handler, config ...fiber.Config) *fiber.App {
	app := fiber.New(config...)
	app.Post(path, handler)
	return app
}

// send sends a prepared request to app and returns the response with its body.
func send(t *testing.T, app *fiber.App, request *http.Request, config ...fiber.TestConfig) (*http.Response, string) {
	t.Helper()
	response, err := app.Test(request, config...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response, string(content)
}

// newRequest builds a request. It has no content type when contentType is empty.
func newRequest(method, target, contentType, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set(fiber.HeaderContentType, contentType)
	}
	return request
}

// do sends a request to app and returns the response with its body.
func do(
	t *testing.T, app *fiber.App, method, target, contentType, body string, config ...fiber.TestConfig,
) (*http.Response, string) {
	t.Helper()
	return send(t, app, newRequest(method, target, contentType, body), config...)
}

// post sends a JSON body to app.
func post(t *testing.T, app *fiber.App, target, body string) (*http.Response, string) {
	t.Helper()
	return do(t, app, fiber.MethodPost, target, fiber.MIMEApplicationJSON, body)
}

// concurrently posts body to target?user=user-N from 16 goroutines and returns
// the response bodies by N. It does not touch the *testing.T from the goroutines.
func concurrently(t *testing.T, app *fiber.App, target, body string) []string {
	t.Helper()
	const requests = 16
	var (
		group  sync.WaitGroup
		bodies = make([]string, requests)
		errs   = make([]error, requests)
	)
	for i := range requests {
		group.Go(func() {
			request := newRequest(fiber.MethodPost, fmt.Sprintf("%s?user=user-%d", target, i), fiber.MIMEApplicationJSON, body)
			response, err := app.Test(request)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = response.Body.Close() }()
			content, err := io.ReadAll(response.Body)
			bodies[i], errs[i] = string(content), err
		})
	}
	group.Wait()
	for i := range requests {
		require.NoError(t, errs[i])
	}
	return bodies
}

// sseRecord is one record of a server-sent events stream.
type sseRecord struct {
	// data holds the data lines of the record, joined by a newline.
	data string
	// comment is true for a record without data, which is a heartbeat.
	comment bool
}

// newScanner reads the lines of a server-sent events stream.
func newScanner(stream io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return scanner
}

// nextRecord reads the next record of a stream. It returns false when the stream
// has ended. A record that the stream was cut off in is dropped.
func nextRecord(scanner *bufio.Scanner) (sseRecord, bool) {
	var (
		data  []string
		lines int
	)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if lines == 0 {
				continue
			}
			return sseRecord{data: strings.Join(data, "\n"), comment: len(data) == 0}, true
		}
		lines++
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, value)
		}
	}
	return sseRecord{}, false
}

// nextData reads the next record with data and skips heartbeats.
func nextData(scanner *bufio.Scanner) (string, bool) {
	for {
		record, ok := nextRecord(scanner)
		if !ok {
			return "", false
		}
		if !record.comment {
			return record.data, true
		}
	}
}

// parseSSE splits a server-sent events body into the payload of its data
// records and the number of heartbeats.
func parseSSE(body string) (data []string, heartbeats int) {
	scanner := newScanner(strings.NewReader(body))
	for {
		record, ok := nextRecord(scanner)
		if !ok {
			return data, heartbeats
		}
		if record.comment {
			heartbeats++
			continue
		}
		data = append(data, record.data)
	}
}

// aguiEvents decodes the AG-UI events of a response body.
func aguiEvents(t *testing.T, body string) []agui.Event {
	t.Helper()
	data, _ := parseSSE(body)
	events := make([]agui.Event, 0, len(data))
	for _, record := range data {
		var event agui.Event
		require.NoError(t, json.Unmarshal([]byte(record), &event), record)
		events = append(events, event)
	}
	return events
}

// aguiFailure returns the message of the RUN_ERROR event that ends the stream.
func aguiFailure(t *testing.T, body string) string {
	t.Helper()
	events := aguiEvents(t, body)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, agui.EventRunError, last.Type)
	return last.Message
}

// vercelFailure returns the message of the error chunk that ends the stream.
func vercelFailure(t *testing.T, body string) string {
	t.Helper()
	chunks, _ := vercelChunks(t, body)
	require.NotEmpty(t, chunks)
	last := chunks[len(chunks)-1]
	require.Equal(t, vercel.ChunkError, last.Type)
	return last.ErrorText
}

// vercelChunks decodes the chunks of a response body and reports whether the
// stream ended with the [DONE] record.
func vercelChunks(t *testing.T, body string) (chunks []vercel.Chunk, done bool) {
	t.Helper()
	data, _ := parseSSE(body)
	for _, record := range data {
		if record == "[DONE]" {
			done = true
			continue
		}
		require.False(t, done, "chunk after [DONE]: %s", record)
		var chunk vercel.Chunk
		require.NoError(t, json.Unmarshal([]byte(record), &chunk), record)
		chunks = append(chunks, chunk)
	}
	return chunks, done
}

// aguiText joins the text deltas of AG-UI events.
func aguiText(events []agui.Event) string {
	var text strings.Builder
	for _, event := range events {
		if event.Type == agui.EventTextMessageContent {
			delta, _ := event.Delta.(string)
			text.WriteString(delta)
		}
	}
	return text.String()
}

// vercelText joins the text deltas of Vercel AI chunks.
func vercelText(chunks []vercel.Chunk) string {
	var text strings.Builder
	for _, chunk := range chunks {
		if chunk.Type == vercel.ChunkTextDelta {
			text.WriteString(chunk.Delta)
		}
	}
	return text.String()
}

// receive waits for a value from channel or fails the test.
func receive[T any](t *testing.T, channel <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(observeTimeout):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}
