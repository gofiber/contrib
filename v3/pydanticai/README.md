---
id: pydanticai
---

# Pydantic AI

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*pydanticai*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20pydanticai/badge.svg)

[pydantic-ai-go](https://github.com/Kludex/pydantic-ai-go) support for Fiber. The handlers serve an agent to a chat frontend over the [AG-UI](https://docs.ag-ui.com) or the [Vercel AI SDK](https://ai-sdk.dev) protocol, or answer a JSON request with the output of one run.

- The dependencies and run options of the agent are built from the `fiber.Ctx` of each request, so tools can act on behalf of the authenticated user.
- A run is canceled when its client disconnects or its `Timeout` elapses, and optionally when the server shuts down.
- `OnRunEnd` reports what every run consumed, including runs that failed or lost their client.
- Rejected requests and failed `Run` calls go through the error handler of the app.
- A panic in a tool ends the stream with an error event, also when the tool runs concurrently with other tool calls.
- Agents use [MCP](#mcp) servers as tools, with the credentials of the caller, and `pydanticaimcp` serves an agent as an MCP server.

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/pydanticai
go get -u github.com/Kludex/pydantic-ai-go/ai
```

## Signatures

```go
pydanticai.AGUI[Deps, Output any](agent *ai.Agent[Deps, Output], config ...pydanticai.AGUIConfig[Deps]) fiber.Handler
pydanticai.Vercel[Deps, Output any](agent *ai.Agent[Deps, Output], config ...pydanticai.VercelConfig[Deps]) fiber.Handler
pydanticai.Run[Deps, Output any](agent *ai.Agent[Deps, Output], config ...pydanticai.RunConfig[Deps, Output]) fiber.Handler
pydanticai.StatusCode(err error) int
```

`ai` is `github.com/Kludex/pydantic-ai-go/ai`. The type arguments of a handler are inferred from the agent, so `pydanticai.AGUI(agent)` needs none. A config does: `pydanticai.AGUIConfig[MyDeps]{...}`.

| Handler  | Protocol                                                        | Use it for                                      |
|:---------|:----------------------------------------------------------------|:------------------------------------------------|
| `AGUI`   | [AG-UI](https://docs.ag-ui.com) events as server-sent events    | AG-UI frontends such as CopilotKit              |
| `Vercel` | Vercel AI SDK UI message stream (AI SDK 5 to 7)                 | the `useChat` hook of the AI SDK                |
| `Run`    | JSON request and response                                       | server-to-server calls and structured output    |

All three handlers accept `POST` requests only.

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

The OpenAI model reads its API key from the `OPENAI_API_KEY` environment variable. To set the key in code instead, pass it as an option of the model:

```go
model := openai.NewModel("gpt-5-mini", openai.WithAPIKey(apiKey)) // from your configuration or secret store
agent := ai.NewAgent[struct{}, string](model)
```

The key belongs to the model, not to the handler. [Model providers](#model-providers) lists every provider and how to set its credentials.

Point the frontend at the route (AI SDK 5):

```ts
const { messages, sendMessage } = useChat({
  transport: new DefaultChatTransport({ api: "http://localhost:3000/chat" }),
});
```

The Vercel handler speaks AI SDK 5 by default. Set `Adapter: vercel.Config{SDKVersion: 6}` (or `7`) for a newer SDK. The request body must carry the `id` of the chat, which `useChat` always sends. To serve an AG-UI frontend, mount `pydanticai.AGUI(agent)` on a route in the same way.

### Authenticated users and tools

`Deps` is called with the `fiber.Ctx` of each request and returns the dependencies that the tools and the instructions of the agent receive as `rc.Deps`. Returning an error rejects the request before the stream starts, and the error handler of the app turns it into the response, here a `401`. The handlers do not authenticate requests: do that in a middleware in front of them, or in `Deps`.

```go
package main

import (
    "context"
    "log"

    ai "github.com/Kludex/pydantic-ai-go/ai"
    "github.com/Kludex/pydantic-ai-go/ai/models/openai"
    "github.com/gofiber/contrib/v3/pydanticai"
    "github.com/gofiber/fiber/v3"
)

// OrderStore is whatever your application uses to look up orders.
type OrderStore interface {
    Status(ctx context.Context, userID, orderID string) (string, error)
}

// Deps is what the tools of the agent receive for one request.
type Deps struct {
    UserID string
    Orders OrderStore
}

type OrderStatusArgs struct {
    OrderID string `json:"order_id"`
}

func main() {
    var orders OrderStore // your implementation

    agent := ai.NewAgent[Deps, string](openai.NewModel("gpt-5-mini"))
    ai.AddTool(agent, "order_status", func(
        ctx context.Context, rc *ai.RunContext[Deps], args OrderStatusArgs,
    ) (string, error) {
        // ctx ends with the run, so the lookup stops when the client disconnects.
        return rc.Deps.Orders.Status(ctx, rc.Deps.UserID, args.OrderID)
    })

    app := fiber.New()
    app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
        Deps: func(c fiber.Ctx) (Deps, error) {
            user, ok := c.Locals("user").(string) // set by your authentication middleware
            if !ok {
                return Deps{}, fiber.ErrUnauthorized
            }
            return Deps{UserID: user, Orders: orders}, nil
        },
    }))

    log.Fatal(app.Listen(":3000"))
}
```

`Deps` runs after the headers of the request are checked and before its body is read, so a caller that is turned away does not get its payload parsed.

What a tool can rely on:

- **`rc.Deps`** is the value that `Deps` returned for this request. Nothing is shared between requests.
- **`ctx`** carries the values of `c.Context()`, for example the span of the [otel](https://github.com/gofiber/contrib/tree/main/v3/otel) middleware, so spans that a tool starts become children of the span of the request. It is canceled when the client disconnects, when `Timeout` elapses and when the stream ends. Pass it to database and HTTP calls so that they stop with the run.
- **Request data stays valid.** Strings that `Deps` reads from `c`, and values that tools read from `ctx`, remain valid for the whole run, including after the client has disconnected. Do not use `c` once the run has ended, and copy what a goroutine that outlives the run needs.
- **A tool that ignores `ctx`** keeps running after the client has left, and the request stays allocated until the tool returns. Watch `ctx.Done()` in tools that take long.

### Run options

`RunOptions` returns extra `ai.RunOption` values for each request, for example usage limits per caller:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    RunOptions: func(c fiber.Ctx) ([]ai.RunOption, error) {
        return []ai.RunOption{
            ai.WithRunUsageLimits(ai.UsageLimits{RequestLimit: 10, TotalTokenLimit: 50_000}),
            ai.WithRunMetadata(map[string]any{"ip": c.IP()}),
        }, nil
    },
}))
```

