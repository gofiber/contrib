package pydanticaimcp_test

import (
	"log"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/openai"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaimcp"
	"github.com/gofiber/fiber/v3"
)

type supportDeps struct{ UserID string }

// New serves an agent to MCP clients at one route. The route is authenticated
// by the middleware in front of it, and the dependencies of the agent are those
// of the authenticated user.
func ExampleNew() {
	agent := ai.NewAgent[supportDeps, string](openai.NewModel("gpt-5-mini"))

	app := fiber.New()
	app.Use("/mcp", func(c fiber.Ctx) error { // your authentication middleware
		c.Locals("user", c.Get("X-User"))
		return c.Next()
	})
	app.Post("/mcp", pydanticaimcp.New(agent, pydanticaimcp.Config[supportDeps]{
		Deps: func(c fiber.Ctx) (supportDeps, error) {
			return supportDeps{UserID: fiber.Locals[string](c, "user")}, nil
		},
		Description: "Ask the support agent about orders and refunds.",
		Timeout:     2 * time.Minute,
	}))

	log.Fatal(app.Listen(":3000"))
}
