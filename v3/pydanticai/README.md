---
id: pydanticai
---

# Pydantic AI

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*pydanticai*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20pydanticai/badge.svg)

[pydantic-ai-go](https://github.com/Kludex/pydantic-ai-go) support for Fiber. It serves an agent to a chat frontend over the [AG-UI](https://docs.ag-ui.com) and the [Vercel AI SDK](https://ai-sdk.dev) protocols, or as a plain JSON endpoint.

- **Per-request dependencies.** The dependencies of the agent are built from the `fiber.Ctx` of each request, so tools see the authenticated user and nothing is shared between requests.
- **Runs stop when the client goes away.** fasthttp does not cancel the context of a disconnected client. The streaming handlers detect a failed write or heartbeat and cancel the run, so no tokens are spent on an answer nobody reads.
- **Fiber-native errors.** A rejected request goes through the error handler of your app instead of being written as plain text.
- **Same protocol behavior as pydantic-ai-go.** The streams carry the same events and chunks as the `net/http` handlers of pydantic-ai-go, and the same request limits and the same history sanitization apply.

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

`ai` is `github.com/Kludex/pydantic-ai-go/ai`. The type parameters are inferred from the agent, so `pydanticai.AGUI(agent)` needs no instantiation. A config does: `pydanticai.AGUIConfig[MyDeps]{...}`.

| Handler  | Protocol                                                       | Use it for                                                     |
|:---------|:---------------------------------------------------------------|:---------------------------------------------------------------|
| `AGUI`   | [AG-UI](https://docs.ag-ui.com) events over server-sent events | AG-UI frontends such as CopilotKit                             |
| `Vercel` | Vercel AI SDK UI message stream (AI SDK 5 to 7)                 | the `useChat` hook of the AI SDK                               |
| `Run`    | JSON request and response                                      | server-to-server calls and typed, structured output            |

## Quick start

An agent that answers a chat made with the `useChat` hook of the AI SDK:

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

Set `OPENAI_API_KEY` and point the frontend at the route (AI SDK 5):

```ts
const { messages, sendMessage } = useChat({
  transport: new DefaultChatTransport({ api: "http://localhost:3000/chat" }),
});
```

Serve the same agent to an AG-UI frontend with `app.Post("/agui", pydanticai.AGUI(agent))`. The Vercel handler speaks the AI SDK 5 protocol by default. Set `Adapter: vercel.Config{SDKVersion: 6}` (or `7`) for a newer AI SDK.

## Dependencies for each request

`Deps` receives the `fiber.Ctx` of the request and returns the dependencies that the tools and the instructions of the agent see. Put the authentication of the caller here, or in a middleware in front of the handler, as the handlers do not authenticate requests.

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

type Deps struct {
    UserID string
}

func main() {
    agent := ai.NewAgent[Deps, string](openai.NewModel("gpt-5-mini"))
    ai.AddTool(agent, "my_orders", func(
        ctx context.Context, rc *ai.RunContext[Deps], _ struct{},
    ) (string, error) {
        return "orders of " + rc.Deps.UserID, nil // query your database with rc.Deps.UserID
    })

    app := fiber.New()
    app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
        Deps: func(c fiber.Ctx) (Deps, error) {
            user, ok := c.Locals("user").(string) // set by your authentication middleware
            if !ok {
                return Deps{}, fiber.ErrUnauthorized
            }
            return Deps{UserID: user}, nil
        },
    }))

    log.Fatal(app.Listen(":3000"))
}
```

An error from `Deps` aborts the request before the stream starts. Fiber's error handler turns it into the response, here a `401`. `Deps` runs before the body of the request is parsed, so a caller that is turned away costs next to nothing.

The stream outlives the handler. Copy what the dependencies need out of `c` and do not keep `c` itself. The context passed to the tools is `c.Context()` extended with the cancellation described below, so values that a middleware stored with `c.SetContext`, such as the span of the [otel](../otel/README.md) middleware, reach the agent and its spans become children of the span of the request.

## Run options

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

The AG-UI and the Vercel adapter set the message history and the conversation ID of the run themselves, whatever `RunOptions` returns.

## Client disconnects, timeouts and heartbeats

A request context that is canceled when the client leaves is what Go servers rely on to stop work nobody waits for, and fasthttp does not provide one. The streaming handlers use the stream of Fiber's [`sse`](https://docs.gofiber.io/next/middleware/sse) middleware instead: a failed write cancels the context of the run, and so does a failed heartbeat. The comment line `:` that the middleware writes every `HeartbeatInterval` (15 seconds by default) also keeps idle connections open while a slow tool runs, so a client that left during a long tool call is noticed within one interval.

| Situation                     | AG-UI                                          | Vercel AI                              |
|:------------------------------|:-----------------------------------------------|:---------------------------------------|
| The client closes the stream  | the run is canceled, `OnClose` gets the error  | same                                   |
| `Timeout` elapses             | `RUN_FINISHED` without an outcome              | `abort` chunk, then `[DONE]`           |
| The run fails                 | `RUN_ERROR`                                    | `error` chunk                          |

Heartbeat comments are valid server-sent events that every compliant parser skips. Set `DisableHeartbeat` if a client cannot cope with them. Without heartbeats a client that leaves during a long run can go unnoticed until a write to it fails, which can be after the run has ended.

`Run` is a plain request and response, and fasthttp cannot tell it that the client left. Set `Timeout` so that a run nobody waits for is bounded.

## Errors

Problems with the request are returned as `*fiber.Error` before the stream starts:

| Status | Reason                                                                       |
|:-------|:-----------------------------------------------------------------------------|
| `405`  | the method is not `POST` (the response carries `Allow: POST`)                |
| `415`  | the content type is not allowed, `application/json` unless configured        |
| `413`  | the body is larger than `MaxRequestBytes` of the adapter config (10 MiB)      |
| `400`  | the body is not a valid request of the protocol                              |

Fiber's own `BodyLimit` (4 MiB by default) applies on top of `MaxRequestBytes`. Raise it in `fiber.Config` to accept larger bodies, for example inline images. The content type check protects the route from cross-site form posts. `DisableContentTypeCheck` in the adapter config turns it off, which is only safe when another CSRF control protects the route.

Once the stream has started, the status is `200` and a failed run is reported in the protocol, with the message of the error. The pydantic-ai-go adapters send `err.Error()` to the client, and provider errors can carry request IDs or parts of credentials. `ErrorMessage` replaces the message the client sees, and `OnClose` still receives the original error for your logs:

```go
app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[Deps]{
    ErrorMessage: func(err error) string { return "The assistant is not available." },
    OnClose: func(c fiber.Ctx, err error) {
        if err != nil {
            log.Printf("chat stream ended: %v", err)
        }
    },
}))
```

`OnClose` gets `nil` when the run completed, the error of the agent when it failed, and the write error when the client left. It runs on the goroutine of the stream, so do not use `c` after it returns.

### Errors of `Run`

`Run` returns a `*pydanticai.Error` when the agent fails. Its message is the reason phrase of the status code, so the default Fiber error handler never sends provider details to the client. The agent error stays available for `errors.Is`, `errors.As` and your logs, and the status code is found by `errors.As` with a `*fiber.Error`, which is what Fiber's error handlers look for.

`StatusCode` picks the status. A `*fiber.Error` in the chain wins, so an error that chose its status keeps it:

| Error                                                                          | Status                    |
|:-------------------------------------------------------------------------------|:--------------------------|
| `ai.ErrUsageLimitExceeded`                                                     | `429 Too Many Requests`   |
| `context.DeadlineExceeded`, including `Timeout`                                | `504 Gateway Timeout`     |
| `ai.ErrRunCancelled`, `context.Canceled`                                       | `503 Service Unavailable` |
| `*ai.ContentFilterError`                                                       | `422 Unprocessable Entity`|
| `*ai.UnexpectedModelBehaviorError`, `ai.ErrMaxRetriesExceeded`, `ai.ModelAPIError` | `502 Bad Gateway`     |
| anything else                                                                  | `500 Internal Server Error` |

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

## Run

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

The history of the conversation, the usage and the cost of the run stay on the server. Use `Respond` to send more, `Prompt` to read the prompt from somewhere else than the JSON body, and `RunOptions` with `ai.WithMessageHistory` to continue a conversation that you keep yourself:

```go
app.Get("/weather", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, Weather]{
    Prompt: func(c fiber.Ctx) (string, error) {
        return "What is the weather in " + c.Query("city") + "?", nil
    },
    Respond: func(c fiber.Ctx, result *ai.RunResult[Weather]) error {
        return c.JSON(fiber.Map{
            "weather":       result.Output,
            "total_tokens":  result.Usage().InputTokens + result.Usage().OutputTokens,
        })
    },
}))
```

A run that ends with deferred tool calls, which are tools that need approval or an external result, cannot be resumed by a single request and response. The default `Respond` answers `501 Not Implemented` for it. Serve such an agent with `AGUI` or `Vercel`, which resume deferred calls, or handle `result.Deferred()` in your own `Respond`.

## Client-held history

A chat frontend sends the whole conversation with every request, so the history is untrusted input. Both adapters sanitize it before the run: system prompts, provider-hosted file references, unsafe download settings and unresolved trailing tool calls from the client are removed. The zero value of `Adapter.Sanitization` is the secure one. Relax it only when your application has a reason to trust the client:

```go
app.Post("/chat", pydanticai.AGUI(agent, pydanticai.AGUIConfig[Deps]{
    Adapter: agui.Config{
        Sanitization: ai.MessageSanitizationOptions{AllowSystemPrompts: true},
    },
}))
```

See the [AG-UI](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/ag-ui.md) and [Vercel AI](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/vercel-ai.md) pages of pydantic-ai-go for everything the adapters support: multimodal input, frontend tools, approvals, external tools, state and reasoning.

## Config

### AGUIConfig

| Property          | Type                                          | Description                                                                                                                                                  | Default                |
|:------------------|:----------------------------------------------|:-------------------------------------------------------------------------------------------------------------------------------------------------------------|:-----------------------|
| Next              | `func(fiber.Ctx) bool`                        | Skips the handler when it returns `true`.                                                                                                                    | `nil`                  |
| Deps              | `func(fiber.Ctx) (Deps, error)`               | Resolves the dependencies of the agent for one request. An error aborts the request before the stream starts.                                               | zero value of `Deps`   |
| RunOptions        | `func(fiber.Ctx) ([]ai.RunOption, error)`     | Returns extra run options for one request.                                                                                                                   | `nil`                  |
| ErrorMessage      | `func(error) string`                          | Maps the error of a failed run to the message in the `RUN_ERROR` event.                                                                                      | `err.Error()`          |
| OnClose           | `func(fiber.Ctx, error)`                      | Called once when the stream has ended. The error is `nil` when the run completed.                                                                            | `nil`                  |
| Adapter           | `agui.Config`                                 | Protocol version, sanitization of client history and request limits (`MaxRequestBytes`, `AllowedContentTypes`, `DisableContentTypeCheck`).                    | `agui.Config{}`        |
| Timeout           | `time.Duration`                               | Bounds the duration of one run.                                                                                                                              | `0` (no timeout)       |
| HeartbeatInterval | `time.Duration`                               | Interval of the heartbeat comments. A value of zero or less uses the default.                                                                                | `15 * time.Second`     |
| DisableHeartbeat  | `bool`                                        | Turns the heartbeat off.                                                                                                                                     | `false`                |

### VercelConfig

| Property          | Type                                          | Description                                                                                                                                                  | Default                |
|:------------------|:----------------------------------------------|:-------------------------------------------------------------------------------------------------------------------------------------------------------------|:-----------------------|
| Next              | `func(fiber.Ctx) bool`                        | Skips the handler when it returns `true`.                                                                                                                    | `nil`                  |
| Deps              | `func(fiber.Ctx) (Deps, error)`               | Resolves the dependencies of the agent for one request. An error aborts the request before the stream starts.                                               | zero value of `Deps`   |
| RunOptions        | `func(fiber.Ctx) ([]ai.RunOption, error)`     | Returns extra run options for one request.                                                                                                                   | `nil`                  |
| ErrorMessage      | `func(error) string`                          | Maps the error of a failed run to the message in the `error` chunk.                                                                                          | `err.Error()`          |
| OnClose           | `func(fiber.Ctx, error)`                      | Called once when the stream has ended. The error is `nil` when the run completed.                                                                            | `nil`                  |
| Adapter           | `vercel.Config`                               | AI SDK version, sanitization of client history and request limits (`MaxRequestBytes`, `AllowedContentTypes`, `DisableContentTypeCheck`).                      | `vercel.Config{}`      |
| Timeout           | `time.Duration`                               | Bounds the duration of one run.                                                                                                                              | `0` (no timeout)       |
| HeartbeatInterval | `time.Duration`                               | Interval of the heartbeat comments. A value of zero or less uses the default.                                                                                | `15 * time.Second`     |
| DisableHeartbeat  | `bool`                                        | Turns the heartbeat off.                                                                                                                                     | `false`                |

### RunConfig

| Property   | Type                                              | Description                                                                                                                                  | Default                                  |
|:-----------|:--------------------------------------------------|:---------------------------------------------------------------------------------------------------------------------------------------------|:-----------------------------------------|
| Next       | `func(fiber.Ctx) bool`                            | Skips the handler when it returns `true`.                                                                                                    | `nil`                                    |
| Deps       | `func(fiber.Ctx) (Deps, error)`                   | Resolves the dependencies of the agent for one request. An error is returned from the handler as it is.                                      | zero value of `Deps`                     |
| RunOptions | `func(fiber.Ctx) ([]ai.RunOption, error)`         | Returns extra run options for one request. They take precedence over the conversation ID of the body.                                         | `nil`                                    |
| Prompt     | `func(fiber.Ctx) (string, error)`                 | Reads the user prompt from the request.                                                                                                      | the `prompt` field of the JSON body      |
| Respond    | `func(fiber.Ctx, *ai.RunResult[Output]) error`    | Writes the response for a completed run.                                                                                                     | `{"output", "run_id", "conversation_id"}` |
| Timeout    | `time.Duration`                                   | Bounds the duration of one run. A run that exceeds it fails with `504`.                                                                       | `0` (no timeout)                         |

## Testing without a provider

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

## Other pydantic-ai-go integrations

pydantic-ai-go also serves agents as [A2A](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/a2a.md) agents and [MCP](https://github.com/Kludex/pydantic-ai-go/blob/main/docs/mcp-server.md) servers. Their transports are `net/http` handlers, and Fiber's [`adaptor`](https://docs.gofiber.io/next/middleware/adaptor) middleware converts those into Fiber handlers. A converted handler does not see the `fiber.Ctx`, so it cannot build dependencies for each request the way the handlers of this module do.
