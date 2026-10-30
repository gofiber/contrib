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

var mcpToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

var errMCPDeferred = errors.New("the run ended with deferred tool calls, which a tool call cannot resume")

// MCPConfig defines the config of the MCP handler.
type MCPConfig[Deps any] struct {
	// Next defines a function to skip this handler when it returns true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// Deps resolves the dependencies of the agent for one request: derive them from
	// the authenticated request, never from the arguments of the tool. An error is
	// returned as it is, before the message is read. It runs for every message of the
	// route, so keep it cheap.
	//
	// Optional. Default: the zero value of Deps
	Deps func(c fiber.Ctx) (Deps, error)

	// RunOptions returns additional agent run options for one request.
	//
	// Optional. Default: nil
	RunOptions func(c fiber.Ctx) ([]ai.RunOption, error)

	// AllowedOrigins lists the origins of the browser pages that may call the handler,
	// as browsers send them, for example "https://app.example.com". A request with
	// another Origin header gets 403. It panics on a malformed origin.
	//
	// Optional. Default: nil (no browser page may call the handler)
	AllowedOrigins []string

	// Name and Version identify the server to its clients.
	//
	// Optional. Default: the name of the agent, or "pydantic-ai-go", and "1.0.0"
	Name    string
	Version string

	// Tool is the name of the tool that runs the agent, 1 to 128 characters of
	// A-Z, a-z, 0-9, _, . and -. Description tells the model what it is for.
	//
	// Optional. Default: "ask" and "Ask the agent a question"
	Tool        string
	Description string

	// ErrorMessage returns the text that the client receives for a failed run, to keep
	// provider details away from it. It also applies to panics and encoding errors.
	//
	// Optional. Default: err.Error()
	ErrorMessage func(err error) string

	// OnRunEnd is called once the agent has run, with what the run consumed and the
	// error that ended it: the place to record token usage. run is nil if the agent
	// did not start the run. A panic in it is logged and recovered.
	//
	// Optional. Default: nil
	OnRunEnd func(deps Deps, run *ai.RunInfo, err error)

	// Timeout bounds the duration of one run, which fails with the context error. A
	// negative value turns it off.
	//
	// Optional. Default: 5 * time.Minute
	Timeout time.Duration

	// CancelOnShutdown cancels the runs when the server starts shutting down, with
	// an error that matches ai.ErrRunCancelled. Without it the shutdown waits for them.
	//
	// Optional. Default: false
	CancelOnShutdown bool
}

func (cfg *MCPConfig[Deps]) failure(err error) error {
	return errors.New(agentrun.Text(cfg.ErrorMessage, err))
}

type mcpAskInput struct {
	Question string `json:"question" jsonschema:"The question or the task for the agent"`
}

// MCP returns a handler that serves agent as an MCP server over Streamable HTTP,
// with one tool that runs the agent with the question of the caller (see
// ExampleMCP). It has no sessions: each request is answered with one JSON
// response. It panics when agent is nil, the tool name is invalid or an allowed
// origin is malformed.
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
		// No notifications: a subscribing client would keep a request open.
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

		shutdown := shutdownSignal(c, cfg.CancelOnShutdown)

		// Tools get the values of the request context, not its cancellation or the pooled
		// RequestCtx: fasthttp closes it on shutdown, which the SDK takes for a client
		// that left. CancelOnShutdown stops runs instead.
		values := context.WithoutCancel(c.Context())

		server := mcpsdk.NewServer(implementation, serverOptions)
		mcpsdk.AddTool(server, tool, mcpTool(agent, cfg, deps, options, shutdown))
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, httpOptions)
		return adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler.ServeHTTP(w, r.WithContext(values))
		}))(c)
	}
}

// mcpOrigins returns the allowed origins as browsers send them (lower case, no
// default port). It panics on one that is not an origin.
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

func mcpOriginAllowed(allowed []string, origin string) bool {
	for _, candidate := range allowed {
		if utils.EqualFold(candidate, origin) {
			return true
		}
	}
	return false
}

// mcpTool returns the handler of the tool, which recovers its own panics: nothing
// else does on this goroutine.
func mcpTool[Deps, Output any](
	agent *ai.Agent[Deps, Output], cfg MCPConfig[Deps], deps Deps, options []ai.RunOption, shutdown <-chan struct{},
) mcpsdk.ToolHandlerFor[mcpAskInput, any] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, in mcpAskInput) (result *mcpsdk.CallToolResult, out any, err error) {
		defer agentrun.Recover(func(panicErr error) {
			result, out, err = nil, nil, cfg.failure(panicErr)
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
			return nil, nil, cfg.failure(runErr)
		}
		result, out, err = mcpOutput(run.Output)
		if err != nil {
			return nil, nil, cfg.failure(err)
		}
		return result, out, nil
	}
}

// mcpOutput returns text as it is and anything else as JSON text, with
// structured content too when it is an object.
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
