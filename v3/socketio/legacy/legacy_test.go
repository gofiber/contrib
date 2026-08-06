package legacy

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	fws "github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp/fasthttputil"
)

func TestLegacyPlainWebSocketEventShim(t *testing.T) {
	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()

	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	app.Use(func(c fiber.Ctx) error {
		if fws.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	On(EventMessage, func(payload *EventPayload) {
		require.Equal(t, "message", payload.Name)
		payload.Kws.Emit([]byte("legacy:"+string(payload.Data)), TextMessage)
	})

	app.Get("/", New(func(kws *Websocket) {
		kws.SetAttribute("mode", "legacy")
	}))

	go func() {
		_ = app.Listener(ln)
	}()

	dialer := &websocket.Dialer{
		NetDial: func(network, addr string) (net.Conn, error) {
			return ln.Dial()
		},
		HandshakeTimeout: 5 * time.Second,
	}
	conn, err := dialWebSocket(t, dialer, "ws://"+ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("ping")))

	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, TextMessage, messageType)
	require.Equal(t, "legacy:ping", string(message))
}

func dialWebSocket(t *testing.T, dialer *websocket.Dialer, url string) (*websocket.Conn, error) {
	t.Helper()

	var lastErr error
	for range 20 {
		conn, _, err := dialer.Dial(url, nil)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("websocket dial did not run")
	}
	return nil, lastErr
}
