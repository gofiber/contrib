package pydanticai

import (
	"cmp"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"github.com/gofiber/utils/v2"
)

const streamEndGrace = 5 * time.Second

// serveStream sends what serve writes as a server-sent events response for a run
// it supervises. The context of serve ends when a write fails, the client is gone,
// Timeout elapses, the server shuts down or the stream ends, not with the request.
func serveStream[Deps, A any](
	c fiber.Ctx,
	opts *StreamConfig[Deps, A],
	deps Deps,
	options []ai.RunOption,
	serve func(ctx context.Context, run *agentrun.Run[Deps], options []ai.RunOption, send func(data []byte) error) error,
) error {
	// A request that a middleware gave up on is never answered, so run nothing.
	if err := context.Cause(c.Context()); err != nil {
		return err
	}

	// The run outlives the handler: keep the request values, not its cancellation.
	run, options := agentrun.New(context.WithoutCancel(c.Context()), agentrun.Config[Deps]{
		Deps:     deps,
		OnRunEnd: opts.OnRunEnd,
		Timeout:  opts.Timeout,
		Shutdown: shutdownSignal(c, opts.CancelOnShutdown),
	}, options)
	hold := newRequestHold(c, c.RequestCtx().Conn(), run, cmp.Or(opts.grace, streamEndGrace), func(err error) {
		if opts.OnClose != nil {
			agentrun.Safely("OnClose", func() { opts.OnClose(deps, err) })
		}
	})
	// fasthttp closes the user values of a request once it is done with the response,
	// which is later than the Close of the body when a compressor reads it.
	c.RequestCtx().SetUserValue(responseDoneKey{}, responseDone{hold})
	config := sse.Config{
		HeartbeatInterval: heartbeatInterval(opts.HeartbeatInterval),
		DisableHeartbeat:  opts.DisableHeartbeat,
		Handler: func(_ fiber.Ctx, stream *sse.Stream) error {
			hold.started.Store(true)
			return serve(stream.Context(), run, options, func(data []byte) error {
				// Safe: Event writes the string before it returns.
				return stream.Event(sse.Event{Data: utils.UnsafeString(data)})
			})
		},
		OnClose: func(_ fiber.Ctx, err error) { hold.streamEnded(err) },
	}
	return sse.New(config)(hold)
}

// supervised is the run that a hold serves, as agentrun.Run supervises it.
type supervised interface {
	Context() context.Context
	// AfterExpiry calls fn once when the server ends the run, not when it is canceled.
	AfterExpiry(fn func()) (stop func() bool)
	Cancel()
	End(err error)
}

// requestHold is the fiber.Ctx that sse writes to. It holds the request until the
// run has ended, as fasthttp reuses a request once its connection is done with
// it, and closes the connection of a client that stopped reading.
type requestHold struct {
	fiber.Ctx

	run      supervised
	onClose  func(err error)
	ended    chan error
	released sync.Once
	// started is set when the handler of the stream runs. The sse middleware starts it
	// with the first read of the body, so a body that is closed unread never runs it.
	started atomic.Bool

	// conn is the connection of the request, taken while the handler runs.
	conn net.Conn
	// base is the origin of writing. Its monotonic clock reading keeps a step of the
	// wall clock, as an NTP correction or a restored snapshot, from changing how long
	// a write has been blocked.
	base time.Time
	// writing is when fasthttp started the write it is in, in ns since base plus one;
	// 0 if none.
	writing atomic.Int64

	grace time.Duration

	mu sync.Mutex
	// done is set once fasthttp is done with the response: the connection is left alone.
	done  bool
	check *time.Timer

	// stopExpiry stops the call to expire once fasthttp is done: it would only
	// start a goroutine.
	stopExpiry func() bool
}

func newRequestHold(c fiber.Ctx, conn net.Conn, run supervised, grace time.Duration, onClose func(err error)) *requestHold {
	h := &requestHold{
		Ctx:     c,
		run:     run,
		onClose: onClose,
		ended:   make(chan error, 1),
		conn:    conn,
		base:    time.Now(),
		grace:   grace,
	}
	h.stopExpiry = run.AfterExpiry(h.expire)
	return h
}

