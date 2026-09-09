package websocket

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedConn feeds reads from a channel and records each write.
type scriptedConn struct {
	reads chan []byte

	mu        sync.Mutex
	writes    [][]byte
	closed    bool
	deadlines []time.Time
	writeErr  error
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{reads: make(chan []byte, 8)}
}

func (s *scriptedConn) Read(p []byte) (int, error) {
	b, ok := <-s.reads
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (s *scriptedConn) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	s.writes = append(s.writes, bytes.Clone(p))
	return len(p), nil
}

func (s *scriptedConn) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (*scriptedConn) LocalAddr() net.Addr             { return &net.TCPAddr{} }
func (*scriptedConn) RemoteAddr() net.Addr            { return &net.TCPAddr{} }
func (*scriptedConn) SetDeadline(time.Time) error     { return nil }
func (*scriptedConn) SetReadDeadline(time.Time) error { return nil }

func (s *scriptedConn) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadlines = append(s.deadlines, t)
	return nil
}

func (s *scriptedConn) written() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.writes)
}

func (s *scriptedConn) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *scriptedConn) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *scriptedConn) failWrites(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeErr = err
}

func (s *scriptedConn) writeDeadlines() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deadlines)
}

// hijackedConn mimics fasthttp's hijacked conn; UnsafeConn is the socket underneath.
type hijackedConn struct {
	*scriptedConn
	raw *scriptedConn
}

func (h *hijackedConn) UnsafeConn() net.Conn { return h.raw }

// attachedConn returns an attached conn with the safety timer effectively off.
func attachedConn(t *testing.T) (*coalescingConn, *scriptedConn) {
	t.Helper()
	src := newScriptedConn()
	c := newCoalescingConn()
	c.maxDelay = time.Hour
	require.NoError(t, c.attach(src, 0))
	return c, src
}

// feedAndRead leaves the reader holding a batch.
func feedAndRead(t *testing.T, c *coalescingConn, src *scriptedConn, data string) {
	t.Helper()
	src.reads <- []byte(data)
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, data, string(buf[:n]))
}

// readInBackground parks the reader and returns a function that releases it.
func readInBackground(t *testing.T, c *coalescingConn, src *scriptedConn) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		_, _ = c.Read(buf)
	}()
	require.Eventually(t, c.parked.Load, time.Second, time.Millisecond)
	return func() {
		close(src.reads)
		<-done
	}
}

func TestCoalescingConnHoldsRepliesUntilReaderReturns(t *testing.T) {
	c, src := attachedConn(t)
	feedAndRead(t, c, src, "frame1frame2")

	_, err := c.Write([]byte("reply1"))
	require.NoError(t, err)
	_, err = c.Write([]byte("reply2"))
	require.NoError(t, err)
	assert.Equal(t, 0, src.writeCount())

	release := readInBackground(t, c, src)
	defer release()
	assert.Equal(t, [][]byte{[]byte("reply1reply2")}, src.written())
}

func TestCoalescingConnWritesThroughWithoutPendingInput(t *testing.T) {
	c, src := attachedConn(t)

	_, err := c.Write([]byte("push"))
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("push")}, src.written())
}

func TestCoalescingConnWritesThroughWhileReaderParked(t *testing.T) {
	c, src := attachedConn(t)
	feedAndRead(t, c, src, "hello")
	release := readInBackground(t, c, src)
	defer release()

	_, err := c.Write([]byte("push"))
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("push")}, src.written())
}

func TestCoalescingConnTimerFlushesWhenReaderStalls(t *testing.T) {
	c, src := attachedConn(t)
	c.maxDelay = 5 * time.Millisecond
	feedAndRead(t, c, src, "hello")

	_, err := c.Write([]byte("late"))
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return src.writeCount() == 1 }, time.Second, time.Millisecond)

	require.Eventually(t, func() bool { return !c.batch.Load() }, time.Second, time.Millisecond)
	_, err = c.Write([]byte("next"))
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("late"), []byte("next")}, src.written())
}

func TestCoalescingConnLargeWriteGoesDirectAfterPending(t *testing.T) {
	c, src := attachedConn(t)
	feedAndRead(t, c, src, "hello")

	_, err := c.Write([]byte("small"))
	require.NoError(t, err)
	big := bytes.Repeat([]byte{'x'}, coalesceLimit)
	_, err = c.Write(big)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("small"), big}, src.written())
}

