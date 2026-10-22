// Package pydanticaimcp serves a pydantic-ai-go agent as an MCP server from Fiber.
//
// New returns the handler of one route. It answers MCP over Streamable HTTP
// without sessions, and offers one tool that runs the agent with the question
// of the caller:
//
//	app.Use("/mcp", authenticate) // stores the user in c.Locals
//	app.Post("/mcp", pydanticaimcp.New(agent, pydanticaimcp.Config[Deps]{
//		Deps: func(c fiber.Ctx) (Deps, error) {
//			return Deps{UserID: fiber.Locals[string](c, "user")}, nil
//		},
//	}))
//
// The handler lives in a package of its own, so that an application that does
// not serve MCP does not build the MCP SDK.
package pydanticaimcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultName        = "pydantic-ai-go"
	defaultVersion     = "1.0.0"
	defaultTool        = "ask"
	defaultDescription = "Ask the agent a question"
)

// toolName is what MCP allows as the name of a tool.
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// errDeferred reports a run that stopped for approvals or external tool results.
var errDeferred = errors.New("the run ended with deferred tool calls, which a tool call cannot resume")

// Config defines the config of the handler.
type Config[Deps any] struct {
	// Next defines a function to skip this handler when it returns true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// Deps resolves the dependencies of the agent for one request. Derive them
	// from the authenticated request, for example from c.Locals, and never from
	// the arguments of the tool: a caller can send any argument. An error is
	// returned from the handler as it is, before the MCP message is read, so
	// fiber.ErrUnauthorized answers the HTTP request with a 401.
	//
	// It is called for every MCP message that the route receives, including
	// those that set up a connection or list the tool, so keep it cheap.
	//
	// Optional. Default: the zero value of Deps
	Deps func(c fiber.Ctx) (Deps, error)

	// RunOptions returns additional agent run options for one request, such as
	// ai.WithRunUsageLimits or ai.WithRunMetadata.
	//
	// Optional. Default: nil
	RunOptions func(c fiber.Ctx) ([]ai.RunOption, error)

	// Name and Version identify the server to its clients.
	//
	// Optional. Default: the name of the agent, or "pydantic-ai-go", and "1.0.0"
	Name    string
	Version string

	// Tool is the name of the tool that runs the agent. It has to be 1 to 128
	// characters of A-Z, a-z, 0-9, underscore, dot and hyphen. Description tells
	// the model of the client what the tool is for, so describe the agent.
	//
	// Optional. Default: "ask" and "Ask the agent a question"
	Tool        string
	Description string

	// ErrorMessage returns the text that the client receives in the result of a
	// failed run. Use it to keep provider error details, which can carry request
	// IDs or parts of credentials, away from the client. It also applies when the
	// output of the agent cannot be encoded, and when the run panics, with an
	// error whose text is "internal error".
	//
	// Optional. Default: err.Error()
	ErrorMessage func(err error) string

	// OnRunEnd is called once the agent has run, with the dependencies of the
	// run, what the run consumed and the error that ended it, which is nil when
	// the run completed. It is called for a run that failed or panicked as well,
	// which makes it the place to record token usage. run is nil when the agent
	// did not start the run. A panic in OnRunEnd is logged and recovered.
	//
	// Optional. Default: nil
	OnRunEnd func(deps Deps, run *ai.RunInfo, err error)

	// Timeout bounds the duration of one run. A run that exceeds it fails with
	// the context error. A client that went away is not noticed before the
	// response is written, so a timeout keeps the run of such a client from
	// going on for as long as the agent likes: set one.
	//
	// Optional. Default: 0 (no timeout)
	Timeout time.Duration

	// CancelOnShutdown cancels the runs of the handler when the server starts
	// shutting down, and the calls end with an error that matches
	// ai.ErrRunCancelled. Without it a shutdown waits for the runs to end on
	// their own, until the timeout of the shutdown elapses.
	//
	// Optional. Default: false
	CancelOnShutdown bool
}

// askInput is the input of the tool.
type askInput struct {
	Question string `json:"question" jsonschema:"The question or the task for the agent"`
}

