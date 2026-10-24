package pydanticai

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/pydanticai/internal/agenttest"
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

// fakeRun is a supervised run that records what the hold does to it. The test is
// the server: expire ends the run, as the server does when its timeout elapses or
// it starts shutting down.
type fakeRun struct {
	ctx     context.Context
	cancel  context.CancelFunc
	stopped atomic.Bool

	mu     sync.Mutex
	calls  []string
	expiry func() // what the hold asked to be called when the server ends the run
}

func newFakeRun() *fakeRun {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeRun{ctx: ctx, cancel: cancel}
}

func (f *fakeRun) Context() context.Context { return f.ctx }

func (f *fakeRun) AfterExpiry(fn func()) (stop func() bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expiry = fn
	return func() bool { return !f.stopped.Swap(true) }
}

func (f *fakeRun) Cancel() {
	f.record("cancel")
	f.cancel()
}

func (f *fakeRun) End(err error) { f.record("end " + text(err)) }

func (f *fakeRun) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeRun) recorded() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ", ")
}

// expire is the server ending the run, which calls what the hold asked for.
func (f *fakeRun) expire(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	expiry := f.expiry
	f.mu.Unlock()
	require.NotNil(t, expiry, "the hold asked to be told when the server ends the run")
	expiry()
}

func text(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// newTestHold returns the hold of a fake run, on a connection that records that
// it was closed. OnClose is recorded as a call of the run.
func newTestHold(grace time.Duration) (*requestHold, *fakeRun, *closeRecorder) {
	run, conn := newFakeRun(), new(closeRecorder)
	hold := newRequestHold(nil, conn, run, grace, func(err error) { run.record("close " + text(err)) })
	return hold, run, conn
}

func Test_requestHold_expire(t *testing.T) {
	t.Parallel()

	const grace = time.Hour // no check fires during the test

	t.Run("a check is armed", func(t *testing.T) {
		t.Parallel()

		h, _, _ := newTestHold(grace)
		t.Cleanup(h.finish)
		h.expire()
		require.NotNil(t, h.check)
	})

	t.Run("no check without a connection", func(t *testing.T) {
		t.Parallel()

		// A RequestCtx that no server serves has none.
		h := newRequestHold(nil, nil, newFakeRun(), grace, func(error) {})
		h.expire()
		require.Nil(t, h.check)
	})

	t.Run("no check once fasthttp is done with the body", func(t *testing.T) {
		t.Parallel()

		h, _, _ := newTestHold(grace)
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
		h, _, conn := newTestHold(grace)
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
		require.False(t, h.check.Stop(), "finish stopped the check")
		h.cut() // a check that fired while finish stopped it
		require.False(t, conn.closed.Load(), "the connection serves another request")
	})
}

func Test_requestHold_release(t *testing.T) {
	t.Parallel()

	failure := errors.New("the client went away")

	t.Run("reports how the stream ended, once", func(t *testing.T) {
		t.Parallel()

		hold, run, _ := newTestHold(time.Hour)
		hold.streamEnded(failure)

		var wg sync.WaitGroup
		for range 3 {
			wg.Go(hold.release)
		}
		wg.Wait()
		hold.release()
		require.Equal(t, "cancel, end the client went away, close the client went away", run.recorded(),
			"the run is canceled, ended, and then OnClose is told, each once")
		require.Error(t, run.Context().Err(), "the run was canceled")
	})

	t.Run("waits for the run to return", func(t *testing.T) {
		t.Parallel()

		hold, run, _ := newTestHold(time.Hour)
		released := make(chan struct{})
		go func() {
			hold.release()
			close(released)
		}()

		agenttest.Receive(t, run.Context().Done(), "the run to be canceled") // first, so that it can return
		select {
		case <-released:
			t.Fatal("the hold let go of the request while the run was still running")
		case <-time.After(50 * time.Millisecond):
		}

		hold.streamEnded(nil)
		agenttest.Receive(t, released, "the release to return")
		require.Equal(t, "cancel, end ok, close ok", run.recorded())
	})

	t.Run("the context of the stream is the context of the run", func(t *testing.T) {
		t.Parallel()

		hold, run, _ := newTestHold(time.Hour)
		require.Equal(t, run.Context(), hold.Context())
	})
}

func Test_requestHold_expiry(t *testing.T) {
	t.Parallel()

	hold, run, conn := newTestHold(20 * time.Millisecond)
	t.Cleanup(hold.finish)
	hold.writing.Store(time.Now().Add(-time.Hour).UnixNano()) // a write to a client that stopped reading

	run.expire(t) // the server ended the run
	require.Eventually(t, conn.closed.Load, agenttest.ObserveTimeout, time.Millisecond)
}

func Test_holdingBody_Close(t *testing.T) {
	t.Parallel()

	const grace = 20 * time.Millisecond
	hold, run, conn := newTestHold(grace)
	hold.streamEnded(nil)
	body := &holdingBody{ReadCloser: io.NopCloser(strings.NewReader("events")), hold: hold}

	// The server ended the run while a write to the client is blocked: the check
	// that closes the connection is armed.
	hold.writing.Store(time.Now().Add(-time.Hour).UnixNano())
	hold.expire()

	// fasthttp is done with the body, and the connection can serve another request.
	require.NoError(t, body.Close())
	time.Sleep(4 * grace) // a check that Close did not stop would have fired by now
	require.False(t, conn.closed.Load(), "the connection of the next request is left alone")
	require.True(t, run.stopped.Load(), "the server can end the run, and nothing is left to look for")
	require.Equal(t, "cancel, end ok, close ok", run.recorded(), "Close releases the run")
}
