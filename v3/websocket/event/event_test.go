package event

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fasthttp/websocket"
	fws "github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp/fasthttputil"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// fasthttp keeps a worker pool and a server-date refresher that
		// drain lazily after Server.Shutdown. The top of stack is
		// time.Sleep, so match these by any-frame.
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*workerPool).Start.func1"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*workerPool).Start.func2"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*workerPool).workerFunc"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.updateServerDate.func1"),
	)
}

const (
	numTestConn         = 10
	numParallelTestConn = 200
)

type HandlerMock struct {
	mock.Mock
	wg sync.WaitGroup
}

type WebsocketMock struct {
	mock.Mock
	mu         sync.RWMutex
	wg         sync.WaitGroup
	Conn       *websocket.Conn
	isAlive    bool
	queue      map[string]message
	attributes map[string]string
	UUID       string
	Locals     func(key string) interface{}
	Params     func(key string, defaultValue ...string) string
	Query      func(key string, defaultValue ...string) string
	Cookies    func(key string, defaultValue ...string) string
}

func resetState() {
	pool.Lock()
	pool.conn = make(map[string]ws)
	pool.Unlock()
	listeners.Lock()
	listeners.list = make(map[string][]EventCallback)
	listeners.Unlock()
	draining.Store(false)
}

func (s *WebsocketMock) SetUUID(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := pool.get(uuid); err == nil {
		return ErrorUUIDDuplication
	}
	s.UUID = uuid
	return nil
}

func (s *WebsocketMock) GetIntAttribute(key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.attributes[key]
	if ok {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return 0
}

func (s *WebsocketMock) GetStringAttribute(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.attributes[key]
	if ok {
		return value
	}
	return ""
}

func (h *HandlerMock) OnCustomEvent(payload *EventPayload) {
	h.Called(payload)
	h.wg.Done()
}

func (s *WebsocketMock) Emit(message []byte, _ ...int) {
	s.Called(message)
	s.wg.Done()
}

func (s *WebsocketMock) IsAlive() bool {
	args := s.Called()
	return args.Bool(0)
}

func (s *WebsocketMock) GetUUID() string {
	return s.UUID
}

func TestPlainWebSocketClientReceivesEventResponse(t *testing.T) {
	resetState()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	wg := sync.WaitGroup{}

	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	app.Use(upgradeMiddleware)
	On(EventMessage, func(payload *EventPayload) {
		if string(payload.Data) == "test" {
			payload.Kws.Emit([]byte("response"))
		}
	})
	app.Get("/", New(func(_ *Websocket) {}))

	go func() {
		_ = app.Listener(ln)
	}()

	wsURL := "ws://" + ln.Addr().String()
	for i := 0; i < numParallelTestConn; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dialer := &websocket.Dialer{
				NetDial: func(network, addr string) (net.Conn, error) {
					return ln.Dial()
				},
				HandshakeTimeout: 45 * time.Second,
			}
			dial, _, err := dialer.Dial(wsURL, nil)
			require.NoError(t, err)
			defer func() { _ = dial.Close() }()

			require.NoError(t, dial.WriteMessage(websocket.TextMessage, []byte("test")))

			tp, msg, err := dial.ReadMessage()
			require.NoError(t, err)
			require.Equal(t, TextMessage, tp)
			require.Equal(t, "response", string(msg))
		}()
	}
	wg.Wait()
}

func TestGlobalFire(t *testing.T) {
	resetState()

	for i := 0; i < numTestConn; i++ {
		kws := createWS()
		pool.set(kws)
	}

	h := new(HandlerMock)
	h.On("OnCustomEvent", mock.Anything).Return(nil)
	h.wg.Add(numTestConn)

	On("customevent", h.OnCustomEvent)
	Fire("customevent", []byte("test"))

	h.wg.Wait()
	h.AssertNumberOfCalls(t, "OnCustomEvent", numTestConn)
}

func TestGlobalBroadcast(t *testing.T) {
	resetState()

	for i := 0; i < numParallelTestConn; i++ {
		mws := new(WebsocketMock)
		require.NoError(t, mws.SetUUID(mws.createUUID()))
		pool.set(mws)

		mws.On("Emit", mock.Anything).Return(nil)
		mws.wg.Add(1)
	}

	Broadcast([]byte("test"), TextMessage)

	for _, mws := range pool.snapshot() {
		mws.(*WebsocketMock).wg.Wait()
		mws.(*WebsocketMock).AssertNumberOfCalls(t, "Emit", 1)
	}
}

