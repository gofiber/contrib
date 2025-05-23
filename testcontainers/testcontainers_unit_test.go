package testcontainers

import (
	"context"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

func Test_buildKey(t *testing.T) {
	t.Run("no-suffix", func(t *testing.T) {
		key := "test"
		got := buildKey(key)
		require.Equal(t, key+serviceSuffix, got)
	})

	t.Run("with-suffix", func(t *testing.T) {
		key := "test-suffix" + serviceSuffix
		got := buildKey(key)
		require.Equal(t, key, got)
	})
}

func Test_ContainersService_Start(t *testing.T) {
	t.Run("twice-error", func(t *testing.T) {
		cfg := fiber.Config{}

		moduleConfig := NewModuleConfig("redis-module-twice-error", "redis:alpine", redis.Run)

		srv, err := AddService(&cfg, moduleConfig)
		require.NoError(t, err)

		opts1 := srv.opts

		require.NoError(t, srv.Start(context.Background()))
		t.Cleanup(func() {
			require.NoError(t, srv.Terminate(context.Background()))
		})

		require.Error(t, srv.Start(context.Background()))

		// verify that the opts are not modified
		opts2 := srv.opts
		require.Equal(t, opts1, opts2)
	})
}
