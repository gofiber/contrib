package pydanticai_test

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai"
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
