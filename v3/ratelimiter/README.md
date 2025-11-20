# Rate Limiter

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=ratelimiter*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20RateLimit/badge.svg)

Rate Limiter middleware for [Fiber](https://github.com/gofiber/fiber) that provides flexible rate limiting functionality with multiple storage backends.

**Compatible with Fiber v3.**

## Features

- 🚀 **Multiple Storage Backends**: In-memory and Redis support
- 🎯 **Flexible Key Generation**: Rate limit by IP, user ID, API key, or custom logic
- ⏱️ **Configurable Windows**: Set custom rate limiting windows and max requests
- 📊 **Rate Limit Headers**: Optional X-RateLimit-* headers support
- 🔧 **Customizable Responses**: Define custom rate limit exceeded responses
- 🛡️ **Skip Conditions**: Skip rate limiting based on conditions
- 📈 **Request Type Filtering**: Skip failed or successful requests

## Install

```bash
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/ratelimiter
```

## Signature

```go
ratelimiter.New(config ...ratelimiter.Config) fiber.Handler
```

## Config

| Property | Type | Description | Default |
|:---|:---|:---|:---|
| Next | `func(fiber.Ctx) bool` | Next defines a function to skip this middleware when returned true | `nil` |
| Max | `int` | Max number of requests allowed within the expiration duration | `10` |
| Expiration | `time.Duration` | Expiration defines the duration for which the limit is enforced | `1 minute` |
| KeyFunc | `func(fiber.Ctx) string` | KeyFunc defines a function to generate the rate limit key | `func(c fiber.Ctx) string { return c.IP() }` |
| LimitReached | `func(fiber.Ctx) error` | LimitReached defines the response when rate limit is exceeded | `429 status with JSON error` |
| Storage | `storage.Storage` | Storage defines the storage backend for rate limiting data | In-memory storage |
| SkipFailedRequests | `bool` | When set to true, failed requests won't consume rate limit | `false` |
| SkipSuccessfulRequests | `bool` | When set to true, successful requests won't consume rate limit | `false` |
| EnableDraftSpec | `bool` | Enable X-RateLimit-* headers | `false` |

## Default Config

```go
var ConfigDefault = Config{
    Max:        10,
    Expiration: 1 * time.Minute,
    KeyFunc: func(c fiber.Ctx) string {
        return c.IP()
    },
    LimitReached: func(c fiber.Ctx) error {
        return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
            "error": "Too many requests",
        })
    },
    SkipFailedRequests:     false,
    SkipSuccessfulRequests: false,
    EnableDraftSpec:        false,
}
```

## Examples

### Basic Usage

```go
package main

import (
    "log"
    "time"

    "github.com/gofiber/contrib/v3/ratelimiter"
    "github.com/gofiber/fiber/v3"
)

func main() {
    app := fiber.New()

    // Default rate limiter: 10 requests per minute per IP
    app.Use(ratelimiter.New())

    app.Get("/", func(c fiber.Ctx) error {
        return c.SendString("Hello, World!")
    })

    log.Fatal(app.Listen(":3000"))
}
```

### Custom Configuration

```go
package main

import (
    "log"
    "time"

    "github.com/gofiber/contrib/v3/ratelimiter"
    "github.com/gofiber/fiber/v3"
)

func main() {
    app := fiber.New()

    // Custom rate limiter: 100 requests per hour
    app.Use(ratelimiter.New(ratelimiter.Config{
        Max:        100,
        Expiration: time.Hour,
        KeyFunc: func(c fiber.Ctx) string {
            return c.IP()
        },
        LimitReached: func(c fiber.Ctx) error {
            return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
                "error": "Rate limit exceeded",
                "retry_after": time.Now().Add(time.Hour).Unix(),
            })
        },
        EnableDraftSpec: true, // Enable rate limit headers
    }))

    app.Get("/api/data", func(c fiber.Ctx) error {
        return c.JSON(fiber.Map{"data": "sensitive information"})
    })

    log.Fatal(app.Listen(":3000"))
}
```

### Rate Limiting by API Key

```go
package main

import (
    "log"
    "time"

    "github.com/gofiber/contrib/v3/ratelimiter"
    "github.com/gofiber/fiber/v3"
)

func main() {
    app := fiber.New()

    // Rate limit by API key: 1000 requests per hour per API key
    app.Use(ratelimiter.New(ratelimiter.Config{
        Max:        1000,
        Expiration: time.Hour,
        KeyFunc: func(c fiber.Ctx) string {
            apiKey := c.Get("X-API-Key")
            if apiKey == "" {
                return c.IP() // Fallback to IP if no API key
            }
            return "api_key:" + apiKey
        },
    }))

    app.Get("/api/premium", func(c fiber.Ctx) error {
        return c.JSON(fiber.Map{"message": "premium content"})
    })

    log.Fatal(app.Listen(":3000"))
}
```

### Using Redis Storage

```go
package main

import (
    "log"
    "time"

    "github.com/gofiber/contrib/v3/ratelimiter"
    "github.com/gofiber/contrib/v3/ratelimiter/storage"
    "github.com/gofiber/fiber/v3"
    "github.com/redis/go-redis/v9"
)

func main() {
    app := fiber.New()

    // Create Redis client
    rdb := redis.NewClient(&redis.Options{
        Addr:     "localhost:6379",
        Password: "", // no password
        DB:       0,  // default DB
    })

    // Use Redis storage for distributed rate limiting
    app.Use(ratelimiter.New(ratelimiter.Config{
        Max:        50,
        Expiration: 15 * time.Minute,
        Storage:    storage.NewRedis(rdb, "rate_limit:"),
    }))

    app.Get("/distributed", func(c fiber.Ctx) error {
        return c.SendString("Distributed rate limiting!")
    })

    log.Fatal(app.Listen(":3000"))
}
```

### Skip Conditions

```go
package main

import (
    "log"
    "time"

    "github.com/gofiber/contrib/v3/ratelimiter"
    "github.com/gofiber/fiber/v3"
)

func main() {
    app := fiber.New()

    app.Use(ratelimiter.New(ratelimiter.Config{
        Max:        10,
        Expiration: time.Minute,
        Next: func(c fiber.Ctx) bool {
            // Skip rate limiting for admin users
            return c.Get("X-Admin-Token") == "admin-secret"
        },
        SkipFailedRequests: true, // Don't count failed requests
    }))

    app.Get("/protected", func(c fiber.Ctx) error {
        return c.SendString("Protected endpoint")
    })

    log.Fatal(app.Listen(":3000"))
}
```

### Rate Limit Headers

When `EnableDraftSpec` is set to `true`, the following headers will be added to responses:

- `X-RateLimit-Limit`: Maximum requests allowed
- `X-RateLimit-Remaining`: Remaining requests in current window  
- `X-RateLimit-Reset`: Time when window resets (Unix timestamp)

```bash
curl -I http://localhost:3000/api/data
HTTP/1.1 200 OK
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 99
X-RateLimit-Reset: 1701234567
```

## Storage Backends

### Memory Storage (Default)

The default in-memory storage is suitable for single-instance applications:

```go
import "github.com/gofiber/contrib/v3/ratelimiter/storage"

store := storage.NewMemory()
defer store.Close() // Important: close to stop cleanup goroutine
```

### Redis Storage

For distributed applications or when you need persistence:

```go
import (
    "github.com/gofiber/contrib/v3/ratelimiter/storage"
    "github.com/redis/go-redis/v9"
)

rdb := redis.NewClient(&redis.Options{
    Addr: "localhost:6379",
})

store := storage.NewRedis(rdb, "my_app_rate_limit:")
defer store.Close() // Will close Redis client if it's *redis.Client
```

## Rate Limit Strategies

### Fixed Window

The default implementation uses a fixed window approach where the counter resets after the expiration period.

### Custom Storage

You can implement custom storage by implementing the `storage.Storage` interface:

```go
type CustomStorage struct{}

func (s *CustomStorage) Get(ctx context.Context, key string) (int, error) {
    // Implementation
}

func (s *CustomStorage) Increment(ctx context.Context, key string, expiration time.Duration) (int, bool, error) {
    // Implementation
}

func (s *CustomStorage) Reset(ctx context.Context, key string) error {
    // Implementation
}

func (s *CustomStorage) Close() error {
    // Implementation
}
```

## Performance Considerations

- **Memory Storage**: Fast for single-instance apps, automatic cleanup of expired entries
- **Redis Storage**: Slightly slower but supports distributed applications and persistence
- **Key Functions**: Keep key generation logic simple to avoid bottlenecks
- **Headers**: Disable rate limit headers in production if not needed to reduce response size

## Contributing

We welcome contributions! Please see the [main contribution guide](../../CONTRIBUTING.md) for details.

## License

This project is licensed under the MIT License - see the [LICENSE](../../LICENSE) file for details.