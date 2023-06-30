package websocket

import (
	"net"
	"testing"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
)

func TestWebSocketMiddleware(t *testing.T) {
	app := fiber.New()
	defer app.Shutdown()

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
	}))

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
	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.Nil(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.Nil(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}
