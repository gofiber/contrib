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

var errScriptedTimeout = errors.New("scripted: i/o timeout")

// scriptedConn feeds reads from a channel and records each write. A past read
// deadline makes a read with nothing queued fail at once; a past write
// deadline, or Close, releases a write that was told to block.
type scriptedConn struct {
	reads   chan []byte
	entered chan struct{} // one token per read that reached the socket
	writing chan struct{} // one token per write that blocked

	mu             sync.Mutex
	writes         [][]byte
	closed         bool
	writeErr       error
	blockWrites    bool
	unblock        chan struct{}
	readDeadline   time.Time
	writeDeadlines []time.Time
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{
		reads:   make(chan []byte, 8),
		entered: make(chan struct{}, 8),
		writing: make(chan struct{}, 8),
		unblock: make(chan struct{}),
	}
}

func (s *scriptedConn) Read(p []byte) (int, error) {
	s.mu.Lock()
	expired := !s.readDeadline.IsZero() && s.readDeadline.Before(time.Now())
	s.mu.Unlock()
	if expired {
		select {
		case b := <-s.reads:
			return copy(p, b), nil
		default:
			return 0, errScriptedTimeout
		}
	}
	select {
	case s.entered <- struct{}{}:
	default:
	}
	b, ok := <-s.reads
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (s *scriptedConn) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.writeErr != nil {
		err := s.writeErr
		s.mu.Unlock()
		return 0, err
	}
	block := s.blockWrites
	s.mu.Unlock()
	if block {
		select {
		case s.writing <- struct{}{}:
		default:
		}
		<-s.unblock
		return 0, errScriptedTimeout
	}
	s.mu.Lock()
	s.writes = append(s.writes, bytes.Clone(p))
	s.mu.Unlock()
	return len(p), nil
}

func (s *scriptedConn) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.release()
	return nil
}

func (s *scriptedConn) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blockWrites {
		s.blockWrites = false
		close(s.unblock)
	}
}

func (*scriptedConn) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (*scriptedConn) RemoteAddr() net.Addr { return &net.TCPAddr{} }

func (s *scriptedConn) SetDeadline(t time.Time) error {
	_ = s.SetReadDeadline(t)
	return s.SetWriteDeadline(t)
}

func (s *scriptedConn) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.readDeadline = t
	s.mu.Unlock()
	return nil
}

func (s *scriptedConn) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.writeDeadlines = append(s.writeDeadlines, t)
	s.mu.Unlock()
	if !t.IsZero() && t.Before(time.Now()) {
		s.release()
	}
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

func (s *scriptedConn) stallWrites() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockWrites = true
}

// hijackedConn mimics fasthttp's hijacked conn: reads come through it,
// UnsafeConn is the socket underneath.
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
	c.attach(src)
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

// readInBackground parks the reader on the socket and releases it at cleanup.
func readInBackground(t *testing.T, c *coalescingConn, src *scriptedConn) {
	t.Helper()
	for len(src.entered) > 0 {
		<-src.entered
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		_, _ = c.Read(buf)
	}()
	select {
	case <-src.entered:
	case <-time.After(time.Second):
		t.Fatal("reader never reached the socket")
	}
	t.Cleanup(func() {
		close(src.reads)
		<-done
	})
}

func TestCoalescingConnHoldsRepliesUntilReaderReturns(t *testing.T) {
	c, src := attachedConn(t)
	feedAndRead(t, c, src, "frame1frame2")

	_, err := c.Write([]byte("reply1"))
	require.NoError(t, err)
	_, err = c.Write([]byte("reply2"))
	require.NoError(t, err)
	assert.Equal(t, 0, src.writeCount())

	readInBackground(t, c, src)
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
	readInBackground(t, c, src)

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

func TestCoalescingConnCloseInterruptsStalledWriter(t *testing.T) {
	c, src := attachedConn(t)
	src.stallWrites()

	writeErr := make(chan error, 1)
	go func() {
		_, err := c.Write([]byte("stuck"))
		writeErr <- err
	}()
	select {
	case <-src.writing:
	case <-time.After(time.Second):
		t.Fatal("writer never reached the socket")
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close waited behind a stalled writer")
	}
	require.Error(t, <-writeErr)
	assert.True(t, src.isClosed())
}

func TestCoalescingConnReaderDoesNotWaitForStalledWriter(t *testing.T) {
	c, src := attachedConn(t)
	src.stallWrites()
	go func() { _, _ = c.Write([]byte("stuck")) }()
	select {
	case <-src.writing:
	case <-time.After(time.Second):
		t.Fatal("writer never reached the socket")
	}

	src.reads <- []byte("in")
	buf := make([]byte, 64)
	read := make(chan int, 1)
	go func() {
		n, _ := c.Read(buf)
		read <- n
	}()
	select {
	case n := <-read:
		assert.Equal(t, "in", string(buf[:n]))
	case <-time.After(time.Second):
		t.Fatal("reader waited behind a stalled writer")
	}
	src.release()
}

func TestCoalescingConnHandshakeIsTakenNotSent(t *testing.T) {
	c := newCoalescingConn()
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n\r\n")
	_, err := c.Write(response)
	require.NoError(t, err)
	assert.Equal(t, response, c.takeHandshake())

	src := newScriptedConn()
	c.attach(src)
	assert.Equal(t, 0, src.writeCount())
}

func TestCoalescingConnServesBytesFasthttpBufferedFirst(t *testing.T) {
	src, raw := newScriptedConn(), newScriptedConn()
	src.reads <- []byte("early")
	c := newCoalescingConn()
	c.maxDelay = time.Hour
	c.attach(&hijackedConn{scriptedConn: src, raw: raw})

	buf := make([]byte, 64)
	n, err := c.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "early", string(buf[:n]))
	assert.Empty(t, raw.entered, "the stash is served without touching the socket")

	raw.reads <- []byte("later")
	n, err = c.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "later", string(buf[:n]))

	_, err = c.Write([]byte("out")) // held: the reader has a batch in hand
	require.NoError(t, err)
	require.NoError(t, c.Close())
	assert.Equal(t, 0, src.writeCount())
	assert.Equal(t, [][]byte{[]byte("out")}, raw.written(), "writes reach the socket, not fasthttp's wrapper")
	assert.True(t, src.isClosed(), "closing the hijacked conn hands it back to fasthttp")
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

