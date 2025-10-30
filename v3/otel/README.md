---
id: otel
---

# OTel

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=otel*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20otel/badge.svg)

[OpenTelemetry](https://opentelemetry.io/) support for Fiber.

This package is listed on the [OpenTelemetry Registry](https://opentelemetry.io/registry/instrumentation-go-fiber/).

**Note: Requires Go 1.25 and above**

## Install

**Compatible with Fiber v3.**

```sh
go get -u github.com/gofiber/contrib/v3/otel
```

## Signature

```go
otel.Middleware(opts ...otel.Option) fiber.Handler
```

## Config
You can configure the middleware using functional parameters


| Function                | Argument Type                            | Description                                                                      | Default                                                             |
| :------------------------ | :-------------------------------- | :--------------------------------------------------------------------------------- | :-------------------------------------------------------------------- |
| `WithNext`                    | `func(fiber.Ctx) bool`         | Define a function to skip this middleware when returned true .| nil                                                                 |
| `WithTracerProvider`          | `oteltrace.TracerProvider`      | Specifies a tracer provider to use for creating a tracer.                         | nil - the global tracer provider is used                                   |
| `WithMeterProvider`           | `otelmetric.MeterProvider`      | Specifies a meter provider to use for reporting.                                     | nil - the global meter provider is used                                                             |
| `WithPort`                    | `int`                          | Specifies the value to use when setting the `net.host.port` attribute on metrics/spans.                            | Defaults to (`80` for `http`, `443` for `https`)              |
| `WithPropagators`             | `propagation.TextMapPropagator` | Specifies propagators to use for extracting information from the HTTP requests.                     | If none are specified, global ones will be used                                                               |
| (❌ **Removed**) `WithServerName`             | `string`                       | This option was removed because the `http.server_name` attribute is deprecated in the OpenTelemetry semantic conventions. Beginning with v1.21.0, the recommended attribute is `server.address`, which this middleware already fills with the hostname reported by Fiber.                                            | -                                                                   |
| `WithSpanNameFormatter`       | `func(fiber.Ctx) string`       | Takes a function that will be called on every request and the returned string will become the span Name.                                   | Default formatter returns the route pathRaw |
| `WithCustomAttributes`        | `func(fiber.Ctx) []attribute.KeyValue` | Define a function to add custom attributes to the span.                  | nil                                                                 |
| `WithCustomMetricAttributes`  | `func(fiber.Ctx) []attribute.KeyValue` | Define a function to add custom attributes to the metrics.               | nil                                                                 |
| `WithCollectClientIP`         | `bool` | Specifies whether to collect the client's IP address from the request. | true |

## Usage

Please refer to [example](./example)

## Example

```go
package main

import (
    "context"
    "errors"
    "log"

    "go.opentelemetry.io/otel/sdk/resource"

    "github.com/gofiber/fiber/v3"

    fiberotel "github.com/gofiber/contrib/v3/otel"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    stdout "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"

    //"go.opentelemetry.io/otel/exporters/jaeger"
    "go.opentelemetry.io/otel/propagation"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
    oteltrace "go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("fiber-server")

func main() {
    tp := initTracer()
    defer func() {
        if err := tp.Shutdown(context.Background()); err != nil {
            log.Printf("Error shutting down tracer provider: %v", err)
        }
    }()

    app := fiber.New()

    app.Use(fiberotel.Middleware())

    app.Get("/error", func(ctx fiber.Ctx) error {
        return errors.New("abc")
    })

    app.Get("/users/:id", func(c fiber.Ctx) error {
        id := c.Params("id")
        name := getUser(c.UserContext(), id)
        return c.JSON(fiber.Map{"id": id, "name": name})
    })

    log.Fatal(app.Listen(":3000"))
}

func initTracer() *sdktrace.TracerProvider {
    exporter, err := stdout.New(stdout.WithPrettyPrint())
    if err != nil {
        log.Fatal(err)
    }
    tp := sdktrace.NewTracerProvider(
        sdktrace.WithSampler(sdktrace.AlwaysSample()),
        sdktrace.WithBatcher(exporter),
        sdktrace.WithResource(
            resource.NewWithAttributes(
                semconv.SchemaURL,
                semconv.ServiceNameKey.String("my-service"),
            )),
    )
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
    return tp
}

func getUser(ctx context.Context, id string) string {
    _, span := tracer.Start(ctx, "getUser", oteltrace.WithAttributes(attribute.String("id", id)))
    defer span.End()
    if id == "123" {
        return "otel tester"
    }
    return "unknown"
}
```