func TestCoalescingConnCloseFlushesAndIsIdempotent(t *testing.T) {
	c, src := attachedConn(t)
	feedAndRead(t, c, src, "hello")

	_, err := c.Write([]byte("bye"))
	require.NoError(t, err)
	require.NoError(t, c.Close())
	assert.Equal(t, [][]byte{[]byte("bye")}, src.written())
	assert.True(t, src.isClosed())

	require.NoError(t, c.Close())
	_, err = c.Write([]byte("after"))
	assert.ErrorIs(t, err, net.ErrClosed)
}

func TestCoalescingConnHandshakeWaitsForAttach(t *testing.T) {
	c := newCoalescingConn()
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n\r\n")
	_, err := c.Write(response)
	require.NoError(t, err)

	src := newScriptedConn()
	require.NoError(t, c.attach(src, time.Second))
	assert.Equal(t, [][]byte{response}, src.written())

	deadlines := src.writeDeadlines()
	require.Len(t, deadlines, 2)
	assert.False(t, deadlines[0].IsZero())
	assert.True(t, deadlines[1].IsZero())
}

func TestCoalescingConnReadBeforeAttachFails(t *testing.T) {
	c := newCoalescingConn()
	_, err := c.Read(make([]byte, 1))
	assert.ErrorIs(t, err, errNotAttached)
}

func TestCoalescingConnWritesBypassHijackedReader(t *testing.T) {
	src, raw := newScriptedConn(), newScriptedConn()
	c := newCoalescingConn()
	c.maxDelay = time.Hour
	require.NoError(t, c.attach(&hijackedConn{scriptedConn: src, raw: raw}, 0))

	_, err := c.Write([]byte("out"))
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("out")}, raw.written())
	assert.Equal(t, 0, src.writeCount())

	feedAndRead(t, c, src, "in")

	require.NoError(t, c.Close())
	assert.True(t, src.isClosed())
}

func TestCoalescingConnWriteErrorIsSticky(t *testing.T) {
	c, src := attachedConn(t)
	src.failWrites(errors.New("boom"))

	_, err := c.Write([]byte("x"))
	require.EqualError(t, err, "boom")
	src.failWrites(nil)
	_, err = c.Write([]byte("y"))
	require.EqualError(t, err, "boom")
}

// clientFrame builds one masked client frame (payload up to 125 bytes).
func clientFrame(op int, payload []byte) []byte {
	key := [4]byte{1, 2, 3, 4}
	f := []byte{0x80 | byte(op), 0x80 | byte(len(payload))}
	f = append(f, key[:]...)
	for i, b := range payload {
		f = append(f, b^key[i&3])
	}
	return f
}

func TestPipelinedEcho(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		defer c.Close()
		for {
			mt, p, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, p); err != nil {
				return
			}
		}
	})
	defer app.Shutdown()

	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	require.NoError(t, err)
	defer conn.Close()

	// Sixteen frames in one write, as a pipelining peer sends them.
	const frames = 16
	var burst []byte
	for i := range frames {
		burst = append(burst, clientFrame(websocket.TextMessage, fmt.Appendf(nil, "frame-%02d", i))...)
	}
	_, err = conn.NetConn().Write(burst)
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for i := range frames {
		mt, p, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, websocket.TextMessage, mt)
		assert.Equal(t, fmt.Sprintf("frame-%02d", i), string(p))
	}
}

