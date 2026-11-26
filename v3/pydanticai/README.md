---
id: pydanticai
---

# Pydantic AI

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*pydanticai*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20pydanticai/badge.svg)

[pydantic-ai-go](https://github.com/Kludex/pydantic-ai-go) support for Fiber. The handlers serve an agent to a chat frontend over the [AG-UI](https://docs.ag-ui.com) or the [Vercel AI SDK](https://ai-sdk.dev) protocol, as a JSON endpoint, or as an [MCP](https://modelcontextprotocol.io) server.

- The dependencies and run options of the agent come from the `fiber.Ctx` of each request, so tools can act on behalf of the authenticated user.
- A streamed run is canceled when its client disconnects or its `Timeout` elapses.
- `OnRunEnd` reports what every run consumed, including runs that failed or lost their client.
- A panic in a tool fails the run and not the process, also when tools run concurrently.

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/pydanticai
go get -u github.com/Kludex/pydantic-ai-go/ai
```

The module needs Fiber v3.7 or later.

## Signatures

```go
pydanticai.AGUI[Deps, Output any](agent *ai.Agent[Deps, Output], config ...pydanticai.AGUIConfig[Deps]) fiber.Handler
pydanticai.Vercel[Deps, Output any](agent *ai.Agent[Deps, Output], config ...pydanticai.VercelConfig[Deps]) fiber.Handler
pydanticai.Run[Deps, Output any](agent *ai.Agent[Deps, Output], config ...pydanticai.RunConfig[Deps, Output]) fiber.Handler
pydanticai.MCP[Deps, Output any](agent *ai.Agent[Deps, Output], config ...pydanticai.MCPConfig[Deps]) fiber.Handler
pydanticai.StatusCode(err error) int
```

`ai` is `github.com/Kludex/pydantic-ai-go/ai`. The type arguments of a handler are inferred from the agent, so `pydanticai.AGUI(agent)` needs none. A config names them: `pydanticai.AGUIConfig[MyDeps]{...}`.

| Handler  | Serves                                                       | Use it for                                              |
|:---------|:-------------------------------------------------------------|:--------------------------------------------------------|
| `AGUI`   | [AG-UI](https://docs.ag-ui.com) events as server-sent events | AG-UI frontends such as CopilotKit                      |
| `Vercel` | the UI message stream of the Vercel AI SDK 5 to 7            | the `useChat` hook of the AI SDK                        |
| `Run`    | one JSON response                                            | server-to-server calls and structured output            |
| `MCP`    | [MCP](https://modelcontextprotocol.io) over Streamable HTTP  | MCP clients, such as desktop assistants or other agents |

The handlers answer `POST` requests, so mount them with `app.Post`.

## Examples

### Quick start

Serve an agent to the `useChat` hook of the AI SDK:

```go
package main

import (
    "log"

    ai "github.com/Kludex/pydantic-ai-go/ai"
    "github.com/Kludex/pydantic-ai-go/ai/models/openai"
    "github.com/gofiber/contrib/v3/pydanticai"
    "github.com/gofiber/fiber/v3"
)

func main() {
    agent := ai.NewAgent[struct{}, string](
        openai.NewModel("gpt-5-mini"),
        ai.WithInstructions("Answer in one sentence."),
    )

    app := fiber.New()
    app.Post("/chat", pydanticai.Vercel(agent))

    log.Fatal(app.Listen(":3000"))
}
```

The model reads its API key from the environment, here `OPENAI_API_KEY`. To set it in code, pass it to the model: `openai.NewModel("gpt-5-mini", openai.WithAPIKey(key))`. See [Model providers](#model-providers) for other providers.

Point the frontend at the route (AI SDK 5):

```ts
const { messages, sendMessage } = useChat({
  transport: new DefaultChatTransport({ api: "http://localhost:3000/chat" }),
});
```

For a newer SDK, set `Adapter: vercel.Config{SDKVersion: 6}` (or `7`). For an AG-UI frontend, mount `pydanticai.AGUI(agent)` in the same way.

### Dependencies and authentication

`Deps` is called with the `fiber.Ctx` of each request and returns what the tools and the instructions of the agent receive as `rc.Deps`. An error rejects the request through the error handler of the app, before its body is read. The handlers do not authenticate requests: do that in a middleware in front of them, or in `Deps`.

```go
type Deps struct {
    UserID string
}

type OrderStatusArgs struct {
    OrderID string `json:"order_id"`
}

agent := ai.NewAgent[Deps, string](openai.NewModel("gpt-5-mini"))
ai.AddTool(agent, "order_status", func(ctx context.Context, rc *ai.RunContext[Deps], args OrderStatusArgs) (string, error) {
    // The user comes from the request, not from the model.
    return orders.Status(ctx, rc.Deps.UserID, args.OrderID)
})

app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    Deps: func(c fiber.Ctx) (Deps, error) {
        user, ok := c.Locals("user").(string) // set by your authentication middleware
        if !ok {
            return Deps{}, fiber.ErrUnauthorized
        }
        return Deps{UserID: user}, nil
    },
}))
```

The `ctx` of a tool carries the values of `c.Context()`, such as the span of the [otel](https://github.com/gofiber/contrib/tree/main/v3/otel) middleware, and ends with the run. Pass it to database and HTTP calls, so that they stop when the client disconnects. What `Deps` read from `c` stays valid for the whole run. Do not use `c` itself after the run.

### Run options

`RunOptions` returns `ai.RunOption` values for one request, for example usage limits for the caller, or another model with `ai.WithRunModel`:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    RunOptions: func(c fiber.Ctx) ([]ai.RunOption, error) {
        options := []ai.RunOption{
            ai.WithRunUsageLimits(ai.UsageLimits{RequestLimit: 10, TotalTokenLimit: 50_000}),
        }
        if fiber.Locals[string](c, "plan") == "pro" {
            options = append(options, ai.WithRunModel(openai.NewModel("gpt-5")))
        }
        return options, nil
    },
}))
```

