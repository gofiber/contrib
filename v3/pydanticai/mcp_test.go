package pydanticai_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	aimcp "github.com/Kludex/pydantic-ai-go/ai/mcp"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// The tests in this file are the recipes of the MCP section of the README. They
// run against an MCP server over Streamable HTTP, like the ones that a
// deployment connects to.

type mcpSearchArgs struct {
	Query string `json:"query" jsonschema:"what to look for"`
}

type mcpSearchResult struct {
	Hits []string `json:"hits"`
}

type mcpWeatherArgs struct {
	City string `json:"city" jsonschema:"the city"`
}

type mcpWeatherResult struct {
	TempC int `json:"temp_c"`
}

type mcpRefreshArgs struct{}

// mcpCall is a call of a tool that reached the MCP server.
type mcpCall struct{ tool, authorization string }

// mcpCatalog is an MCP server with a tool that searches a catalog, an MCP App
// that shows the weather, and a tool that only the page of that app may call. It
// records the calls that reach it, and the Authorization header of every HTTP
// request.
type mcpCatalog struct {
	URL string

	mu       sync.Mutex
	calls    []mcpCall
	requests []string

	// searching reports that a search has started, and search waits for its
	// context or for release when block is set.
	block     bool
	searching chan struct{}
}

func (c *mcpCatalog) record(tool string, req *mcpsdk.CallToolRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, mcpCall{tool: tool, authorization: req.Extra.Header.Get("Authorization")})
}

// called returns the calls that reached the server, in a stable order.
func (c *mcpCatalog) called() []mcpCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	calls := slices.Clone(c.calls)
	slices.SortFunc(calls, func(a, b mcpCall) int {
		return strings.Compare(a.tool+"/"+a.authorization, b.tool+"/"+b.authorization)
	})
	return calls
}

// authorizations returns the Authorization header of every HTTP request.
func (c *mcpCatalog) authorizations() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.requests)
}

func newMCPCatalog(t *testing.T, block ...bool) *mcpCatalog {
	t.Helper()

	catalog := &mcpCatalog{searching: make(chan struct{}, 8), block: len(block) > 0 && block[0]}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "catalog", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "search", Description: "Search the catalog"},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, in mcpSearchArgs) (*mcpsdk.CallToolResult, mcpSearchResult, error) {
			catalog.record("search", req)
			if catalog.block {
				catalog.searching <- struct{}{}
				<-ctx.Done()
				return nil, mcpSearchResult{}, ctx.Err()
			}
			return nil, mcpSearchResult{Hits: []string{"guide for " + in.Query}}, nil
		})
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "weather", Description: "Show the weather",
		Meta: mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://catalog/weather"}},
	}, func(_ context.Context, req *mcpsdk.CallToolRequest, in mcpWeatherArgs) (*mcpsdk.CallToolResult, mcpWeatherResult, error) {
		catalog.record("weather", req)
		return nil, mcpWeatherResult{TempC: 21}, nil
	})
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "refresh", Description: "Refresh the weather page",
		Meta: mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"app"}}},
	}, func(_ context.Context, req *mcpsdk.CallToolRequest, _ mcpRefreshArgs) (*mcpsdk.CallToolResult, mcpWeatherResult, error) {
		catalog.record("refresh", req)
		return nil, mcpWeatherResult{TempC: 22}, nil
	})
	server.AddResource(&mcpsdk.Resource{
		URI: "ui://catalog/weather", Name: "weather", MIMEType: "text/html;profile=mcp-app",
	}, func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{
			URI: req.Params.URI, MIMEType: "text/html;profile=mcp-app", Text: "<p>weather</p>",
		}}}, nil
	})
	server.AddResource(&mcpsdk.Resource{URI: "file:///secrets", Name: "secrets"},
		func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, Text: "secret"}}}, nil
		})

	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalog.mu.Lock()
		catalog.requests = append(catalog.requests, r.Header.Get("Authorization"))
		catalog.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	catalog.URL = ts.URL
	return catalog
}

// toolCallNames returns the names of the tools that an AG-UI stream called, sorted.
func toolCallNames(events []agui.Event) []string {
	var names []string
	for _, event := range events {
		if event.Type == agui.EventToolCallStart {
			names = append(names, event.ToolCallName)
		}
	}
	slices.Sort(names)
	return names
}

