package pydanticai

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// closeRecorder is a connection that records whether it was closed.
type closeRecorder struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return nil
}

func Test_requestHold_expire(t *testing.T) {
	t.Parallel()

	const grace = time.Hour // no check fires during the test

	t.Run("Timeout and a shutdown arm one check", func(t *testing.T) {
		t.Parallel()

		h := &requestHold{conn: new(closeRecorder), grace: grace}
		t.Cleanup(h.finish)
		h.expire()
		check := h.check
		require.NotNil(t, check)
		h.expire()
		require.Same(t, check, h.check)
	})

	t.Run("no check without a connection", func(t *testing.T) {
		t.Parallel()

		h := &requestHold{grace: grace} // a RequestCtx that no server serves
		h.expire()
		require.Nil(t, h.check)
	})

	t.Run("no check once fasthttp is done with the body", func(t *testing.T) {
		t.Parallel()

		h := &requestHold{conn: new(closeRecorder), grace: grace}
		h.finish()
		h.expire()
		require.Nil(t, h.check)
	})
}

func Test_requestHold_cut(t *testing.T) {
	t.Parallel()

	// armed returns a hold whose check is armed, and whose write has been
	// blocked for blocked, or that waits for the run when blocked is 0.
	armed := func(t *testing.T, grace, blocked time.Duration) (*requestHold, *closeRecorder) {
		t.Helper()
		conn := new(closeRecorder)
		h := &requestHold{conn: conn, grace: grace}
		if blocked > 0 {
			h.writing.Store(time.Now().Add(-blocked).UnixNano())
		}
		h.expire()
		t.Cleanup(h.finish)
		return h, conn
	}

	t.Run("a write that is blocked for the grace", func(t *testing.T) {
		t.Parallel()

		h, conn := armed(t, time.Hour, time.Hour)
		h.cut()
		require.True(t, conn.closed.Load())
	})

	t.Run("no write", func(t *testing.T) {
		t.Parallel()

		h, conn := armed(t, time.Hour, 0)
		h.cut()
		require.False(t, conn.closed.Load(), "the client reads, and the run has nothing to send")
	})

	t.Run("a write that is blocked for part of the grace", func(t *testing.T) {
		t.Parallel()

		const grace = time.Second
		h, conn := armed(t, grace, grace-100*time.Millisecond)
		h.cut()
		require.False(t, conn.closed.Load())
		// The next check comes when the write has been blocked for the grace,
		// not a whole grace later.
		require.Eventually(t, conn.closed.Load, grace/2, time.Millisecond)
	})

	t.Run("a body that fasthttp is done with", func(t *testing.T) {
		t.Parallel()

		h, conn := armed(t, time.Hour, time.Hour)
		h.finish()
		h.cut() // a check that fired while finish stopped it
		require.False(t, conn.closed.Load(), "the connection serves another request")
	})
}