func TestGlobalEmitTo(t *testing.T) {
	resetState()

	aliveUUID := "80a80sdf809dsf"
	closedUUID := "las3dfj09808"

	alive := new(WebsocketMock)
	alive.UUID = aliveUUID
	pool.set(alive)

	closed := new(WebsocketMock)
	closed.UUID = closedUUID
	pool.set(closed)

	alive.On("Emit", mock.Anything).Return(nil)
	alive.On("IsAlive").Return(true)
	closed.On("IsAlive").Return(false)

	err := EmitTo("non-existent", []byte("error"))
	require.ErrorIs(t, err, ErrorInvalidConnection)

	err = EmitTo(closedUUID, []byte("error"))
	require.ErrorIs(t, err, ErrorInvalidConnection)

	alive.wg.Add(1)
	err = EmitTo(aliveUUID, []byte("test"))
	require.NoError(t, err)

	alive.wg.Wait()
	alive.AssertNumberOfCalls(t, "Emit", 1)
}

func TestGlobalEmitToList(t *testing.T) {
	resetState()

	uuids := []string{
		"80a80sdf809dsf",
		"las3dfj09808",
	}

	for _, id := range uuids {
		kws := new(WebsocketMock)
		require.NoError(t, kws.SetUUID(id))
		kws.On("Emit", mock.Anything).Return(nil)
		kws.On("IsAlive").Return(true)
		kws.wg.Add(1)
		pool.set(kws)
	}

	EmitToList(uuids, []byte("test"), TextMessage)

	for _, kws := range pool.snapshot() {
		kws.(*WebsocketMock).wg.Wait()
		kws.(*WebsocketMock).AssertNumberOfCalls(t, "Emit", 1)
	}
}

func TestWebsocketGetIntAttribute(t *testing.T) {
	kws := &Websocket{
		attributes: make(map[string]interface{}),
	}

	kws.SetAttribute("notInt", "")
	kws.SetAttribute("int", 3)

	require.Equal(t, 3, kws.GetIntAttribute("int"))
	require.Equal(t, 0, kws.GetIntAttribute("notInt"))
	require.Equal(t, 0, kws.GetIntAttribute("missing"))
}

func TestWebsocketGetStringAttribute(t *testing.T) {
	kws := &Websocket{
		attributes: make(map[string]interface{}),
	}

	kws.SetAttribute("notString", 3)
	kws.SetAttribute("str", "3")

	require.Equal(t, "3", kws.GetStringAttribute("str"))
	require.Equal(t, "", kws.GetStringAttribute("notString"))
	require.Equal(t, "", kws.GetStringAttribute("missing"))
}

func TestWebsocketSetUUIDUpdatesPool(t *testing.T) {
	resetState()

	kws := createWS()
	pool.set(kws)

	oldUUID := kws.GetUUID()
	newUUID := "new-uuid"

	err := kws.SetUUID(newUUID)
	require.NoError(t, err)
	require.Equal(t, newUUID, kws.GetUUID())

	_, err = pool.get(oldUUID)
	require.ErrorIs(t, err, ErrorInvalidConnection)

	poolEntry, err := pool.get(newUUID)
	require.NoError(t, err)
	require.Equal(t, kws, poolEntry)

	other := createWS()
	other.UUID = "other-uuid"
	pool.set(other)

	err = kws.SetUUID(other.UUID)
	require.ErrorIs(t, err, ErrorUUIDDuplication)
	require.Equal(t, newUUID, kws.GetUUID())

	poolEntry, err = pool.get(newUUID)
	require.NoError(t, err)
	require.Equal(t, kws, poolEntry)
}

func TestWebsocketCloseRemovesConnectionFromPool(t *testing.T) {
	resetState()

	kws := createWS()
	pool.set(kws)
	closeEvents := 0
	disconnectEvents := 0
	On(EventClose, func(*EventPayload) {
		closeEvents++
	})
	On(EventDisconnect, func(*EventPayload) {
		disconnectEvents++
	})

	kws.Close()
	kws.Close()
	var wg sync.WaitGroup
	wg.Add(numTestConn)
	for range numTestConn {
		go func() {
			defer wg.Done()
			kws.Close()
		}()
	}
	wg.Wait()

	require.False(t, kws.IsAlive())
	_, err := pool.get(kws.GetUUID())
	require.ErrorIs(t, err, ErrorInvalidConnection)
	require.Equal(t, 1, closeEvents)
	require.Equal(t, 1, disconnectEvents)
}

