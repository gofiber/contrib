package pydanticai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

type weather struct {
	City  string `json:"city"`
	TempC int    `json:"temp_c"`
}

func runApp(handler fiber.Handler, config ...fiber.Config) *fiber.App {
	app := fiber.New(config...)
	app.Post("/ask", handler)
	return app
}

// weatherOutputAgent answers with a structured output.
func weatherOutputAgent() *ai.Agent[struct{}, weather] {
	model := fakes.NewTestModel()
	model.CustomOutputArgs = json.RawMessage(`{"city":"Paris","temp_c":21}`)
	return ai.NewAgent[struct{}, weather](model)
}

// decodeObject decodes a JSON object response.
func decodeObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var object map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &object), body)
	return object
}

func Test_Run_ReturnsOutput(t *testing.T) {
	t.Parallel()

	app := runApp(pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel())))

	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON)

	response := decodeObject(t, body)
	require.Equal(t, "success (no tool calls)", response["output"])
	require.NotEmpty(t, response["run_id"])
	require.NotEmpty(t, response["conversation_id"], "the agent generates a conversation ID")
	require.NotContains(t, response, "usage", "usage and cost stay on the server unless Respond sends them")
	require.NotContains(t, response, "messages", "the history stays on the server")
}

func Test_Run_StructuredOutput(t *testing.T) {
	t.Parallel()

	app := runApp(pydanticai.Run(weatherOutputAgent()))

	_, body := post(t, app, "/ask", runBody)
	require.JSONEq(t, `{"city":"Paris","temp_c":21}`, mustMarshal(t, decodeObject(t, body)["output"]))
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func Test_Run_ConversationID(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	app := runApp(pydanticai.Run(agent))

	_, body := post(t, app, "/ask", `{"prompt":"Hello","conversation_id":"conversation-1"}`)
	require.Equal(t, "conversation-1", decodeObject(t, body)["conversation_id"])

	// Options chosen by the server win over what the client sent.
	app = runApp(pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{
		RunOptions: func(fiber.Ctx) ([]ai.RunOption, error) {
			return []ai.RunOption{ai.WithConversationID("server-chosen")}, nil
		},
	}))
	_, body = post(t, app, "/ask", `{"prompt":"Hello","conversation_id":"conversation-1"}`)
	require.Equal(t, "server-chosen", decodeObject(t, body)["conversation_id"])
}

func Test_Run_DepsOptionsAndContext(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxKey{}, "trace-1"))
		return c.Next()
	})
	app.Post("/ask", pydanticai.Run(userAgent(t), pydanticai.RunConfig[userDeps, string]{
		Deps: func(c fiber.Ctx) (userDeps, error) {
			if c.Query("user") == "" {
				return userDeps{}, fiber.ErrUnauthorized
			}
			return userDeps{User: c.Query("user")}, nil
		},
		RunOptions: func(fiber.Ctx) ([]ai.RunOption, error) {
			return []ai.RunOption{ai.WithRunInstructions("from-options")}, nil
		},
	}))

	_, body := post(t, app, "/ask?user=alice", runBody)
	output, _ := decodeObject(t, body)["output"].(string)
	require.Contains(t, output, "user=alice", "the dependencies of the request")
	require.Contains(t, output, "from-options", "the run options of the request")
	require.True(t, strings.HasSuffix(output, "|trace-1"), "the context of the request: %q", output)

	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, "Unauthorized", body)

	// A caller that is turned away does not get its payload parsed.
	resp, _ = post(t, app, "/ask", "{")
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func Test_Run_RunOptionsError(t *testing.T) {
	t.Parallel()

	app := runApp(pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.RunConfig[struct{}, string]{
		RunOptions: func(fiber.Ctx) ([]ai.RunOption, error) { return nil, fiber.ErrForbidden },
	}))

	resp, _ := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func Test_Run_RejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())

	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		status      int
	}{
		{"missing prompt", fiber.MethodPost, fiber.MIMEApplicationJSON, `{}`, fiber.StatusBadRequest},
		{"empty prompt", fiber.MethodPost, fiber.MIMEApplicationJSON, `{"prompt":""}`, fiber.StatusBadRequest},
		{"invalid json", fiber.MethodPost, fiber.MIMEApplicationJSON, `{`, fiber.StatusBadRequest},
		{"wrong content type", fiber.MethodPost, fiber.MIMETextPlain, runBody, fiber.StatusUnsupportedMediaType},
		{"missing content type", fiber.MethodPost, "", runBody, fiber.StatusUnsupportedMediaType},
		{"method not allowed", fiber.MethodGet, fiber.MIMEApplicationJSON, "", fiber.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.All("/ask", pydanticai.Run(agent))

			resp, _ := do(t, app, test.method, "/ask", test.contentType, test.body)
			require.Equal(t, test.status, resp.StatusCode)
			if test.status == fiber.StatusMethodNotAllowed {
				require.Equal(t, fiber.MethodPost, resp.Header.Get(fiber.HeaderAllow))
			}
		})
	}
}

