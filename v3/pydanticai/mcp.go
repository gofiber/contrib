package pydanticai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/utils/v2"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMCPName        = "pydantic-ai-go"
	defaultMCPVersion     = "1.0.0"
	defaultMCPTool        = "ask"
	defaultMCPDescription = "Ask the agent a question"
)

// mcpToolName is what MCP allows as the name of a tool.
var mcpToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// errMCPDeferred reports a run that stopped for approvals or external tool results.
var errMCPDeferred = errors.New("the run ended with deferred tool calls, which a tool call cannot resume")

// MCPConfig defines the config of the MCP handler.
type MCPConfig[Deps any] struct {
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

	// AllowedOrigins lists the origins of the browser pages that may call the
	// handler, as the browsers send them: a scheme, a host and a port that is not
	// the default of the scheme, for example "https://app.example.com". A request
	// with an Origin header that is not listed is answered with 403 Forbidden
	// before anything else happens, as the MCP specification has a server
	// validate the origin of its callers: that keeps a page of another site, or
	// one that reaches the server by DNS rebinding and so looks like its own,
	// from calling the tool in the browser of a user. A client that is not a
	// browser sends no Origin header and is not affected. The cors middleware
	// answers the preflight request of a browser, and has to allow the same
	// origins.
	//
	// It panics when an origin is not a scheme, a host and an optional port.
	//
	// Optional. Default: nil (no browser page may call the handler)
	AllowedOrigins []string

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
	// response is written, so the timeout is what ends the run of such a client.
	// A negative value turns it off.
	//
	// Optional. Default: 5 * time.Minute
	Timeout time.Duration

	// CancelOnShutdown cancels the runs of the handler when the server starts
	// shutting down, and the calls end with an error that matches
	// ai.ErrRunCancelled. Without it a shutdown waits for the runs to end on
	// their own, until the timeout of the shutdown elapses.
	//
	// Optional. Default: false
	CancelOnShutdown bool
}

// mcpAskInput is the input of the tool.
type mcpAskInput struct {
	Question string `json:"question" jsonschema:"The question or the task for the agent"`
}

// MCP returns a handler that serves agent as an MCP server over Streamable HTTP.
// It offers one tool, which runs the agent with the question of the caller:
//
//	app.Use("/mcp", authenticate) // stores the user in c.Locals
//	app.Post("/mcp", pydanticai.MCP(agent, pydanticai.MCPConfig[Deps]{
//		Deps: func(c fiber.Ctx) (Deps, error) {
//			return Deps{UserID: fiber.Locals[string](c, "user")}, nil
//		},
//	}))
//
// There are no sessions: every request is a connection of its own. That keeps
// the handler free of state, and answers each request with one JSON response
// instead of a stream, so a server that is shut down never waits for an idle
// connection. A client that wants notifications from the server gets none,
// because nothing in this server changes.
//
// A request that carries an Origin header is answered with 403 unless
// MCPConfig.AllowedOrigins lists it.
//
// It panics when agent is nil, the name of the tool is not valid or an allowed
// origin is not an origin.
func MCP[Deps, Output any](agent *ai.Agent[Deps, Output], config ...MCPConfig[Deps]) fiber.Handler {
	if agent == nil {
		panic("pydanticai: agent must not be nil")
	}
	var cfg MCPConfig[Deps]
	if len(config) > 0 {
		cfg = config[0]
	}
	tool := &mcpsdk.Tool{
		Name:        cmp.Or(cfg.Tool, defaultMCPTool),
		Description: cmp.Or(cfg.Description, defaultMCPDescription),
	}
	if !mcpToolName.MatchString(tool.Name) {
		panic(fmt.Sprintf("pydanticai: invalid MCP tool name %q", tool.Name))
	}
	origins := mcpOrigins(cfg.AllowedOrigins)
	implementation := &mcpsdk.Implementation{
		Name:    cmp.Or(cfg.Name, agent.Name(), defaultMCPName),
		Version: cmp.Or(cfg.Version, defaultMCPVersion),
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
		if origin := c.Get(fiber.HeaderOrigin); origin != "" && !mcpOriginAllowed(origins, origin) {
			return fiber.NewError(fiber.StatusForbidden, "forbidden origin")
		}

		deps, options, err := resolve(c, cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}

		var shutdown <-chan struct{}
		if cfg.CancelOnShutdown {
			shutdown = serverDone(c.RequestCtx())
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
		mcpsdk.AddTool(server, tool, mcpTool(agent, cfg, deps, options, shutdown))
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, httpOptions)
		return adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler.ServeHTTP(w, r.WithContext(values))
		}))(c)
	}
}

