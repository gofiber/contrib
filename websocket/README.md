---
id: websocket
---

# Websocket

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=websocket*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

Based on [Fasthttp WebSocket](https://github.com/fasthttp/websocket) for [Fiber](https://github.com/gofiber/fiber) with available `*fiber.Ctx` methods like [Locals](http://docs.gofiber.io/ctx#locals), [Params](http://docs.gofiber.io/ctx#params), [Query](http://docs.gofiber.io/ctx#query) and [Cookies](http://docs.gofiber.io/ctx#cookies).

**Note: Requires Go 1.18 and above**

## Install

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/websocket
```

## Signatures
```go
func New(handler func(*websocket.Conn), config ...websocket.Config) fiber.Handler {
```

## Config

| Property            | Type                        | Description                                                                                                                   | Default                |
|:--------------------|:----------------------------|:------------------------------------------------------------------------------------------------------------------------------|:-----------------------|
| Filter              | `func(*fiber.Ctx) bool`     | Defines a function to skip middleware.                                                                                        | `nil`                  |
| HandshakeTimeout    | `time.Duration`             | HandshakeTimeout specifies the duration for the handshake to complete.                                                       | `0` (No timeout)       |
| Subprotocols        | `[]string`                  | Subprotocols specifies the client's requested subprotocols.                                                                   | `nil`                  |
| Origins             | `[]string`                  | Allowed Origins based on the Origin header. If empty, everything is allowed.                                                  | `nil`                  |
| ReadBufferSize      | `int`                       | ReadBufferSize specifies the I/O buffer size in bytes for incoming messages.                                                  | `0` (Use default size) |
| WriteBufferSize     | `int`                       | WriteBufferSize specifies the I/O buffer size in bytes for outgoing messages.                                                 | `0` (Use default size) |
| WriteBufferPool     | `websocket.BufferPool`      | WriteBufferPool is a pool of buffers for write operations.                                                                     | `nil`                  |
| EnableCompression   | `bool`                      | EnableCompression specifies if the client should attempt to negotiate per message compression (RFC 7692).                     | `false`                |
| RecoverHandler      | `func(*websocket.Conn) void` | RecoverHandler is a panic handler function that recovers from panics.                                                         | `defaultRecover`       |


## Example

```go
package main

import (
	"log"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/contrib/websocket"
)

func main() {
	app := fiber.New()

	app.Use("/ws", func(c *fiber.Ctx) error {
		// IsWebSocketUpgrade returns true if the client
		// requested upgrade to the WebSocket protocol.
		if websocket.IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	app.Get("/ws/:id", websocket.New(func(c *websocket.Conn) {
		// c.Locals is added to the *websocket.Conn
		log.Println(c.Locals("allowed"))  // true
		log.Println(c.Params("id"))       // 123
		log.Println(c.Query("v"))         // 1.0
		log.Println(c.Cookies("session")) // ""

		// websocket.Conn bindings https://pkg.go.dev/github.com/fasthttp/websocket?tab=doc#pkg-index
		var (
			mt  int
			msg []byte
			err error
		)
		for {
			if mt, msg, err = c.ReadMessage(); err != nil {
				log.Println("read:", err)
				break
			}
			log.Printf("recv: %s", msg)

			if err = c.WriteMessage(mt, msg); err != nil {
				log.Println("write:", err)
				break
			}
		}

	}))

	log.Fatal(app.Listen(":3000"))
	// Access the websocket server: ws://localhost:3000/ws/123?v=1.0
	// https://www.websocket.org/echo.html
}

```

## Note with cache middleware

If you get the error `websocket: bad handshake` when using the [cache middleware](https://github.com/gofiber/fiber/tree/master/middleware/cache), please use `config.Next` to skip websocket path.

```go
app := fiber.New()
app.Use(cache.New(cache.Config{
		Next: func(c *fiber.Ctx) bool {
			return strings.Contains(c.Route().Path, "/ws")
		},
}))

app.Get("/ws/:id", websocket.New(func(c *websocket.Conn) {}))
```

## Note with recover middleware

For internal implementation reasons, currently recover middleware is not work with websocket middleware, please use `config.RecoverHandler` to add recover handler to websocket endpoints.
By default, config `RecoverHandler` is recovers from panic and writes stack trace to stderr, also returns a response that contains panic message in **error** field.


```go
app := fiber.New()

app.Use(cache.New(cache.Config{
    Next: func(c *fiber.Ctx) bool {
        return strings.Contains(c.Route().Path, "/ws")
    },
}))

cfg := Config{
    RecoverHandler: func(conn *Conn) {
        if err := recover(); err != nil {
            conn.WriteJSON(fiber.Map{"customError": "error occurred"})
        }
    },
}

app.Get("/ws/:id", websocket.New(func(c *websocket.Conn) {}, cfg))


```