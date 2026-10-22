package pydanticaimcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	aimcp "github.com/Kludex/pydantic-ai-go/ai/mcp"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaimcp"
	"github.com/gofiber/fiber/v3"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// observeTimeout bounds how long a test waits for something to happen.
const observeTimeout = 5 * time.Second

// The tests talk to the handler as an MCP client does, over a real socket.

// serve serves app on a free local port and returns the URL of the MCP route.
func serve(t *testing.T, app *fiber.App) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() {
		served <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() {
		// fasthttp counts a connection that has not sent a request as idle only
		// after 5 seconds. A client that dialed a connection it did not need,
		// as the HTTP client does when requests overlap, would keep the
		// shutdown waiting for that long.
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		require.NoError(t, app.ShutdownWithTimeout(observeTimeout))
		require.NoError(t, <-served)
	})
	return "http://" + ln.Addr().String() + "/mcp"
}

// header adds the header to every request that the client sends.
type header struct{ name, value string }

func (h header) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.name, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

// connect connects an MCP client to the route, with an Authorization header
// when authorization is not empty.
func connect(t *testing.T, endpoint, authorization string, options ...*mcpsdk.ClientOptions) *mcpsdk.ClientSession {
	t.Helper()

	var clientOptions *mcpsdk.ClientOptions
	if len(options) > 0 {
		clientOptions = options[0]
	}
	transport := &mcpsdk.StreamableClientTransport{Endpoint: endpoint}
	if authorization != "" {
		transport.HTTPClient = &http.Client{Transport: header{name: "Authorization", value: authorization}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*observeTimeout)
	t.Cleanup(cancel)
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "client", Version: "1.0.0"}, clientOptions).
		Connect(ctx, transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// ask calls the tool of the handler with the question.
func ask(t *testing.T, session *mcpsdk.ClientSession, question string) *mcpsdk.CallToolResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*observeTimeout)
	defer cancel()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "ask", Arguments: map[string]any{"question": question},
	})
	require.NoError(t, err)
	return result
}

// resultText returns the text of the first content of a result.
func resultText(t *testing.T, result *mcpsdk.CallToolResult) string {
	t.Helper()

	require.NotEmpty(t, result.Content)
	content, ok := result.Content[0].(*mcpsdk.TextContent)
	require.True(t, ok, "%T", result.Content[0])
	return content.Text
}

func newApp(handler fiber.Handler) *fiber.App {
	app := fiber.New()
	app.All("/mcp", handler)
	return app
}

func failing(err error) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		return nil, err
	})
}