func TestWebsocketCloseDoesNotBlockOnFullQueue(t *testing.T) {
	resetState()

	kws := createWS()
	pool.set(kws)

	// Fill the send queue to capacity so any further write would block on the
	// queue channel if it were not guarded by the done channel.
	for i := 0; i < cap(kws.queue); i++ {
		kws.queue <- message{mType: TextMessage, data: []byte("queued")}
	}

	kws.Close()
	require.False(t, kws.IsAlive())

	// write() is the path that genuinely interacts with the queue. After Close
	// the done channel is closed, so write must return immediately via its
	// <-kws.done case instead of blocking on the full queue.
	done := make(chan struct{})
	go func() {
		kws.write(TextMessage, []byte("after close"))
		close(done)
	}()

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

func TestDrainFlag(t *testing.T) {
	resetState()
	require.False(t, IsDraining())
	Drain()
	require.True(t, IsDraining())
	draining.Store(false)
	require.False(t, IsDraining())
}

func TestCloseAllSendsGoingAway(t *testing.T) {
	resetState()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	app.Use(upgradeMiddleware)
	app.Get("/", New(func(_ *Websocket) {}))

	go func() { _ = app.Listener(ln) }()

	dialer := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 5 * time.Second,
	}

	const numConn = 5
	clients := make([]*websocket.Conn, 0, numConn)
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	for i := 0; i < numConn; i++ {
		conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String())
		require.NoError(t, err)
		clients = append(clients, conn)
	}

	// Give upgrades a moment to register in the pool.
	require.Eventually(t, func() bool {
		return len(pool.snapshot()) == numConn
	}, 2*time.Second, 10*time.Millisecond, "expected %d pool entries", numConn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, CloseAll(ctx, websocket.CloseGoingAway, "bye"))

	for _, c := range clients {
		_, _, err := c.ReadMessage()
		require.Error(t, err)
		ce, ok := err.(*websocket.CloseError)
		require.True(t, ok, "expected *websocket.CloseError, got %T", err)
		require.Equal(t, websocket.CloseGoingAway, ce.Code)
		require.Equal(t, "bye", ce.Text)
	}

	require.Empty(t, pool.snapshot())
}

func TestCloseSendsFormatCloseMessage(t *testing.T) {
	resetState()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	upgraded := make(chan *Websocket, 1)
	app.Use(upgradeMiddleware)
	app.Get("/", New(func(kws *Websocket) {
		upgraded <- kws
	}))

	go func() { _ = app.Listener(ln) }()

	dialer := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	var kws *Websocket
	select {
	case kws = <-upgraded:
	case <-time.After(2 * time.Second):
		t.Fatal("upgrade did not complete")
	}

	go kws.Close()

	_, _, err = conn.ReadMessage()
	require.Error(t, err)
	ce, ok := err.(*websocket.CloseError)
	require.True(t, ok, "expected *websocket.CloseError, got %T (%v)", err, err)
	require.Equal(t, websocket.CloseNormalClosure, ce.Code)
	require.Equal(t, "Connection closed", ce.Text)
}

func TestCloseInterruptsReadDespitePongs(t *testing.T) {
	resetState()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	upgraded := make(chan *Websocket, 1)
	app.Use(upgradeMiddleware)
	app.Get("/", NewWithConfig(func(kws *Websocket) {
		upgraded <- kws
	}, Config{
		PingInterval: time.Hour,
		// Long enough that shutdown completing at all proves it does not
		// depend on the read deadline expiring, whether the initial one or
		// one refreshed by a pong racing with Close.
		ReadIdleTimeout: time.Hour,
	}))

	go func() { _ = app.Listener(ln) }()

	dialer := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	var kws *Websocket
	select {
	case kws = <-upgraded:
	case <-time.After(2 * time.Second):
		t.Fatal("upgrade did not complete")
	}

	// Keep pongs in flight before and during Close so a pong handler can be
	// mid-flight, past its done check, exactly when shutdown starts.
	stopPongs := make(chan struct{})
	pongsStopped := make(chan struct{})
	go func() {
		defer close(pongsStopped)
		for {
			select {
			case <-stopPongs:
				return
			default:
			}
			if err := conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second)); err != nil {
				// The server closed the connection; nothing left to send.
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() {
		close(stopPongs)
		<-pongsStopped
	}()

	// Give the pong flood a moment to reach the read goroutine.
	time.Sleep(20 * time.Millisecond)

	kws.Close()

	require.Eventually(t, func() bool {
		kws.mu.RLock()
		defer kws.mu.RUnlock()
		return kws.Conn == nil
	}, 2*time.Second, 10*time.Millisecond)
}

func TestCloseConnNilsConnField(t *testing.T) {
	kws := createWS()
	kws.settings = resolveSettings(Config{})
	kws.closeConn()
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	require.Nil(t, kws.Conn)
}

func TestPingIsSentAtInterval(t *testing.T) {
	resetState()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	app.Use(upgradeMiddleware)
	app.Get("/", NewWithConfig(func(_ *Websocket) {},
		Config{PingInterval: 50 * time.Millisecond}))

	go func() { _ = app.Listener(ln) }()

	dialer := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	pings := make(chan struct{}, 4)
	conn.SetPingHandler(func(_ string) error {
		select {
		case pings <- struct{}{}:
		default:
		}
		return nil
	})

	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	deadline := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-pings:
		case <-deadline:
			t.Fatalf("expected at least 2 pings within 2s, got %d", i)
		}
	}
}