func Test_MCP_ToolsOfAServer(t *testing.T) {
	t.Parallel()

	catalog := newMCPCatalog(t)
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	agent.AddToolset(aimcp.NewHTTPToolset[struct{}](
		aimcp.HTTPToolsetConfig{URL: catalog.URL},
		aimcp.WithID("catalog"),
	))

	// Neither subtest is parallel: the calls of both runs are counted after them.
	t.Run("AG-UI", func(t *testing.T) {
		_, body := post(t, newApp("/agui", pydanticai.AGUI(agent)), "/agui", aguiBody)
		events := aguiEvents(t, body)
		require.Equal(t, agui.EventRunFinished, events[len(events)-1].Type)

		// The app-only tool is not offered to the model.
		require.Equal(t, []string{"search", "weather"}, toolCallNames(events))
		var results []string
		for _, event := range events {
			if event.Type == agui.EventToolCallResult {
				results = append(results, event.Content.(string))
			}
		}
		require.Contains(t, strings.Join(results, "\n"), "guide for a")
	})

	t.Run("Vercel", func(t *testing.T) {
		_, body := post(t, newApp("/chat", pydanticai.Vercel(agent)), "/chat", vercelBody)
		chunks, done := vercelChunks(t, body)
		require.True(t, done)
		var inputs, outputs int
		for _, chunk := range chunks {
			switch chunk.Type {
			case vercel.ChunkToolInputAvailable:
				inputs++
			case vercel.ChunkToolOutputAvailable:
				outputs++
			}
		}
		require.Equal(t, 2, inputs)
		require.Equal(t, 2, outputs)
	})

	// Each run opened a session of its own: the two runs called each tool once.
	var tools []string
	for _, call := range catalog.called() {
		tools = append(tools, call.tool)
	}
	require.Equal(t, []string{"search", "search", "weather", "weather"}, tools)
}

func Test_MCP_ToolsActOnBehalfOfTheUser(t *testing.T) {
	t.Parallel()

	catalog := newMCPCatalog(t)
	tokens := map[string]string{"alice": "token-of-alice", "bob": "token-of-bob"}
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error { // stands in for the authentication of the application
		token, ok := tokens[c.Get("X-User")]
		if !ok {
			return fiber.ErrUnauthorized
		}
		c.Locals("mcp-token", token)
		return c.Next()
	})
	app.Post("/chat", pydanticai.Vercel(agent, pydanticai.VercelConfig[struct{}]{
		RunOptions: func(c fiber.Ctx) ([]ai.RunOption, error) {
			token := fiber.Locals[string](c, "mcp-token")
			catalog := aimcp.NewHTTPToolset[struct{}](aimcp.HTTPToolsetConfig{
				URL:     catalog.URL,
				Headers: map[string]string{"Authorization": "Bearer " + token},
			}, aimcp.WithID("catalog"))
			return []ai.RunOption{ai.WithRunToolsets(catalog)}, nil
		},
	}))

	var wg sync.WaitGroup
	for _, user := range []string{"alice", "bob", "alice", "bob"} {
		wg.Go(func() {
			request := newRequest(fiber.MethodPost, "/chat", fiber.MIMEApplicationJSON, vercelBody)
			request.Header.Set("X-User", user)
			response, body := send(t, app, request)
			require.Equal(t, fiber.StatusOK, response.StatusCode)
			_, done := vercelChunks(t, body)
			require.True(t, done)
		})
	}
	wg.Wait()

	// Every call carried the token of the user that made the request.
	var seen []string
	for _, call := range catalog.called() {
		if call.tool == "search" {
			seen = append(seen, call.authorization)
		}
	}
	require.Equal(t, []string{"Bearer token-of-alice", "Bearer token-of-alice", "Bearer token-of-bob", "Bearer token-of-bob"}, seen)

	// A caller that is not identified never reaches the MCP server.
	before := len(catalog.authorizations())
	response, _ := send(t, app, newRequest(fiber.MethodPost, "/chat", fiber.MIMEApplicationJSON, vercelBody))
	require.Equal(t, fiber.StatusUnauthorized, response.StatusCode)
	require.Len(t, catalog.authorizations(), before)
}

// The headers of a toolset are for the server that it was configured with. A
// server that redirects to another origin does not get them.
func Test_MCP_CredentialsStayWithTheServer(t *testing.T) {
	t.Parallel()

	catalog := newMCPCatalog(t)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, catalog.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	agent.AddToolset(aimcp.NewHTTPToolset[struct{}](aimcp.HTTPToolsetConfig{
		URL:     redirect.URL,
		Headers: map[string]string{"Authorization": "Bearer secret"},
	}, aimcp.WithID("catalog")))

	_, body := post(t, newApp("/agui", pydanticai.AGUI(agent)), "/agui", aguiBody)
	require.Equal(t, []string{"search", "weather"}, toolCallNames(aguiEvents(t, body)), "the redirect was followed")

	requests := catalog.authorizations()
	require.NotEmpty(t, requests)
	for _, authorization := range requests {
		require.Empty(t, authorization, "the other origin got the credentials")
	}
}