func Test_Run_Next(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Post("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel()),
		pydanticai.RunConfig[struct{}, string]{
			Next: func(c fiber.Ctx) bool { return c.Query("skip") == "1" },
		}), func(c fiber.Ctx) error {
		return c.SendString("skipped")
	})

	_, body := post(t, app, "/ask?skip=1", runBody)
	require.Equal(t, "skipped", body)

	_, body = post(t, app, "/ask", runBody)
	require.Contains(t, body, `"output"`)
}

func Test_Run_CustomPrompt(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Get("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](echoModel(t)), pydanticai.RunConfig[struct{}, string]{
		Prompt: func(c fiber.Ctx) (string, error) {
			if c.Query("q") == "" {
				return "", fiber.NewError(fiber.StatusBadRequest, "q is required")
			}
			return c.Query("q"), nil
		},
		RunOptions: func(fiber.Ctx) ([]ai.RunOption, error) {
			return []ai.RunOption{ai.WithRunInstructions("custom")}, nil
		},
	}))

	resp, body := do(t, app, fiber.MethodGet, "/ask?q=hello", "", "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, "custom", decodeObject(t, body)["output"])

	resp, body = do(t, app, fiber.MethodGet, "/ask", "", "")
	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "q is required", body)
}

func Test_Run_CustomRespond(t *testing.T) {
	t.Parallel()

	app := runApp(pydanticai.Run(weatherOutputAgent(), pydanticai.RunConfig[struct{}, weather]{
		Respond: func(c fiber.Ctx, result *ai.RunResult[weather]) error {
			return c.JSON(fiber.Map{"city": result.Output.City, "requests": result.Usage().Requests})
		},
	}))

	_, body := post(t, app, "/ask", runBody)
	require.JSONEq(t, `{"city":"Paris","requests":1}`, body)
}

func Test_Run_AgentErrorsAreMappedAndHidden(t *testing.T) {
	t.Parallel()

	secret := errors.New("provider rejected the key sk-secret")

	var captured error
	app := runApp(pydanticai.Run(ai.NewAgent[struct{}, string](failingModel(secret))), fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			captured = err
			return fiber.DefaultErrorHandler(c, err)
		},
	})

	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, "Internal Server Error", body, "the error of the provider must not reach the client")

	// The cause stays available to the error handler.
	var runErr *pydanticai.Error
	require.ErrorAs(t, captured, &runErr)
	require.Equal(t, fiber.StatusInternalServerError, runErr.Code)
	require.ErrorIs(t, runErr.Err, secret)
	require.ErrorIs(t, captured, secret)
	var fiberErr *fiber.Error
	require.ErrorAs(t, captured, &fiberErr)
	require.Equal(t, fiber.StatusInternalServerError, fiberErr.Code)
}

func Test_Run_UsageLimitIsTooManyRequests(t *testing.T) {
	t.Parallel()

	// The tool makes the run need a second model request.
	app := runApp(pydanticai.Run(weatherAgent(), pydanticai.RunConfig[struct{}, string]{
		RunOptions: func(fiber.Ctx) ([]ai.RunOption, error) {
			return []ai.RunOption{ai.WithRunUsageLimits(ai.UsageLimits{RequestLimit: 1})}, nil
		},
	}))

	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "Too Many Requests", body)
}

func Test_Run_Timeout(t *testing.T) {
	t.Parallel()

	model, _, canceled := blockingModel()
	app := runApp(pydanticai.Run(ai.NewAgent[struct{}, string](model), pydanticai.RunConfig[struct{}, string]{
		Timeout: 100 * time.Millisecond,
	}))

	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusGatewayTimeout, resp.StatusCode)
	require.Equal(t, "Gateway Timeout", body)
	require.ErrorIs(t, receive(t, canceled, "the cancellation of the model call"), context.DeadlineExceeded)
}

func Test_Run_DeferredToolCallsAreNotImplemented(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "delete", func(context.Context, struct{}) (string, error) {
		return "deleted", nil
	}, ai.WithApprovalRequired())

	t.Run("default response", func(t *testing.T) {
		t.Parallel()

		var captured error
		app := runApp(pydanticai.Run(agent), fiber.Config{
			ErrorHandler: func(c fiber.Ctx, err error) error {
				captured = err
				return fiber.DefaultErrorHandler(c, err)
			},
		})

		resp, body := post(t, app, "/ask", runBody)
		require.Equal(t, fiber.StatusNotImplemented, resp.StatusCode)
		require.Equal(t, "Not Implemented", body)
		require.ErrorContains(t, captured, "Not Implemented")
		var runErr *pydanticai.Error
		require.ErrorAs(t, captured, &runErr)
		require.ErrorContains(t, runErr.Err, "AGUI or Vercel")
	})

	t.Run("Respond receives the deferred run", func(t *testing.T) {
		t.Parallel()

		app := runApp(pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{
			Respond: func(c fiber.Ctx, result *ai.RunResult[string]) error {
				deferred := result.Deferred()
				require.NotNil(t, deferred)
				return c.Status(http.StatusAccepted).JSON(fiber.Map{"approvals": len(deferred.Approvals)})
			},
		}))

		resp, body := post(t, app, "/ask", runBody)
		require.Equal(t, fiber.StatusAccepted, resp.StatusCode)
		require.JSONEq(t, `{"approvals":1}`, body)
	})
}

func Test_Run_Panics(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() { pydanticai.Run[struct{}, string](nil) })
}
