package testcontainers

import (
	"testing"

	"github.com/stretchr/testify/require"
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
