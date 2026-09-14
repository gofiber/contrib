---
id: circuitbreaker
---

# Circuit Breaker

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*circuitbreaker*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20CircuitBreaker/badge.svg)

A **Circuit Breaker** is a software design pattern used to prevent system failures when a service is experiencing high failures or slow responses. It helps improve system resilience by **stopping requests** to an unhealthy service and **allowing recovery** once it stabilizes.

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## How It Works

1. **Closed State:**  
   - Requests are allowed to pass normally.  
   - Failures are counted.  
   - If failures exceed a defined **threshold**, the circuit switches to **Open** state.  

2. **Open State:**  
   - Requests are **blocked immediately** to prevent overload.  
   - The circuit stays open for a **timeout period** before moving to **Half-Open**.  
   - Recovery is derived from the clock rather than scheduled in the background, so it is
     applied by the first request or state read that arrives after the timeout has elapsed.
     A circuit with no traffic keeps reporting `open` until something asks.  

3. **Half-Open State:**  
   - Allows a limited number of requests to test service recovery.  
   - `HalfOpenMaxConcurrent` bounds how many of those probes run at once; the rest are
     refused by `OnHalfOpen`.  
   - If enough requests **succeed** (`SuccessThreshold`), the circuit resets to **Closed**.  
   - If a single request **fails**, the circuit returns to **Open**.

## Benefits of Using a Circuit Breaker

✅ **Prevents cascading failures** in microservices.  
✅ **Improves system reliability** by avoiding repeated failed requests.  
✅ **Reduces load on struggling services** and allows recovery.  

## Install

```bash
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/circuitbreaker
```

## Signature

```go
// Build a circuit breaker, then mount it in front of the routes it protects.
circuitbreaker.New(config circuitbreaker.Config) *circuitbreaker.CircuitBreaker
circuitbreaker.Middleware(cb *circuitbreaker.CircuitBreaker) fiber.Handler
```

`Middleware` admits a request, runs the handler chain, reports the outcome and releases
the half-open slot it took, so none of that has to be paired up by hand. `AllowRequest`,
`ReleaseSemaphore`, `ReportSuccess` and `ReportFailure` remain available for callers that
already drive the circuit themselves, but they are **deprecated**: a half-open slot taken
through `AllowRequest` is held until `ReleaseSemaphore` returns it or the next state change
reclaims it.

## Config

| Property | Type | Description | Default |
|:---------|:-----|:------------|:--------|
| FailureThreshold | `int` | Number of failures required to open the circuit | `5` |
| Timeout | `time.Duration` | How long the circuit stays open before a probe is allowed | `5 * time.Second` |
| SuccessThreshold | `int` | Number of successful probes required to close the circuit | `1` |
| HalfOpenMaxConcurrent | `int` | Max concurrent probes in half-open state | `1` |
| Interval | `time.Duration` | Period after which failure counts reset in closed state. Zero means failures accumulate until the circuit opens. | `0` |
| IsFailure | `func(c fiber.Ctx, err error) bool` | Decides whether a served request counts as a failure | `err != nil \|\| Status >= 500` |
| Clock | `func() time.Time` | Reads the current time. Recovery is derived from it, so substituting a clock drives the circuit through every state without waiting. | `time.Now` |
| OnOpen | `func(fiber.Ctx) error` | Answers a request **refused because the circuit is open** | `503 response` |
| OnHalfOpen | `func(fiber.Ctx) error` | Answers a request **refused because half-open is already at `HalfOpenMaxConcurrent`** | `429 response` |
| OnClose | `func(fiber.Ctx) error` | Runs after the successful probe that **closed** the circuit, once the handler has already answered. Its return value is discarded. | `no-op` |

### About the callbacks

`OnOpen`, `OnHalfOpen` and `OnClose` write responses for requests the circuit breaker
handled itself. They are **not** transition observers, and none of them fires when the
circuit merely changes state:

