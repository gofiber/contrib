package pydanticai

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func Test_heartbeatInterval(t *testing.T) {
	t.Parallel()

	require.Equal(t, 5*time.Second, defaultHeartbeatInterval)
	require.Equal(t, defaultHeartbeatInterval, heartbeatInterval(0))
	require.Equal(t, defaultHeartbeatInterval, heartbeatInterval(-time.Second))
	require.Equal(t, 2*time.Second, heartbeatInterval(2*time.Second))
}