// stamp is the value of writing for a write that starts now.
func (h *requestHold) stamp() int64 { return int64(time.Since(h.base)) + 1 }

// streamEnded reports why the stream ended, the first time.
func (h *requestHold) streamEnded(err error) {
	select {
	case h.ended <- err:
	default:
	}
}

// release cancels the run if it is still going, waits for it and reports its end,
// once.
func (h *requestHold) release() {
	h.released.Do(func() {
		var err error
		select {
		case err = <-h.ended:
			// The run is over, but fasthttp may still be writing its end, which Timeout
			// and a shutdown give a grace for: finish cancels the run.
		default:
			h.run.Cancel()
			err = <-h.ended
		}
		h.run.End(err)
		h.onClose(err)
	})
}

// expire looks for a client that stopped reading: its blocked write only fails
// when TCP gives up.
func (h *requestHold) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done || h.conn == nil {
		return
	}
	h.check = time.AfterFunc(h.grace, h.cut)
}

// cut closes the connection if a write has been blocked for the grace.
func (h *requestHold) cut() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return
	}
	next := h.grace
	if since := h.writing.Load(); since != 0 {
		blocked := time.Since(h.base) - time.Duration(since-1)
		if blocked >= h.grace {
			_ = h.conn.Close()
			return
		}
		next -= blocked
	}
	h.check.Reset(next)
}

// finish is called once fasthttp is done with the response: nothing is left to
// cut, and the run lets go of its timers.
func (h *requestHold) finish() {
	h.stopExpiry()
	h.mu.Lock()
	h.done = true
	if h.check != nil {
		h.check.Stop()
	}
	h.mu.Unlock()
	h.run.Cancel()
}

type responseDoneKey struct{}

// responseDone is stored in the request, which fasthttp resets after it wrote the
// response, or could not, and closes the values of that implement io.Closer.
type responseDone struct{ hold *requestHold }

func (d responseDone) Close() error {
	d.hold.finish()
	return nil
}

// Context is the context of the run, which sse derives the stream context from.
func (h *requestHold) Context() context.Context { return h.run.Context() }

// gaveUp returns why a middleware gave up on the request, if it did, and lets go
// of the run: nothing reads what the hold would send.
func (h *requestHold) gaveUp() error {
	err := context.Cause(h.Ctx.Context())
	if err != nil {
		h.finish()
	}
	return err
}

// SendStream is how the sse middleware sends its stream, which starts with the
// first read of the body: the response is the hold's from here on.
func (h *requestHold) SendStream(stream io.Reader, size ...int) error {
	body, ok := stream.(io.ReadCloser)
	if !ok {
		body = io.NopCloser(stream)
	}
	if err := h.gaveUp(); err != nil {
		_ = body.Close()
		return err
	}
	return h.install(&holdingBody{ReadCloser: body, hold: h}, size...)
}

func (h *requestHold) install(body *holdingBody, size ...int) error {
	err := h.Ctx.SendStream(body, size...)
	// A middleware that gave up while the stream was set up never answers the request
	// either, and fasthttp would not close the body of a response that it drops.
	if err == nil && context.Cause(h.Ctx.Context()) != nil {
		_ = body.Close()
		h.finish()
	}
	return err
}

type holdingBody struct {
	io.ReadCloser
	hold *requestHold

	closing  sync.Once
	closeErr error
}

func (b *holdingBody) Read(p []byte) (int, error) {
	b.hold.writing.Store(0)
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.hold.writing.Store(b.hold.stamp())
	}
	return n, err
}

// Close ends the stream. It does not return before the run has ended.
func (b *holdingBody) Close() error {
	b.closing.Do(func() {
		// The callbacks of release are not a write to the client.
		b.hold.writing.Store(0)
		b.closeErr = b.ReadCloser.Close()
		if !b.hold.started.Load() {
			// Closing waited for a stream that started: this one never will.
			b.hold.streamEnded(errStreamFailed)
		}
		b.hold.release()
		// fasthttp calls Close when it has read the stream or cannot write it, and a
		// compressor when it has read it, while the server may still be writing what it
		// compressed. The rest of the response is being written until fasthttp is done.
		b.hold.writing.Store(b.hold.stamp())
	})
	return b.closeErr
}