- `OnOpen` fires for each request *refused while* open — not at the moment the circuit opens.
- `OnHalfOpen` fires for each probe *refused while* half-open — not when half-open is entered.
- `OnClose` fires once, after the probe whose success closed the circuit — and it is a
  **notification, not a response writer**. The protected handler has already written the
  response by then, and Fiber writes eagerly, so a `Send`, `JSON` or `Status` call in
  `OnClose` **replaces the handler's answer** even though the error it returns is
  discarded. Record the recovery; do not write to the response, and do not advance the
  handler chain.

## Operator controls

| Method | Effect |
|:-------|:-------|
| `GetState()` / `IsOpen()` | Current state, with any due recovery applied first |
| `Metrics()` / `GetStateStats()` | Counters, and the thresholds and timestamps behind them |
| `HealthHandler()` | A Fiber handler answering 503 while open, 200 otherwise |
| `ForceOpen()` | Opens the circuit **and keeps it open** |
| `ForceClose()` / `Reset()` | Returns the circuit to closed and starts a new failure-count window |
| `SetTimeout(d)` | Changes the recovery timeout, including for a circuit that is already open |

`ForceOpen` is sticky: unlike a circuit that opened on failures, a forced-open circuit does
**not** recover on its own when `Timeout` elapses. Call `ForceClose` or `Reset` to end it.

## Circuit Breaker Usage in Fiber (Example)

This guide explains how to use a Circuit Breaker in a Fiber application at different levels, from basic setup to advanced customization.

### 1. Basic Setup

A **global** Circuit Breaker protects all routes.

**Example: Applying Circuit Breaker to All Routes**

```go
package main

import (
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/contrib/v3/circuitbreaker"
)

func main() {
    app := fiber.New()
    
    // Create a new Circuit Breaker with custom configuration
    cb := circuitbreaker.New(circuitbreaker.Config{
        FailureThreshold: 3,               // Max failures before opening the circuit
        Timeout:          5 * time.Second, // Wait time before retrying
        SuccessThreshold: 2,               // Required successes to move back to closed state
    })

    // Apply Circuit Breaker to ALL routes
    app.Use(circuitbreaker.Middleware(cb))

    // Sample Route
    app.Get("/", func(c fiber.Ctx) error {
        return c.SendString("Hello, world!")
    })

    // Optional: Expose health check endpoint
    app.Get("/health/circuit", cb.HealthHandler())

    // Optional: Expose metrics about the circuit breaker:
    app.Get("/metrics/circuit", func(c fiber.Ctx) error {
          return c.JSON(cb.GetStateStats())
    })

    app.Listen(":3000")
}
```

> The circuit breaker starts nothing in the background, so it needs no shutdown step.
> `cb.Stop()` is **deprecated** and does nothing; it is kept only so existing callers
> still compile.

### 2. Route & Route-Group Specific Circuit Breaker

Apply the Circuit Breaker **only to specific routes**.

```go
app.Get("/protected", circuitbreaker.Middleware(cb), func(c fiber.Ctx) error {
    return c.SendString("Protected service running")
})
```
Apply the Circuit Breaker **only to specific routes groups**.

```go
app := route.Group("/api")
app.Use(circuitbreaker.Middleware(cb))

// All routes in this group will be protected
app.Get("/users", getUsersHandler)
app.Post("/users", createUserHandler)
```

### 3. Circuit Breaker with Custom Failure Handling

Customize the response when the circuit **opens**.

