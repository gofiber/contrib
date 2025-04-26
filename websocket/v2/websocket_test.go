package websocket

import (
	"net"
	"net/http"
	"testing"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
)

func TestWebSocketMiddlewareDefaultConfig(t *testing.T) {
	app := setupTestApp(Config{}, nil)
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
		}, nil)
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
		}, nil)
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
		}, nil)
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
	}, nil)
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

func TestWebSocketConnParams(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		param1 := c.Params("param1")
		param2 := c.Params("param2")
		paramDefault := c.Params("paramDefault", "default")

		assert.Equal(t, "value1", param1)
		assert.Equal(t, "value2", param2)
		assert.Equal(t, "default", paramDefault)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message/value1/value2", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnQuery(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		query1 := c.Query("query1")
		query2 := c.Query("query2")
		queryDefault := c.Query("queryDefault", "default")

		assert.Equal(t, "value1", query1)
		assert.Equal(t, "value2", query2)
		assert.Equal(t, "default", queryDefault)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message?query1=value1&query2=value2", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnHeaders(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		header1 := c.Headers("Header1")
		header2 := c.Headers("Header2")
		headerDefault := c.Headers("HeaderDefault", "valueDefault")

		assert.Equal(t, "value1", header1)
		assert.Equal(t, "value2", header2)
		assert.Equal(t, "valueDefault", headerDefault)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
		"header1": []string{"value1"},
		"header2": []string{"value2"},
	})
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnCookies(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		cookie1 := c.Cookies("Cookie1")
		cookie2 := c.Cookies("Cookie2")
		cookieDefault := c.Headers("CookieDefault", "valueDefault")

		assert.Equal(t, "value1", cookie1)
		assert.Equal(t, "value2", cookie2)
		assert.Equal(t, "valueDefault", cookieDefault)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
		"header1": []string{"value1"},
		"header2": []string{"value2"},
		"Cookie":  []string{"Cookie1=value1; Cookie2=value2"},
	})
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnLocals(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		local1 := c.Locals("local1")
		local2 := c.Locals("local2")

		assert.Equal(t, "value1", local1)
		assert.Equal(t, "value2", local2)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
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

func TestWebSocketConnIP(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		ip := c.IP()

		assert.Equal(t, "127.0.0.1", ip)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
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

func setupTestApp(cfg Config, h func(c *Conn)) *fiber.App {
	var handler fiber.Handler
	if h == nil {
		handler = New(func(c *Conn) {
			c.WriteJSON(fiber.Map{
				"message": "hello websocket",
			})
		}, cfg)
	} else {
		handler = New(h, cfg)
	}

	app := fiber.New(fiber.Config{})

	app.Use("/ws", func(c fiber.Ctx) error {
		if IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			c.Locals("local1", "value1")
			c.Locals("local2", "value2")
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	app.Get("/ws/message", handler)
	app.Get("/ws/message/:param1/:param2", handler)
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

func TestWebSocketIsCloseError(t *testing.T) {
	closeError := IsCloseError(&websocket.CloseError{
		Code: websocket.CloseNormalClosure,
	}, websocket.CloseNormalClosure)
	assert.Equal(t, true, closeError)
}

func TestWebSocketIsUnexpectedCloseError(t *testing.T) {
	closeError := IsUnexpectedCloseError(&websocket.CloseError{
		Code: websocket.CloseNormalClosure,
	}, websocket.CloseAbnormalClosure)
	assert.Equal(t, true, closeError)
}

func TestWebSocketFormatCloseMessage(t *testing.T) {
	closeMsg := FormatCloseMessage(websocket.CloseNormalClosure, "test")

	assert.Equal(t, []byte{0x3, 0xe8, 0x74, 0x65, 0x73, 0x74}, closeMsg)
}

func TestWebsocketRecoverDefaultHandlerShouldNotPanic(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		panic("test panic")

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
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
	assert.Equal(t, "test panic", msg["error"])
}

func TestWebsocketRecoverCustomHandlerShouldNotPanic(t *testing.T) {
	app := setupTestApp(Config{
		RecoverHandler: func(conn *Conn) {
			if err := recover(); err != nil {
				conn.WriteJSON(fiber.Map{"customError": "error occurred"})
			}
		},
	}, func(c *Conn) {
		panic("test panic")

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
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
	assert.Equal(t, "error occurred", msg["customError"])
}
