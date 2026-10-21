package pydanticai

import (
	"testing"
	"time"
)

// SetStreamEndGrace sets how long a write to a client that stopped reading may
// block before its connection is closed, until the test ends. The test must not
// run in parallel with others.
func SetStreamEndGrace(t testing.TB, grace time.Duration) {
	t.Helper()
	previous := streamEndGrace
	streamEndGrace = grace
	t.Cleanup(func() { streamEndGrace = previous })
}
