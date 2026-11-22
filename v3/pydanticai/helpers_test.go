package pydanticai_test

import (
	"context"
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
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

const (
	aguiBody   = `{"threadId":"thread-1","runId":"run-1","messages":[{"id":"user-1","role":"user","content":"Hello"}]}`
	vercelBody = `{"trigger":"submit-message","id":"chat-1","messages":[{"id":"user-1","role":"user","parts":[{"type":"text","text":"Hello"}]}]}`
	runBody    = `{"prompt":"Hello"}`

	observeTimeout = agenttest.ObserveTimeout
)

// testConfig gives app.Test more than its default second, short for a runner under the race detector.
var testConfig = fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true}

type userDeps struct {
	User string
}

func userFromQuery(c fiber.Ctx) (userDeps, error) {
	return userDeps{User: c.Query("user")}, nil
}

type ctxKey struct{}

type closeEvent struct {
	deps userDeps
	err  error
}

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

func panicAgent() *ai.Agent[struct{}, string] {
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "explode", func(context.Context, struct{}) (string, error) {
		panic("kaboom")
	})
	return agent
}

func weatherAgent() *ai.Agent[struct{}, string] {
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "weather", func(context.Context, struct{}) (map[string]int, error) {
		return map[string]int{"temperature": 20}, nil
	})
	return agent
}

func textResponse(text string) *ai.ModelResponse {
	return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: text}}}
}

// echoModel answers with the instructions of the request and the value under ctxKey in its context.
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

type errorLog struct {
	mu   sync.Mutex
	last error
}

func (l *errorLog) config() fiber.Config {
	return fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error {
		l.mu.Lock()
		l.last = err
		l.mu.Unlock()
		return fiber.DefaultErrorHandler(c, err)
	}}
}

func (l *errorLog) err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func newApp(path string, handler fiber.Handler, config ...fiber.Config) *fiber.App {
	app := fiber.New(config...)
	app.Post(path, handler)
	return app
}

func send(t *testing.T, app *fiber.App, request *http.Request, config ...fiber.TestConfig) (*http.Response, string) {
	t.Helper()
	if len(config) == 0 {
		config = []fiber.TestConfig{testConfig}
	}
	response, err := app.Test(request, config...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response, string(content)
}

// newRequest builds a request, without a content type when contentType is empty.
func newRequest(method, target, contentType, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set(fiber.HeaderContentType, contentType)
	}
	return request
}

func do(
	t *testing.T, app *fiber.App, method, target, contentType, body string, config ...fiber.TestConfig,
) (*http.Response, string) {
	t.Helper()
	return send(t, app, newRequest(method, target, contentType, body), config...)
}

func post(t *testing.T, app *fiber.App, target, body string) (*http.Response, string) {
	t.Helper()
	return do(t, app, fiber.MethodPost, target, fiber.MIMEApplicationJSON, body)
}

// concurrently posts as user-N from 16 goroutines and returns the bodies by N, without touching t there.
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
			response, err := app.Test(request, testConfig)
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

func parseSSE(body string) (data []string, heartbeats int) {
	reader := pydanticaitest.NewReader(strings.NewReader(body))
	for record, ok := reader.Next(); ok; record, ok = reader.Next() {
		if record.Heartbeat {
			heartbeats++
			continue
		}
		data = append(data, record.Data)
	}
	return data, heartbeats
}

func aguiEvents(t *testing.T, body string) []agui.Event {
	t.Helper()
	return pydanticaitest.AGUIEvents(t, strings.NewReader(body))
}