func Test_MCP_ASessionThatAllRequestsShare(t *testing.T) {
	t.Parallel()

	catalog := newMCPCatalog(t, true)
	session, err := aimcp.Connect(context.Background(),
		&mcpsdk.StreamableClientTransport{Endpoint: catalog.URL}, aimcp.WithID("catalog"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	agent.AddToolset(aimcp.NewSessionToolset[struct{}](session))

	var (
		mu    sync.Mutex
		order []string
	)
	step := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, what)
	}
	closed := make(chan error, 1)
	app := fiber.New()
	app.Post("/agui", pydanticai.AGUI(agent, pydanticai.AGUIConfig[struct{}]{
		CancelOnShutdown: true, // the runs stop before the session closes
		OnClose:          func(_ struct{}, err error) { step("the run ended"); closed <- err },
	}))
	// The session outlives the requests, so it closes once the server is down.
	app.Hooks().OnPostShutdown(func(error) error {
		step("the session closed")
		return session.Close()
	})

	response, _ := openStream(t, listen(t, app)+"/agui", aguiBody)
	_, ok := pydanticaitest.NewReader(response.Body).NextData()
	require.True(t, ok)
	agenttest.Receive(t, catalog.searching, "the tool call")

	begin := time.Now()
	require.NoError(t, app.ShutdownWithTimeout(10*time.Second))
	require.Less(t, time.Since(begin), 5*time.Second, "the shutdown waited for the run")
	require.ErrorIs(t, agenttest.Receive(t, closed, "OnClose"), ai.ErrRunCancelled)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"the run ended", "the session closed"}, order)
	require.Error(t, session.Ping(context.Background()), "the session is closed")
}

func Test_MCP_ProviderHostedServerWithALocalFallback(t *testing.T) {
	t.Parallel()

	catalog := newMCPCatalog(t)
	// A model that cannot run MCP servers itself, like the fakes, gets the tools
	// through the local client.
	capability := aimcp.NewHTTPServerCapability[struct{}](aimcp.HTTPServerCapabilityConfig{
		Native: ai.MCPServerTool{
			ID:                 "catalog",
			URL:                catalog.URL,
			AuthorizationToken: "Bearer service-token",
			AllowedTools:       []string{"search"},
		},
	})
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel(), ai.WithCapabilities(capability))

	_, body := post(t, newApp("/agui", pydanticai.AGUI(agent)), "/agui", aguiBody)
	require.Equal(t, []string{"search"}, toolCallNames(aguiEvents(t, body)), "only the allowed tool is offered")
	require.Equal(t, []mcpCall{{tool: "search", authorization: "Bearer service-token"}}, catalog.called())
}