func TestHandshakeRejectionStatuses(t *testing.T) {
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Set("X-Request-ID", "req-1")
		return c.Next()
	})
	// All, so a POST reaches the middleware's 405 rather than the router's.
	app.All("/ws", New(func(*Conn) {}, Config{Origins: []string{"http://allowed"}}))

	full := [][2]string{
		{fiber.HeaderConnection, "Upgrade"},
		{fiber.HeaderUpgrade, "websocket"},
		{fiber.HeaderSecWebSocketVersion, "13"},
		{fiber.HeaderSecWebSocketKey, "dGhlIHNhbXBsZSBub25jZQ=="},
	}
	with := func(extra ...[2]string) [][2]string { return append(slices.Clone(full), extra...) }
	cases := []struct {
		name    string
		method  string
		headers [][2]string
		status  int
	}{
		{"no upgrade signal", fiber.MethodGet, nil, fiber.StatusUpgradeRequired},
		{"connection without upgrade", fiber.MethodGet, [][2]string{{fiber.HeaderConnection, "Upgrade"}}, fiber.StatusBadRequest},
		{"upgrade without connection", fiber.MethodGet, [][2]string{{fiber.HeaderUpgrade, "websocket"}}, fiber.StatusBadRequest},
		{"wrong protocol", fiber.MethodGet, [][2]string{{fiber.HeaderConnection, "Upgrade"}, {fiber.HeaderUpgrade, "h2c"}}, fiber.StatusBadRequest},
		{"unsupported version", fiber.MethodGet, with([2]string{fiber.HeaderSecWebSocketVersion, "12"}), fiber.StatusBadRequest},
		{"malformed key", fiber.MethodGet, with([2]string{fiber.HeaderOrigin, "http://allowed"}, [2]string{fiber.HeaderSecWebSocketKey, "not-base64"}), fiber.StatusBadRequest},
		{"forbidden origin", fiber.MethodGet, with([2]string{fiber.HeaderOrigin, "http://evil"}), fiber.StatusForbidden},
		{"not a GET", fiber.MethodPost, full, fiber.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/ws", nil)
			for _, h := range tc.headers {
				req.Header.Set(h[0], h[1])
			}
			resp, err := app.Test(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tc.status, resp.StatusCode)
			assert.Equal(t, "req-1", resp.Header.Get("X-Request-ID"))
			if tc.status != fiber.StatusUpgradeRequired {
				assert.Equal(t, supportedVersion, resp.Header.Get(fiber.HeaderSecWebSocketVersion))
			}
		})
	}
}

func TestUpgradeResponseKeepsMiddlewareHeaders(t *testing.T) {
	app := fiber.New(fiber.Config{ServerHeader: "Fiber"})
	app.Use(func(c fiber.Ctx) error {
		c.Set("X-Request-ID", "req-1")
		c.Cookie(&fiber.Cookie{Name: "session", Value: "s1"})
		return c.Next()
	})
	app.Get("/ws", New(func(*Conn) {}))
	listenTestApp(t, app)
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws", nil)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
	assert.Equal(t, "req-1", resp.Header.Get("X-Request-ID"))
	assert.Contains(t, resp.Header.Get(fiber.HeaderSetCookie), "session=s1")
	assert.Equal(t, "Fiber", resp.Header.Get(fiber.HeaderServer))
	assert.Equal(t, "websocket", resp.Header.Get(fiber.HeaderUpgrade))
}

func TestConnReadMessageReturnsOwnedCopy(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		defer c.Close()
		for {
			mt, p, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, p); err != nil {
				return
			}
		}
	})
	defer app.Shutdown()

	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	// Sizes around the pooled buffer's growth points, including an exact fill.
	for _, size := range []int{0, 5, frameBufferInitial, frameBufferInitial + 1, 100 << 10} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, payload))
		mt, p, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, websocket.BinaryMessage, mt)
		assert.Equal(t, payload, p)
	}
}

func TestFrameReaderCopyIsExactAndPoolIsTrimmed(t *testing.T) {
	fr := &frameReader{}
	msg, err := fr.readAll(bytes.NewReader(bytes.Repeat([]byte{'x'}, frameBufferRetained)))
	require.NoError(t, err)
	assert.Len(t, msg, frameBufferRetained)
	assert.Equal(t, len(msg), cap(msg))
	fr.release()
	assert.Equal(t, frameBufferRetained, cap(fr.buf), "a buffer within the cap goes back to the pool")

	msg, err = fr.readAll(bytes.NewReader(bytes.Repeat([]byte{'x'}, frameBufferRetained+1)))
	require.NoError(t, err)
	assert.Len(t, msg, frameBufferRetained+1)
	fr.release()
	assert.Nil(t, fr.buf, "a buffer past the cap is dropped")
}

func TestSubprotocolServerPreference(t *testing.T) {
	app := setupTestApp(Config{Subprotocols: []string{"chat", "json"}}, func(c *Conn) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(c.Subprotocol()))
	})
	defer app.Shutdown()

	dialer := websocket.Dialer{Subprotocols: []string{"json", "chat"}}
	conn, resp, err := dialer.Dial("ws://localhost:3000/ws/message", nil)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, "chat", resp.Header.Get(fiber.HeaderSecWebSocketProtocol))

	_, p, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "chat", string(p))
}

// listenTestApp serves app on :3000 and waits until it accepts connections.
func listenTestApp(t *testing.T, app *fiber.App) {
	t.Helper()
	go func() {
		_ = app.Listen(":3000", fiber.ListenConfig{DisableStartupMessage: true})
	}()
	require.Eventually(t, func() bool {
		conn, err := net.Dial("tcp", "localhost:3000")
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 5*time.Second, 10*time.Millisecond)
}