// New returns a handler that serves agent as an MCP server.
//
// There are no sessions: every request is a connection of its own. That keeps
// the handler free of state, and answers each request with one JSON response
// instead of a stream, so a server that is shut down never waits for an idle
// connection. A client that wants notifications from the server gets none,
// because nothing in this server changes.
//
// It panics when agent is nil or the name of the tool is not valid.
func New[Deps, Output any](agent *ai.Agent[Deps, Output], config ...Config[Deps]) fiber.Handler {
	if agent == nil {
		panic("pydanticaimcp: agent must not be nil")
	}
	var cfg Config[Deps]
	if len(config) > 0 {
		cfg = config[0]
	}
	tool := &mcpsdk.Tool{
		Name:        cmp.Or(cfg.Tool, defaultTool),
		Description: cmp.Or(cfg.Description, defaultDescription),
	}
	if !toolName.MatchString(tool.Name) {
		panic(fmt.Sprintf("pydanticaimcp: invalid tool name %q", tool.Name))
	}
	implementation := &mcpsdk.Implementation{
		Name:    cmp.Or(cfg.Name, agent.Name(), defaultName),
		Version: cmp.Or(cfg.Version, defaultVersion),
	}
	serverOptions := &mcpsdk.ServerOptions{
		// Only the tool, and no notifications about it: a client that subscribes
		// to them would keep a request open for as long as it likes.
		Capabilities: &mcpsdk.ServerCapabilities{Tools: &mcpsdk.ToolCapabilities{}},
		SchemaCache:  mcpsdk.NewSchemaCache(),
	}
	httpOptions := &mcpsdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true}

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		deps, options, err := agentrun.Resolve(c, cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}

		var shutdown <-chan struct{}
		if cfg.CancelOnShutdown {
			shutdown = agentrun.ServerDone(c.RequestCtx())
		}

		// A call is served with the values of the context of the request, such as
		// a trace, and without its cancellation. The context that the SDK would
		// get otherwise is the pooled RequestCtx of fasthttp. A tool that reads it
		// after the call would see the values of another request, and fasthttp
		// closes its done channel when the server starts shutting down, which the
		// handler of MCP takes for a client that went away and answers with
		// nothing. A call that is still running is answered as usual, and
		// CancelOnShutdown decides whether it is stopped.
		values := context.WithoutCancel(c.Context())

		// The tool is made for the request, as it runs the agent with the
		// dependencies of the request.
		server := mcpsdk.NewServer(implementation, serverOptions)
		mcpsdk.AddTool(server, tool, call(agent, cfg, deps, options, shutdown))
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, httpOptions)
		return adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler.ServeHTTP(w, r.WithContext(values))
		}))(c)
	}
}

// call returns the handler of the tool. It runs on a goroutine of the MCP
// server, where no recover middleware guards it, so a panic ends the call and
// not the process.
func call[Deps, Output any](
	agent *ai.Agent[Deps, Output], cfg Config[Deps], deps Deps, options []ai.RunOption, shutdown <-chan struct{},
) mcpsdk.ToolHandlerFor[askInput, any] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, in askInput) (result *mcpsdk.CallToolResult, out any, err error) {
		guard, options := agentrun.NewGuard(options, deps, cfg.OnRunEnd)
		defer func() {
			if recovered := recover(); recovered != nil {
				guard.End(agentrun.Recovered(recovered))
				result, out, err = nil, nil, errors.New(agentrun.Text(cfg.ErrorMessage, agentrun.ErrPanicked))
			}
		}()

		// strings.TrimSpace and not utils.TrimSpace: a question of Unicode spaces is
		// as empty as one of ASCII spaces.
		if strings.TrimSpace(in.Question) == "" {
			return nil, nil, errors.New("the question must not be empty")
		}
		if shutdown != nil {
			var cancel context.CancelCauseFunc
			ctx, cancel = context.WithCancelCause(ctx)
			defer cancel(nil)
			go func(done <-chan struct{}) { // done of this context, not of the one that Timeout makes
				select {
				case <-shutdown:
					cancel(agentrun.ErrShutdown)
				case <-done:
				}
			}(ctx.Done())
		}
		if cfg.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()
		}

		run, runErr := agent.Run(ctx, in.Question, deps, options...)
		panicked := false
		if panicErr := guard.Err(); panicErr != nil && runErr != nil {
			// The error of the run carries the value of the panic, or the error
			// of a tool that the panic canceled.
			runErr, panicked = panicErr, true
		}
		if runErr == nil && run.Deferred() != nil {
			runErr = errDeferred
		}
		if runErr != nil && errors.Is(context.Cause(ctx), agentrun.ErrShutdown) &&
			(errors.Is(runErr, ai.ErrRunCancelled) || errors.Is(runErr, context.Canceled)) {
			runErr = agentrun.ErrShutdown // say why the run was canceled
		}
		guard.End(runErr)
		if panicked {
			return nil, nil, errors.New(agentrun.Text(cfg.ErrorMessage, agentrun.ErrPanicked))
		}
		if runErr != nil {
			return nil, nil, errors.New(agentrun.Text(cfg.ErrorMessage, runErr))
		}
		result, out, err = output(run.Output)
		if err != nil {
			return nil, nil, errors.New(agentrun.Text(cfg.ErrorMessage, err))
		}
		return result, out, nil
	}
}

// output returns the result of the tool for the output of the agent. Text goes
// to the client as it is. Anything else goes as JSON text, and as structured
// content when it is an object, which every version of MCP allows.
func output[Output any](value Output) (*mcpsdk.CallToolResult, any, error) {
	if reflected := reflect.ValueOf(value); reflected.Kind() == reflect.String {
		return text(reflected.String()), nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the output of the agent: %w", err)
	}
	result := text(string(encoded))
	if bytes.HasPrefix(encoded, []byte("{")) {
		return result, json.RawMessage(encoded), nil
	}
	return result, nil, nil
}

func text(content string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: content}}}
}