```go
cb := circuitbreaker.New(circuitbreaker.Config{
    FailureThreshold: 3,
    Timeout:   10 * time.Second,
    OnOpen: func(c fiber.Ctx) error {
        return c.Status(fiber.StatusServiceUnavailable).
            JSON(fiber.Map{"error": "Circuit Open: Service unavailable"})
    },
    OnHalfOpen: func(c fiber.Ctx) error {
        return c.Status(fiber.StatusTooManyRequests).
            JSON(fiber.Map{"error": "Circuit Half-Open: Retrying service"})
    },
    // OnClose runs after the protected handler has answered, so writing here
    // would replace its response. Record the recovery instead.
    OnClose: func(c fiber.Ctx) error {
        log.Printf("circuit closed: %s recovered", c.Path())
        return nil
    },
})

// Apply to a specific route
app.Get("/custom", circuitbreaker.Middleware(cb), func(c fiber.Ctx) error {
    return c.SendString("This service is protected by a Circuit Breaker")
})
```

✅ Now, when failures exceed the threshold, ***custom error responses** will be sent.

### 4. Circuit Breaker for External API Calls

Use a Circuit Breaker **when calling an external API.**

```go

app.Get("/external-api", circuitbreaker.Middleware(cb), func(c fiber.Ctx) error {
    // Simulating an external API call
    resp, err := fiber.Get("https://example.com/api")
    if err != nil {
        return fiber.NewError(fiber.StatusInternalServerError, "External API failed")
    }
    return c.SendString(resp.Body())
})
```

✅ If the external API fails repeatedly, **the circuit breaker prevents further calls.**

### 5. Circuit Breaker with Concurrent Requests Handling

**Limit how many probes run at once** while the circuit is recovering.

```go
cb := circuitbreaker.New(circuitbreaker.Config{
    FailureThreshold:      3,
    Timeout:               5 * time.Second,
    SuccessThreshold:      2,
    HalfOpenMaxConcurrent: 2, // Allow only 2 concurrent probes in half-open
})

app.Get("/half-open-limit", circuitbreaker.Middleware(cb), func(c fiber.Ctx) error {
    time.Sleep(2 * time.Second) // Simulating slow response
    return c.SendString("Half-Open: Limited concurrent requests")
})
```

✅ When in **half-open** state, only **2 concurrent requests are allowed**.

### 6. Circuit Breaker with Custom Metrics

Integrate **Prometheus metrics** and **structured logging**.

```go
cb := circuitbreaker.New(circuitbreaker.Config{
    FailureThreshold: 5,
    Timeout:   10 * time.Second,
    OnOpen: func(c fiber.Ctx) error {
        log.Println("Circuit Breaker Opened!")
        prometheus.Inc("circuit_breaker_open_count")
        return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Service Down"})
    },
})
```

✅ Logs when the circuit opens & increments Prometheus metrics.

### 7. Circuit Breaker with Failure Count Reset Interval

Use `Interval` to reset the failure count once the interval has elapsed in the closed state. The reset is applied lazily: the next failure reported after the interval has elapsed starts a fresh count instead of carrying the old one over. Without `Interval`, failures accumulate indefinitely in the closed state until the threshold is reached.

```go
cb := circuitbreaker.New(circuitbreaker.Config{
	FailureThreshold: 5,
	Timeout:          10 * time.Second,
	Interval:         30 * time.Second, // Reset failure count every 30 seconds
})

app.Use(circuitbreaker.Middleware(cb))
```

✅ If 4 failures occur and the next failure is reported more than 30 seconds after the window started, the count restarts at 1 instead of reaching the threshold. The circuit only opens when 5 failures accumulate within one 30-second window.

### 8. Advanced: Multiple Circuit Breakers for Different Services

Use different Circuit Breakers for different services.

```go

dbCB := circuitbreaker.New(circuitbreaker.Config{FailureThreshold: 5, Timeout: 10 * time.Second})
apiCB := circuitbreaker.New(circuitbreaker.Config{FailureThreshold: 3, Timeout: 5 * time.Second})

app.Get("/db-service", circuitbreaker.Middleware(dbCB), func(c fiber.Ctx) error {
    return c.SendString("DB service request")
})

app.Get("/api-service", circuitbreaker.Middleware(apiCB), func(c fiber.Ctx) error {
    return c.SendString("External API service request")
})
```

✅ Each service has its own failure threshold & timeout.
