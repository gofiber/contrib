---
id: circuitbreaker
---

# Circuit Breaker

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=circuitbreaker*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20CircuitBreaker/badge.svg)

A **Circuit Breaker** is a software design pattern used to prevent system failures when a service is experiencing high failures or slow responses. It helps improve system resilience by **stopping requests** to an unhealthy service and **allowing recovery** once it stabilizes.

**Compatible with Fiber v3.**


## How It Works

1. **Closed State:**  
   - Requests are allowed to pass normally.  
   - Failures are counted.  
   - If failures exceed a defined **threshold**, the circuit switches to **Open** state.  

2. **Open State:**  
   - Requests are **blocked immediately** to prevent overload.  
   - The circuit stays open for a **timeout period** before moving to **Half-Open**.  

3. **Half-Open State:**  
   - Allows a limited number of requests to test service recovery.  
   - If requests **succeed**, the circuit resets to **Closed**.  
   - If requests **fail**, the circuit returns to **Open**.

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
circuitbreaker.New(config ...circuitbreaker.Config) *circuitbreaker.Middleware 
```

## Config

| Property | Type | Description | Default |
|:---------|:-----|:------------|:--------|
| FailureThreshold | `int` | Number of consecutive errors required to open the circuit | `5` |
| Timeout | `time.Duration` | Timeout for the circuit breaker | `10 * time.Second` |
| SuccessThreshold | `int` | Number of successful requests required to close the circuit | `5` |
| HalfOpenMaxConcurrent | `int` | Max concurrent requests in half-open state | `1` |
| IsFailure | `func(error) bool` | Custom function to determine if an error is a failure | `Status >= 500` |
| OnOpen | `func(fiber.Ctx) error` | Callback function when the circuit is opened | `503 response` |
| OnClose | `func(fiber.Ctx) error` | Callback function when the circuit is closed | `Continue request` |
| OnHalfOpen | `func(fiber.Ctx) error` | Callback function when the circuit is half-open | `429 response` |

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

    // In your application shutdown logic
    app.Shutdown(func() {
        // Make sure to stop the circuit breaker when your application shuts down:
        cb.Stop()
    })
}
```

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
    OnClose: func(c fiber.Ctx) error {
        return c.Status(fiber.StatusOK).
            JSON(fiber.Map{"message": "Circuit Closed: Service recovered"})
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

Use a **semaphore-based** approach to **limit concurrent requests.**

```go
cb := circuitbreaker.New(circuitbreaker.Config{
    FailureThreshold:  3,
    Timeout:           5 * time.Second,
    SuccessThreshold:  2,
    HalfOpenSemaphore: make(chan struct{}, 2), // Allow only 2 concurrent requests
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

### 7. Advanced: Multiple Circuit Breakers for Different Services

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