func TestReadDeadlineFiresDisconnectOnSilentPeer(t *testing.T) {
	resetState()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	disconnected := make(chan error, 1)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disconnected <- p.Error:
		default:
		}
	})

	app.Use(upgradeMiddleware)
	app.Get("/", NewWithConfig(func(_ *Websocket) {},
		Config{
			PingInterval:    50 * time.Millisecond,
			ReadIdleTimeout: 150 * time.Millisecond,
		}))

	go func() { _ = app.Listener(ln) }()

	dialer := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String())
	require.NoError(t, err)
	// Suppress automatic pong response so the server's read deadline fires.
	conn.SetPingHandler(func(_ string) error { return nil })
	defer func() { _ = conn.Close() }()

	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case discErr := <-disconnected:
		require.Error(t, discErr)
		type timeoutErr interface{ Timeout() bool }
		te, ok := discErr.(timeoutErr)
		require.True(t, ok, "expected error with Timeout() method, got %T", discErr)
		require.True(t, te.Timeout(), "expected timeout error, got %v", discErr)
	case <-time.After(2 * time.Second):
		t.Fatal("expected disconnect after read deadline")
	}
}

func TestReadLimitRejectsOversizedFrame(t *testing.T) {
	resetState()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	disconnected := make(chan error, 1)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disconnected <- p.Error:
		default:
		}
	})

	app.Use(upgradeMiddleware)
	app.Get("/", NewWithConfig(func(_ *Websocket) {}, Config{MaxMessageSize: 16}))

	go func() {
		_ = app.Listener(ln)
	}()

	dialer := &websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			return ln.Dial()
		},
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, make([]byte, 1024)))

	select {
	case discErr := <-disconnected:
		require.Error(t, discErr)
	case <-time.After(2 * time.Second):
		t.Fatal("expected disconnect event after oversize frame")
	}
}

func dialWithRetry(dialer *websocket.Dialer, url string) (*websocket.Conn, *http.Response, error) {
	var lastErr error
	for i := 0; i < 50; i++ {
		conn, resp, err := dialer.Dial(url, nil)
		if err == nil {
			return conn, resp, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, nil, lastErr
}

func TestListenerPanicIsRecovered(t *testing.T) {
	resetState()

	kws := createWS()
	kws.settings = resolveSettings(Config{
		RecoverHandler: func(event string, r any) {
			atomic.AddInt32(panicCounter, 1)
		},
	})
	atomic.StoreInt32(panicCounter, 0)
	pool.set(kws)

	On(EventMessage, func(*EventPayload) {
		panic("listener boom")
	})
	survived := 0
	On(EventMessage, func(*EventPayload) {
		survived++
	})

	kws.fireEvent(EventMessage, []byte("ignored"), nil)

	require.Equal(t, int32(1), atomic.LoadInt32(panicCounter))
	require.Equal(t, 1, survived)
}

func TestEventPayloadDataIsIndependentOfReadBuffer(t *testing.T) {
	resetState()

	kws := createWS()
	pool.set(kws)

	var captured []byte
	On(EventMessage, func(p *EventPayload) {
		captured = p.Data
	})

	buf := []byte("first")
	kws.fireEvent(EventMessage, buf, nil)
	buf[0] = 'x'

	require.Equal(t, "first", string(captured))
}

func TestOffRemovesGlobalListener(t *testing.T) {
	resetState()

	var calls int32
	On(EventMessage, func(*EventPayload) { atomic.AddInt32(&calls, 1) })

	kws := createWS()
	kws.fireEvent(EventMessage, nil, nil)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	Off(EventMessage)
	kws.fireEvent(EventMessage, nil, nil)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestConnectionScopedListeners(t *testing.T) {
	resetState()

	var calls int32
	kws1 := createWS()
	kws2 := createWS()
	kws1.On(EventMessage, func(*EventPayload) { atomic.AddInt32(&calls, 1) })

	// A per-connection listener does not fire for other connections.
	kws2.fireEvent(EventMessage, nil, nil)
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))

	// It fires for its own connection.
	kws1.fireEvent(EventMessage, nil, nil)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// Off removes the per-connection listener.
	kws1.Off(EventMessage)
	kws1.fireEvent(EventMessage, nil, nil)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestGlobalAndScopedListenersBothFire(t *testing.T) {
	resetState()

	var global, local int32
	On(EventMessage, func(*EventPayload) { atomic.AddInt32(&global, 1) })

	kws := createWS()
	kws.On(EventMessage, func(*EventPayload) { atomic.AddInt32(&local, 1) })
	kws.fireEvent(EventMessage, nil, nil)

	require.Equal(t, int32(1), atomic.LoadInt32(&global))
	require.Equal(t, int32(1), atomic.LoadInt32(&local))
}

func TestSendDropFiresEventErrorAfterRetries(t *testing.T) {
	resetState()

	var errs int32
	On(EventError, func(p *EventPayload) {
		if errors.Is(p.Error, ErrorInvalidConnection) {
			atomic.AddInt32(&errs, 1)
		}
	})

	// createWS leaves Conn nil, so conn() returns nil and zero settings give
	// maxSendRetry 0: the message is requeued once and then dropped.
	kws := createWS()
	kws.queue <- message{mType: TextMessage, data: []byte("x")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go kws.send(ctx)

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&errs) == 1
	}, time.Second, time.Millisecond)
}

