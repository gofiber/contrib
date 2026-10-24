package pydanticai_test

import (
	"log"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/openai"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

type supportDeps struct{ UserID string }

// MCP serves an agent to MCP clients at one route. The route is authenticated
// by the middleware in front of it, and the dependencies of the agent are those
// of the authenticated user.
func ExampleMCP() {
	agent := ai.NewAgent[supportDeps, string](openai.NewModel("gpt-5-mini"))

	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error { // your authentication middleware
		c.Locals("user", c.Get("X-User"))
		return c.Next()
	})
	app.Post("/mcp", pydanticai.MCP(agent, pydanticai.MCPConfig[supportDeps]{
		Deps: func(c fiber.Ctx) (supportDeps, error) {
			return supportDeps{UserID: fiber.Locals[string](c, "user")}, nil
		},
		Description: "Ask the support agent about orders and refunds.",
		Timeout:     2 * time.Minute,
	}))

	log.Fatal(app.Listen(":3000"))
}

// The README shows how RunOptions choose the model of a run per request.
func Test_RunOptions_ChooseTheModelPerRequest(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](agenttest.Answering("the model of the agent"))
	pro := ai.WithRunModel(agenttest.Answering("the model of the plan"))
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