// mcpOrigins returns the form of the allowed origins that a browser sends: the
// scheme and the host in lower case, and no port when it is the default of the
// scheme. It panics when one of them is not an origin, which no browser would
// send, so that it would allow nobody.
func mcpOrigins(allowed []string) []string {
	origins := make([]string, len(allowed))
	for i, origin := range allowed {
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			panic(fmt.Sprintf("pydanticai: invalid MCP origin %q, want a scheme, a host and a port", origin))
		}
		host := parsed.Host
		if port := parsed.Port(); (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
			host = strings.TrimSuffix(host, ":"+port)
		}
		origins[i] = strings.ToLower(parsed.Scheme + "://" + host)
	}
	return origins
}

// mcpOriginAllowed reports whether origin, the Origin header of a request, is
// one of the allowed origins.
func mcpOriginAllowed(allowed []string, origin string) bool {
	for _, candidate := range allowed {
		if utils.EqualFold(candidate, origin) {
			return true
		}
	}
	return false
}

// mcpTool returns the handler of the tool. It runs on a goroutine of the MCP
// server, where no recover middleware guards it, so a panic ends the call and
// not the process.
func mcpTool[Deps, Output any](
	agent *ai.Agent[Deps, Output], cfg MCPConfig[Deps], deps Deps, options []ai.RunOption, shutdown <-chan struct{},
) mcpsdk.ToolHandlerFor[mcpAskInput, any] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, in mcpAskInput) (result *mcpsdk.CallToolResult, out any, err error) {
		defer agentrun.Recover(func(panicErr error) {
			result, out, err = nil, nil, errors.New(agentrun.Text(cfg.ErrorMessage, panicErr))
		})

		if utils.TrimSpace(in.Question) == "" {
			return nil, nil, errors.New("the question must not be empty")
		}
		run, runErr := agentrun.Execute(ctx, agent, in.Question, options, agentrun.Config[Deps]{
			Deps:     deps,
			OnRunEnd: cfg.OnRunEnd,
			Timeout:  agentrun.OneShotTimeout(cfg.Timeout),
			Shutdown: shutdown,
			Deferred: errMCPDeferred,
		})
		if runErr != nil {
			return nil, nil, errors.New(agentrun.Text(cfg.ErrorMessage, runErr))
		}
		result, out, err = mcpOutput(run.Output)
		if err != nil {
			return nil, nil, errors.New(agentrun.Text(cfg.ErrorMessage, err))
		}
		return result, out, nil
	}
}

// mcpOutput returns the result of the tool for the output of the agent. Text
// goes to the client as it is. Anything else goes as JSON text, and as
// structured content when it is an object, which every version of MCP allows.
func mcpOutput[Output any](value Output) (*mcpsdk.CallToolResult, any, error) {
	if reflected := reflect.ValueOf(value); reflected.Kind() == reflect.String {
		return mcpText(reflected.String()), nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the output of the agent: %w", err)
	}
	result := mcpText(string(encoded))
	if bytes.HasPrefix(encoded, []byte("{")) {
		return result, json.RawMessage(encoded), nil
	}
	return result, nil, nil
}

func mcpText(content string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: content}}}
}