func TestMethodEmitToFiresEventError(t *testing.T) {
	resetState()

	var errs int32
	On(EventError, func(p *EventPayload) {
		if errors.Is(p.Error, ErrorInvalidConnection) {
			atomic.AddInt32(&errs, 1)
		}
	})

	kws := createWS()
	err := kws.EmitTo("does-not-exist", []byte("x"))
	require.ErrorIs(t, err, ErrorInvalidConnection)
	require.Equal(t, int32(1), atomic.LoadInt32(&errs))
}

func TestMethodEmitToListFiresEventError(t *testing.T) {
	resetState()

	var errs int32
	On(EventError, func(*EventPayload) { atomic.AddInt32(&errs, 1) })

	kws := createWS()
	kws.EmitToList([]string{"missing-1", "missing-2"}, []byte("x"))
	require.Equal(t, int32(2), atomic.LoadInt32(&errs))
}

var panicCounter = new(int32)

func TestWebsocketDisconnectedFiresOnce(t *testing.T) {
	resetState()

	kws := createWS()
	pool.set(kws)
	disconnectEvents := 0
	errorEvents := 0
	On(EventDisconnect, func(payload *EventPayload) {
		require.Error(t, payload.Error)
		disconnectEvents++
	})
	On(EventError, func(payload *EventPayload) {
		require.Error(t, payload.Error)
		errorEvents++
	})

	testErr := errors.New("disconnect")
	kws.disconnected(testErr)
	kws.disconnected(testErr)
	kws.disconnected(nil)

	require.False(t, kws.IsAlive())
	_, err := pool.get(kws.GetUUID())
	require.ErrorIs(t, err, ErrorInvalidConnection)
	require.Equal(t, 1, disconnectEvents)
	require.Equal(t, 1, errorEvents)
}

func createWS() *Websocket {
	kws := &Websocket{
		Conn: nil,
		Locals: func(key string) interface{} {
			return ""
		},
		Params: func(key string, defaultValue ...string) string {
			return ""
		},
		Query: func(key string, defaultValue ...string) string {
			return ""
		},
		Cookies: func(key string, defaultValue ...string) string {
			return ""
		},
		queue:      make(chan message, 1),
		done:       make(chan struct{}, 1),
		attributes: make(map[string]interface{}),
		isAlive:    true,
	}

	kws.UUID = kws.createUUID()
	return kws
}

func upgradeMiddleware(c fiber.Ctx) error {
	if fws.IsWebSocketUpgrade(c) {
		fiber.StoreInContext(c, "allowed", true)
		return c.Next()
	}
	return fiber.ErrUpgradeRequired
}

func (s *WebsocketMock) SetAttribute(_ string, _ interface{}) {
	panic("implement me")
}

func (s *WebsocketMock) GetAttribute(_ string) interface{} {
	panic("implement me")
}