func Test_MCP_ServersOfAConfigFile(t *testing.T) {
	// Not parallel: t.Setenv, which the ${CATALOG_TOKEN} of the file reads, panics in a parallel test.
	catalog := newMCPCatalog(t)
	t.Setenv("CATALOG_TOKEN", "from-the-environment")
	path := filepath.Join(t.TempDir(), ".mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "mcpServers": {
    "catalog": {
      "url": "`+catalog.URL+`",
      "headers": {"Authorization": "Bearer ${CATALOG_TOKEN}"}
    }
  }
}`), 0o600))

	toolsets, err := aimcp.LoadToolsets[struct{}](path)
	require.NoError(t, err)
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	for _, toolset := range toolsets {
		agent.AddToolset(toolset)
	}

	_, body := post(t, newApp("/agui", pydanticai.AGUI(agent)), "/agui", aguiBody)
	names := toolCallNames(aguiEvents(t, body))
	require.Len(t, names, 2)
	for _, name := range names {
		require.True(t, strings.HasPrefix(name, "catalog"), "the name of the server prefixes the tools: %s", name)
	}
	for _, call := range catalog.called() {
		require.Equal(t, "Bearer from-the-environment", call.authorization)
	}
}

func Test_MCP_Apps(t *testing.T) {
	t.Parallel()

	catalog := newMCPCatalog(t)
	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	agent.AddToolset(aimcp.NewHTTPToolset[struct{}](aimcp.HTTPToolsetConfig{URL: catalog.URL}, aimcp.WithID("catalog")))

	t.Run("AG-UI", func(t *testing.T) {
		t.Parallel()

		_, body := post(t, newApp("/agui", pydanticai.AGUI(agent)), "/agui", aguiBody)
		var snapshots []agui.Event
		for _, event := range aguiEvents(t, body) {
			if event.Type == agui.EventActivitySnapshot {
				snapshots = append(snapshots, event)
			}
		}
		require.Len(t, snapshots, 1, "one tool names an app")
		require.Equal(t, "mcp-apps", snapshots[0].ActivityType)
		content, ok := snapshots[0].Content.(map[string]any)
		require.True(t, ok)
		require.Equal(t, "ui://catalog/weather", content["resourceUri"])
		require.Equal(t, "catalog", content["serverId"])
		require.Contains(t, content, "toolInput")
		require.Contains(t, content, "result")
	})

	t.Run("Vercel", func(t *testing.T) {
		t.Parallel()

		_, body := post(t, newApp("/chat", pydanticai.Vercel(agent)), "/chat", vercelBody)
		chunks, done := vercelChunks(t, body)
		require.True(t, done)
		var apps []vercel.Chunk
		for _, chunk := range chunks {
			if chunk.Type == "data-mcp-apps" {
				apps = append(apps, chunk)
			}
		}
		require.Len(t, apps, 1)
		require.NotEmpty(t, apps[0].ID, "the ID of the tool call")
		data, ok := apps[0].Data.(map[string]any)
		require.True(t, ok)
		require.Equal(t, "ui://catalog/weather", data["resourceUri"])
	})
}

// The page of an MCP App cannot reach the MCP server, so the application relays
// what the page asks for on a session that it keeps.
func Test_MCP_RelayForAnApp(t *testing.T) {
	t.Parallel()

	catalog := newMCPCatalog(t)
	session, err := aimcp.Connect(context.Background(),
		&mcpsdk.StreamableClientTransport{Endpoint: catalog.URL}, aimcp.WithID("catalog"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	app := fiber.New()
	app.Get("/mcp-app/resource", func(c fiber.Ctx) error {
		uri := c.Query("uri")
		if !strings.HasPrefix(uri, "ui://") { // a page may read the pages of apps, not every resource
			return fiber.ErrForbidden
		}
		result, err := session.ReadResource(c.Context(), uri)
		if err != nil {
			return err
		}
		return c.JSON(result)
	})
	app.Post("/mcp-app/tools/call", func(c fiber.Ctx) error {
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := c.Bind().JSON(&call); err != nil {
			return err
		}
		result, err := session.CallToolForApp(c.Context(), call.Name, call.Arguments)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return c.JSON(result)
	})

	response, body := do(t, app, fiber.MethodGet, "/mcp-app/resource?uri=ui://catalog/weather", "", "")
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	var page aimcp.ReadResourceResult
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Contents, 1)
	require.Equal(t, "<p>weather</p>", page.Contents[0].Text)
	require.Equal(t, "text/html;profile=mcp-app", page.Contents[0].MIMEType)

	response, _ = do(t, app, fiber.MethodGet, "/mcp-app/resource?uri=file:///secrets", "", "")
	require.Equal(t, fiber.StatusForbidden, response.StatusCode)

	// The page calls the tool that is for apps only, and a tool of the model.
	response, body = do(t, app, fiber.MethodPost, "/mcp-app/tools/call", fiber.MIMEApplicationJSON, `{"name":"refresh"}`)
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	require.Contains(t, body, `"temp_c":22`)
	response, body = do(t, app, fiber.MethodPost, "/mcp-app/tools/call", fiber.MIMEApplicationJSON, `{"name":"weather","arguments":{"city":"Paris"}}`)
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	require.Contains(t, body, `"temp_c":21`)

	// Tools that the server hides from apps, or does not have, are not relayed.
	response, body = do(t, app, fiber.MethodPost, "/mcp-app/tools/call", fiber.MIMEApplicationJSON, `{"name":"nope"}`)
	require.Equal(t, fiber.StatusBadRequest, response.StatusCode)
	require.Contains(t, body, "not available to apps")
}

// A server in memory tests an agent with MCP tools without a network.
func Test_MCP_ServerInMemory(t *testing.T) {
	t.Parallel()

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "catalog", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "search", Description: "Search the catalog"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in mcpSearchArgs) (*mcpsdk.CallToolResult, mcpSearchResult, error) {
			return nil, mcpSearchResult{Hits: []string{"guide for " + in.Query}}, nil
		})

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	agent.AddToolset(aimcp.NewToolset(func(ctx context.Context, _ *ai.RunContext[struct{}]) (mcpsdk.Transport, error) {
		clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
		if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
			return nil, err
		}
		return clientTransport, nil
	}, aimcp.WithID("catalog")))

	app := fiber.New()
	app.Post("/chat", pydanticai.AGUI(agent))

	request := newRequest(fiber.MethodPost, "/chat", fiber.MIMEApplicationJSON, aguiBody)
	response, err := app.Test(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	events := pydanticaitest.AGUIEvents(t, response.Body)
	require.Equal(t, []string{"search"}, toolCallNames(events))
}