func TestFrameReaderCopyIsExactAndTrimmed(t *testing.T) {
	fr := &frameReader{}
	msg, err := fr.readAll(bytes.NewReader(bytes.Repeat([]byte{'x'}, frameBufferRetained)))
	require.NoError(t, err)
	assert.Len(t, msg, frameBufferRetained)
	assert.Equal(t, len(msg), cap(msg))
	fr.trim()
	assert.Equal(t, frameBufferRetained, cap(fr.buf), "a buffer within the cap is kept")

	msg, err = fr.readAll(bytes.NewReader(bytes.Repeat([]byte{'x'}, frameBufferRetained+1)))
	require.NoError(t, err)
	assert.Len(t, msg, frameBufferRetained+1)
	fr.trim()
	assert.Nil(t, fr.buf, "a buffer past the cap is dropped")
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

func echoHandler(c *Conn) {
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
}

func handshakeRequestHeaders() [][2]string {
	return [][2]string{
		{fiber.HeaderConnection, "Upgrade"},
		{fiber.HeaderUpgrade, "websocket"},
		{fiber.HeaderSecWebSocketVersion, "13"},
		{fiber.HeaderSecWebSocketKey, "dGhlIHNhbXBsZSBub25jZQ=="},
	}
}

func TestPipelinedEcho(t *testing.T) {
	app := setupTestApp(Config{}, echoHandler)
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

	full := handshakeRequestHeaders()
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

func TestUpgradeThroughAppTest(t *testing.T) {
	app := fiber.New()
	app.Get("/ws", New(func(*Conn) {}))

	req := httptest.NewRequest(fiber.MethodGet, "/ws", nil)
	for _, h := range handshakeRequestHeaders() {
		req.Header.Set(h[0], h[1])
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get(fiber.HeaderUpgrade))
	assert.Equal(t, "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=", resp.Header.Get(fiber.HeaderSecWebSocketAccept))
}

func TestUpgradeResponseIsVisibleAfterNext(t *testing.T) {
	type seen struct {
		status  int
		upgrade string
	}
	observed := make(chan seen, 1)
	app := fiber.New(fiber.Config{ServerHeader: "Fiber"})
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		// What logger, metrics and session middleware see and do after Next.
		observed <- seen{c.Response().StatusCode(), string(c.Response().Header.Peek(fiber.HeaderUpgrade))}
		c.Set("X-After", "1")
		c.Cookie(&fiber.Cookie{Name: "session", Value: "s1"})
		return err
	})
	app.Get("/ws", New(func(*Conn) {}))
	listenTestApp(t, app)
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws", nil)
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, seen{fiber.StatusSwitchingProtocols, "websocket"}, <-observed)
	assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
	assert.Equal(t, "1", resp.Header.Get("X-After"))
	assert.Contains(t, resp.Header.Get(fiber.HeaderSetCookie), "session=s1")
	assert.Equal(t, "Fiber", resp.Header.Get(fiber.HeaderServer))
	assert.NotEmpty(t, resp.Header.Get(fiber.HeaderDate))
}

func TestConnNetConnExposesSocket(t *testing.T) {
	socket := make(chan net.Conn, 1)
	app := setupTestApp(Config{}, func(c *Conn) {
		u, ok := c.NetConn().(interface{ UnsafeConn() net.Conn })
		if !ok {
			socket <- nil
			return
		}
		socket <- u.UnsafeConn()
	})
	defer app.Shutdown()

	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	require.NoError(t, err)
	defer conn.Close()
	_, isTCP := (<-socket).(*net.TCPConn)
	assert.True(t, isTCP)
}

func TestConnReadMessageReturnsOwnedCopy(t *testing.T) {
	app := setupTestApp(Config{}, echoHandler)
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
