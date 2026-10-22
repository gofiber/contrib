package pydanticaimcp_test

import (
	"testing"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaimcp"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// The benchmarks send the messages of an MCP client to the handler without a
// server, the second as a client of the 2026-07-28 protocol sends it.
const (
	discoverMessage = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	callMessage     = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"name":"ask","arguments":{"question":"Hello"}}}`
)

func Benchmark_New(b *testing.B) {
	app := fiber.New()
	app.Post("/mcp", pydanticaimcp.New(ai.NewAgent[struct{}, string](agenttest.Answering("ok"))))
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
