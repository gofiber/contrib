package event

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	fws "github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp/fasthttputil"
)

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
	listeners.list = make(map[string][]eventCallback)
	listeners.Unlock()
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

	for _, mws := range pool.all() {
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

	for _, kws := range pool.all() {
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
	kws.queue <- message{mType: TextMessage, data: []byte("queued")}

	done := make(chan struct{})
	go func() {
		kws.Close()
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
	require.False(t, kws.IsAlive())
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
