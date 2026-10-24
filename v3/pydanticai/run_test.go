package pydanticai_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

type weather struct {
	City  string `json:"city"`
	TempC int    `json:"temp_c"`
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

	app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel())))

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

	app := newApp("/ask", pydanticai.Run(weatherOutputAgent()))

	_, body := post(t, app, "/ask", runBody)
	output, err := json.Marshal(decodeObject(t, body)["output"])
	require.NoError(t, err)
	require.JSONEq(t, `{"city":"Paris","temp_c":21}`, string(output))
}

func Test_Run_ConversationID(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	app := newApp("/ask", pydanticai.Run(agent))

	_, body := post(t, app, "/ask", `{"prompt":"Hello","conversation_id":"conversation-1"}`)
	require.Equal(t, "conversation-1", decodeObject(t, body)["conversation_id"])

	// Options chosen by the server win over what the client sent.
	app = newApp("/ask", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{
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
	app.Post("/ask", pydanticai.Run(userAgent(), pydanticai.RunConfig[userDeps, string]{
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

	app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.RunConfig[struct{}, string]{
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

func Test_Run_RejectsEncodedBodiesBeforeResolvingDeps(t *testing.T) {
	t.Parallel()

	var resolved atomic.Int32
	app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.RunConfig[struct{}, string]{
		Deps: func(fiber.Ctx) (struct{}, error) {
			resolved.Add(1)
			return struct{}{}, nil
		},
	}))
	checkEncodedBodies(t, app, "/ask", runBody, &resolved)
}

func Test_Run_MaxRequestBytes(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	app := newApp("/ask", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{
		MaxRequestBytes: int64(len(runBody)),
	}))

	resp, _ := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "a body at the limit is accepted")

	resp, _ = post(t, app, "/ask", runBody+" ")
	require.Equal(t, fiber.StatusRequestEntityTooLarge, resp.StatusCode, "a body one byte above the limit is not")

	require.Panics(t, func() {
		pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{MaxRequestBytes: -1})
	})
}

func Test_Run_CustomPrompt(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Get("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](echoModel()), pydanticai.RunConfig[struct{}, string]{
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

func Test_Run_CustomPromptResolvesDepsFirst(t *testing.T) {
	t.Parallel()

	model, called := unreachableModel()
	var prompted atomic.Bool
	app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](model), pydanticai.RunConfig[struct{}, string]{
		Deps: func(fiber.Ctx) (struct{}, error) { return struct{}{}, fiber.ErrUnauthorized },
		Prompt: func(fiber.Ctx) (string, error) {
			prompted.Store(true)
			return "Hello", nil
		},
	}))

	resp, _ := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	require.False(t, prompted.Load(), "a caller that is turned away does not get its request read")
	require.False(t, called.Load())
}

func Test_Run_CustomPromptOwnsTheRequest(t *testing.T) {
	t.Parallel()

	// A Prompt function replaces every request check of the default path, as documented.
	app := fiber.New()
	app.All("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](fakes.NewTestModel()), pydanticai.RunConfig[struct{}, string]{
		Prompt: func(c fiber.Ctx) (string, error) { return string(c.Body()), nil },
	}))

	for _, method := range []string{fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete} {
		resp, _ := do(t, app, method, "/ask", fiber.MIMETextPlain, "Hello")
		require.Equal(t, fiber.StatusOK, resp.StatusCode, method)
	}
}

func Test_Run_CustomRespond(t *testing.T) {
	t.Parallel()

	app := newApp("/ask", pydanticai.Run(weatherOutputAgent(), pydanticai.RunConfig[struct{}, weather]{
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

	var log errorLog
	app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](agenttest.Failing(secret))), log.config())

	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, "Internal Server Error", body, "the error of the provider must not reach the client")

	// The cause stays available to the error handler.
	runErr, ok := errors.AsType[*pydanticai.Error](log.err())
	require.True(t, ok)
	require.Equal(t, fiber.StatusInternalServerError, runErr.Code)
	require.Equal(t, pydanticai.StatusCode(runErr.Err), runErr.Code, "Code is what StatusCode derives from Err")
	require.ErrorIs(t, runErr.Err, secret)
	require.ErrorIs(t, log.err(), secret)
	fiberErr, ok := errors.AsType[*fiber.Error](log.err())
	require.True(t, ok)
	require.Equal(t, fiber.StatusInternalServerError, fiberErr.Code)
}

