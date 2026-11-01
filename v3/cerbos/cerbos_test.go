package cerbos

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cerbos/cerbos-sdk-go/cerbos"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

type ctxKey struct{}

type mockClient struct {
	allowed bool
	err     error

	calls     int
	ctx       context.Context
	principal *cerbos.Principal
	resource  *cerbos.Resource
	action    string
}

func (m *mockClient) IsAllowed(ctx context.Context, principal *cerbos.Principal, resource *cerbos.Resource, action string) (bool, error) {
	m.calls++
	m.ctx, m.principal, m.resource, m.action = ctx, principal, resource, action
	return m.allowed, m.err
}

func alice(fiber.Ctx) (*cerbos.Principal, error) { return cerbos.NewPrincipal("alice", "user"), nil }

func post(c fiber.Ctx) (*cerbos.Resource, error) {
	return cerbos.NewResource("post", c.Params("id")), nil
}

func serve(t *testing.T, cfg Config, method string) (status int, body string, reached bool) {
	t.Helper()
	app := fiber.New()
	app.Use("/posts/:id", New(cfg))
	app.Add([]string{method}, "/posts/:id", func(c fiber.Ctx) error {
		reached = true
		return c.SendString("handler")
	})
	resp, err := app.Test(httptest.NewRequest(method, "/posts/1", http.NoBody))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b), reached
}

func TestAllowedReachesHandler(t *testing.T) {
	t.Parallel()
	client := &mockClient{allowed: true}

	status, body, reached := serve(t, Config{Client: client, Principal: alice, Resource: post}, http.MethodDelete)

	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, "handler", body)
	require.True(t, reached)
	require.Equal(t, 1, client.calls)
	require.Equal(t, "alice", client.principal.Obj.GetId())
	require.Equal(t, "post", client.resource.Obj.GetKind())
	require.Equal(t, "1", client.resource.Obj.GetId())
	require.Equal(t, http.MethodDelete, client.action, "the action defaults to the HTTP method")
}

func TestDeniedStopsRequest(t *testing.T) {
	t.Parallel()

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		status, _, reached := serve(t, Config{Client: &mockClient{}, Principal: alice, Resource: post}, http.MethodGet)
		require.Equal(t, fiber.StatusForbidden, status)
		require.False(t, reached)
	})

	t.Run("custom Forbidden", func(t *testing.T) {
		t.Parallel()
		status, body, reached := serve(t, Config{
			Client:    &mockClient{},
			Principal: alice,
			Resource:  post,
			Forbidden: func(c fiber.Ctx) error { return c.Status(fiber.StatusNotFound).SendString("no such post") },
		}, http.MethodGet)
		require.Equal(t, fiber.StatusNotFound, status)
		require.Equal(t, "no such post", body)
		require.False(t, reached)
	})
}

func TestNilPrincipalIsUnauthorized(t *testing.T) {
	t.Parallel()
	anonymous := func(fiber.Ctx) (*cerbos.Principal, error) { return nil, nil }

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		client := &mockClient{allowed: true}
		status, _, reached := serve(t, Config{Client: client, Principal: anonymous, Resource: post}, http.MethodGet)
		require.Equal(t, fiber.StatusUnauthorized, status)
		require.False(t, reached)
		require.Zero(t, client.calls)
	})

	t.Run("custom Unauthorized", func(t *testing.T) {
		t.Parallel()
		status, body, reached := serve(t, Config{
			Client:       &mockClient{allowed: true},
			Principal:    anonymous,
			Resource:     post,
			Unauthorized: func(c fiber.Ctx) error { return c.Status(fiber.StatusTeapot).SendString("log in") },
		}, http.MethodGet)
		require.Equal(t, fiber.StatusTeapot, status)
		require.Equal(t, "log in", body)
		require.False(t, reached)
	})
}

func TestErrorsFailClosed(t *testing.T) {
	t.Parallel()
	boom := errors.New("secret detail")

	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{
			name: "Principal",
			cfg: Config{
				Client:    &mockClient{allowed: true},
				Principal: func(fiber.Ctx) (*cerbos.Principal, error) { return nil, boom },
				Resource:  post,
			},
			want: boom,
		},
		{
			name: "Resource",
			cfg: Config{
				Client:    &mockClient{allowed: true},
				Principal: alice,
				Resource:  func(fiber.Ctx) (*cerbos.Resource, error) { return nil, boom },
			},
			want: boom,
		},
		{
			name: "nil resource",
			cfg: Config{
				Client:    &mockClient{allowed: true},
				Principal: alice,
				Resource:  func(fiber.Ctx) (*cerbos.Resource, error) { return nil, nil },
			},
			want: ErrMissingResource,
		},
		{
			name: "Client, even when it also says allowed",
			cfg:  Config{Client: &mockClient{allowed: true, err: boom}, Principal: alice, Resource: post},
			want: boom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			status, body, reached := serve(t, tt.cfg, http.MethodGet)
			require.Equal(t, fiber.StatusInternalServerError, status)
			require.NotContains(t, body, boom.Error())
			require.False(t, reached)

			var got error
			custom := tt.cfg
			custom.ErrorHandler = func(c fiber.Ctx, err error) error {
				got = err
				return c.SendStatus(fiber.StatusBadGateway)
			}
			status, _, reached = serve(t, custom, http.MethodGet)
			require.Equal(t, fiber.StatusBadGateway, status)
			require.ErrorIs(t, got, tt.want)
			require.False(t, reached)
		})
	}
}

func TestCustomAction(t *testing.T) {
	t.Parallel()
	client := &mockClient{allowed: true}

	status, _, _ := serve(t, Config{
		Client:    client,
		Principal: alice,
		Resource:  post,
		Action:    func(fiber.Ctx) string { return "view:public" },
	}, http.MethodGet)

	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, "view:public", client.action)
}

func TestNextSkipsTheCheck(t *testing.T) {
	t.Parallel()
	client := &mockClient{}

	status, _, reached := serve(t, Config{
		Next:      func(fiber.Ctx) bool { return true },
		Client:    client,
		Principal: alice,
		Resource:  post,
	}, http.MethodGet)

	require.Equal(t, fiber.StatusOK, status)
	require.True(t, reached)
	require.Zero(t, client.calls)
}

func TestClientGetsRequestContext(t *testing.T) {
	t.Parallel()
	client := &mockClient{allowed: true}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), ctxKey{}, "value"))
		return c.Next()
	})
	app.Use(New(Config{Client: client, Principal: alice, Resource: post}))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, "value", client.ctx.Value(ctxKey{}))
}

func TestNewPanicsOnMissingConfig(t *testing.T) {
	t.Parallel()
	client := &mockClient{}

	require.PanicsWithValue(t, "cerbos: Client is required", func() { New(Config{Principal: alice, Resource: post}) })
	require.PanicsWithValue(t, "cerbos: Principal is required", func() { New(Config{Client: client, Resource: post}) })
	require.PanicsWithValue(t, "cerbos: Resource is required", func() { New(Config{Client: client, Principal: alice}) })
}