// vercelChunks decodes the chunks of a body, and reports whether [DONE] ended it.
func vercelChunks(t *testing.T, body string) ([]vercel.Chunk, bool) {
	t.Helper()
	return pydanticaitest.VercelChunks(t, strings.NewReader(body))
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

// vercelFailure returns the message of the error chunk that ends the stream before [DONE].
func vercelFailure(t *testing.T, body string) string {
	t.Helper()
	chunks, done := vercelChunks(t, body)
	require.True(t, done, "the error chunk is followed by [DONE]")
	require.NotEmpty(t, chunks)
	last := chunks[len(chunks)-1]
	require.Equal(t, vercel.ChunkError, last.Type)
	return last.ErrorText
}

// tellError sends the client the error itself, so that tests can tell failures apart.
func tellError(err error) string { return err.Error() }

func drain(reader *pydanticaitest.Reader) []string {
	var rest []string
	for data, ok := reader.NextData(); ok; data, ok = reader.NextData() {
		rest = append(rest, data)
	}
	return rest
}

// timeoutCases are Timeout values of Run and MCP, and the deadline of the run, 0 for none.
var timeoutCases = []struct {
	name          string
	timeout, want time.Duration
}{
	{name: "unset is five minutes", want: 5 * time.Minute},
	{name: "a negative value turns it off", timeout: -time.Second},
	{name: "a positive value is used as it is", timeout: 30 * time.Second, want: 30 * time.Second},
}

// adapterLimits are the request limits that the adapter configs of both protocols have.
type adapterLimits struct {
	MaxRequestBytes         int64
	AllowedContentTypes     []string
	DisableContentTypeCheck bool
}

func (l adapterLimits) agui() agui.Config {
	return agui.Config{
		MaxRequestBytes: l.MaxRequestBytes, AllowedContentTypes: l.AllowedContentTypes,
		DisableContentTypeCheck: l.DisableContentTypeCheck,
	}
}

func (l adapterLimits) vercel() vercel.Config {
	return vercel.Config{
		MaxRequestBytes: l.MaxRequestBytes, AllowedContentTypes: l.AllowedContentTypes,
		DisableContentTypeCheck: l.DisableContentTypeCheck,
	}
}

// streamConfig is the config of a streaming handler of either protocol.
type streamConfig[Deps any] = pydanticai.StreamConfig[Deps, adapterLimits]

// withAdapter turns a streamConfig into the config of a protocol.
func withAdapter[Deps, A any](c streamConfig[Deps], adapter A) pydanticai.StreamConfig[Deps, A] {
	return pydanticai.StreamConfig[Deps, A]{
		Next: c.Next, Deps: c.Deps, RunOptions: c.RunOptions, ErrorMessage: c.ErrorMessage,
		OnClose: c.OnClose, OnRunEnd: c.OnRunEnd, Adapter: adapter, Timeout: c.Timeout,
		HeartbeatInterval: c.HeartbeatInterval, DisableHeartbeat: c.DisableHeartbeat,
		CancelOnShutdown: c.CancelOnShutdown,
	}
}

// streamProtocol is the streaming handler of one protocol, and how its stream reads.
type streamProtocol[Deps any] struct {
	name, path, body string
	emptyBody        string // a request that the adapter rejects before the agent runs
	encodeError      string
	startRecord      string
	endRecord        string
	errorRecord      string
	uiMessageStream  string // the x-vercel-ai-ui-message-stream header
	timeoutReason    string
	records          int // the data records of a run that answers with text
	handler          func(*ai.Agent[Deps, string], streamConfig[Deps]) fiber.Handler
	reference        func(*ai.Agent[Deps, string], adapterLimits) http.Handler // the net/http handler of pydantic-ai-go
	text, failure    func(t *testing.T, body string) string
	completed        func(t *testing.T, body string)
	canceled         func(t *testing.T, body string) (reason string)
}

func streamProtocols[Deps any]() []streamProtocol[Deps] {
	var deps Deps
	return []streamProtocol[Deps]{
		{
			name: "AG-UI", path: "/agui", body: aguiBody,
			emptyBody:   `{"threadId":"thread-1","runId":"run-1","messages":[]}`,
			encodeError: "encode AG-UI event", startRecord: `"type":"RUN_STARTED"`,
			endRecord: `"type":"RUN_FINISHED"`, errorRecord: `"type":"RUN_ERROR"`, records: 5,
			handler: func(agent *ai.Agent[Deps, string], c streamConfig[Deps]) fiber.Handler {
				return pydanticai.AGUI(agent, withAdapter(c, c.Adapter.agui()))
			},
			reference: func(agent *ai.Agent[Deps, string], l adapterLimits) http.Handler {
				return agui.NewAdapter(agent, l.agui()).Handler(deps)
			},
			text:    func(t *testing.T, body string) string { return pydanticaitest.AGUIText(aguiEvents(t, body)) },
			failure: aguiFailure,
			completed: func(t *testing.T, body string) {
				outcome := aguiFinish(t, body).Outcome
				require.NotNil(t, outcome, "the client was told that the run completed")
				require.Equal(t, "success", outcome.Type)
			},
			canceled: func(t *testing.T, body string) string {
				require.Nil(t, aguiFinish(t, body).Outcome, "a canceled run has no outcome")
				return ""
			},
		},
		{
			name: "Vercel", path: "/chat", body: vercelBody,
			emptyBody:   `{"trigger":"submit-message","id":"chat-1","messages":[]}`,
			encodeError: "encode Vercel AI chunk", startRecord: `"type":"start"`,
			endRecord: "[DONE]", errorRecord: `"type":"error"`, records: 8,
			uiMessageStream: "v1", timeoutReason: "The agent run timed out.",
			handler: func(agent *ai.Agent[Deps, string], c streamConfig[Deps]) fiber.Handler {
				return pydanticai.Vercel(agent, withAdapter(c, c.Adapter.vercel()))
			},
			reference: func(agent *ai.Agent[Deps, string], l adapterLimits) http.Handler {
				return vercel.NewAdapter(agent, l.vercel()).Handler(deps)
			},
			text: func(t *testing.T, body string) string {
				chunks, done := vercelChunks(t, body)
				require.True(t, done, "the stream ends with [DONE]")
				return pydanticaitest.VercelText(chunks)
			},
			failure: vercelFailure,
			completed: func(t *testing.T, body string) {
				require.Equal(t, vercel.ChunkFinish, vercelEnd(t, body).Type, "the client was told that the run completed")
			},
			canceled: func(t *testing.T, body string) string {
				end := vercelEnd(t, body)
				require.Equal(t, vercel.ChunkAbort, end.Type)
				return end.Reason
			},
		},
	}
}

// aguiFinish returns the RUN_FINISHED event that ends the stream.
func aguiFinish(t *testing.T, body string) agui.Event {
	t.Helper()
	events := aguiEvents(t, body)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, agui.EventRunFinished, last.Type, body)
	return last
}

// vercelEnd returns the chunk that ends the stream, which [DONE] follows.
func vercelEnd(t *testing.T, body string) vercel.Chunk {
	t.Helper()
	chunks, done := vercelChunks(t, body)
	require.True(t, done, "the stream ends with [DONE]")
	require.NotEmpty(t, chunks)
	return chunks[len(chunks)-1]
}