A user who brings their own key gets a model with that key: `ai.WithRunModel(openai.NewModel("gpt-5-mini", openai.WithAPIKey(key)))`. `AGUI` and `Vercel` set the message history and the conversation ID themselves, so a run fails when `RunOptions` returns `ai.WithConversation`.

### Errors, logging and usage

`ErrorMessage` decides what the client reads when a run fails, and `OnRunEnd` and `OnClose` are where you log:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    ErrorMessage: func(err error) string { return "The assistant is not available." },
    OnRunEnd: func(deps Deps, run *ai.RunInfo, err error) {
        if run == nil {
            return // the agent did not start the run
        }
        usage := run.Usage()
        log.Printf("%s used %d input and %d output tokens (%v)", deps.UserID, usage.InputTokens, usage.OutputTokens, err)
    },
    OnClose: func(deps Deps, err error) {
        if err != nil {
            log.Printf("chat stream of %s ended: %v", deps.UserID, err)
        }
    },
}))
```

- **What the client reads.** A failed run is reported inside the stream. By default the client reads the kind of failure and none of its details, because the error of a provider or a tool can name a host or a part of a credential: the message is the reason phrase of the status in the table below. Return `err.Error()` from `ErrorMessage` to see the error in the browser during development.
- **What you log.** `OnRunEnd` is called once for every run, also for one that failed, timed out or lost its client, with what the run consumed. `OnClose` is called once when the stream is over, with `nil` for a normal end. Both receive the error itself. The end of the response waits for them, so keep them short. A request that the handler rejects before the run, such as one whose body does not decode, gets neither, although `Deps` ran for it: count requests in a middleware.
- **Tool results.** What a tool returns to the model is part of both protocols and reaches the client as it is, whatever `ErrorMessage` says.
- **Panics.** A panic in a tool is logged with its stack and fails the run, and the client reads `Internal Server Error`. `AGUI`, `Vercel` and `MCP` treat a panic in the model the same way, and recover one in `OnRunEnd`, `OnClose` or `ErrorMessage`. Code that pydantic-ai-go runs for you outside a tool call, such as an event listener of the agent, is not covered.

`StatusCode` maps the error of a run to a status. A `*fiber.Error` in the chain keeps its own status.

| Error                                                                              | Status                      |
|:-----------------------------------------------------------------------------------|:----------------------------|
| `ai.ErrUsageLimitExceeded`                                                         | `429 Too Many Requests`     |
| `context.DeadlineExceeded`, such as an elapsed `Timeout`                           | `504 Gateway Timeout`       |
| `ai.ErrRunCancelled`, `context.Canceled`                                           | `503 Service Unavailable`   |
| `*ai.ContentFilterError`                                                           | `422 Unprocessable Entity`  |
| `*ai.UnexpectedModelBehaviorError`, `ai.ErrMaxRetriesExceeded`, `ai.ModelAPIError` | `502 Bad Gateway`           |
| anything else, including a panic                                                   | `500 Internal Server Error` |

### JSON endpoint

`Run` answers one request with the output of one run:

```go
type Weather struct {
    City  string `json:"city"`
    TempC int    `json:"temp_c"`
}

