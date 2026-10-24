package pydanticai_test

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// streamingModel streams a text of deltas chunks, one event each, like a
// provider does.
type streamingModel struct{ deltas int }

func (streamingModel) Name() string { return "streaming-benchmark" }

func (m streamingModel) Request(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
	return textResponse(strings.Repeat("token ", m.deltas)), nil
}

func (m streamingModel) StreamRequest(
	context.Context, []ai.ModelMessage, ai.ModelRequestParams,
) (iter.Seq2[ai.ModelStreamEvent, error], error) {
	return func(yield func(ai.ModelStreamEvent, error) bool) {
		for range m.deltas {
			if !yield(ai.TextDeltaEvent{Delta: "token "}, nil) {
				return
			}
		}
		yield(ai.FinishEvent{Usage: ai.Usage{Requests: 1}}, nil)
	}, nil
}

// serve runs one request through handler without a server, and reads the
// response to its end.
func serve(b *testing.B, handler fasthttp.RequestHandler, path, body string) {
	b.Helper()

	ctx := new(fasthttp.RequestCtx)
	ctx.Request.Header.SetMethod(fiber.MethodPost)
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.SetContentType(fiber.MIMEApplicationJSON)
	ctx.Request.SetBodyString(body)
	handler(ctx)
	if len(ctx.Response.Body()) == 0 { // reads the stream
		b.Fatal("empty response")
	}
}

// The benchmarks run a whole request: the admission, the run with a model that
// streams deltas chunks, and the events written to the response.
func Benchmark_AGUI(b *testing.B) {
	for _, deltas := range []int{1, 100} {
		b.Run(fmt.Sprintf("%d deltas", deltas), func(b *testing.B) {
			app := fiber.New()
			app.Post("/agui", pydanticai.AGUI(ai.NewAgent[struct{}, string](streamingModel{deltas})))
			handler := app.Handler()
			b.ReportAllocs()
			for b.Loop() {
				serve(b, handler, "/agui", aguiBody)
			}
		})
	}
}

func Benchmark_Vercel(b *testing.B) {
	for _, deltas := range []int{1, 100} {
		b.Run(fmt.Sprintf("%d deltas", deltas), func(b *testing.B) {
			app := fiber.New()
			app.Post("/chat", pydanticai.Vercel(ai.NewAgent[struct{}, string](streamingModel{deltas})))
			handler := app.Handler()
			b.ReportAllocs()
			for b.Loop() {
				serve(b, handler, "/chat", vercelBody)
			}
		})
	}
}

func Benchmark_Run(b *testing.B) {
	app := fiber.New()
	app.Post("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](streamingModel{1}), pydanticai.RunConfig[struct{}, string]{}))
	handler := app.Handler()
	b.ReportAllocs()
	for b.Loop() {
		serve(b, handler, "/ask", runBody)
	}
}

// The benchmarks send the messages of an MCP client to the handler without a
// server, the second as a client of the 2026-07-28 protocol sends it.
const (
	discoverMessage = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	callMessage     = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"name":"ask","arguments":{"question":"Hello"}}}`
)

func Benchmark_MCP(b *testing.B) {
	app := fiber.New()
	app.Post("/mcp", pydanticai.MCP(ai.NewAgent[struct{}, string](agenttest.Answering("ok"))))
	handler := app.Handler()

	// The headers tell which method a message calls, without parsing the body.
	messages := []struct{ method, name, body string }{
		{method: "server/discover", body: discoverMessage},
		{method: "tools/call", name: "ask", body: callMessage},
	}
	for _, message := range messages {
		b.Run(message.method, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ctx := new(fasthttp.RequestCtx)
				ctx.Request.Header.SetMethod(fiber.MethodPost)
				ctx.Request.SetRequestURI("/mcp")
				ctx.Request.Header.SetContentType(fiber.MIMEApplicationJSON)
				ctx.Request.Header.Set("Accept", "application/json, text/event-stream")
				ctx.Request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
				ctx.Request.Header.Set("Mcp-Method", message.method)
				if message.name != "" {
					ctx.Request.Header.Set("Mcp-Name", message.name)
				}
				ctx.Request.SetBodyString(message.body)
				handler(ctx)
				if ctx.Response.StatusCode() != fiber.StatusOK || len(ctx.Response.Body()) == 0 {
					b.Fatalf("%d %q", ctx.Response.StatusCode(), ctx.Response.Body())
				}
			}
		})
	}
}
