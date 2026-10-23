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

// fakeRun is a supervised run that records what the hold does to it.
type fakeRun struct {
	ctx     context.Context
	stop    context.CancelFunc
	expired atomic.Bool
	// asked is closed when the run ended and the hold was told, or not, that the
	// server did it.
	asked     chan struct{}
	askedOnce sync.Once

	mu    sync.Mutex
	calls []string
}

func newFakeRun() *fakeRun {
	ctx, stop := context.WithCancel(context.Background())
	return &fakeRun{ctx: ctx, stop: stop, asked: make(chan struct{})}
}

func (f *fakeRun) Context() context.Context { return f.ctx }

// AfterExpiry calls fn when the run ends, if the test said that the server ended
// it.
func (f *fakeRun) AfterExpiry(fn func()) (stop func() bool) {
	return context.AfterFunc(f.ctx, func() {
		f.askedOnce.Do(func() { close(f.asked) })
		if f.expired.Load() {
			fn()
		}
	})
}

func (f *fakeRun) Cancel() {
	f.record("cancel")
	f.stop()
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

func text(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func Test_requestHold_release(t *testing.T) {
	t.Parallel()

	failure := errors.New("the client went away")
	newHold := func(run *fakeRun) *requestHold {
		return newRequestHold(nil, new(closeRecorder), run, time.Hour, func(err error) { run.record("close " + text(err)) })
	}

	t.Run("reports how the stream ended, once", func(t *testing.T) {
		t.Parallel()

		run := newFakeRun()
		hold := newHold(run)
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

		run := newFakeRun()
		hold := newHold(run)
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

		run := newFakeRun()
		require.Equal(t, run.Context(), newHold(run).Context())
	})
}

func Test_requestHold_expiry(t *testing.T) {
	t.Parallel()

	const grace = 20 * time.Millisecond
	// blocked returns a hold whose write to the client has been blocked for longer
	// than the grace.
	blocked := func(run *fakeRun) (*requestHold, *closeRecorder) {
		conn := new(closeRecorder)
		hold := newRequestHold(nil, conn, run, grace, func(error) {})
		hold.writing.Store(time.Now().Add(-time.Hour).UnixNano())
		return hold, conn
	}

	t.Run("a run that the server ended closes the connection of a client that stopped reading", func(t *testing.T) {
		t.Parallel()

		run := newFakeRun()
		hold, conn := blocked(run)
		t.Cleanup(hold.finish)

		run.expired.Store(true)
		run.Cancel()
		require.Eventually(t, conn.closed.Load, 5*time.Second, time.Millisecond)
	})

	t.Run("a run that its owner or its client ended leaves the connection alone", func(t *testing.T) {
		t.Parallel()

		run := newFakeRun()
		hold, conn := blocked(run)
		t.Cleanup(hold.finish)

		run.Cancel()
		agenttest.Receive(t, run.asked, "the hold to be told that the run ended")
		time.Sleep(4 * grace)
		hold.mu.Lock()
		defer hold.mu.Unlock()
		require.Nil(t, hold.check, "no check was started")
		require.False(t, conn.closed.Load())
	})
}

func Test_holdingBody_Close(t *testing.T) {
	t.Parallel()

	const grace = 20 * time.Millisecond
	run := newFakeRun()
	conn := new(closeRecorder)
	hold := newRequestHold(nil, conn, run, grace, func(err error) { run.record("close " + text(err)) })
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
	require.Equal(t, "cancel, end ok, close ok", run.recorded(), "Close releases the run")
}