The adapters set the message history and the conversation ID of a run themselves, whatever `RunOptions` returns. A run therefore fails when `RunOptions` returns `ai.WithConversation`, which pydantic-ai-go does not combine with either.

### Choosing the model per request

`ai.WithRunModel` replaces the model of the agent for one run, so the model, or the API key of the model, can depend on the request, for example on the plan of the user:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    RunOptions: func(c fiber.Ctx) ([]ai.RunOption, error) {
        if fiber.Locals[string](c, "plan") == "pro" {
            return []ai.RunOption{ai.WithRunModel(openai.NewModel("gpt-5"))}, nil
        }
        return nil, nil // the model of the agent
    },
}))
```

A user who brings their own key gets a model with that key: `ai.WithRunModel(openai.NewModel("gpt-5-mini", openai.WithAPIKey(key)))`, with `key` read from the request in `RunOptions`.

### Hiding error details and logging

A run that fails after the stream has started is reported to the client inside the stream, with `err.Error()` as the message. Provider errors can contain request IDs or parts of credentials. `ErrorMessage` replaces the message that the client receives, and `OnClose` still gets the original error for your logs:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    ErrorMessage: func(err error) string { return "The assistant is not available." },
    OnClose: func(deps Deps, err error) {
        if err != nil {
            log.Printf("chat stream of %s ended: %v", deps.UserID, err)
        }
    },
}))
```

`OnClose` is called once when the stream is over, with the dependencies of the run and the way the stream ended:

- `nil` when it ended normally, which includes a run that a tool canceled.
- The error of the agent when the run failed.
- The context error when `Timeout` elapsed.
- The write error when the client disconnected.

The end of the response waits for `OnClose`, so keep it short, or start a goroutine and copy what it needs. A panic in `OnClose` is logged and recovered.

### Recording usage

`OnRunEnd` is called once for every run that the handler started, with the dependencies of the run, the run itself and the error that ended it. Unlike `ai.AfterRunFunc` of the agent, which is only called for runs that complete, it also reports runs that failed, timed out or lost their client, and those still cost tokens:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    OnRunEnd: func(deps Deps, run *ai.RunInfo, err error) {
        if run == nil {
            return // the agent did not start
        }
        usage := run.Usage()
        // Record it in your accounting.
        log.Printf("run %s of %s used %d input and %d output tokens (%v)",
            run.RunID, deps.UserID, usage.InputTokens, usage.OutputTokens, err)
    },
}))
```

`run` is an `ai.RunInfo`: besides the usage it gives the messages of the conversation, the IDs of the run and its metadata. It is nil when the agent did not start the run: when the adapter rejected the history of the request, or when a run wrapper of the agent answered without starting it. Such a run used no tokens. `OnRunEnd` runs before `OnClose` and under the same conditions, so keep it short as well. `RunConfig` has an `OnRunEnd` too, which is called once the agent has run, also when the run panicked.

### JSON endpoint

`Run` answers one request with the output of one run:

```go
type Weather struct {
    City  string `json:"city"`
    TempC int    `json:"temp_c"`
}

agent := ai.NewAgent[struct{}, Weather](openai.NewModel("gpt-5-mini"))