agent := ai.NewAgent[struct{}, Weather](openai.NewModel("gpt-5-mini"))

app.Post("/weather", pydanticai.Run(agent))
```

```console
$ curl localhost:3000/weather -H 'Content-Type: application/json' \
    -d '{"prompt": "What is the weather in Paris?", "conversation_id": "trip-planning"}'
{"output":{"city":"Paris","temp_c":21},"run_id":"52bdcf0873a4f6b3","conversation_id":"trip-planning"}
```

`conversation_id` is optional. `Prompt` reads the prompt from somewhere other than the JSON body, and `Respond` writes another response:

```go
app.Post("/weather-of-city", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, Weather]{
    Prompt: func(c fiber.Ctx) (string, error) {
        return "What is the weather in " + c.Query("city") + "?", nil
    },
    Respond: func(c fiber.Ctx, result *ai.RunResult[Weather]) error {
        return c.JSON(fiber.Map{
            "weather":      result.Output,
            "total_tokens": result.Usage().InputTokens + result.Usage().OutputTokens,
        })
    },
}))
```

With `Prompt`, the handler applies none of its [request checks](#requests): mount such a route with `app.Post` and protect it against cross-site requests yourself.

A failed run returns a `*pydanticai.Error` to the error handler of the app, with the status that `StatusCode` derives. Its message is the reason phrase of that status, so the details stay on the server. `Err` holds the cause:

```go
app := fiber.New(fiber.Config{
    ErrorHandler: func(c fiber.Ctx, err error) error {
        if runErr, ok := errors.AsType[*pydanticai.Error](err); ok {
            log.Printf("agent failed: %v", runErr.Err) // never sent to the client
        }
        return fiber.DefaultErrorHandler(c, err)
    },
})
```

`Run` does not recover a panic in the model: it goes on to the [recover](https://docs.gofiber.io/next/middleware/recover) middleware of the app. A run that ends with deferred tool calls, such as a tool that needs approval, gets `501`: serve such an agent with `AGUI` or `Vercel`, or handle `result.Deferred()` in `Respond`.

### Testing

pydantic-ai-go ships fake models, so the handlers can be tested without a network or an API key. The `pydanticaitest` package decodes the streams of `AGUI` and `Vercel`:

```go
func TestChat(t *testing.T) {
    agent := ai.NewAgent[struct{}, string](fakes.NewTestModel()) // github.com/Kludex/pydantic-ai-go/ai/models/fakes

    app := fiber.New()
    app.Post("/chat", pydanticai.Vercel(agent))

    body := `{"trigger":"submit-message","id":"chat-1","messages":[{"id":"1","role":"user","parts":[{"type":"text","text":"Hello"}]}]}`
    req := httptest.NewRequest(fiber.MethodPost, "/chat", strings.NewReader(body))
    req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

    resp, err := app.Test(req)
    require.NoError(t, err)

    chunks, done := pydanticaitest.VercelChunks(t, resp.Body) // github.com/gofiber/contrib/v3/pydanticai/pydanticaitest
    require.True(t, done)
    require.Equal(t, "success (no tool calls)", pydanticaitest.VercelText(chunks))
}
```

`AGUIEvents` and `AGUIText` do the same for AG-UI, and `NewReader` reads a stream record by record while the handler still writes it. `fakes.NewFunctionModel` is a model that answers what a test needs.

## MCP

Agents use [MCP](https://modelcontextprotocol.io) servers as tools through `github.com/Kludex/pydantic-ai-go/ai/mcp` (`aimcp` below), and the handlers stream the calls of such tools like any other tool call. The [MCP guide](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/mcp.md) of pydantic-ai-go covers what is not specific to Fiber: provider-hosted servers, `.mcp.json` files, MCP Apps and OAuth.

### Tools from an MCP server

```go
agent := ai.NewAgent[Deps, string](openai.NewModel("gpt-5-mini"))
agent.AddToolset(aimcp.NewHTTPToolset[Deps](
    aimcp.HTTPToolsetConfig{URL: "https://mcp.example.com/mcp"},
    aimcp.WithID("catalog"),
))

