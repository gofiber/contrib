package testcontainers_test

import (
	"context"
	"testing"
	"time"

	"github.com/gofiber/contrib/testcontainers"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	nginxAlpineImg    = "nginx:alpine"
	redisAlpineImg    = "redis:alpine"
	postgresAlpineImg = "postgres:alpine"
)

func TestAdd(t *testing.T) {
	t.Run("nil-config", func(t *testing.T) {
		srv, err := testcontainers.Add(context.Background(), nil, "nginx-generic", nginxAlpineImg)
		require.ErrorIs(t, err, testcontainers.ErrNilConfig)
		require.Nil(t, srv)
	})

	t.Run("success", func(t *testing.T) {
		cfg := fiber.Config{}

		srv, err := testcontainers.Add(
			context.Background(),
			&cfg,
			"nginx-generic",
			nginxAlpineImg,
			tc.WithExposedPorts("80/tcp"),
		)
		require.NoError(t, err)
		require.Equal(t, "nginx-generic (using testcontainers-go)", srv.Key())

		app := fiber.New(cfg)

		require.Len(t, app.State().Services(), 1)
		require.Equal(t, 1, app.State().ServicesLen())
	})
}

func TestAddModule(t *testing.T) {
	t.Run("nil-config", func(t *testing.T) {
		srv, err := testcontainers.AddModule(context.Background(), nil, "redis-module", redis.Run, redisAlpineImg)
		require.ErrorIs(t, err, testcontainers.ErrNilConfig)
		require.Nil(t, srv)
	})

	t.Run("add-modules", func(t *testing.T) {
		cfg := fiber.Config{}

		srv, err := testcontainers.AddModule(context.Background(), &cfg, "redis-module", redis.Run, redisAlpineImg)
		require.NoError(t, err)
		require.Equal(t, "redis-module (using testcontainers-go)", srv.Key())

		app := fiber.New(cfg)

		require.Len(t, app.State().Services(), 1)
		require.Equal(t, 1, app.State().ServicesLen())
	})
}

func TestContainerService(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		cfg := fiber.Config{}

		t.Run("success", func(t *testing.T) {
			srv, err := testcontainers.AddModule(context.Background(), &cfg, "redis-module", redis.Run, redisAlpineImg)
			require.NoError(t, err)

			require.NoError(t, srv.Start(context.Background()))

			st, err := srv.State(context.Background())
			require.NoError(t, err)

			require.Equal(t, "running", st)
		})

		t.Run("error", func(t *testing.T) {
			srv, err := testcontainers.AddModule(
				context.Background(), &cfg,
				"redis-module-error",
				redis.Run,
				redisAlpineImg,
				tc.WithWaitStrategy(wait.ForLog("never happens").WithStartupTimeout(time.Second)),
			)
			require.NoError(t, err)

			require.Error(t, srv.Start(context.Background()))
		})
	})

	t.Run("state", func(t *testing.T) {
		cfg := fiber.Config{}
		t.Run("running", func(t *testing.T) {
			srv, err := testcontainers.AddModule(context.Background(), &cfg, "redis-module-running", redis.Run, redisAlpineImg)
			require.NoError(t, err)
			require.Equal(t, "redis-module-running (using testcontainers-go)", srv.String())

			require.NoError(t, srv.Start(context.Background()))

			st, err := srv.State(context.Background())
			require.NoError(t, err)
			require.Equal(t, "running", st)
		})

		t.Run("not-running", func(t *testing.T) {
			srv, err := testcontainers.AddModule(context.Background(), &cfg, "redis-module-not-running", redis.Run, redisAlpineImg)
			require.NoError(t, err)
			require.Equal(t, "redis-module-not-running (using testcontainers-go)", srv.String())

			st, err := srv.State(context.Background())
			require.ErrorIs(t, err, testcontainers.ErrContainerNotRunning)
			require.Empty(t, st)
		})
	})

	t.Run("terminate", func(t *testing.T) {
		cfg := fiber.Config{}

		srv, err := testcontainers.AddModule(context.Background(), &cfg, "redis-module", redis.Run, redisAlpineImg)
		require.NoError(t, err)

		// Start the service to be able to terminate it.
		require.NoError(t, srv.Start(context.Background()))

		require.NoError(t, srv.Terminate(context.Background()))

		// The container is terminated, so the state should not be available.
		_, err = srv.State(context.Background())
		require.Error(t, err)
	})
}