app.Post("/weather", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, Weather]{
    Timeout: 30 * time.Second,
}))
```

```console
$ curl localhost:3000/weather -H 'Content-Type: application/json' \
    -d '{"prompt": "What is the weather in Paris?", "conversation_id": "trip-planning"}'
{"output":{"city":"Paris","temp_c":21},"run_id":"52bdcf0873a4f6b3","conversation_id":"trip-planning"}
```

`conversation_id` is optional. Without it, the agent generates the ID of a new conversation.

The history of the conversation, the usage and the cost of the run stay on the server. `Respond` writes a different response, `Prompt` reads the prompt from somewhere other than the JSON body, and `RunOptions` with `ai.WithMessageHistory` continues a conversation that you store yourself:

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

The `conversation_id` of the default request body is passed to the run ahead of the options of `RunOptions`, so an `ai.WithConversationID` that they return wins over it. `ai.WithConversation` cannot be combined with it: pydantic-ai-go rejects the run, and the handler answers `500`, when the body carries a `conversation_id`.

The default request is a `POST` with an `application/json` body of at most `MaxRequestBytes`. A `Prompt` function owns the request instead: none of these checks applies to it, so the route needs its own protection against cross-site requests. Register it with `app.Post` as well: Fiber answers `HEAD` on every `GET` route with the same handler, and browsers, crawlers and link previews send `GET` requests on their own, each of which would run the agent.

A run that ends with deferred tool calls, which are tools that need approval or an external result, cannot be completed by one request and response. The default `Respond` answers `501 Not Implemented` for it. Serve such an agent with `AGUI` or `Vercel`, which resume deferred calls, or handle `result.Deferred()` in your own `Respond`.

### Testing without a provider

pydantic-ai-go ships fake models, so the handlers can be tested without a network or an API key. The `pydanticaitest` package decodes the streams that `AGUI` and `Vercel` write, so that a test can assert on the text and the events of a run:

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

`fakes.NewTestModel` answers `success (no tool calls)`. Use `fakes.NewFunctionModel` for a model that answers what the test needs.

| Function                                  | Reads                                                                                              |
|:------------------------------------------|:---------------------------------------------------------------------------------------------------|
| `AGUIEvents(t, body)` and `AGUIText`      | The events of an AG-UI response, and the text of its messages.                                     |
| `VercelChunks(t, body)` and `VercelText`  | The chunks of a Vercel AI response, whether it ended with `[DONE]`, and the text of its messages.  |
| `NewReader(stream)`                       | The records of a stream one at a time, with `Next` and `NextData`, while the handler still writes it. A test that talks to a real server uses it to look at the first events of a run before it ends. |

The functions fail the test when the body cannot be read or decoded.

## Model providers

The handlers work with any model of pydantic-ai-go; the provider is chosen by the model that you pass to `ai.NewAgent`. Each provider is a package under `github.com/Kludex/pydantic-ai-go/ai/models/`. A model reads its credentials from the environment by default, and the options of its package set them in code.

| Provider | Constructor | Environment variables | Credentials in code |
|:---------|:------------|:----------------------|:--------------------|
| OpenAI Chat Completions | `openai.NewModel("gpt-5-mini")` | `OPENAI_API_KEY`, `OPENAI_BASE_URL` | `openai.WithAPIKey(key)` |
| OpenAI Responses | `openai.NewResponsesModel("gpt-5-mini")` | `OPENAI_API_KEY`, `OPENAI_BASE_URL` | `openai.WithAPIKey(key)` |
| OpenAI Codex subscription | `openaicodex.NewModel("gpt-5.6-luna")` † | the login of the Codex CLI (`CODEX_HOME`) | `openaicodex.WithCredentials(...)` or `openaicodex.WithCredentialSource(...)` |
| OpenAI Decisions | `openaidecisions.NewModel("gpt-6-luna")` | `OPENAI_API_KEY`, `OPENAI_BASE_URL` | `openaidecisions.WithAPIKey(key)` |
| Azure OpenAI | `azure.NewModel("deployment", azure.Config{})` † | `AZURE_OPENAI_ENDPOINT`, `AZURE_OPENAI_API_KEY`, `OPENAI_API_VERSION` | `APIKey` or `TokenProvider` of `azure.Config` |
| Amazon Bedrock | `bedrock.NewModel("us.amazon.nova-lite-v1:0")` | the standard AWS SDK configuration | `bedrock.WithAWSConfig(cfg)` |
| Amazon Bedrock Mantle | `bedrockmantle.NewModel("openai.gpt-5.6-luna")` | `AWS_BEARER_TOKEN_BEDROCK`, or the standard AWS SDK configuration | `bedrockmantle.WithAPIKey(token)` or `bedrockmantle.WithAWSConfig(cfg)` |
| Anthropic | `anthropic.NewModel("claude-sonnet-4-5")` | `ANTHROPIC_API_KEY` | `anthropic.WithAPIKey(key)` |
| Google Gemini | `google.NewModel("gemini-2.5-flash")` | `GOOGLE_API_KEY` or `GEMINI_API_KEY` | `google.WithAPIKey(key)` |
| Google Vertex AI | `google.NewVertexModel("gemini-2.5-flash", google.VertexConfig{})` † | `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`, `GOOGLE_APPLICATION_CREDENTIALS` | `APIKey` or `TokenProvider` of `google.VertexConfig` |
| DeepSeek Chat Completions | `deepseek.NewModel("deepseek-chat")` | `DEEPSEEK_API_KEY` | `deepseek.WithAPIKey(key)` |
| DeepSeek Responses | `deepseek.NewResponsesModel("deepseek-v4-flash")` | `DEEPSEEK_API_KEY` | `deepseek.WithAPIKey(key)` |
| GitHub Copilot | `githubcopilot.NewModel("claude-haiku-4.5")` † | `GITHUB_COPILOT_API_KEY` | `githubcopilot.WithAPIKey(key)` |
| Cohere | `cohere.NewModel("command-r7b-12-2024")` | `CO_API_KEY` | `cohere.WithAPIKey(key)` |
| Crusoe | `crusoe.NewModel("openai/gpt-oss-120b")` | `CRUSOE_API_KEY` | `crusoe.WithAPIKey(key)` |
| Cerebras | `cerebras.NewModel("gpt-oss-120b")` | `CEREBRAS_API_KEY` | `cerebras.WithAPIKey(key)` |
| Groq | `groq.NewModel("openai/gpt-oss-20b")` | `GROQ_API_KEY` | `groq.WithAPIKey(key)` |
| Hugging Face | `huggingface.NewModel("Qwen/Qwen3-32B")` | `HF_TOKEN` | `huggingface.WithToken(token)` |
| Meta Muse | `meta.NewModel("muse-spark-1.3")` | `META_API_KEY` | `meta.WithAPIKey(key)` |
| Mistral | `mistral.NewModel("mistral-large-latest")` | `MISTRAL_API_KEY` | `mistral.WithAPIKey(key)` |
| Ollama | `ollama.NewModel("qwen3")` | `OLLAMA_BASE_URL`, `OLLAMA_API_KEY` | `ollama.WithAPIKey(key)` |
| OpenRouter | `openrouter.NewModel("anthropic/claude-sonnet-4.6")` | `OPENROUTER_API_KEY` | `openrouter.WithAPIKey(key)` |
| Snowflake Cortex | `snowflake.NewModel("claude-sonnet-4-6")` | `SNOWFLAKE_ACCOUNT`, `SNOWFLAKE_TOKEN` | `snowflake.WithAccount(account)` and `snowflake.WithToken(token)` |
| Together AI | `together.NewModel("Qwen/Qwen3-32B")` | `TOGETHER_API_KEY` | `together.WithAPIKey(key)` |
| System One decision models | `systemone.NewModel("clm-latest")` † | `SYSTEM_ONE_BASE_URL`, `SYSTEM_ONE_API_KEY` (optional) | `systemone.WithAPIKey(key)` |
| TypeSafe Jev | `typesafe.NewModel("jev-latest")` | `TYPESAFE_API_KEY` | `typesafe.WithAPIKey(key)` |
| vLLM | `vllm.NewModel("Qwen/Qwen3-32B")` † | `VLLM_BASE_URL`, `VLLM_API_KEY` | `vllm.WithAPIKey(key)` |
| xAI | `xai.NewModel("grok-4.3")` | `XAI_API_KEY` | `xai.WithAPIKey(key)` |
| Z.AI | `zai.NewModel("glm-5.3-flash")` | `ZAI_API_KEY` | `zai.WithAPIKey(key)` |

† The constructor returns an error as well as the model. The model names are examples; use the name of the model that you want.

Most packages also take `WithBaseURL`, to point the model at a proxy or a compatible endpoint, and `WithHTTPClient`. See [Provider configuration](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/providers.md) in the documentation of pydantic-ai-go, and the pages on [Amazon Bedrock](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/bedrock.md) and the [OpenAI Codex subscription](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/openai-codex.md).

```go
// The credentials are options of the model.
claude := anthropic.NewModel("claude-sonnet-4-5", anthropic.WithAPIKey(apiKey))

