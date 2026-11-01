package cerbos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cerbos/cerbos-sdk-go/cerbos"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const postPolicy = `
apiVersion: api.cerbos.dev/v1
resourcePolicy:
  version: default
  resource: post
  rules:
    - actions: ["GET"]
      effect: EFFECT_ALLOW
      roles: ["user", "admin"]
    - actions: ["DELETE"]
      effect: EFFECT_ALLOW
      roles: ["admin"]
    - actions: ["PUT"]
      effect: EFFECT_ALLOW
      roles: ["user"]
      condition:
        match:
          expr: request.resource.attr.owner == request.principal.id
`

func TestAgainstCerbosServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "ghcr.io/cerbos/cerbos:0.57.0",
			ExposedPorts: []string{"3592/tcp", "3593/tcp"},
			Files: []testcontainers.ContainerFile{{
				Reader:            strings.NewReader(postPolicy),
				ContainerFilePath: "/policies/post.yaml",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForHTTP("/_cerbos/health").WithPort("3592/tcp"),
		},
		Started: true,
	})
	if container != nil {
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			require.NoError(t, container.Terminate(cleanupCtx))
		})
	}
	require.NoError(t, err)

	addr, err := container.PortEndpoint(ctx, "3593/tcp", "")
	require.NoError(t, err)
	client, err := cerbos.New(addr, cerbos.WithPlaintext())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	app := fiber.New()
	app.Use(New(Config{
		Client: client,
		Principal: func(c fiber.Ctx) (*cerbos.Principal, error) {
			user := c.Get("X-User")
			if user == "" {
				return nil, nil
			}
			return cerbos.NewPrincipal(user, c.Get("X-Role")), nil
		},
		Resource: func(c fiber.Ctx) (*cerbos.Resource, error) {
			return cerbos.NewResource("post", "1").WithAttr("owner", "alice"), nil
		},
	}))
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	tests := []struct {
		name       string
		method     string
		user, role string
		wantStatus int
	}{
		{name: "allowed by role", method: http.MethodGet, user: "bob", role: "user", wantStatus: fiber.StatusOK},
		{name: "denied by role", method: http.MethodDelete, user: "bob", role: "user", wantStatus: fiber.StatusForbidden},
		{name: "admin allowed", method: http.MethodDelete, user: "bob", role: "admin", wantStatus: fiber.StatusOK},
		{name: "allowed by condition", method: http.MethodPut, user: "alice", role: "user", wantStatus: fiber.StatusOK},
		{name: "denied by condition", method: http.MethodPut, user: "bob", role: "user", wantStatus: fiber.StatusForbidden},
		{name: "action without a rule", method: http.MethodPost, user: "bob", role: "admin", wantStatus: fiber.StatusForbidden},
		{name: "no principal", method: http.MethodGet, wantStatus: fiber.StatusUnauthorized},
		{name: "principal without a role fails validation", method: http.MethodGet, user: "bob", wantStatus: fiber.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/", http.NoBody)
			req.Header.Set("X-User", tt.user)
			req.Header.Set("X-Role", tt.role)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, tt.wantStatus, resp.StatusCode)
		})
	}
}
