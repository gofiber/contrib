---
id: circuitbreaker
---

# CircuitBreaker

CircuitBreaker is a circuit breaker middleware for [Fiber](https://github.com/gofiber/fiber).

## Install
```bash
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/circuitbreaker
```
## Signature

```go
circuitbreaker.NewCircuitBreaker(config ...circuitbreaker.Config) *circuitbreaker.Middleware 
```

## Config

| Property | Type | Description | Default |
|:---------|:-----|:------------|:--------|
| Threshold | `int` | Number of consecutive errors required to open the circuit | `5` |
| Timeout | `time.Duration` | Timeout for the circuit breaker | `10 * time.Second` |
| MaxRequests | `int` | Maximum number of requests allowed before opening the circuit | `10` |
| SuccessReset | `int` | Number of successful requests required to close the circuit | `5` |
| OnOpen | `func(*fiber.Ctx)` | Callback function when the circuit is opened | `nil` |
| OnClose | `func(*fiber.Ctx)` | Callback function when the circuit is closed | `nil` |
| OnHalfOpen | `func(*fiber.Ctx)` | Callback function when the circuit is half-open | `nil` |

## Example

```go
package main

import (
	"github.com/gofiber/fiber/v2"
	"github.com/MitulShah1/contrib/circuitbreaker"
)

func main() {
	app := fiber.New()
	
	// Create circuit breaker with default configuration
	cb := circuitbreaker.New(circuitbreaker.DefaultCircuitBreakerConfig)
	
	// Apply middleware to all routes
	app.Use(circuitbreaker.Middleware(cb))
	
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})
	
	app.Listen(":3000")
}
```