// A local vLLM server.
local, err := vllm.NewModel("Qwen/Qwen3-32B", vllm.WithBaseURL("http://localhost:8000/v1"))
if err != nil {
    log.Fatal(err)
}

// Azure OpenAI takes its settings in a config.
azureModel, err := azure.NewModel("my-deployment", azure.Config{
    Endpoint: "https://my-resource.openai.azure.com",
    APIKey:   apiKey,
})
if err != nil {
    log.Fatal(err)
}
```

### Choosing the provider from configuration

`infer.Model` (package `github.com/Kludex/pydantic-ai-go/ai/models/infer`) creates a model from a name with a provider prefix, so the provider can come from configuration. The model reads its credentials from the environment:

```go
model, err := infer.Model(os.Getenv("MODEL")) // for example MODEL=anthropic:claude-sonnet-4-5
if err != nil {
    log.Fatal(err)
}
agent := ai.NewAgent[struct{}, string](model)
```

The prefixes are `openai`, `openai-responses`, `openai-codex`, `openai-decisions`, `anthropic`, `google`, `github-copilot`, `bedrock`, `bedrock-mantle`, `cerebras`, `cohere`, `crusoe`, `deepseek`, `deepseek-responses`, `groq`, `huggingface`, `meta`, `mistral`, `ollama`, `openrouter`, `snowflake`, `system-one`, `together`, `typesafe`, `vllm`, `xai` and `zai`. Azure OpenAI and Vertex AI need a config and have no prefix. The package imports the provider packages behind these prefixes, so a binary that uses it links all of them.

## MCP

[MCP](https://modelcontextprotocol.io) servers give an agent tools. pydantic-ai-go connects to them with `github.com/Kludex/pydantic-ai-go/ai/mcp` (`aimcp` below), and the handlers serve such an agent like any other: a call of an MCP tool is streamed as a tool call, with the result of the server. The recipes here are the parts that depend on Fiber: whose credentials the server sees, when its session closes, and what reaches the frontend.

### Tools from an MCP server

```go
agent := ai.NewAgent[Deps, string](openai.NewModel("gpt-5-mini"))
agent.AddToolset(aimcp.NewHTTPToolset[Deps](
    aimcp.HTTPToolsetConfig{URL: "https://mcp.example.com/mcp"},
    aimcp.WithID("catalog"),
))

