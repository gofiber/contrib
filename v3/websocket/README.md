---
id: websocket
---

# Websocket

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*websocket*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20websocket/badge.svg)

Based on [Fasthttp WebSocket](https://github.com/fasthttp/websocket) for [Fiber](https://github.com/gofiber/fiber) with available `fiber.Ctx` methods like [Locals](http://docs.gofiber.io/ctx#locals), [Params](http://docs.gofiber.io/ctx#params), [Query](http://docs.gofiber.io/ctx#query) and [Cookies](http://docs.gofiber.io/ctx#cookies).

For a plain WebSocket event-bus helper, use the [`event`](./event/README.md) subpackage. It keeps ordinary WebSocket wire compatibility and is separate from the Socket.IO protocol implementation in `github.com/gofiber/contrib/v3/socketio`.


**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/websocket
```

## Signatures
```go
func New(handler func(*websocket.Conn), config ...websocket.Config) fiber.Handler {
```

## Config

| Property            | Type                         | Description                                                                                                                   | Default                |
|:--------------------|:-----------------------------|:------------------------------------------------------------------------------------------------------------------------------|:-----------------------|
| Next                | `func(fiber.Ctx) bool`       | Defines a function to skip this middleware when it returns true.                                                              | `nil`                  |
| HandshakeTimeout    | `time.Duration`              | HandshakeTimeout specifies the duration for the handshake to complete.                                                        | `0` (No timeout)       |
| Subprotocols        | `[]string`                   | Subprotocols this server supports, in order of preference. The first entry the client also offers is negotiated.               | `nil`                  |
| Origins             | `[]string`                   | Allowed Origins based on the Origin header, compared case-insensitively. If empty, everything is allowed.                     | `nil`                  |
| AllowEmptyOrigin    | `bool`                       | Allows connections without an Origin header when Origins is configured. Useful for non-browser clients.                       | `false`                |
| ReadBufferSize      | `int`                        | ReadBufferSize specifies the I/O buffer size in bytes for incoming messages.                                                  | `0` (Use default size) |
| WriteBufferSize     | `int`                        | WriteBufferSize specifies the I/O buffer size in bytes for outgoing messages.                                                 | `0` (Use default size) |
| WriteBufferPool     | `websocket.BufferPool`       | WriteBufferPool is a pool of buffers for write operations.                                                                    | `nil`                  |
| EnableCompression   | `bool`                       | EnableCompression specifies if the client should attempt to negotiate per message compression (RFC 7692).                     | `false`                |
| RecoverHandler      | `func(*websocket.Conn)`      | RecoverHandler is a panic handler function that recovers from panics.                                                         | `defaultRecover`       |
| CoalesceWrites      | `bool`                       | Answers a burst of pipelined frames with one write instead of one per frame. See [Write coalescing](#write-coalescing).      | `false`                |

## Example

```go
package main

import (
    "log"

    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/contrib/v3/websocket"
)

func main() {
    app := fiber.New()

    app.Use("/ws", func(c fiber.Ctx) error {
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

## Handshake rejections

A request that carries neither an `Upgrade` header nor an `upgrade` token in
`Connection` never asked to switch protocols and is answered with `426 Upgrade Required`.

A request that *does* ask to upgrade but whose handshake is rejected is returned as a
`*fiber.Error` carrying the status RFC 6455 defines for that failure, so it reaches
the app's `ErrorHandler` like any other error and takes its body and format from
there:

| Reason                                                     | Status                    |
|:-----------------------------------------------------------|:--------------------------|
| Origin not in `Origins` (RFC 6455 section 4.2.2)            | `403 Forbidden`           |
| Missing/blank `Sec-WebSocket-Key`, unsupported version, or only one of the `Upgrade`/`Connection` signals | `400 Bad Request` |
| Request method is not `GET`                                 | `405 Method Not Allowed`  |

Rejected handshakes also carry `Sec-WebSocket-Version: 13` so a client that asked for
another version learns which one the server speaks (RFC 6455 section 4.4). Headers set
by earlier middleware are left in place.

## Write coalescing

`fasthttp/websocket` puts every frame on the wire with its own write syscall. When a peer
pipelines frames, sending several before it waits for the replies, the server reads them
all in one go but still answers them one packet at a time. With `CoalesceWrites: true`
the replies to such a burst leave in a single write instead.

The rule is narrow so that nothing else changes:

- A write is held back only while the handler is still working through frames the peer
  already sent, that is between one read that delivered input and the next.
- What is held leaves the moment the handler asks for the next frame, on `Close`, when it
  reaches 64 KiB, or after one millisecond, whichever comes first.
- A write made while nothing is waiting to be read goes out immediately. A handler that
  only pushes, or a goroutine writing while the reader is parked on the socket, is not
  delayed at all.
- Frames are never reordered: a frame that does not fit is written after what was pending.

What it costs and buys, measured with 5-byte frames on a server pinned to one core with
`GOMAXPROCS=1`, the shape of one prefork child under the
[HttpArena](https://github.com/MDA2AV/HttpArena) `echo-ws` profiles:

| workload                          | default            | `CoalesceWrites`   |
|:----------------------------------|:-------------------|:-------------------|
| 1 frame in flight, 8 connections  | 12.6 µs CPU/frame  | unchanged, within run-to-run noise |
| 16 frames in flight, 8 connections | 5.7 µs CPU/frame  | 1.0 µs CPU/frame   |
| 16 frames in flight, 64 connections | 5.7 µs CPU/frame | 0.8 µs CPU/frame   |
| syscalls per frame, 16 in flight  | 1.06               | 0.13               |

A held frame costs one extra copy into the pending buffer, and an idle connection holds no
pending buffer at all.

The library only ever sees a `net.Conn`, so coalescing means owning it. With the option on,
the upgrade runs through the library's `net/http` `Upgrader` on a fasthttp-backed hijack
instead of `FastHTTPUpgrader`. Three things follow:

- The `101 Switching Protocols` response carries the handshake headers and whatever earlier
  middleware set (a cookie, a request id), but not fasthttp's `Server` and `Date` defaults.
- `Sec-WebSocket-Key` is validated as RFC 6455 asks, a base64 encoding of 16 bytes; the
  fasthttp upgrader only checks that it is present.
- A message larger than the write buffer, which the library hands to the socket as two
  writes, leaves as one when it is held back with the rest of a batch.

Everything else, `Locals`, `Params`, `Query`, `Cookies`, `Headers`, `IP`, the rejection
statuses, compression, subprotocols and `RecoverHandler`, behaves the same.

## Note with cache middleware

If you get the error `websocket: bad handshake` when using the [cache middleware](https://github.com/gofiber/fiber/tree/master/middleware/cache), please use `config.Next` to skip websocket path.

```go
app := fiber.New()
app.Use(cache.New(cache.Config{
        Next: func(c fiber.Ctx) bool {
            return strings.Contains(c.Route().Path, "/ws")
        },
}))

app.Get("/ws/:id", websocket.New(func(c *websocket.Conn) {}))
```

## Note with recover middleware

For internal implementation reasons, currently recover middleware does not work with websocket middleware, please use `config.RecoverHandler` to add recover handler to websocket endpoints.
By default, config `RecoverHandler` recovers from panic, writes the value and stack trace to stderr, and sends the peer a fixed `{"error":"internal error"}`.

Nothing derived from the panic reaches the client: the operator already has the
full detail on stderr, and a panic value can carry internal paths, connection
strings or schema names. If you want the peer told more than that, supply your
own `RecoverHandler` — it receives the `*websocket.Conn` and calls `recover()`
itself, so it can write whatever the application considers safe.

Once the recover handler returns, the connection is closed: a handler that panicked
cannot be assumed to still own it, and nothing else closes a hijacked connection.

A handler that returns normally leaves the socket open, so it may hand the
connection to another goroutine before returning and keep writing after. The
`*websocket.Conn` it receives is allocated for that upgrade alone and never reused,
so `Locals`, `Params`, `Query`, `Cookies`, `Headers` and `IP` keep answering with
that connection's own data for as long as anything holds it.

```go
app := fiber.New()

app.Use(cache.New(cache.Config{
    Next: func(c fiber.Ctx) bool {
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

## Note for WebSocket subprotocols

The config `Subprotocols` only helps you negotiate subprotocols and sets a `Sec-Websocket-Protocol` header if it has a suitable subprotocol. For more about negotiates process, check the comment for `Subprotocols` in [fasthttp.Upgrader](https://pkg.go.dev/github.com/fasthttp/websocket#Upgrader) .

All connections will be sent to the handler function no matter whether the subprotocol negotiation is successful or not. You can get the selected subprotocol from `conn.Subprotocol()`. 

If a connection includes the `Sec-Websocket-Protocol` header in the request but the protocol negotiation fails, the browser will immediately disconnect the connection after receiving the upgrade response.
