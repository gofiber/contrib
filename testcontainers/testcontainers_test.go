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

func TestAddService_fromContainerConfig(t *testing.T) {
	t.Run("nil-config", func(t *testing.T) {
		containerConfig := testcontainers.NewContainerConfig("nginx-generic", nginxAlpineImg)

		srv, err := testcontainers.AddService(nil, containerConfig)
		require.ErrorIs(t, err, testcontainers.ErrNilConfig)
		require.Nil(t, srv)
	})

	t.Run("empty-service-key", func(t *testing.T) {
		containerConfig := testcontainers.NewContainerConfig("", nginxAlpineImg)

		srv, err := testcontainers.AddService(&fiber.Config{}, containerConfig)
		require.ErrorIs(t, err, testcontainers.ErrEmptyServiceKey)
		require.Nil(t, srv)
	})

	t.Run("empty-image", func(t *testing.T) {
		containerConfig := testcontainers.NewContainerConfig("nginx-generic", "")

		srv, err := testcontainers.AddService(&fiber.Config{}, containerConfig)
		require.ErrorIs(t, err, testcontainers.ErrImageEmpty)
		require.Nil(t, srv)
	})

	t.Run("success", func(t *testing.T) {
		cfg := fiber.Config{}

		containerConfig := testcontainers.NewContainerConfig("nginx-generic", nginxAlpineImg, tc.WithExposedPorts("80/tcp"))

		srv, err := testcontainers.AddService(&cfg, containerConfig)
		require.NoError(t, err)
		require.Equal(t, "nginx-generic (using testcontainers-go)", srv.Key())

		app := fiber.New(cfg)

		require.Len(t, app.State().Services(), 1)
		require.Equal(t, 1, app.State().ServicesLen())
	})
}

func TestAddService_fromModuleConfig(t *testing.T) {
	t.Run("nil-fiber-config", func(t *testing.T) {
		moduleConfig := testcontainers.NewModuleConfig("redis-module", redisAlpineImg, redis.Run)

		srv, err := testcontainers.AddService(nil, moduleConfig)
		require.ErrorIs(t, err, testcontainers.ErrNilConfig)
		require.Nil(t, srv)
	})

	t.Run("empty-service-key", func(t *testing.T) {
		moduleConfig := testcontainers.NewModuleConfig("", redisAlpineImg, redis.Run)

		srv, err := testcontainers.AddService(&fiber.Config{}, moduleConfig)
		require.ErrorIs(t, err, testcontainers.ErrEmptyServiceKey)
		require.Nil(t, srv)
	})

	t.Run("empty-image", func(t *testing.T) {
		moduleConfig := testcontainers.NewModuleConfig("redis-module", "", redis.Run)

		srv, err := testcontainers.AddService(&fiber.Config{}, moduleConfig)
		require.ErrorIs(t, err, testcontainers.ErrImageEmpty)
		require.Nil(t, srv)
	})

	t.Run("nil-run-fn", func(t *testing.T) {
		var runFn func(ctx context.Context, img string, opts ...tc.ContainerCustomizer) (tc.Container, error)

		moduleConfig := testcontainers.NewModuleConfig("redis-module", redisAlpineImg, runFn)

		srv, err := testcontainers.AddService(&fiber.Config{}, moduleConfig)
		require.ErrorIs(t, err, testcontainers.ErrRunFnNil)
		require.Nil(t, srv)
	})

	t.Run("add-modules", func(t *testing.T) {
		cfg := fiber.Config{}

		moduleConfig := testcontainers.NewModuleConfig("redis-module", redisAlpineImg, redis.Run)

		srv, err := testcontainers.AddService(&cfg, moduleConfig)
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
			moduleConfig := testcontainers.NewModuleConfig("redis-module", redisAlpineImg, redis.Run)

			srv, err := testcontainers.AddService(&cfg, moduleConfig)
			require.NoError(t, err)

			require.NoError(t, srv.Start(context.Background()))
			t.Cleanup(func() {
				require.NoError(t, srv.Terminate(context.Background()))
			})

			ctr := srv.Container()
			require.NotNil(t, ctr)

			st, err := srv.State(context.Background())
			require.NoError(t, err)

			require.Equal(t, "running", st)

			// verify the container has the correct labels
			inspect, err := srv.Container().Inspect(context.Background())
			require.NoError(t, err)

			require.Equal(t, "Go Fiber", inspect.Config.Labels["org.testcontainers.golang.framework"])
		})

		t.Run("error", func(t *testing.T) {
			moduleConfig := testcontainers.NewModuleConfig("redis-module-error", redisAlpineImg, redis.Run, tc.WithWaitStrategy(wait.ForLog("never happens").WithStartupTimeout(time.Second)))

			srv, err := testcontainers.AddService(&cfg, moduleConfig)
			require.NoError(t, err)

			require.Error(t, srv.Start(context.Background()))

			ctr := srv.Container()
			require.Nil(t, ctr)
		})
	})

	t.Run("state", func(t *testing.T) {
		cfg := fiber.Config{}

		t.Run("running", func(t *testing.T) {
			moduleConfig := testcontainers.NewModuleConfig("redis-module-running", redisAlpineImg, redis.Run)

			srv, err := testcontainers.AddService(&cfg, moduleConfig)
			require.NoError(t, err)
			require.Equal(t, "redis-module-running (using testcontainers-go)", srv.String())

			require.NoError(t, srv.Start(context.Background()))
			t.Cleanup(func() {
				require.NoError(t, srv.Terminate(context.Background()))
			})

			st, err := srv.State(context.Background())
			require.NoError(t, err)
			require.Equal(t, "running", st)
		})

		t.Run("not-running", func(t *testing.T) {
			moduleConfig := testcontainers.NewModuleConfig("redis-module-not-running", redisAlpineImg, redis.Run)

			srv, err := testcontainers.AddService(&cfg, moduleConfig)
			require.NoError(t, err)
			require.Equal(t, "redis-module-not-running (using testcontainers-go)", srv.String())

			st, err := srv.State(context.Background())
			require.ErrorIs(t, err, testcontainers.ErrContainerNotRunning)
			require.Empty(t, st)
		})
	})

	t.Run("terminate", func(t *testing.T) {
		cfg := fiber.Config{}

		t.Run("running", func(t *testing.T) {
			moduleConfig := testcontainers.NewModuleConfig("redis-module", redisAlpineImg, redis.Run)

			srv, err := testcontainers.AddService(&cfg, moduleConfig)
			require.NoError(t, err)

			// Start the service to be able to terminate it.
			require.NoError(t, srv.Start(context.Background()))

			require.NoError(t, srv.Terminate(context.Background()))

			// The container is terminated, so the state should not be available.
			_, err = srv.State(context.Background())
			require.Error(t, err)
		})

		t.Run("not-running", func(t *testing.T) {
			moduleConfig := testcontainers.NewModuleConfig("redis-module-not-running", redisAlpineImg, redis.Run)

			srv, err := testcontainers.AddService(&cfg, moduleConfig)
			require.NoError(t, err)
			require.Equal(t, "redis-module-not-running (using testcontainers-go)", srv.String())

			err = srv.Terminate(context.Background())
			require.ErrorIs(t, err, testcontainers.ErrContainerNotRunning)
		})
	})
}