app.Post("/chat", pydanticai.Vercel(agent))
```

Each run opens a session of its own and closes it when the run ends, however it ends, so concurrent requests share neither state nor credentials. A URL that ends with `/sse` uses the legacy SSE transport; any other URL uses Streamable HTTP. Tools that the server marks as for apps only (`_meta.ui.visibility` without `model`) are not offered to the model.

`aimcp.NewCommandToolset` starts a subprocess for each run. That suits a tool that is cheap to start, not a server that handles many requests: share one session instead, see below.

### Tools on behalf of the user

The headers of a toolset are fixed when it is created. To send the credentials of the caller, create the toolset for the request in `RunOptions` and attach it to the run:

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

Creating a toolset is cheap: it connects when the run starts. The headers go to the server of the URL only, and a redirect to another origin does not get them.

### A session that all requests share

Share one session when connecting is expensive or the server keeps state, for example a subprocess that speaks over standard input and output (`mcpsdk.CommandTransport`). The session is yours to close, and only after the last run that uses it has ended. `CancelOnShutdown` makes a shutdown stop the runs first, and `OnPostShutdown` runs once the server is down:

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

`mcpsdk` is `github.com/modelcontextprotocol/go-sdk/mcp`. Runs on a shared session are not isolated from each other, so use it for servers that hold nothing of a user, or keep a session for each user.

### Provider-hosted MCP servers

Some providers run MCP servers themselves, for example the OpenAI Responses API. One capability lets the provider do it where it can, and connects from your application where it cannot:

```go
capability := aimcp.NewHTTPServerCapability[Deps](aimcp.HTTPServerCapabilityConfig{
    Native: ai.MCPServerTool{
        ID:                 "catalog",
        URL:                "https://mcp.example.com/mcp",
        AuthorizationToken: os.Getenv("MCP_TOKEN"),
        AllowedTools:       []string{"search"},
    },
})
agent := ai.NewAgent[Deps, string](openai.NewResponsesModel("gpt-5"), ai.WithCapabilities(capability))
```

The URL, the credentials and `AllowedTools` apply to both paths. Use `ai.NewDynamicMCPServerCapability` when they depend on the request.

### Servers from a configuration file

`aimcp.LoadToolsets` reads the `mcpServers` format of Claude Desktop and Cursor. The name of each server becomes its toolset ID and the prefix of its tool names, and `${NAME}` in a value is replaced by the environment variable:

```go
toolsets, err := aimcp.LoadToolsets[Deps](".mcp.json")
if err != nil {
    log.Fatal(err)
}
for _, toolset := range toolsets {
    agent.AddToolset(toolset)
}
```

Treat the file as trusted input: an entry with a `command` runs a local program.

### MCP Apps

A tool that names an HTML page in `_meta.ui.resourceUri` is an [MCP App](https://modelcontextprotocol.io/extensions/apps/overview). After its result, the AG-UI stream carries an `ACTIVITY_SNAPSHOT` event with the `activityType` `mcp-apps`, and the Vercel AI stream a `data-mcp-apps` chunk whose `id` is the ID of the tool call. Both hold the `resourceUri`, the `toolInput`, the `result` and the `serverId` (the ID of the toolset). The model never sees them.

The page runs in the browser and cannot reach the MCP server, so the application relays what it asks for on a session that it keeps. How the requests get to these routes depends on the frontend that hosts the page:

```go
app.Get("/mcp-app/resource", func(c fiber.Ctx) error {
    uri := c.Query("uri")
    if !strings.HasPrefix(uri, "ui://") { // a page may read the pages of apps, not every resource
        return fiber.ErrForbidden
    }
    result, err := session.ReadResource(c.Context(), uri)
    if err != nil {
        return err
    }
    return c.JSON(result)
})

app.Post("/mcp-app/tools/call", func(c fiber.Ctx) error {
    var call struct {
        Name      string         `json:"name"`
        Arguments map[string]any `json:"arguments"`
    }
    if err := c.Bind().JSON(&call); err != nil {
        return err
    }
    result, err := session.CallToolForApp(c.Context(), call.Name, call.Arguments) // only tools that apps may call
    if err != nil {
        return fiber.NewError(fiber.StatusBadRequest, err.Error())
    }
    return c.JSON(result)
})
```

Authenticate these routes like the chat route. The server does not say which resources a page may read, so check the URI before `ReadResource`.

### Testing an agent with MCP tools

A server in memory needs no network. `mcpsdk.NewInMemoryTransports` connects the client of a run to it:

```go
server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "catalog", Version: "1.0.0"}, nil)
mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "search", Description: "Search the catalog"},
    func(_ context.Context, _ *mcpsdk.CallToolRequest, in SearchInput) (*mcpsdk.CallToolResult, SearchOutput, error) {
        return nil, SearchOutput{Hits: []string{"guide for " + in.Query}}, nil
    })

agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
agent.AddToolset(aimcp.NewToolset(func(ctx context.Context, _ *ai.RunContext[struct{}]) (mcpsdk.Transport, error) {
    clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
    if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
        return nil, err
    }
    return clientTransport, nil
}, aimcp.WithID("catalog")))
```

`fakes.NewTestModel` calls every tool once, so the stream of a request holds the call of `search` and its result, which `pydanticaitest.AGUIEvents` decodes.

### Serving an agent as an MCP server

`pydanticaimcp.New` serves an agent to MCP clients, such as desktop assistants or other agents, over Streamable HTTP. `pydanticaimcp` is `github.com/gofiber/contrib/v3/pydanticai/pydanticaimcp`, a package of its own, so that an application that does not serve MCP does not build the MCP SDK:

```go
app.Use("/mcp", authenticate) // your authentication middleware, which stores the user in c.Locals
app.Post("/mcp", pydanticaimcp.New(agent, pydanticaimcp.Config[Deps]{
    Deps: func(c fiber.Ctx) (Deps, error) {
        return Deps{UserID: fiber.Locals[string](c, "user")}, nil
    },
    Description: "Ask the support agent about orders and refunds.",
    Timeout:     2 * time.Minute,
}))
```

The client sees one tool, `ask`, with the argument `question`. Its result is the output of the agent: text as it is, and any other output as JSON in the text and as structured content. `Tool` and `Description` name the tool and say what it is for, which the model of the client reads when it chooses a tool.

- **Authenticate first.** Derive `Deps` from the authenticated request, never from the arguments of the tool, which a caller can set to anything. `Deps` is called for every message of the client, including those that set up the connection and list the tool, so keep it cheap.
- **Errors.** A failed run is an error of the tool, which the model of the client reads and can react to. `ErrorMessage` replaces its text, as for the other handlers, and a panic is reported as `internal error`.
- **No sessions, no streams.** Every message is a request with a JSON response, so the server holds no state, and a shutdown never waits for an idle connection. The server cannot send progress, sampling or elicitation requests to the client. A server that needs them mounts `mcpsdk.NewStreamableHTTPHandler` with the `adaptor` middleware instead.
- **Timeouts.** A client that went away is not noticed before the response is written, so set a `Timeout`. `CancelOnShutdown` ends the calls when the server shuts down, and the client reads an error that says so.
- **Middleware.** Clients of the 2026-07-28 protocol send the method and the tool name in the headers `Mcp-Method` and `Mcp-Name`, so a middleware can authorize or limit `tools/call` without reading the body. A browser client also sends `Mcp-Protocol-Version`: allow these headers in the `cors` middleware.

Another agent uses the server as a toolset:

```go
agent.AddToolset(aimcp.NewHTTPToolset[Deps](aimcp.HTTPToolsetConfig{
    URL:     "https://support.example.com/mcp",
    Headers: map[string]string{"Authorization": "Bearer " + token},
}, aimcp.WithID("support")))
```

## Graceful shutdown

A stream that is still running keeps a shutdown waiting: `app.ShutdownWithTimeout` returns when the run ends on its own, or fails with `context deadline exceeded` when its timeout elapses first. `CancelOnShutdown` cancels the runs of the handler as soon as the server starts shutting down. The streams end like one that timed out, with the end-of-stream event of their protocol, `OnRunEnd` and `OnClose` get an error that wraps `ai.ErrRunCancelled`, and the shutdown does not wait for them. The connection of a client that stopped reading is closed once a write to it stayed blocked for 5 seconds:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{CancelOnShutdown: true}))

go func() {
    if err := app.Listen(":3000"); err != nil {
        log.Fatal(err)
    }
}()

ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
<-ctx.Done()
if err := app.ShutdownWithTimeout(30 * time.Second); err != nil {
    log.Print(err)
}
```

The option is off by default, because some applications prefer to let a short run finish. `Run` has no such option, because it is a plain request: set its `Timeout`.

## Middleware in front of the handlers

The handlers neither authenticate nor limit requests, and they do not answer cross-origin requests. Fiber's middleware cover that. Put the middleware that identifies the caller before the one that limits the caller:

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

