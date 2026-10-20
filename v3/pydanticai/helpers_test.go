package pydanticai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// lastRequest returns the newest request in the history a model receives.
func lastRequest(messages []ai.ModelMessage) ai.ModelRequest {
	for i := len(messages) - 1; i >= 0; i-- {
		if request, ok := messages[i].(ai.ModelRequest); ok {
			return request
		}
	}
	return ai.ModelRequest{}
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
		text := lastRequest(messages).Instructions
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

// do sends a request to app and returns the response with its body.
func do(
	t *testing.T, app *fiber.App, method, target, contentType, body string, config ...fiber.TestConfig,
) (*http.Response, string) {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set(fiber.HeaderContentType, contentType)
	}
	response, err := app.Test(request, config...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	content, err := io.ReadAll(response.Body)
	if err != nil {
		// A stream that the test abandons ends without its terminating chunk.
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	}
	return response, string(content)
}

// post sends a JSON body to app.
func post(t *testing.T, app *fiber.App, target, body string) (*http.Response, string) {
	t.Helper()
	return do(t, app, fiber.MethodPost, target, fiber.MIMEApplicationJSON, body)
}

// postRaw sends a JSON body to app. Unlike post it never touches a *testing.T,
// so it can run in a goroutine other than the one of the test.
func postRaw(app *fiber.App, target, body string) (string, error) {
	request := httptest.NewRequest(fiber.MethodPost, target, strings.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	response, err := app.Test(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	return string(content), err
}

// concurrently posts body to target?user=user-N from n goroutines and returns
// the response bodies by N.
func concurrently(t *testing.T, app *fiber.App, target, body string, n int) []string {
	t.Helper()
	var (
		group  sync.WaitGroup
		bodies = make([]string, n)
		errs   = make([]error, n)
	)
	for i := range n {
		group.Go(func() {
			bodies[i], errs[i] = postRaw(app, fmt.Sprintf("%s?user=user-%d", target, i), body)
		})
	}
	group.Wait()
	for i := range n {
		require.NoError(t, errs[i])
	}
	return bodies
}

// parseSSE splits a server-sent events body into the payload of its data
// records and the number of comment-only records, which are heartbeats.
func parseSSE(body string) (data []string, comments int) {
	for record := range strings.SplitSeq(body, "\n\n") {
		if record == "" {
			continue
		}
		var payload []string
		for line := range strings.SplitSeq(record, "\n") {
			if value, ok := strings.CutPrefix(line, "data: "); ok {
				payload = append(payload, value)
			}
		}
		if len(payload) == 0 {
			comments++
			continue
		}
		data = append(data, strings.Join(payload, "\n"))
	}
	return data, comments
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

// abandon is the test config of a client that walks away while the stream is open.
func abandon(after time.Duration) fiber.TestConfig {
	return fiber.TestConfig{Timeout: after, FailOnTimeout: false}
}

// jsonUnmarshal decodes a JSON record.
func jsonUnmarshal(record string, target any) error {
	return json.Unmarshal([]byte(record), target)
}