app.Post("/chat", pydanticai.Vercel(agent))
```

Each run opens a session of its own and closes it when the run ends, so concurrent requests share neither state nor credentials.

### Tools on behalf of the user

The headers of a toolset are fixed when it is created. To send the credentials of the caller, create the toolset for the request in `RunOptions`:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    Deps: depsOfUser,
    RunOptions: func(c fiber.Ctx) ([]ai.RunOption, error) {
        token, ok := c.Locals("mcp-token").(string) // set by your authentication middleware
        if !ok {
            return nil, fiber.ErrUnauthorized
        }
        catalog := aimcp.NewHTTPToolset[Deps](aimcp.HTTPToolsetConfig{
            URL:     "https://mcp.example.com/mcp",
            Headers: map[string]string{"Authorization": "Bearer " + token},
        }, aimcp.WithID("catalog"))
        return []ai.RunOption{ai.WithRunToolsets(catalog)}, nil
    },
}))
```

Creating a toolset is cheap: it connects when the run starts.

### A session that all requests share

Share one session when connecting is expensive or the server keeps state. The session is yours to close, after the last run that uses it has ended: `CancelOnShutdown` makes a shutdown stop the runs first.

```go
session, err := aimcp.Connect(context.Background(),
    &mcpsdk.StreamableClientTransport{Endpoint: "https://mcp.example.com/mcp"},
    aimcp.WithID("catalog"),
)
if err != nil {
    log.Fatal(err)
}
agent.AddToolset(aimcp.NewSessionToolset[Deps](session))

app.Hooks().OnPostShutdown(func(error) error { return session.Close() })
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{CancelOnShutdown: true}))
```

`mcpsdk` is `github.com/modelcontextprotocol/go-sdk/mcp`. Runs on a shared session are not isolated from each other, so use one for servers that hold nothing of a user.

### Serving an agent as an MCP server

`pydanticai.MCP` serves an agent to MCP clients over Streamable HTTP:

```go
app.Use("/mcp", authenticate) // your authentication middleware, which stores the user in c.Locals
app.Post("/mcp", pydanticai.MCP(agent, pydanticai.MCPConfig[Deps]{
    Deps: func(c fiber.Ctx) (Deps, error) {
        return Deps{UserID: fiber.Locals[string](c, "user")}, nil
    },
    Description: "Ask the support agent about orders and refunds.",
    Timeout:     2 * time.Minute,
}))
```