func (s *WebsocketMock) EmitToList(_ []string, _ []byte, _ ...int) {
	panic("implement me")
}

func (s *WebsocketMock) EmitTo(_ string, _ []byte, _ ...int) error {
	panic("implement me")
}

func (s *WebsocketMock) Broadcast(_ []byte, _ bool, _ ...int) {
	panic("implement me")
}

func (s *WebsocketMock) Fire(_ string, _ []byte) {
	panic("implement me")
}

func (s *WebsocketMock) Close() {
	panic("implement me")
}

func (s *WebsocketMock) pong(_ context.Context) {
	panic("implement me")
}

func (s *WebsocketMock) write(_ int, _ []byte) {
	panic("implement me")
}

func (s *WebsocketMock) run() {
	panic("implement me")
}

func (s *WebsocketMock) read(_ context.Context) {
	panic("implement me")
}

func (s *WebsocketMock) disconnected(_ error) {
	panic("implement me")
}

func (s *WebsocketMock) createUUID() string {
	return s.randomUUID()
}

func (s *WebsocketMock) randomUUID() string {
	return uuid.New().String()
}

func (s *WebsocketMock) fireEvent(_ string, _ []byte, _ error) {
	panic("implement me")
}

func (s *WebsocketMock) fireOwnedEvent(_ string, _ []byte, _ error) {
	panic("implement me")
}

func TestCloseReasonStaysValidUTF8(t *testing.T) {
	t.Run("short reason is untouched", func(t *testing.T) {
		require.Equal(t, "bye", closeReason("bye"))
	})

	t.Run("truncation lands on a rune boundary", func(t *testing.T) {
		// Each euro sign is three bytes, so a plain byte cut at 123 would slice
		// the 41st one in half and put invalid UTF-8 in the close frame.
		reason := closeReason(strings.Repeat("€", 50))
		require.LessOrEqual(t, len(reason), closeFrameMaxReason)
		require.True(t, utf8.ValidString(reason))
		require.Equal(t, 41, utf8.RuneCountInString(reason))
	})

	t.Run("ascii reason fills the limit exactly", func(t *testing.T) {
		reason := closeReason(strings.Repeat("a", 200))
		require.Len(t, reason, closeFrameMaxReason)
	})

	t.Run("invalid input is scrubbed", func(t *testing.T) {
		reason := closeReason("ok\xff\xfebye")
		require.True(t, utf8.ValidString(reason))
		require.Equal(t, "okbye", reason)
	})
}

func TestSanitizeCloseCode(t *testing.T) {
	// See sanitizeCloseCode for why each of these is rewritten.
	for _, code := range []int{
		0, 999, 1004, websocket.CloseAbnormalClosure, 1014,
		websocket.CloseTLSHandshake, 1016, 2999, 5000,
	} {
		require.Equal(t, websocket.CloseNormalClosure, sanitizeCloseCode(code),
			"code %d must not reach the wire", code)
	}

	// Everything a peer accepts travels unchanged.
	for _, code := range []int{
		websocket.CloseNormalClosure, websocket.CloseGoingAway,
		websocket.CloseProtocolError, websocket.CloseUnsupportedData,
		websocket.CloseInvalidFramePayloadData, websocket.ClosePolicyViolation,
		websocket.CloseMessageTooBig, websocket.CloseMandatoryExtension,
		websocket.CloseInternalServerErr, websocket.CloseServiceRestart,
		websocket.CloseTryAgainLater, 3000, 4000, 4999,
	} {
		require.Equal(t, code, sanitizeCloseCode(code), "code %d must survive", code)
	}

	// 1005 keeps its meaning: FormatCloseMessage renders it as an empty payload.
	require.Equal(t, websocket.CloseNoStatusReceived, sanitizeCloseCode(websocket.CloseNoStatusReceived))
	require.Empty(t, websocket.FormatCloseMessage(sanitizeCloseCode(websocket.CloseNoStatusReceived), "ignored"))
}

