package event

import (
	"context"
	"net"
	"strconv"
	"sync"
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
	pool.reset()
	listeners.Lock()
	listeners.list = make(map[string][]eventCallback)
	listeners.Unlock()
}

func (s *WebsocketMock) SetUUID(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if pool.contains(uuid) {
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

	kws.Close()

	require.False(t, kws.IsAlive())
	_, err := pool.get(kws.GetUUID())
	require.ErrorIs(t, err, ErrorInvalidConnection)
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
