---
id: pydanticai
---

# Pydantic AI

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*pydanticai*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20pydanticai/badge.svg)

[pydantic-ai-go](https://github.com/Kludex/pydantic-ai-go) support for Fiber. The handlers serve an agent to a chat frontend over the [AG-UI](https://docs.ag-ui.com) or the [Vercel AI SDK](https://ai-sdk.dev) protocol, or answer a JSON request with the output of one run.

- The dependencies and run options of the agent are built from the `fiber.Ctx` of each request, so tools can act on behalf of the authenticated user.
- A run is canceled when its client disconnects or its `Timeout` elapses.
- Rejected requests and failed `Run` calls go through the error handler of the app.
- A panic in a tool ends the stream with an error event.

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

The key belongs to the model, not to the handler. The model packages of pydantic-ai-go take their own options, such as `anthropic.WithAPIKey` and `google.WithAPIKey`.

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

The adapters set the message history and the conversation ID of a run themselves, whatever `RunOptions` returns.

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
    -d '{"prompt": "What is the weather in Paris?", "conversation_id": "optional"}'
{"output":{"city":"Paris","temp_c":21},"run_id":"52bdcf0873a4f6b3","conversation_id":"41407e02ee266cb2"}
```

The history of the conversation, the usage and the cost of the run stay on the server. `Respond` writes a different response, `Prompt` reads the prompt from somewhere other than the JSON body, and `RunOptions` with `ai.WithMessageHistory` continues a conversation that you store yourself:

```go
app.Get("/weather", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, Weather]{
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

The default request is a `POST` with an `application/json` body of at most `MaxRequestBytes`. A `Prompt` function owns the request instead: none of these checks applies to it, so a route that reads the body itself needs its own protection against cross-site form posts.

A run that ends with deferred tool calls, which are tools that need approval or an external result, cannot be completed by one request and response. The default `Respond` answers `501 Not Implemented` for it. Serve such an agent with `AGUI` or `Vercel`, which resume deferred calls, or handle `result.Deferred()` in your own `Respond`.

### Testing without a provider

pydantic-ai-go ships fake models, so the handlers can be tested without a network or an API key:

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
    require.Equal(t, "v1", resp.Header.Get("x-vercel-ai-ui-message-stream"))
}
```

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

While a response is open, the streaming handlers write a comment line (`:`) every `HeartbeatInterval`, 5 seconds by default. A failed write cancels the run. Writes keep succeeding for a moment after a client has closed its connection, so a disconnect is noticed by the second or third heartbeat: with the default interval, a run that nobody waits for is canceled within 15 seconds. A failed write of an event cancels the run as well. A client that vanishes without closing its connection, for example because it lost its network, is only noticed when TCP gives up on the connection, so set `Timeout` to bound such a run.

Heartbeat comments are ignored by server-sent events parsers. `DisableHeartbeat` turns them off. A client that leaves during a long silent phase, such as a tool call, is then only noticed by the next write to it, which can be after the run has ended.

`Timeout` bounds the duration of a run for all three handlers. The cancellation and the deadline of the request context do not apply to a streaming run, so a middleware that cancels that context when the handler returns, such as the `timeout` middleware of Fiber, does not stop the stream. `Run` writes no heartbeat and cannot tell that the client left, so `Timeout` is what bounds a run that nobody waits for.

`fiber.Config.WriteTimeout` applies to a whole response, so it cuts every stream that is longer than it, however many heartbeats it carries. Leave it at zero, or set it above the longest run you accept. The `compress` middleware can be mounted in front of the handlers, and each event still reaches the client as it is written.

## How a stream ends

| Event                       | AG-UI                                | Vercel AI                      |
|:----------------------------|:-------------------------------------|:-------------------------------|
| The run completes           | `RUN_FINISHED`                       | `finish` chunk, then `[DONE]`  |
| `Timeout` elapses           | `RUN_FINISHED` without an outcome    | `abort` chunk, then `[DONE]`   |
| The run fails               | `RUN_ERROR`                          | `error` chunk                  |
| The run panics              | `RUN_ERROR` with `internal error`    | `error` chunk, then `[DONE]`   |

The status of the response is `200` in all of these cases, because it is sent before the run starts. When the client disconnects, the run is canceled, nothing more is sent and `OnClose` receives the error.

A panic in a tool, in the model or in a callback ends the stream with an error event too. The client sees `internal error`, or what `ErrorMessage` returns, and the panic with its stack goes to the logger of Fiber and to `OnClose`.

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
| ErrorMessage      | `func(error) string`                          | Maps the error of a failed run to the message that the client receives, in the `RUN_ERROR` event (AG-UI) or the `error` chunk (Vercel AI). Also applies to a panic and to an event that cannot be encoded. | `err.Error()`          |
| OnClose           | `func(Deps, error)`                           | Called once when the stream is over, with the dependencies of the run. The error is `nil` when the stream ended normally. The end of the response waits for it.                           | `nil`                  |
| Adapter           | `agui.Config` or `vercel.Config`              | Protocol version (`Version` for AG-UI, `SDKVersion` for Vercel AI), sanitization of client history and request limits (`MaxRequestBytes`, `AllowedContentTypes`, `DisableContentTypeCheck`). | the zero value         |
| Timeout           | `time.Duration`                               | Bounds the duration of one run.                                                                                                                                                           | `0` (no timeout)       |
| HeartbeatInterval | `time.Duration`                               | Interval of the heartbeat comments. A client that left is noticed by the second or third one. A value of zero or less uses the default.                                                   | `5 * time.Second`      |
| DisableHeartbeat  | `bool`                                        | Turns the heartbeat off.                                                                                                                                                                  | `false`                |

### RunConfig

| Property        | Type                                              | Description                                                                                          | Default                                   |
|:----------------|:--------------------------------------------------|:-----------------------------------------------------------------------------------------------------|:------------------------------------------|
| Next            | `func(fiber.Ctx) bool`                            | Skips the handler when it returns `true`.                                                            | `nil`                                     |
| Deps            | `func(fiber.Ctx) (Deps, error)`                   | Resolves the dependencies of the agent for one request. An error is returned from the handler as it is. | zero value of `Deps`                   |
| RunOptions      | `func(fiber.Ctx) ([]ai.RunOption, error)`         | Returns extra run options for one request. They take precedence over the conversation ID of the body. | `nil`                                   |
| Prompt          | `func(fiber.Ctx) (string, error)`                 | Reads the user prompt from the request. None of the request checks of the default applies.           | the `prompt` field of the JSON body       |
| Respond         | `func(fiber.Ctx, *ai.RunResult[Output]) error`    | Writes the response for a completed run.                                                             | `{"output", "run_id", "conversation_id"}` |
| MaxRequestBytes | `int64`                                           | Bounds the body that the default `Prompt` reads. The `BodyLimit` of the app applies as well.         | `10 MiB`                                  |
| Timeout         | `time.Duration`                                   | Bounds the duration of one run. A run that exceeds it fails with `504`.                              | `0` (no timeout)                          |

## See also

pydantic-ai-go also serves agents as [A2A](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/a2a.md) agents and [MCP](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/mcp-server.md) servers. Those transports are `net/http` handlers. Mount them with the [`adaptor`](https://docs.gofiber.io/next/middleware/adaptor) middleware of Fiber, keeping in mind that they do not receive the `fiber.Ctx` of the request.