func Test_Run_UsageLimitIsTooManyRequests(t *testing.T) {
	t.Parallel()

	// The tool makes the run need a second model request.
	var log errorLog
	app := newApp("/ask", pydanticai.Run(weatherAgent(), pydanticai.RunConfig[struct{}, string]{
		RunOptions: func(fiber.Ctx) ([]ai.RunOption, error) {
			return []ai.RunOption{ai.WithRunUsageLimits(ai.UsageLimits{RequestLimit: 1})}, nil
		},
	}), log.config())

	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "Too Many Requests", body)
	require.ErrorIs(t, log.err(), fiber.ErrTooManyRequests, "errors.Is matches the status of a Fiber error")
	require.ErrorIs(t, log.err(), ai.ErrUsageLimitExceeded)
}

func Test_Run_Timeout(t *testing.T) {
	t.Parallel()

	model, _, canceled := agenttest.Blocking()
	app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](model), pydanticai.RunConfig[struct{}, string]{
		Timeout: 100 * time.Millisecond,
	}))

	resp, body := post(t, app, "/ask", runBody)
	require.Equal(t, fiber.StatusGatewayTimeout, resp.StatusCode)
	require.Equal(t, "Gateway Timeout", body)
	require.ErrorIs(t, agenttest.Receive(t, canceled, "the cancellation of the model call"), context.DeadlineExceeded)
}

// Run cannot tell that its client left, so a run is bounded unless the
// timeout is turned off.
func Test_Run_TimeoutDefaults(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    time.Duration // the deadline of the run, 0 for none
	}{
		{name: "unset is five minutes", want: 5 * time.Minute},
		{name: "a negative value turns it off", timeout: -time.Second},
		{name: "a positive value is used as it is", timeout: 30 * time.Second, want: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			model, deadlines := agenttest.DeadlineProbe()
			app := newApp("/ask", pydanticai.Run(ai.NewAgent[struct{}, string](model), pydanticai.RunConfig[struct{}, string]{
				Timeout: tc.timeout,
			}))

			resp, _ := post(t, app, "/ask", runBody)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
			agenttest.Receive(t, deadlines, "the model call").Require(t, tc.want)
		})
	}
}

func Test_Run_DeferredToolCallsAreNotImplemented(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	ai.AddSimpleTool(agent, "delete", func(context.Context, struct{}) (string, error) {
		return "deleted", nil
	}, ai.WithApprovalRequired())

	t.Run("default response", func(t *testing.T) {
		t.Parallel()

		var log errorLog
		app := newApp("/ask", pydanticai.Run(agent), log.config())

		resp, body := post(t, app, "/ask", runBody)
		require.Equal(t, fiber.StatusNotImplemented, resp.StatusCode)
		require.Equal(t, "Not Implemented", body)
		require.ErrorContains(t, log.err(), "Not Implemented")
		runErr, ok := errors.AsType[*pydanticai.Error](log.err())
		require.True(t, ok)
		require.ErrorContains(t, runErr.Err, "AGUI or Vercel")
		require.Equal(t, pydanticai.StatusCode(runErr.Err), runErr.Code, "Code is what StatusCode derives from Err")
	})

	t.Run("Respond receives the deferred run", func(t *testing.T) {
		t.Parallel()

		app := newApp("/ask", pydanticai.Run(agent, pydanticai.RunConfig[struct{}, string]{
			Respond: func(c fiber.Ctx, result *ai.RunResult[string]) error {
				deferred := result.Deferred()
				if deferred == nil {
					return errors.New("the run did not defer its tool call")
				}
				return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"approvals": len(deferred.Approvals)})
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