func Test_New_RunsTheAgent(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](agenttest.Answering("Paris."))
	session := connect(t, serve(t, newApp(pydanticaimcp.New(agent))), "")

	ctx, cancel := context.WithTimeout(context.Background(), observeTimeout)
	defer cancel()
	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 1)
	tool := tools.Tools[0]
	require.Equal(t, "ask", tool.Name)
	require.Equal(t, "Ask the agent a question", tool.Description)
	schema, err := json.Marshal(tool.InputSchema)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"type": "object",
		"properties": {"question": {"type": "string", "description": "The question or the task for the agent"}},
		"required": ["question"],
		"additionalProperties": false
	}`, string(schema))

	result := ask(t, session, "What is the capital of France?")
	require.False(t, result.IsError)
	require.Equal(t, "Paris.", resultText(t, result), "text goes to the client as it is")
	require.Nil(t, result.StructuredContent)

	info := session.InitializeResult().ServerInfo
	require.Equal(t, "pydantic-ai-go", info.Name)
	require.Equal(t, "1.0.0", info.Version)
}

func Test_New_Identity(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](agenttest.Answering("ok"), ai.WithAgentName("support"))

	t.Run("the name of the agent", func(t *testing.T) {
		t.Parallel()

		session := connect(t, serve(t, newApp(pydanticaimcp.New(agent))), "")
		require.Equal(t, "support", session.InitializeResult().ServerInfo.Name)
	})

	t.Run("configured", func(t *testing.T) {
		t.Parallel()

		app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
			Name: "helpdesk", Version: "2.3.4", Tool: "consult.agent", Description: "Consult the support agent",
		}))
		session := connect(t, serve(t, app), "")
		info := session.InitializeResult().ServerInfo
		require.Equal(t, "helpdesk", info.Name)
		require.Equal(t, "2.3.4", info.Version)

		ctx, cancel := context.WithTimeout(context.Background(), observeTimeout)
		defer cancel()
		tools, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		require.Len(t, tools.Tools, 1)
		require.Equal(t, "consult.agent", tools.Tools[0].Name)
		require.Equal(t, "Consult the support agent", tools.Tools[0].Description)
	})
}

type forecast struct {
	City  string `json:"city"`
	TempC int    `json:"temp_c"`
}

// listing is an output whose JSON is an array, not an object.
type listing struct{ Items []string }

func (l listing) MarshalJSON() ([]byte, error) { return json.Marshal(l.Items) }

// unwritable is an output that cannot be encoded.
type unwritable struct{ Value string }

func (unwritable) MarshalJSON() ([]byte, error) { return nil, errors.New("cannot be encoded") }

func Test_New_Output(t *testing.T) {
	t.Parallel()

	t.Run("an object is structured content as well", func(t *testing.T) {
		t.Parallel()

		agent := ai.NewAgent[struct{}, forecast](fakes.NewTestModel())
		session := connect(t, serve(t, newApp(pydanticaimcp.New(agent))), "")

		result := ask(t, session, "Weather in Paris?")
		require.False(t, result.IsError)
		structured, ok := result.StructuredContent.(map[string]any)
		require.True(t, ok, "%T", result.StructuredContent)
		require.Contains(t, structured, "city")
		require.Contains(t, structured, "temp_c")
		var decoded forecast
		require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &decoded), "the text is the same JSON")
	})

	t.Run("anything but an object is text only", func(t *testing.T) {
		t.Parallel()

		agent := ai.NewAgent[struct{}, listing](fakes.NewTestModel())
		session := connect(t, serve(t, newApp(pydanticaimcp.New(agent))), "")

		result := ask(t, session, "List the cities")
		require.False(t, result.IsError)
		require.Nil(t, result.StructuredContent, "older clients expect structured content to be an object")
		var items []string
		require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &items))
	})

	t.Run("an output that cannot be encoded", func(t *testing.T) {
		t.Parallel()

		ends := make(chan error, 1)
		agent := ai.NewAgent[struct{}, unwritable](fakes.NewTestModel())
		app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
			OnRunEnd: func(_ struct{}, _ *ai.RunInfo, err error) { ends <- err },
		}))
		result := ask(t, connect(t, serve(t, app), ""), "Hello")
		require.True(t, result.IsError)
		require.Contains(t, resultText(t, result), "encode the output of the agent")
		require.NoError(t, <-ends, "the run had completed")
	})

	t.Run("ErrorMessage applies to an output that cannot be encoded", func(t *testing.T) {
		t.Parallel()

		agent := ai.NewAgent[struct{}, unwritable](fakes.NewTestModel())
		app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
			ErrorMessage: func(error) string { return "The assistant is not available." },
		}))
		result := ask(t, connect(t, serve(t, app), ""), "Hello")
		require.True(t, result.IsError)
		require.Equal(t, "The assistant is not available.", resultText(t, result), "what went wrong stays on the server")
	})
}

type user struct{ Name string }

// userAgent calls a tool that reports the user that the run works for, and
// answers with what the tool returned.
func userAgent() *ai.Agent[user, string] {
	agent := ai.NewAgent[user, string](fakes.NewFunctionModel(func(
		_ context.Context, messages []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		for _, message := range messages {
			request, ok := message.(ai.ModelRequest)
			if !ok {
				continue
			}
			for _, part := range request.Parts {
				if returned, ok := part.(ai.ToolReturnPart); ok {
					return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: fmt.Sprint(returned.Content)}}}, nil
				}
			}
		}
		return &ai.ModelResponse{Parts: []ai.ResponsePart{
			ai.ToolCallPart{ToolName: "whoami", Args: json.RawMessage(`{}`), ToolCallID: "call-1"},
		}}, nil
	}))
	ai.AddTool(agent, "whoami", func(_ context.Context, rc *ai.RunContext[user], _ struct{}) (string, error) {
		return "user " + rc.Deps.Name, nil
	})
	return agent
}

func Test_New_DepsAreThoseOfTheRequest(t *testing.T) {
	t.Parallel()

	var seen sync.Map
	agent := ai.NewAgent[user, string](fakes.NewFunctionModel(func(
		_ context.Context, messages []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: "answered"}}}, nil
	}))

	tokens := map[string]string{"Bearer alice-token": "alice", "Bearer bob-token": "bob"}
	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error { // stands in for the authentication of the application
		name, ok := tokens[c.Get("Authorization")]
		if !ok {
			return fiber.ErrUnauthorized
		}
		c.Locals("user", name)
		return c.Next()
	})
	app.All("/mcp", pydanticaimcp.New(agent, pydanticaimcp.Config[user]{
		Deps: func(c fiber.Ctx) (user, error) { return user{Name: fiber.Locals[string](c, "user")}, nil },
		OnRunEnd: func(deps user, _ *ai.RunInfo, _ error) {
			count, _ := seen.LoadOrStore(deps.Name, new(atomic.Int32))
			count.(*atomic.Int32).Add(1)
		},
	}))
	endpoint := serve(t, app)

	// Without credentials the client cannot connect, and no run is started.
	_, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "client", Version: "1.0.0"}, nil).
		Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: endpoint}, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "Unauthorized")

	var wg sync.WaitGroup
	for _, authorization := range []string{"Bearer alice-token", "Bearer bob-token", "Bearer alice-token", "Bearer bob-token"} {
		wg.Go(func() {
			result := ask(t, connect(t, endpoint, authorization), "Hello")
			require.False(t, result.IsError)
		})
	}
	wg.Wait()

	for name, want := range map[string]int32{"alice": 2, "bob": 2} {
		count, ok := seen.Load(name)
		require.True(t, ok, name)
		require.Equal(t, want, count.(*atomic.Int32).Load(), name)
	}
}

func Test_New_ToolsActOnBehalfOfTheUser(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error {
		c.Locals("user", strings.TrimPrefix(c.Get("Authorization"), "Bearer "))
		return c.Next()
	})
	app.All("/mcp", pydanticaimcp.New(userAgent(), pydanticaimcp.Config[user]{
		Deps: func(c fiber.Ctx) (user, error) { return user{Name: fiber.Locals[string](c, "user")}, nil },
	}))
	endpoint := serve(t, app)

	for _, name := range []string{"alice", "bob"} {
		result := ask(t, connect(t, endpoint, "Bearer "+name), "Who am I?")
		require.False(t, result.IsError)
		require.Equal(t, "user "+name, resultText(t, result), "the tool saw the dependencies of the request")
	}
}

type traceKey struct{}

func Test_New_RequestContextValuesAreAvailableToTools(t *testing.T) {
	t.Parallel()

	var seen atomic.Value
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "trace", func(ctx context.Context, _ struct{}) (string, error) {
		seen.Store(ctx.Value(traceKey{}))
		return "ok", nil
	})

	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error { // as an OpenTelemetry middleware does
		c.SetContext(context.WithValue(c.Context(), traceKey{}, "trace-42"))
		return c.Next()
	})
	app.All("/mcp", pydanticaimcp.New(agent))

	require.False(t, ask(t, connect(t, serve(t, app), ""), "Hello").IsError)
	require.Equal(t, "trace-42", seen.Load())
}

// A tool can keep a goroutine that reads its context after the call was
// answered. That context must not end in the RequestCtx of fasthttp, which is
// pooled: the requests that follow write its values while the goroutine reads
// them. The race detector reports it.
func Test_New_ToolContextOutlivesTheCall(t *testing.T) {
	t.Parallel()

	var (
		once sync.Once
		late sync.WaitGroup
	)
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "late", func(ctx context.Context, _ struct{}) (string, error) {
		once.Do(func() {
			late.Go(func() {
				time.Sleep(30 * time.Millisecond) // until the requests that follow are served
				for range 20000 {
					_ = ctx.Value(traceKey{})
				}
			})
		})
		return "ok", nil
	})

	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error { // writes the values of the RequestCtx
		c.Locals("user", "alice")
		return c.Next()
	})
	app.All("/mcp", pydanticaimcp.New(agent))
	session := connect(t, serve(t, app), "")

	for range 200 {
		require.False(t, ask(t, session, "Hello").IsError)
	}
	late.Wait()
}

func Test_New_RunOptions(t *testing.T) {
	t.Parallel()

	var instructions atomic.Value
	agent := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		_ context.Context, messages []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		for _, message := range messages {
			if request, ok := message.(ai.ModelRequest); ok && request.Instructions != "" {
				instructions.Store(request.Instructions)
			}
		}
		return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: "ok"}}}, nil
	}))
	app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
		RunOptions: func(fiber.Ctx) ([]ai.RunOption, error) {
			return []ai.RunOption{ai.WithRunInstructions("Answer in French.")}, nil
		},
	}))

	require.False(t, ask(t, connect(t, serve(t, app), ""), "Hello").IsError)
	require.Equal(t, "Answer in French.", instructions.Load())
}

func Test_New_FailedCallbacksAnswerTheRequest(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](agenttest.Answering("ok"))
	for name, config := range map[string]pydanticaimcp.Config[struct{}]{
		"Deps": {Deps: func(fiber.Ctx) (struct{}, error) { return struct{}{}, fiber.ErrForbidden }},
		"RunOptions": {RunOptions: func(fiber.Ctx) ([]ai.RunOption, error) {
			return nil, fiber.ErrForbidden
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			endpoint := serve(t, newApp(pydanticaimcp.New(agent, config)))
			response, _ := post(t, endpoint, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil)
			require.Equal(t, fiber.StatusForbidden, response.StatusCode, "the error is returned from the handler")
		})
	}
}

func Test_New_Next(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.All("/mcp", pydanticaimcp.New(ai.NewAgent[struct{}, string](agenttest.Answering("ok")), pydanticaimcp.Config[struct{}]{
		Next: func(c fiber.Ctx) bool { return c.Get("X-Skip") != "" },
	}), func(c fiber.Ctx) error {
		return c.SendString("skipped")
	})
	endpoint := serve(t, app)

	response, body := post(t, endpoint, "", map[string]string{"X-Skip": "yes"})
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	require.Equal(t, "skipped", body)
}

func Test_New_Panics(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() { pydanticaimcp.New[struct{}, string](nil) })
	agent := ai.NewAgent[struct{}, string](agenttest.Answering("ok"))
	for _, name := range []string{"has space", "", strings.Repeat("a", 129), "ask?"} {
		if name == "" {
			require.NotPanics(t, func() { pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{Tool: name}) }, "the default")
			continue
		}
		require.Panics(t, func() { pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{Tool: name}) }, name)
	}
	require.NotPanics(t, func() { pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{Tool: "Ask_the-agent.v1"}) })
}

func Test_New_Errors(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")

	t.Run("a failed run is an error of the tool", func(t *testing.T) {
		t.Parallel()

		ends := make(chan error, 1)
		agent := ai.NewAgent[struct{}, string](failing(secret))
		app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
			OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) {
				if run != nil {
					ends <- err
				}
			},
		}))
		result := ask(t, connect(t, serve(t, app), ""), "Hello")
		require.True(t, result.IsError, "the model of the client can read the error")
		require.Contains(t, resultText(t, result), "sk-secret")
		require.ErrorIs(t, <-ends, secret)
	})

	t.Run("ErrorMessage decides what the client reads", func(t *testing.T) {
		t.Parallel()

		var seen atomic.Value
		agent := ai.NewAgent[struct{}, string](failing(secret))
		app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
			ErrorMessage: func(err error) string {
				seen.Store(err)
				return "The assistant is not available."
			},
		}))
		result := ask(t, connect(t, serve(t, app), ""), "Hello")
		require.True(t, result.IsError)
		require.Equal(t, "The assistant is not available.", resultText(t, result))
		require.ErrorIs(t, seen.Load().(error), secret, "the message is made from the error of the run")
	})

	t.Run("an empty question", func(t *testing.T) {
		t.Parallel()

		model := agenttest.Answering("must not be called")
		session := connect(t, serve(t, newApp(pydanticaimcp.New(ai.NewAgent[struct{}, string](model)))), "")
		for _, question := range []string{"", "  \n\t", "\u00a0\u2003"} {
			result := ask(t, session, question)
			require.True(t, result.IsError)
			require.Contains(t, resultText(t, result), "must not be empty")
		}
	})

	t.Run("a missing question", func(t *testing.T) {
		t.Parallel()

		session := connect(t, serve(t, newApp(pydanticaimcp.New(ai.NewAgent[struct{}, string](agenttest.Answering("ok"))))), "")
		ctx, cancel := context.WithTimeout(context.Background(), observeTimeout)
		defer cancel()
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "ask", Arguments: map[string]any{}})
		require.NoError(t, err)
		require.True(t, result.IsError, "the input is validated against the schema of the tool")
	})

	t.Run("deferred tool calls", func(t *testing.T) {
		t.Parallel()

		agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
		ai.AddSimpleTool(agent, "delete", func(context.Context, struct{}) (string, error) {
			return "deleted", nil
		}, ai.WithApprovalRequired())
		ends := make(chan error, 1)
		app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
			OnRunEnd: func(_ struct{}, _ *ai.RunInfo, err error) { ends <- err },
		}))
		result := ask(t, connect(t, serve(t, app), ""), "Delete it")
		require.True(t, result.IsError)
		require.Contains(t, resultText(t, result), "deferred tool calls")
		require.ErrorContains(t, <-ends, "deferred tool calls")
	})
}

func Test_New_Timeout(t *testing.T) {
	t.Parallel()

	canceled := make(chan error, 1)
	agent := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		<-ctx.Done()
		canceled <- ctx.Err()
		return nil, ctx.Err()
	}))
	app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{Timeout: 50 * time.Millisecond}))

	result := ask(t, connect(t, serve(t, app), ""), "Hello")
	require.True(t, result.IsError)
	require.Contains(t, resultText(t, result), "deadline exceeded")
	require.ErrorIs(t, <-canceled, context.DeadlineExceeded)
}

func Test_New_OnRunEnd(t *testing.T) {
	t.Parallel()

	type end struct {
		run *ai.RunInfo
		err error
	}
	ends := make(chan end, 1)
	agent := ai.NewAgent[struct{}, string](agenttest.Answering("ok"))
	app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
		OnRunEnd: func(_ struct{}, run *ai.RunInfo, err error) { ends <- end{run: run, err: err} },
	}))
	require.False(t, ask(t, connect(t, serve(t, app), ""), "Hello").IsError)

	got := <-ends
	require.NoError(t, got.err)
	require.NotNil(t, got.run)
	require.NotZero(t, got.run.Usage().Requests)
}

func Test_New_Panics_AreErrorsOfTheTool(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it. Neither are the
	// subtests, as the log is read after them.
	logs := agenttest.CaptureLog(t)

	boom := func(tools ...string) *ai.Agent[struct{}, string] {
		agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
		for _, name := range tools {
			ai.AddSimpleTool(agent, name, func(context.Context, struct{}) (string, error) {
				panic("kaboom in " + name)
			})
		}
		return agent
	}
	model := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		context.Context, []ai.ModelMessage, ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		panic("kaboom in the model")
	}))

	tests := map[string]struct {
		agent *ai.Agent[struct{}, string]
		cause string
	}{
		"the model":               {agent: model, cause: "kaboom in the model"},
		"a tool":                  {agent: boom("explode"), cause: "kaboom in explode"},
		"tools that run together": {agent: agenttest.ConcurrentAgent("waiting", "explode"), cause: "kaboom in explode"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ends := make(chan error, 1)
			app := newApp(pydanticaimcp.New(test.agent, pydanticaimcp.Config[struct{}]{
				OnRunEnd: func(_ struct{}, _ *ai.RunInfo, err error) { ends <- err },
			}))
			result := ask(t, connect(t, serve(t, app), ""), "Hello")
			require.True(t, result.IsError)
			require.Equal(t, "internal error", resultText(t, result), "the client is not told what the panic was")
			require.ErrorContains(t, <-ends, "panic in the run: "+test.cause)
			require.Contains(t, logs.String(), "panic in the run: "+test.cause)
		})
	}
	require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")

	t.Run("ErrorMessage", func(t *testing.T) {
		var seen atomic.Value
		app := newApp(pydanticaimcp.New(model, pydanticaimcp.Config[struct{}]{
			ErrorMessage: func(err error) string {
				seen.Store(err.Error())
				return "The assistant is not available."
			},
		}))
		result := ask(t, connect(t, serve(t, app), ""), "Hello")
		require.Equal(t, "The assistant is not available.", resultText(t, result))
		require.Equal(t, "internal error", seen.Load(), "ErrorMessage is not given the value of the panic")
	})
}

func Test_New_PanicsInCallbacksAreLogged(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	agent := ai.NewAgent[struct{}, string](agenttest.Answering("ok"))
	app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
		OnRunEnd: func(struct{}, *ai.RunInfo, error) { panic("bug in OnRunEnd") },
	}))
	result := ask(t, connect(t, serve(t, app), ""), "Hello")
	require.False(t, result.IsError, "the answer does not depend on the accounting")
	require.Equal(t, "ok", resultText(t, result))
	require.Contains(t, logs.String(), "panic in OnRunEnd: bug in OnRunEnd")
}

// A panic while the output is encoded ends the call as well.
type explosive struct{}

func (explosive) MarshalJSON() ([]byte, error) { panic("kaboom in MarshalJSON") }

func Test_New_PanicWhileEncodingTheOutput(t *testing.T) {
	// Not parallel: the log is global, and CaptureLog replaces it.
	logs := agenttest.CaptureLog(t)

	ends := make(chan error, 1)
	agent := ai.NewAgent[struct{}, explosive](fakes.NewTestModel())
	app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
		OnRunEnd: func(_ struct{}, _ *ai.RunInfo, err error) { ends <- err },
	}))
	result := ask(t, connect(t, serve(t, app), ""), "Hello")
	require.True(t, result.IsError)
	require.Equal(t, "internal error", resultText(t, result))
	require.NoError(t, <-ends, "the run itself had ended")
	require.Contains(t, logs.String(), "kaboom in MarshalJSON")
}

func Test_New_Methods(t *testing.T) {
	t.Parallel()

	endpoint := serve(t, newApp(pydanticaimcp.New(ai.NewAgent[struct{}, string](agenttest.Answering("ok")))))
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		response, _ := send(t, method, endpoint, "", nil)
		require.Equal(t, fiber.StatusMethodNotAllowed, response.StatusCode, method+": there are no sessions and no stream")
	}
}

// A client that wants to hear about changes of the tools gets an answer at once.
// A subscription that stayed open would keep the shutdown of the server waiting.
func Test_New_SubscriptionsDoNotKeepRequestsOpen(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		methods []string
	)
	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error { // the JSON-RPC methods that reach the route
		var message struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(c.Body(), &message)
		mu.Lock()
		methods = append(methods, message.Method)
		mu.Unlock()
		return c.Next()
	})
	app.All("/mcp", pydanticaimcp.New(ai.NewAgent[struct{}, string](agenttest.Answering("ok"))))
	endpoint := serve(t, app)

	// The response to a subscription ends with its result, because the server
	// has nothing to tell: post returns when the response is over.
	response, body := post(t, endpoint, `{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{`+
		`"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},`+
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"notifications":{"toolsListChanged":true}}}`,
		map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "subscriptions/listen"})
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	require.Contains(t, body, `"notifications":{}`, "the server agreed to no notification")
	require.Contains(t, body, `"result"`)

	// A client that asks for them, as the client of the SDK does when it has a
	// handler, connects and calls the tool.
	changed := make(chan struct{}, 1)
	session := connect(t, endpoint, "", &mcpsdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) { changed <- struct{}{} },
	})
	require.Equal(t, "ok", resultText(t, ask(t, session, "Hello")))
	require.Empty(t, changed)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, strings.Count(strings.Join(methods, " "), "subscriptions/listen"), "the client asked to be told about changes")
}

// callAsync calls the tool without waiting for its result.
func callAsync(session *mcpsdk.ClientSession, question string) <-chan callResult {
	answered := make(chan callResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*observeTimeout)
		defer cancel()
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
			Name: "ask", Arguments: map[string]any{"question": question},
		})
		answered <- callResult{result: result, err: err}
	}()
	return answered
}

type callResult struct {
	result *mcpsdk.CallToolResult
	err    error
}

func Test_New_CancelOnShutdown(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	canceled := make(chan error, 1)
	ends := make(chan error, 1)
	agent := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- context.Cause(ctx)
		return nil, ctx.Err()
	}))
	app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
		CancelOnShutdown: true,
		OnRunEnd:         func(_ struct{}, _ *ai.RunInfo, err error) { ends <- err },
	}))
	session := connect(t, serve(t, app), "")

	answered := callAsync(session, "Hello")
	<-started

	// The run does not end on its own, so a shutdown that waits for it takes the whole timeout.
	begin := time.Now()
	require.NoError(t, app.ShutdownWithTimeout(10*time.Second))
	require.Less(t, time.Since(begin), 5*time.Second, "the shutdown waited for the run")

	require.ErrorIs(t, <-canceled, ai.ErrRunCancelled)
	require.ErrorIs(t, <-ends, ai.ErrRunCancelled)
	got := <-answered
	require.NoError(t, got.err, "the client gets an answer, not a broken connection")
	require.True(t, got.result.IsError)
	require.Contains(t, resultText(t, got.result), "the server is shutting down")
}

// Both options watch the same run: the timeout must not hide the shutdown.
func Test_New_CancelOnShutdownWithATimeout(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	agent := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	app := newApp(pydanticaimcp.New(agent, pydanticaimcp.Config[struct{}]{
		CancelOnShutdown: true,
		Timeout:          time.Minute,
	}))
	session := connect(t, serve(t, app), "")

	answered := callAsync(session, "Hello")
	<-started
	begin := time.Now()
	require.NoError(t, app.ShutdownWithTimeout(10*time.Second))
	require.Less(t, time.Since(begin), 5*time.Second, "the shutdown waited for the run")
	got := <-answered
	require.NoError(t, got.err)
	require.Contains(t, resultText(t, got.result), "the server is shutting down")
}

func Test_New_ShutdownWaitsForARunByDefault(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	canceled := make(chan struct{}, 1)
	agent := ai.NewAgent[struct{}, string](fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			canceled <- struct{}{}
			return nil, ctx.Err()
		case <-release:
			return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: "late"}}}, nil
		}
	}))
	app := newApp(pydanticaimcp.New(agent))
	session := connect(t, serve(t, app), "")

	answered := callAsync(session, "Hello")
	<-started
	require.ErrorIs(t, app.ShutdownWithTimeout(200*time.Millisecond), context.DeadlineExceeded)
	select {
	case <-canceled:
		t.Fatal("the shutdown canceled the run without CancelOnShutdown")
	default:
	}

	close(release) // the run ends on its own
	got := <-answered
	require.NoError(t, got.err)
	require.Equal(t, "late", resultText(t, got.result))
}

func Test_New_Concurrent(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error {
		c.Locals("user", strings.TrimPrefix(c.Get("Authorization"), "Bearer "))
		return c.Next()
	})
	app.All("/mcp", pydanticaimcp.New(userAgent(), pydanticaimcp.Config[user]{
		Deps: func(c fiber.Ctx) (user, error) { return user{Name: fiber.Locals[string](c, "user")}, nil },
	}))
	endpoint := serve(t, app)

	var wg sync.WaitGroup
	for i := range 16 {
		name := "user-" + string(rune('a'+i))
		wg.Go(func() {
			session := connect(t, endpoint, "Bearer "+name)
			for range 3 {
				result := ask(t, session, "Who am I?")
				require.False(t, result.IsError)
				require.Equal(t, "user "+name, resultText(t, result))
			}
		})
	}
	wg.Wait()
}

// An agent can use the server of another agent as a toolset.
func Test_New_AnotherAgentUsesTheServer(t *testing.T) {
	t.Parallel()

	support := ai.NewAgent[struct{}, string](agenttest.Answering("Your order shipped yesterday."))
	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error {
		if c.Get("Authorization") != "Bearer secret" {
			return fiber.ErrUnauthorized
		}
		return c.Next()
	})
	app.Post("/mcp", pydanticaimcp.New(support))
	endpoint := serve(t, app)

	assistant := ai.NewAgent[struct{}, string](fakes.NewTestModel()) // calls every tool once
	assistant.AddToolset(aimcp.NewHTTPToolset[struct{}](aimcp.HTTPToolsetConfig{
		URL:     endpoint,
		Headers: map[string]string{"Authorization": "Bearer secret"},
	}, aimcp.WithID("support")))

	ctx, cancel := context.WithTimeout(context.Background(), 2*observeTimeout)
	defer cancel()
	result, err := assistant.Run(ctx, "Where is my order?", struct{}{})
	require.NoError(t, err)

	var returned []string
	for _, message := range result.NewMessages() {
		if request, ok := message.(ai.ModelRequest); ok {
			for _, part := range request.Parts {
				if part, ok := part.(ai.ToolReturnPart); ok {
					returned = append(returned, fmt.Sprint(part.Content))
				}
			}
		}
	}
	require.Equal(t, []string{"Your order shipped yesterday."}, returned)
}

// send sends one JSON-RPC message to the route as a plain HTTP client does.
func send(t *testing.T, method, endpoint, message string, headers map[string]string) (*http.Response, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), observeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(message))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response, string(body)
}

// post sends one JSON-RPC message with POST.
func post(t *testing.T, endpoint, message string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	return send(t, http.MethodPost, endpoint, message, headers)
}

// Every answer is one JSON document, never a stream: the route has no
// connection to keep open.
func Test_New_AnswersWithJSON(t *testing.T) {
	t.Parallel()

	endpoint := serve(t, newApp(pydanticaimcp.New(ai.NewAgent[struct{}, string](agenttest.Answering("ok")))))
	for name, test := range map[string]struct {
		message string
		headers map[string]string
	}{
		"server/discover": {discoverMessage, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "server/discover"}},
		"tools/call": {callMessage, map[string]string{
			"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "ask",
		}},
	} {
		response, body := post(t, endpoint, test.message, test.headers)
		require.Equal(t, fiber.StatusOK, response.StatusCode, name)
		require.Equal(t, "application/json", response.Header.Get("Content-Type"), name)
		require.Contains(t, body, `"result"`, name)
		require.NoError(t, json.Unmarshal([]byte(body), new(map[string]any)), "%s: one JSON document", name)
	}
}

// Clients of the protocol versions before 2026-07-28 set a connection up with
// initialize, and expect a server without sessions to answer every message on
// its own.
func Test_New_ClientsOfEarlierVersions(t *testing.T) {
	t.Parallel()

	endpoint := serve(t, newApp(pydanticaimcp.New(ai.NewAgent[struct{}, string](agenttest.Answering("Paris.")))))
	headers := map[string]string{"Mcp-Protocol-Version": "2025-06-18"}

	response, body := post(t, endpoint, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{`+
		`"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"legacy","version":"1"}}}`, nil)
	require.Equal(t, fiber.StatusOK, response.StatusCode, body)
	require.Empty(t, response.Header.Get("Mcp-Session-Id"), "there are no sessions")
	require.Contains(t, body, `"protocolVersion":"2025-06-18"`)
	require.Contains(t, body, `"tools"`, "the server offers tools")
	require.NotContains(t, body, "listChanged\":true", "nothing changes while the server runs")

	response, _ = post(t, endpoint, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, headers)
	require.Equal(t, fiber.StatusAccepted, response.StatusCode)

	response, body = post(t, endpoint, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, headers)
	require.Equal(t, fiber.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"name":"ask"`)

	response, body = post(t, endpoint, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{`+
		`"name":"ask","arguments":{"question":"What is the capital of France?"}}}`, headers)
	require.Equal(t, fiber.StatusOK, response.StatusCode, body)
	require.Equal(t, "application/json", response.Header.Get("Content-Type"))
	require.Contains(t, body, `"text":"Paris."`)
}