`cors` comes first, because preflight requests carry no credentials. The limiter counts the requests of each user, not of each address. The `timeout` middleware does not stop a stream, and a request that it gave up on before the stream started, for example while `Deps` ran, is not run. `compress` works, see [Disconnects, timeouts and heartbeats](#disconnects-timeouts-and-heartbeats).

## Requests

A request is checked before the agent runs. A rejected request is returned as a `*fiber.Error` and handled by the error handler of the app:

| Status | Reason                                                                                                   |
|:-------|:---------------------------------------------------------------------------------------------------------|
| `405`  | The method is not `POST`. The response carries `Allow: POST`.                                            |
| `415`  | The content type is not allowed (`application/json` unless configured), or the body has a `Content-Encoding` other than `identity`. Request bodies are not decompressed. |
| `413`  | The body is larger than `MaxRequestBytes`.                                                               |
| `400`  | The body is not a valid request of the protocol, the Vercel request has no chat `id`, or the `Run` request has no `prompt`. |

The body is limited by `MaxRequestBytes` of the adapter config (10 MiB by default) and by Fiber's `BodyLimit` (4 MiB by default). Raise `BodyLimit` in `fiber.Config` to accept larger bodies, such as inline images. With `StreamRequestBody` enabled only `MaxRequestBytes` applies, and the handlers read no more than that plus one byte.

A request body larger than 64 KiB is released once it has been decoded, so that it is not kept for the length of the run. Middleware that runs after the handler sees an empty body.

The content type check keeps cross-site form posts from reaching the agent. `DisableContentTypeCheck` of the adapter config turns it off, which is only safe when another CSRF protection covers the route.

## Disconnects, timeouts and heartbeats

While a response is open, the streaming handlers write a comment line (`:`) every `HeartbeatInterval`, 5 seconds by default. A failed write cancels the run. Writes keep succeeding for a moment after a client has closed its connection, so a disconnect is noticed by the second or third heartbeat: with the default interval, a run that nobody waits for is canceled within 15 seconds. A failed write of an event cancels the run as well. A client that vanishes without closing its connection, for example because it lost its network, is only noticed when TCP gives up on the connection, so set `Timeout` to bound such a run. Once `Timeout` elapsed, a write to a client that stopped reading may stay blocked for 5 seconds before its connection is closed, and `OnClose` then receives the write error.

Heartbeat comments are ignored by server-sent events parsers. `DisableHeartbeat` turns them off. A client that leaves during a long silent phase, such as a tool call, is then only noticed by the next write to it, which can be after the run has ended.

`Timeout` bounds the duration of a run for all three handlers. The cancellation and the deadline of the request context do not apply to a streaming run, so a middleware that cancels that context when the handler returns, such as the `timeout` middleware of Fiber, does not stop the stream. `Run` writes no heartbeat and cannot tell that the client left, so `Timeout` is what bounds a run that nobody waits for.

`fiber.Config.WriteTimeout` applies to a whole response, so it cuts every stream that is longer than it, however many heartbeats it carries. Leave it at zero, or set it above the longest run you accept. The `compress` middleware can be mounted in front of the handlers, and each event still reaches the client as it is written.

## How a stream ends

| Event                       | AG-UI                                | Vercel AI                      |
|:----------------------------|:-------------------------------------|:-------------------------------|
| The run completes           | `RUN_FINISHED`                       | `finish` chunk, then `[DONE]`  |
| `Timeout` elapses, or the server shuts down with `CancelOnShutdown` | `RUN_FINISHED` without an outcome | `abort` chunk, then `[DONE]` |
| The run fails               | `RUN_ERROR`                          | `error` chunk                  |
| The run panics              | `RUN_ERROR` with `internal error`    | `error` chunk, then `[DONE]`   |

The status of the response is `200` in all of these cases, because it is sent before the run starts. When the client disconnects, the run is canceled, nothing more is sent and `OnClose` receives the error.

A panic in a tool, in the model or in a callback of the handler ends the stream with an error event too. The client sees `internal error`, or what `ErrorMessage` returns, and the panic with its stack goes to the logger of Fiber and to `OnClose`. This covers tool calls that run concurrently on goroutines of their own. Other code that pydantic-ai-go runs on those goroutines, such as the event listeners of the agent, is not covered: recover the panics of such code yourself.

## Errors of `Run`

`Run` returns a `*pydanticai.Error` when the agent fails. Its message is the reason phrase of the status code, so the default error handler of Fiber does not send provider details to the client, and it encodes as JSON as `{"code":502}`, for example. The error of the agent is in `Err` and is found by `errors.Is` and `errors.As`. `errors.As` also finds a `*fiber.Error` with the status code, and `errors.Is` matches the errors of Fiber, such as `fiber.ErrTooManyRequests`, by status code.

`StatusCode` picks the status. A `*fiber.Error` in the chain wins when it is a client or server error with a reason phrase, so an error that chose its own status keeps it:

| Error                                                                              | Status                      |
|:-----------------------------------------------------------------------------------|:----------------------------|
| `ai.ErrUsageLimitExceeded`                                                         | `429 Too Many Requests`     |
| `context.DeadlineExceeded`, including `Timeout`                                    | `504 Gateway Timeout`       |
| `ai.ErrRunCancelled`, `context.Canceled`                                           | `503 Service Unavailable`   |
| `*ai.ContentFilterError`                                                           | `422 Unprocessable Entity`  |
| `*ai.UnexpectedModelBehaviorError`, `ai.ErrMaxRetriesExceeded`, `ai.ModelAPIError` | `502 Bad Gateway`           |
| anything else                                                                      | `500 Internal Server Error` |

Log the cause in the error handler of the app:

```go
app := fiber.New(fiber.Config{
    ErrorHandler: func(c fiber.Ctx, err error) error {
        if runErr, ok := errors.AsType[*pydanticai.Error](err); ok {
            log.Printf("agent failed: %v", runErr.Err) // the cause, never sent to the client
        }
        return fiber.DefaultErrorHandler(c, err)
    },
})
```

## Client-held history

A chat frontend sends the whole conversation with each request, so the history is untrusted input. Both adapters sanitize it before the run, for example by removing system prompts, provider-hosted file references and unresolved trailing tool calls. The zero value of `Adapter.Sanitization` is the secure setting. Relax it only when your application has a reason to trust the client:

```go
app.Post("/chat", pydanticai.AGUI(agent, pydanticai.AGUIConfig[Deps]{
    Adapter: agui.Config{
        Sanitization: ai.MessageSanitizationOptions{AllowSystemPrompts: true},
    },
}))
```

See the [AG-UI](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/ag-ui.md) and [Vercel AI](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/vercel-ai.md) pages of pydantic-ai-go for everything the adapters support: multimodal input, frontend tools, approvals, external tools, state and reasoning.

## Config

### AGUIConfig and VercelConfig

The two streaming handlers take the same settings. They differ in the event that carries an error message and in the adapter config.

| Property          | Type                                          | Description                                                                                                                                                                               | Default                |
|:------------------|:----------------------------------------------|:------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:-----------------------|
| Next              | `func(fiber.Ctx) bool`                        | Skips the handler when it returns `true`.                                                                                                                                                 | `nil`                  |
| Deps              | `func(fiber.Ctx) (Deps, error)`               | Resolves the dependencies of the agent for one request. An error aborts the request before the body is read and the stream starts.                                                       | zero value of `Deps`   |
| RunOptions        | `func(fiber.Ctx) ([]ai.RunOption, error)`     | Returns extra run options for one request.                                                                                                                                                | `nil`                  |
| ErrorMessage      | `func(error) string`                          | Maps the error of a failed run to the message that the client receives, in the `RUN_ERROR` event (AG-UI) or the `error` chunk (Vercel AI). Also applies to a panic and to an event that cannot be encoded. A panic in it is logged, and the client receives `internal error`. | `err.Error()`          |
| OnClose           | `func(Deps, error)`                           | Called once when the stream is over, with the dependencies of the run. The error is `nil` when the stream ended normally. The end of the response waits for it.                           | `nil`                  |
| OnRunEnd          | `func(Deps, *ai.RunInfo, error)`              | Called once when the run has ended, before `OnClose`, with what the run consumed and how it ended. The run is `nil` when the agent did not start. The end of the response waits for it.   | `nil`                  |
| Adapter           | `agui.Config` or `vercel.Config`              | Protocol version (`Version` for AG-UI, `SDKVersion` for Vercel AI), sanitization of client history and request limits (`MaxRequestBytes`, `AllowedContentTypes`, `DisableContentTypeCheck`). | the zero value         |
| Timeout           | `time.Duration`                               | Bounds the duration of one run. A client that stopped reading has its connection closed when a write to it stays blocked for 5 seconds after that.                                      | `0` (no timeout)       |
| HeartbeatInterval | `time.Duration`                               | Interval of the heartbeat comments. A client that left is noticed by the second or third one. A value of zero or less uses the default.                                                   | `5 * time.Second`      |
| DisableHeartbeat  | `bool`                                        | Turns the heartbeat off.                                                                                                                                                                  | `false`                |
| CancelOnShutdown  | `bool`                                        | Cancels the run when the server starts shutting down, instead of waiting for it.                                                                                                          | `false`                |

### RunConfig

| Property        | Type                                              | Description                                                                                          | Default                                   |
|:----------------|:--------------------------------------------------|:-----------------------------------------------------------------------------------------------------|:------------------------------------------|
| Next            | `func(fiber.Ctx) bool`                            | Skips the handler when it returns `true`.                                                            | `nil`                                     |
| Deps            | `func(fiber.Ctx) (Deps, error)`                   | Resolves the dependencies of the agent for one request. An error is returned from the handler as it is. | zero value of `Deps`                   |
| RunOptions      | `func(fiber.Ctx) ([]ai.RunOption, error)`         | Returns extra run options for one request. An `ai.WithConversationID` among them takes precedence over the conversation ID of the body; `ai.WithConversation` cannot be combined with one. | `nil`                                   |
| Prompt          | `func(fiber.Ctx) (string, error)`                 | Reads the user prompt from the request. None of the request checks of the default applies.           | the `prompt` field of the JSON body       |
| Respond         | `func(fiber.Ctx, *ai.RunResult[Output]) error`    | Writes the response for a completed run.                                                             | `{"output", "run_id", "conversation_id"}` |
| OnRunEnd        | `func(Deps, *ai.RunInfo, error)`                  | Called once the agent has run, with what the run consumed and the error that ended it, also when the run panicked. The run is `nil` when the agent did not start. | `nil`                                   |
| MaxRequestBytes | `int64`                                           | Bounds the body that the default `Prompt` reads. The `BodyLimit` of the app applies as well, unless `StreamRequestBody` is enabled, see [Requests](#requests). | `10 MiB`                                  |
| Timeout         | `time.Duration`                                   | Bounds the duration of one run. A run that exceeds it fails with `504`.                              | `0` (no timeout)                          |

### pydanticaimcp.Config

| Property         | Type                                      | Description                                                                                                                                  | Default                                  |
|:-----------------|:------------------------------------------|:---------------------------------------------------------------------------------------------------------------------------------------------|:-----------------------------------------|
| Next             | `func(fiber.Ctx) bool`                    | Skips the handler when it returns `true`.                                                                                                    | `nil`                                    |
| Deps             | `func(fiber.Ctx) (Deps, error)`           | Resolves the dependencies of the agent for one request. An error is returned from the handler as it is, before the message is read. Called for every message. | zero value of `Deps`           |
| RunOptions       | `func(fiber.Ctx) ([]ai.RunOption, error)` | Returns extra run options for one request.                                                                                                   | `nil`                                    |
| Name, Version    | `string`                                  | Identify the server to its clients.                                                                                                          | the name of the agent or `pydantic-ai-go`, and `1.0.0` |
| Tool, Description | `string`                                 | The name of the tool that runs the agent, 1 to 128 characters of `A-Z`, `a-z`, `0-9`, `_`, `.` and `-`, and what it is for.                  | `ask`, `Ask the agent a question`        |
| ErrorMessage     | `func(error) string`                      | Maps the error of a failed run to the text that the client reads. Also applies to an output that cannot be encoded. A panic is reported with the error `internal error`. | `err.Error()`            |
| OnRunEnd         | `func(Deps, *ai.RunInfo, error)`          | Called once the agent has run, with what the run consumed and the error that ended it, also when the run panicked. The run is `nil` when the agent did not start. | `nil`                        |
| Timeout          | `time.Duration`                           | Bounds the duration of one run.                                                                                                              | `0` (no timeout)                         |
| CancelOnShutdown | `bool`                                    | Cancels the runs when the server starts shutting down, instead of waiting for them.                                                          | `false`                                  |

## See also

pydantic-ai-go also serves agents as [A2A](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/a2a.md) agents. That transport is a `net/http` handler. Mount it with the [`adaptor`](https://docs.gofiber.io/next/middleware/adaptor) middleware of Fiber, keeping in mind that it does not receive the `fiber.Ctx` of the request. [MCP servers](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/mcp-server.md) are served by `pydanticaimcp`, see [above](#serving-an-agent-as-an-mcp-server).
