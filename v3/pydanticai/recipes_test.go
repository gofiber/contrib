package pydanticai_test

import (
	"context"
	"testing"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// answering is a model that always gives the same answer.
func answering(text string) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		return textResponse(text), nil
	})
}

// The README shows how RunOptions choose the model of a run per request.
func Test_RunOptions_ChooseTheModelPerRequest(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](answering("the model of the agent"))
	pro := ai.WithRunModel(answering("the model of the plan"))
	choose := func(c fiber.Ctx) ([]ai.RunOption, error) {
		if c.Get("X-Plan") == "pro" {
			return []ai.RunOption{pro}, nil
		}
		return nil, nil
	}

	app := fiber.New()
	app.Post("/agui", pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{RunOptions: choose}))
	app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{RunOptions: choose}))
	app.Post("/ask", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{RunOptions: choose}))

	for _, plan := range []struct{ header, answer string }{
		{"", "the model of the agent"},
		{"pro", "the model of the plan"},
	} {
		// body returns the body of the response to a request of this plan.
		body := func(path, content string) string {
			request := newRequest(fiber.MethodPost, path, fiber.MIMEApplicationJSON, content)
			request.Header.Set("X-Plan", plan.header)
			_, text := send(t, app, request)
			return text
		}

		require.Equal(t, plan.answer, pydanticaitest.AGUIText(aguiEvents(t, body("/agui", aguiBody))), "AG-UI")
		chunks, _ := vercelChunks(t, body("/chat", vercelBody))
		require.Equal(t, plan.answer, pydanticaitest.VercelText(chunks), "Vercel")
		require.Contains(t, body("/ask", runBody), plan.answer, "Run")
	}
}
