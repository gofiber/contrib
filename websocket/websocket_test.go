package websocket

import (
	"net"
	"net/http"
	"testing"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
)

func TestWebSocketMiddleware(t *testing.T) {
	app := setupTestApp(Config{
		Origins: []string{"*"},
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketMiddlewareConfigOrigin(t *testing.T) {
	t.Run("allow all origins", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"*"},
		})
		defer app.Shutdown()

		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
			"Origin": []string{"http://localhost:3000"},
		})
		defer conn.Close()
		assert.NoError(t, err)
		assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
		assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

		var msg fiber.Map
		err = conn.ReadJSON(&msg)
		assert.Nil(t, err)
		assert.Equal(t, "hello websocket", msg["message"])
	})

	t.Run("allowed origin", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"http://localhost:3000"},
		})
		defer app.Shutdown()
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
			"Origin": []string{"http://localhost:3000"},
		})
		defer conn.Close()
		assert.NoError(t, err)
		assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
		assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

		var msg fiber.Map
		err = conn.ReadJSON(&msg)
		assert.NoError(t, err)
		assert.Equal(t, "hello websocket", msg["message"])
	})

	t.Run("disallowed origin", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"http://localhost:3000"},
		})
		defer app.Shutdown()
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
			"Origin": []string{"http://localhost:5000"},
		})
		defer conn.Close()
		assert.Equal(t, err.Error(), "websocket: bad handshake")
		assert.Equal(t, fiber.StatusUpgradeRequired, resp.StatusCode)
		assert.Equal(t, "", resp.Header.Get("Upgrade"))

		assert.Nil(t, conn)
	})
}

func TestWebSocketMiddlewareBufferSize(t *testing.T) {
	app := setupTestApp(Config{
		Origins:         []string{"*"},
		WriteBufferSize: 10,
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func setupTestApp(cfg Config) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Use("/ws", func(c *fiber.Ctx) error {
		if IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	app.Get("/ws/message", New(func(c *Conn) {
		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	}, cfg))
	go app.Listen(":3000")

	readyCh := make(chan struct{})

	go func() {
		for {
			conn, err := net.Dial("tcp", "localhost:3000")
			if err != nil {
				continue
			}

			if conn != nil {
				readyCh <- struct{}{}
				conn.Close()
				break
			}
		}
	}()

	<-readyCh

	return app
}