func TestCloseAllSanitizesReservedCode(t *testing.T) {
	// Each of these makes a conformant peer fail the connection with a protocol
	// error instead of reading a close (see sanitizeCloseCode), so CloseAll
	// must not put them on the wire.
	for _, code := range []int{1004, websocket.CloseAbnormalClosure, 1014, websocket.CloseTLSHandshake, 2999} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			resetState()

			app := fiber.New()
			ln := fasthttputil.NewInmemoryListener()
			defer func() {
				_ = app.Shutdown()
				_ = ln.Close()
			}()

			app.Use(upgradeMiddleware)
			app.Get("/", New(func(_ *Websocket) {}))

			go func() { _ = app.Listener(ln) }()

			dialer := &websocket.Dialer{
				NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
				HandshakeTimeout: 5 * time.Second,
			}
			conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String())
			require.NoError(t, err)
			defer conn.Close()

			require.Eventually(t, func() bool {
				return len(pool.snapshot()) == 1
			}, 2*time.Second, 10*time.Millisecond)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			require.NoError(t, CloseAll(ctx, code, strings.Repeat("€", 50)))

			// The peer reads a well formed close rather than rejecting the
			// frame, and the reason survives as valid UTF-8 within the limit.
			_, _, err = conn.ReadMessage()
			require.Error(t, err)
			var ce *websocket.CloseError
			require.ErrorAs(t, err, &ce)
			require.Equal(t, websocket.CloseNormalClosure, ce.Code)
			require.True(t, utf8.ValidString(ce.Text))
			require.LessOrEqual(t, len(ce.Text), closeFrameMaxReason)
		})
	}
}

func TestCallbackPanicDoesNotStrandPoolEntry(t *testing.T) {
	resetState()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	type disconnect struct {
		err      error
		detached bool
	}
	disconnects := make(chan disconnect, 1)
	On(EventDisconnect, func(p *EventPayload) {
		disconnects <- disconnect{err: p.Error, detached: p.Kws.conn() == nil}
	})

	app.Use(upgradeMiddleware)
	app.Get("/", NewWithConfig(func(_ *Websocket) {
		panic("callback boom")
	}, Config{}, fws.Config{
		RecoverHandler: func(c *fws.Conn) {
			if r := recover(); r != nil {
				_ = c.WriteJSON(fiber.Map{"error": r})
			}
		},
	}))

	go func() { _ = app.Listener(ln) }()

	dialer := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	// The cleanup must not run ahead of the middleware's recover handler: the
	// client still receives the error frame the handler is documented to send.
	var msg fiber.Map
	require.NoError(t, conn.ReadJSON(&msg))
	require.Equal(t, "callback boom", msg["error"])

	// The upgrade succeeded, so the connection was registered. A panicking
	// callback must still take it back out: a stranded entry has a done channel
	// nobody closes, so every later Broadcast would fill its queue and block.
	require.Eventually(t, func() bool {
		return len(pool.snapshot()) == 0
	}, 2*time.Second, 10*time.Millisecond, "panicking callback stranded a pool entry")

	// Listeners learn it was a crash, not a clean close, and the helper no
	// longer points at the wrapper the middleware is about to recycle.
	select {
	case d := <-disconnects:
		require.ErrorIs(t, d.err, ErrorCallbackPanic)
		require.True(t, d.detached, "kws.Conn still aliases the pooled wrapper")
	case <-time.After(2 * time.Second):
		t.Fatal("EventDisconnect was not fired for the panicking callback")
	}

	// The socket is closed rather than left dangling on a hijacked connection.
	// The deadline only keeps a regression from blocking forever; a timeout
	// means the socket stayed open, which is the failure being guarded against.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, err = conn.ReadMessage()
	require.Error(t, err)
	var netErr net.Error
	require.False(t, errors.As(err, &netErr) && netErr.Timeout(),
		"panicking callback left the connection open: %v", err)
}

func TestCloseAllListenersStillSeeRequestData(t *testing.T) {
	// EventDisconnect listeners run on the goroutine that called CloseAll,
	// while each connection's handler unwinds concurrently and hands its
	// wrapper back to the middleware pool. The request data those listeners
	// read through the wrapper must survive that: emptying the maps at
	// release time raced with these reads and returned "" (or, under -race,
	// a report against (*Conn).Query).
	resetState()

	const numConn = 25
	var mu sync.Mutex
	seen := make(map[string]int)
	On(EventDisconnect, func(p *EventPayload) {
		mu.Lock()
		seen[p.Kws.Query("who")]++
		mu.Unlock()
	})

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	app.Use(upgradeMiddleware)
	app.Get("/", New(func(_ *Websocket) {}))

	go func() { _ = app.Listener(ln) }()

	dialer := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 5 * time.Second,
	}
	clients := make([]*websocket.Conn, 0, numConn)
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for i := 0; i < numConn; i++ {
		conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String()+"/?who=c"+strconv.Itoa(i))
		require.NoError(t, err)
		clients = append(clients, conn)
	}
	require.Eventually(t, func() bool {
		return len(pool.snapshot()) == numConn
	}, 2*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, CloseAll(ctx, websocket.CloseGoingAway, "bye"))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, numConn, "a listener saw an empty or duplicated query value: %v", seen)
	for i := 0; i < numConn; i++ {
		require.Equal(t, 1, seen["c"+strconv.Itoa(i)])
	}
}