- **The tool.** The client sees one tool, `ask`, with the argument `question`. Its result is the output of the agent as text, and as structured content too when the output is a JSON object. `Tool` and `Description` name the tool and tell the model of the client what it is for.
- **Callers.** Derive `Deps` from the authenticated request, never from the arguments of the tool. `Deps` is called for every message of a client, also for those that only list the tool, so keep it cheap.
- **Browsers.** A request with an `Origin` header gets `403` unless `AllowedOrigins` lists that origin. A client that is not a browser sends none.
- **Messages.** The server keeps no session and answers every message with one JSON response, so it cannot send progress, sampling or elicitation requests. A JSON-RPC batch gets `400`, and a message of more than 4 MiB `413`.
- **Errors.** A failed run is an error of the tool, with the text described under [Errors, logging and usage](#errors-logging-and-usage).

## Model providers

The handlers work with every model of pydantic-ai-go: the provider is chosen by the model that you pass to `ai.NewAgent`. Each provider is a package under `github.com/Kludex/pydantic-ai-go/ai/models/`, among them OpenAI, Azure OpenAI, Anthropic, Google Gemini and Vertex AI, Amazon Bedrock, Mistral, Groq, DeepSeek, xAI, OpenRouter, Ollama and vLLM. A model reads its credentials from the environment, or takes them as options:

```go
// name is the name of the model at the provider.
hosted := anthropic.NewModel(name, anthropic.WithAPIKey(apiKey))

// Any OpenAI-compatible service.
gateway := openai.NewModel("my-model", openai.WithProvider(openai.ProviderConfig{
    Name:    "my-gateway",
    BaseURL: "https://gateway.example.com/v1",
    APIKey:  apiKey,
}))

// The provider from configuration, for example MODEL=openai:gpt-5-mini.
// Package github.com/Kludex/pydantic-ai-go/ai/models/infer.
configured, err := infer.Model(os.Getenv("MODEL"))
```

[Provider configuration](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/providers.md) in the documentation of pydantic-ai-go lists every provider with its environment variables and options.

## Config

### AGUIConfig and VercelConfig

Both are forms of one type, `StreamConfig`, whose `Adapter` is an `agui.Config` or a `vercel.Config`.

| Property          | Type                                      | Description                                                                                                  | Default                                |
|:------------------|:------------------------------------------|:-------------------------------------------------------------------------------------------------------------|:---------------------------------------|
| Next              | `func(fiber.Ctx) bool`                    | Skips the handler when it returns `true`.                                                                    | `nil`                                  |
| Deps              | `func(fiber.Ctx) (Deps, error)`           | Returns the dependencies of the agent for one request. An error rejects the request before its body is read. | zero value of `Deps`                   |
| RunOptions        | `func(fiber.Ctx) ([]ai.RunOption, error)` | Returns extra run options for one request.                                                                   | `nil`                                  |
| ErrorMessage      | `func(error) string`                      | Returns what the client reads for the error of a failed run.                                                 | the reason phrase of `StatusCode(err)` |
| OnClose           | `func(Deps, error)`                       | Called once when the stream is over. The error is `nil` for a normal end.                                    | `nil`                                  |
| OnRunEnd          | `func(Deps, *ai.RunInfo, error)`          | Called once when the run has ended, before `OnClose`, with what the run consumed.                            | `nil`                                  |
| Adapter           | `agui.Config` or `vercel.Config`          | Settings of the protocol: its version, the request limits and the sanitization of the history of the client. | the zero value                         |
| Timeout           | `time.Duration`                           | Ends a run that takes longer.                                                                                | `0` (no timeout)                       |
| HeartbeatInterval | `time.Duration`                           | Interval of the comment lines that keep a stream open and reveal a client that left.                         | `5 * time.Second`                      |
| DisableHeartbeat  | `bool`                                    | Turns the heartbeat off.                                                                                     | `false`                                |
| CancelOnShutdown  | `bool`                                    | Cancels the run when the server starts shutting down, instead of waiting for it.                             | `false`                                |

### RunConfig

| Property        | Type                                           | Description                                                                              | Default                                        |
|:----------------|:-----------------------------------------------|:-----------------------------------------------------------------------------------------|:-----------------------------------------------|
| Next            | `func(fiber.Ctx) bool`                         | Skips the handler when it returns `true`.                                                | `nil`                                          |
| Deps            | `func(fiber.Ctx) (Deps, error)`                | Returns the dependencies of the agent for one request. An error is returned as it is.    | zero value of `Deps`                           |
| RunOptions      | `func(fiber.Ctx) ([]ai.RunOption, error)`      | Returns extra run options for one request.                                               | `nil`                                          |
| Prompt          | `func(fiber.Ctx) (string, error)`              | Reads the prompt from the request. None of the request checks applies with it.           | the `prompt` field of the JSON body            |
| Respond         | `func(fiber.Ctx, *ai.RunResult[Output]) error` | Writes the response for a completed run.                                                 | JSON `{"output", "run_id", "conversation_id"}` |
| OnRunEnd        | `func(Deps, *ai.RunInfo, error)`               | Called once the agent has run, with what the run consumed and the error that ended it.   | `nil`                                          |
| MaxRequestBytes | `int64`                                        | Limits the body that the default `Prompt` reads.                                         | 10 MiB                                         |
| Timeout         | `time.Duration`                                | Ends a run that takes longer, with `504`. A negative value turns it off.                 | `5 * time.Minute`                              |

### MCPConfig

| Property          | Type                                      | Description                                                                                                                                   | Default                                                |
|:------------------|:------------------------------------------|:----------------------------------------------------------------------------------------------------------------------------------------------|:-------------------------------------------------------|
| Next              | `func(fiber.Ctx) bool`                    | Skips the handler when it returns `true`.                                                                                                     | `nil`                                                  |
| Deps              | `func(fiber.Ctx) (Deps, error)`           | Returns the dependencies of the agent for one request. Called for every message of a client.                                                  | zero value of `Deps`                                   |
| RunOptions        | `func(fiber.Ctx) ([]ai.RunOption, error)` | Returns extra run options for one request.                                                                                                    | `nil`                                                  |
| AllowedOrigins    | `[]string`                                | The origins of the browser pages that may call the handler, such as `https://app.example.com`. `MCP` panics on one that is not an origin.     | `nil` (no page)                                        |
| Name, Version     | `string`                                  | Identify the server to its clients.                                                                                                           | the name of the agent or `pydantic-ai-go`, and `1.0.0` |
| Tool, Description | `string`                                  | The name of the tool that runs the agent (letters, digits, `_`, `.` and `-`), and what it is for.                                             | `ask`, `Ask the agent a question`                      |
| ErrorMessage      | `func(error) string`                      | Returns what the client reads for the error of a failed run.                                                                                  | the reason phrase of `StatusCode(err)`                 |
| OnRunEnd          | `func(Deps, *ai.RunInfo, error)`          | Called once the agent has run, with what the run consumed and the error that ended it.                                                        | `nil`                                                  |
| Timeout           | `time.Duration`                           | Ends a run that takes longer. A negative value turns it off.                                                                                  | `5 * time.Minute`                                      |
| CancelOnShutdown  | `bool`                                    | Cancels the runs when the server starts shutting down, instead of waiting for them.                                                           | `false`                                                |

## Timeouts, disconnects and shutdown

- **Disconnects.** While a stream is open, `AGUI` and `Vercel` write a comment line every `HeartbeatInterval`. A write that fails cancels the run, so a client that closed its connection is noticed within a few heartbeats.
- **Timeout.** `Timeout` ends a run that takes longer. `AGUI` and `Vercel` have none by default: set one to bound a run whose client keeps its connection open without reading, which nothing else notices. `Run` and `MCP` cannot notice a client that left at all, so their default is 5 minutes.
- **Shutdown.** A shutdown waits for the runs in flight. With `CancelOnShutdown`, `AGUI`, `Vercel` and `MCP` cancel them when the server starts shutting down, and `OnRunEnd` and `OnClose` receive an error that wraps `ai.ErrRunCancelled`.
- **`WriteTimeout`.** `fiber.Config.WriteTimeout` applies to a whole response and cuts every stream that takes longer. Leave it at zero, or set it above the longest run that you accept.

How a stream ends:

| Event                                                               | AG-UI                             | Vercel AI                     |
|:--------------------------------------------------------------------|:----------------------------------|:------------------------------|
| The run completes                                                   | `RUN_FINISHED`                    | `finish` chunk, then `[DONE]` |
| `Timeout` elapses, or the server shuts down with `CancelOnShutdown` | `RUN_FINISHED` without an outcome | `abort` chunk, then `[DONE]`  |
| The run fails or panics                                             | `RUN_ERROR`                       | `error` chunk, then `[DONE]`  |

The status of the response is `200` in all of these cases, because it is sent before the run starts. When the client disconnects, the run is canceled and nothing more is sent.

## Middleware

The handlers neither authenticate nor limit requests, and they do not answer cross-origin requests. Fiber's middleware cover that:

```go
app.Use(cors.New(cors.Config{ // when the frontend is served from another origin
    AllowOrigins: []string{"https://app.example.com"},
    AllowHeaders: []string{"Authorization", "Content-Type"},
}))
app.Use(authenticate) // your authentication middleware, which stores the user in c.Locals
app.Use(limiter.New(limiter.Config{
    Max:          20,
    Expiration:   time.Minute,
    KeyGenerator: func(c fiber.Ctx) string { return fiber.Locals[string](c, "user") },
}))
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{Deps: depsOfUser}))
```

- **`cors`** comes first, because a preflight request carries no credentials.
- **`timeout`** cancels a run of `Run`. It does not stop a stream or an `MCP` call that has started: their `Timeout` does. A request that it gave up on before the run started is not run.
- **`compress`** works in front of the streaming handlers: each event still reaches the client as it is written.
- **[`hostauthorization`](https://docs.gofiber.io/next/middleware/hostauthorization)** belongs in front of `AGUI`, `Vercel` and `Run` on a server that browsers can reach and that authenticates nobody, such as one on the machine of a developer. It turns away a page of another site that reaches the server by DNS rebinding, which `cors` does not. `MCP` checks the `Origin` header itself.

## Requests

`AGUI`, `Vercel` and `Run` check a request before the agent runs. A rejected request goes to the error handler of the app as a `*fiber.Error`:

| Status | Reason                                                                                                                                                   |
|:-------|:---------------------------------------------------------------------------------------------------------------------------------------------------------|
| `405`  | The method is not `POST`.                                                                                                                                |
| `415`  | The content type is not `application/json`, or the body has a `Content-Encoding` other than `identity`: bodies are not decompressed.                     |
| `413`  | The body is larger than `MaxRequestBytes`, 10 MiB by default.                                                                                            |
| `400`  | The body does not decode into the request of the protocol, a Vercel AI request has no chat `id`, or the `prompt` of a `Run` request is missing or blank. |

- **Body limit.** Fiber's `BodyLimit`, 4 MiB by default, applies as well: raise it to accept larger bodies, such as inline images. `AGUI` and `Vercel` release a body of more than 64 KiB once it is decoded, so a middleware that runs after them sees an empty body.
- **Cross-site requests.** The content type check keeps cross-site form posts from reaching the agent. Turn it off with `DisableContentTypeCheck` of the adapter config only behind another CSRF protection.
- **History.** A chat frontend sends the whole conversation with each request, so the history is untrusted input. The adapters sanitize it, for example by removing system prompts, and `Sanitization` of the adapter config relaxes that. See the [AG-UI](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/ag-ui.md) and [Vercel AI](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/vercel-ai.md) pages of pydantic-ai-go for what the adapters support.

## See also

pydantic-ai-go also serves agents over [A2A](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/a2a.md), as a `net/http` handler: mount it with the [`adaptor`](https://docs.gofiber.io/next/middleware/adaptor) middleware of Fiber.