func TestCloseAllListenersSurviveConcurrentUpgrades(t *testing.T) {
	// The scenario a reviewer raised against a pooled Conn: a disconnect
	// listener runs on CloseAll's goroutine and reads the wrapper's request
	// data while the handler returns, and a new upgrade lands on the same
	// object and rewrites those maps underneath it. Nothing here may see
	// another connection's value, an empty one, or a race report.
	resetState()

	const numConn = 25
	var mu sync.Mutex
	seen := make(map[string]int)
	On(EventDisconnect, func(p *EventPayload) {
		mu.Lock()
		seen[p.Kws.Query("who")]++
		mu.Unlock()
	})

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	app.Use(upgradeMiddleware)
	app.Get("/", New(func(_ *Websocket) {}))

	go func() { _ = app.Listener(ln) }()

	dialer := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 5 * time.Second,
	}
	clients := make([]*websocket.Conn, 0, numConn)
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for i := 0; i < numConn; i++ {
		conn, _, err := dialWithRetry(dialer, "ws://"+ln.Addr().String()+"/?who=c"+strconv.Itoa(i))
		require.NoError(t, err)
		clients = append(clients, conn)
	}
	require.Eventually(t, func() bool {
		return len(pool.snapshot()) == numConn
	}, 2*time.Second, 10*time.Millisecond)

	// Keep new upgrades arriving for the whole of CloseAll so a recycled
	// wrapper would be re-acquired while listeners are still reading it.
	stop := make(chan struct{})
	churned := make(chan struct{})
	go func() {
		defer close(churned)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c, _, err := dialer.Dial("ws://"+ln.Addr().String()+"/?who=late"+strconv.Itoa(i), nil)
			if err == nil {
				_ = c.Close()
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := CloseAll(ctx, websocket.CloseGoingAway, "bye")
	close(stop)
	<-churned
	require.NoError(t, err)

	// Late connections may or may not have been caught by CloseAll; whatever
	// was seen must be a real value that was seen exactly once.
	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < numConn; i++ {
		require.Equal(t, 1, seen["c"+strconv.Itoa(i)], "connection c%d: %v", i, seen)
	}
	require.NotContains(t, seen, "", "a listener read an emptied wrapper")
	for who, n := range seen {
		require.Equal(t, 1, n, "%q seen %d times", who, n)
	}
}

func TestMethodBroadcastSkipsSelfAndReportsDead(t *testing.T) {
	resetState()

	var errs int32
	On(EventError, func(p *EventPayload) {
		if errors.Is(p.Error, ErrorInvalidConnection) {
			atomic.AddInt32(&errs, 1)
		}
	})

	sender := createWS()
	pool.set(sender)

	alive := new(WebsocketMock)
	require.NoError(t, alive.SetUUID("alive"))
	alive.On("IsAlive").Return(true)
	alive.On("Emit", mock.Anything).Return(nil)
	alive.wg.Add(1)
	pool.set(alive)

	dead := new(WebsocketMock)
	require.NoError(t, dead.SetUUID("dead"))
	dead.On("IsAlive").Return(false)
	pool.set(dead)

	sender.Broadcast([]byte("test"), true, TextMessage)

	alive.wg.Wait()
	alive.AssertNumberOfCalls(t, "Emit", 1)
	dead.AssertNumberOfCalls(t, "Emit", 0)
	require.Equal(t, int32(1), atomic.LoadInt32(&errs))
	// The sender was skipped, so nothing was queued on it.
	require.Empty(t, sender.queue)
}

func BenchmarkFireEventNoListeners(b *testing.B) {
	resetState()
	kws := createWS()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kws.fireEvent(EventMessage, nil, nil)
	}
}

func BenchmarkFireEventSingleListener(b *testing.B) {
	resetState()
	On(EventMessage, func(*EventPayload) {})
	kws := createWS()
	payload := []byte("hello websocket")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kws.fireEvent(EventMessage, payload, nil)
	}
	b.StopTimer()
	resetState()
}

func BenchmarkPoolSnapshot(b *testing.B) {
	resetState()
	for i := 0; i < 128; i++ {
		kws := createWS()
		kws.UUID = strconv.Itoa(i)
		pool.set(kws)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(pool.snapshot()) != 128 {
			b.Fatal("unexpected snapshot size")
		}
	}
	b.StopTimer()
	resetState()
}
