---
id: otel
---

# OTel

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*otel*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20otel/badge.svg)

[OpenTelemetry](https://opentelemetry.io/) support for Fiber. The middleware
records a server span and the HTTP server metrics of the
[semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/)
(v1.43.0) for every request, continues the trace its caller propagated, and
hands the trace context to your handlers and back to the client.

This package is listed on the [OpenTelemetry Registry](https://opentelemetry.io/registry/instrumentation-go-fiber/).

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/contrib/v3/otel
```

## Signature

```go
otel.New(opts ...otel.Option) fiber.Handler
```

## Usage

```go
app := fiber.New()
app.Use(recover.New())   // outside: answers panics, after they are recorded
app.Use(fiberotel.New()) // before the routes and the middleware that use the span

app.Get("/users/:id", func(c fiber.Ctx) error {
    // c.Context() carries the request's span: pass it on to anything you trace.
    user, err := store.Get(c.Context(), c.Params("id"))
    ...
})
```

The middleware reports to the global tracer provider, meter provider and
propagator unless options name others, so set them up once at startup - see
the [example](#example). **Set a propagator**: until
`otel.SetTextMapPropagator` is called, the global one propagates nothing, and a
service never joins the traces of its callers.

Where you mount it matters:

- **Recovery middleware goes before it**, so that a panic passes through this
  middleware - which records it - before being answered.
- **Middleware that use the span go after it**: a logger writing trace IDs, or
  anything starting child spans from `c.Context()`.
- **It answers errors itself.** To record the status the client receives, the
  middleware runs the application's error handler on an error returned by the
  handler chain - as Fiber's logger does - and returns `nil`. Middleware
  mounted before it therefore see no error; mount error-reporting middleware
  after it, or read the error from the span.

## Config

You can configure the middleware using functional parameters.

| Function | Argument Type | Description | Default |
| :--- | :--- | :--- | :--- |
| `WithNext` | `func(fiber.Ctx) bool` | Skips the middleware for a request when the function returns true. | `nil` |
| `WithTracerProvider` | `oteltrace.TracerProvider` | The tracer provider spans are created with. With the [no-op provider](https://pkg.go.dev/go.opentelemetry.io/otel/trace/noop), no span is started at all, but the caller's trace context still reaches your handlers and the response. | the global provider |
| `WithMeterProvider` | `otelmetric.MeterProvider` | The meter provider metrics are recorded with. | the global provider |
| `WithoutMetrics` | `bool` | Disables metrics collection when set to true. | `false` |
| `WithPropagators` | `propagation.TextMapPropagator` | The propagators that extract the caller's trace context from the request, and inject the request's into the response. | the global propagator |
| `WithResponsePropagators` | `propagation.TextMapPropagator` | The propagators that inject the trace context into the response. `propagation.TraceContext{}` keeps baggage out of responses; `propagation.NewCompositeTextMapPropagator()` sends no trace context at all. | the `WithPropagators` ones |
| `WithTraceResponseHeader` | `string` | A response header that carries the request's trace ID, such as `X-Trace-Id`. | none |
| `WithPublicEndpoint` | - | Treats every request as coming from a client you do not control: its span starts a new trace and links to the caller's span context instead of continuing it. | off |
| `WithPublicEndpointFn` | `func(fiber.Ctx) bool` | Decides per request, before the span starts, whether it comes from a public endpoint. | `nil` |
| `WithSpanStartOptions` | `...oteltrace.SpanStartOption` | Options added to every server span when it starts, such as links or attributes. | none |
| `WithSpanNameFormatter` | `func(fiber.Ctx) string` | Names the span once the handler chain has run, when the span is recording. | `{method} {route}`, or `{method}` when no route matched |
| `WithPort` | `int` | The value of `server.port` on spans and metrics. | spans: the port the `Host` header names; metrics: none |
| `WithClientIP` | `bool` | Whether to record `client.address`, `network.peer.address` and `network.peer.port`. | `true` |
| `WithCapturedRequestHeaders` | `...string` | Request headers to record on the span as `http.request.header.<name>`. | none |
| `WithCapturedResponseHeaders` | `...string` | Response headers to record on the span as `http.response.header.<name>`. | none |
| `WithRedactedQueryParams` | `...string` | Query parameters whose values `url.query` redacts, in addition to the [sensitive ones](#query-redaction). | none |
| `WithCustomAttributes` | `func(fiber.Ctx) []attribute.KeyValue` | Adds attributes to the span when it starts, where samplers see them. | `nil` |
| `WithCustomResponseAttributes` | `func(fiber.Ctx) []attribute.KeyValue` | Adds span attributes after the handler chain has run, when the span is recording. | `nil` |
| `WithCustomMetricAttributes` | `func(fiber.Ctx) []attribute.KeyValue` | Adds attributes to all metrics of a request, resolved when it starts. | `nil` |
| `WithCustomResponseMetricAttributes` | `func(fiber.Ctx) []attribute.KeyValue` | Adds attributes to the duration and body size metrics after the handler chain has run. | `nil` |
| (⚠️ **Deprecated**) `WithCollectClientIP` | `bool` | Deprecated alias for `WithClientIP`. | `true` |
| (❌ **Removed**) `WithServerName` | `string` | This option was removed because the `http.server_name` attribute is deprecated in the OpenTelemetry semantic conventions. The recommended attribute is `server.address`, which this middleware already fills with the hostname reported by Fiber. | - |

Values read from `fiber.Ctx` - `Params`, `Get`, `GetRespHeader` and the like -
are only valid during the request, while spans are exported and metric
attribute sets kept after it. Copy strings the callbacks return with
`utils.CopyString`, unless `fiber.Config.Immutable` is enabled. Keep metric
attributes low-cardinality: each distinct attribute set is a metric series.

## Traces

Every request the middleware does not skip gets a span of kind `SERVER`. It
continues the trace context the configured propagators extract from the
request, unless the request comes from a [public endpoint](#public-endpoints),
and is handed to your handlers through `c.Context()`.

### Span name and status

The span is named `{method} {route}` after the [route that matched](#routes),
for example `GET /users/:id`, or after the method alone when no route
matched. The route is only known once Fiber's router has run, so the span
starts named after the method; samplers see `url.path` and the other
attributes below.

The span status is left unset for 1xx to 4xx responses - a 4xx is the
client's error, not the server's - and set to `Error` for 5xx responses and
codes outside `[100, 600)`. An error returned by the handler chain is recorded
on the span as an `exception` event.

### Attributes

| Attribute | When it is set |
| :--- | :--- |
| `http.request.method`, `url.scheme`, `url.path`, `server.address`, `network.protocol.name`, `network.protocol.version` | always |
| `network.transport` | `tcp`, or `unix` on a Unix socket |
| `server.port` | from `WithPort`, or else when the `Host` header names a port |
| `url.query` | when the request has a query string; see [query redaction](#query-redaction) |
| `user_agent.original` | when the request has a `User-Agent` header |
| `client.address` | under `WithClientIP` (default); the peer, or the client a trusted proxy reports |
| `network.peer.address`, `network.peer.port` | under `WithClientIP`; the other end of the connection |
| `http.request.body.size` | when the request body size is [known](#body-sizes) |
| `enduser.id` | the username of a `Basic` `Authorization` header |
| `http.request.header.<name>` | for the headers named by `WithCapturedRequestHeaders` |
| `http.response.status_code` | once the response is decided |
| `http.route` | when a route matched |
| `error.type` | the status code, when it makes the span an error; `panic` or `response_callback_panic`, see [panics](#panics) |
| `http.response.body.size` | when the response body size is [known](#body-sizes) |
| `http.response.header.<name>` | for the headers named by `WithCapturedResponseHeaders` |

Everything known before the handler runs is set when the span starts, so that
samplers can see it, as the semantic conventions ask. `url.scheme`,
`server.address` and `client.address` honor `X-Forwarded-Proto`,
`X-Forwarded-Host` and the configured `ProxyHeader` only from a proxy Fiber
trusts - see `fiber.Config.TrustProxy`.

### Query redaction

`url.query` holds the query string as the client sent it, except for the
values of the parameters the semantic conventions name as sensitive - the
signatures and credentials of pre-signed S3 and GCS URLs, which grant access
to whoever holds them - which are replaced by `REDACTED`:
`AWSAccessKeyId`, `Signature`, `sig`, `X-Goog-Signature`, `X-Amz-Signature`,
`X-Amz-Credential` and `X-Amz-Security-Token`. Add your own with
`WithRedactedQueryParams`:

```go
app.Use(fiberotel.New(fiberotel.WithRedactedQueryParams("token", "api_key")))
// GET /download?token=s3cr3t&page=2 → url.query="token=REDACTED&page=2"
```

Names are matched as Fiber decodes them, so `s%69g` counts as `sig`, and
case-sensitively, as the semantic conventions specify.

### Captured headers

No header is recorded unless you name it, since headers often carry
credentials and personal data:

```go
app.Use(fiberotel.New(
    fiberotel.WithCapturedRequestHeaders("X-Request-Id", "Accept-Language"),
    fiberotel.WithCapturedResponseHeaders("X-Cache"),
))
// http.request.header.x-request-id=["4f2a…"], http.response.header.x-cache=["HIT"]
```

Names are matched case-insensitively and recorded lowercased, each with every
value the header has.

### Public endpoints

A service exposed to clients it does not control should not let them decide
which trace its spans join - or, under a parent-based sampler, whether they are
recorded at all. With `WithPublicEndpoint`, the span of every request starts a
new trace and links to the span context the caller sent, if any. Use
`WithPublicEndpointFn` to decide per request, for instance to trust callers on
your internal network:

```go
app.Use(fiberotel.New(fiberotel.WithPublicEndpointFn(func(c fiber.Ctx) bool {
    return !strings.HasPrefix(c.IP(), "10.")
})))
```

Under the no-op tracer provider there is no new trace to start, so the caller's
span context is simply not handed on.

## Metrics

| Metric | Instrument | Unit | Attributes |
| :--- | :--- | :--- | :--- |
| `http.server.request.duration` | Float64Histogram | `s` | `http.request.method`, `url.scheme`, `network.protocol.name`, `network.protocol.version`, `http.response.status_code`, `http.route`, `error.type` |
| `http.server.request.body.size` | Int64Histogram | `By` | same as above |
| `http.server.response.body.size` | Int64Histogram | `By` | same as above |
| `http.server.active_requests` | Int64UpDownCounter | `{request}` | `http.request.method`, `url.scheme` |

`server.port` is added to all four when `WithPort` configures it, and the
`WithCustomMetricAttributes` and `WithCustomResponseMetricAttributes`
attributes as described above. The duration histogram uses the bucket
boundaries the semantic conventions advise. Measurements are recorded in the
request's span context, so an SDK configured for exemplars links histogram
samples to the traces of the requests they came from.

`server.address` is on spans only: the semantic conventions make it opt-in on
metrics because it comes from the `Host` header, so every value a client
invented would open a new series. An application that knows its hosts can add
it back:

```go
fiberotel.WithCustomMetricAttributes(func(c fiber.Ctx) []attribute.KeyValue {
    return []attribute.KeyValue{semconv.ServerAddress(utils.CopyString(c.Hostname()))}
})
```

`http.server.request.duration`, like the span, runs from when the middleware is
reached until the request's telemetry ends, so it covers the middleware and
handlers mounted after this one.

### Routes

`http.route` is the pattern of the route that matched the request, including
group prefixes - `/api/users/:id` - and it is left out when no route matched:
a 404, a 405, and a request answered by an `app.Use` handler, such as
`static.New`. Fiber leaves such a request on the last `Use` middleware it
passed, usually this one at `/`, and reporting that would file every unmatched
request under the root route.

One case is attributed to the wrong pattern: if the matched handler falls
through with `c.Next()` into a trailing `app.Use` middleware, Fiber has already
replaced the route with that middleware's mount path by the time this one
records it. Avoid falling through into a trailing `app.Use` if you need exact
routes.

A request fasthttp rejects before routing - a body over `BodyLimit`, oversized
headers, a read timeout - is answered by Fiber's server error handler, which
runs the `Use` chain again and writes the real status only afterwards; this
middleware records such a request with no route and a status of 200.

### Body sizes

`http.server.request.body.size` and `http.server.response.body.size` record a
payload only when its size is known, and are left out otherwise rather than
recorded as zero bytes, which would drag the reported percentiles down. That is
the case for a stream of unannounced length, such as `c.SendStreamWriter` or
`c.SendStream` without a size, whose length is only known once fasthttp has
written it. A pre-parsed multipart form is measured by its `Content-Length`,
since reading the body back would re-marshal every uploaded file into memory.

Under `fiber.Config{StreamRequestBody: true}`, request bodies reach the handler
as streams and are left out of `http.server.request.body.size`: fasthttp
pre-reads a bounded prefix and hands the rest to the handler, which may never
read it, so the announced `Content-Length` is a client's claim rather than a
measurement.

A response that carries no body on the wire records zero, however much the
handler wrote: a `HEAD`, a status that forbids a body (`1xx`, `204`, `304`),
or a handler that sets `c.Response().SkipBody` itself.

### Disabling metrics

`WithoutMetrics(true)` turns them off entirely. With a no-op meter provider, or
SDK views that drop all four instruments, a request skips all metric work as
well. Likewise, with the no-op tracer provider, the middleware starts no span
and only propagates the caller's trace context.

## Context propagation

The span's context is set as the request's `c.Context()`, so anything traced
from a handler becomes a child of the server span, and outgoing calls carry the
trace. When the request is done, that context is canceled - like a `net/http`
request context - and the middleware around this one gets back the context it
passed in.

A streamed response is the exception, as long as the context the request
arrived with can never be canceled, which is Fiber's default: fasthttp writes
the body after the handler has returned - the writer of `c.SendStreamWriter`,
for instance, or the readers behind `c.SendStream`, `c.SendFile` and the static
middleware - so the context is left to the garbage collector instead of being
canceled under them. Under a cancelable incoming context it is canceled as
usual, since a child left uncanceled would stay registered with that context
until it ends.

The trace context is injected into the response headers too, by the
`WithPropagators` propagators unless `WithResponsePropagators` names others. To
expose the trace ID under a header of your own, for instance so a support
request can quote it:

```go
app.Use(fiberotel.New(
    fiberotel.WithTraceResponseHeader("X-Trace-Id"),
    // Optionally, keep the propagation headers - and baggage - out of responses.
    fiberotel.WithResponsePropagators(propagation.NewCompositeTextMapPropagator()),
))
```

## Error handling

The middleware runs the application's error handler on an error returned by
the handler chain, so that the recorded status is the one the client
receives, and returns `nil`. It picks the handler Fiber would: a mounted
sub-app's own `ErrorHandler` for its routes, and a 500 when the error handler
itself fails.

### Panics

A panic in the handler chain is not recovered: it continues, unchanged, to the
recovery middleware mounted outside this one. On its way out it still ends the
request's telemetry - active requests drop back, the duration and request size
are recorded, the context is restored and canceled - and the span records the
panic as an `exception` event, with the stack that raised it, an `Error`
status and `error.type=panic`. The response status and size are left out of
the span and the metrics, because the recovery middleware decides them after
this one has returned. The trace headers are still set on the response.

### Response attribute callbacks

Response attribute callbacks and the span name formatter run after the handler
chain. If one of them panics, the original panic propagates to the
application's recovery middleware, and the span and request metrics record
`error.type=response_callback_panic`, the span with error status; a span name
formatter that panicked is not called again, and the span keeps the default
name. The final
HTTP status and response body size are omitted because outer recovery and
error handling have not run yet. Active-request accounting and context cleanup
still complete.

## Example

```go
package main

import (
    "context"
    "errors"
    "log"

    fiberotel "github.com/gofiber/contrib/v3/otel"
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/middleware/recover"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
    "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
    "go.opentelemetry.io/otel/propagation"
    sdkmetric "go.opentelemetry.io/otel/sdk/metric"
    "go.opentelemetry.io/otel/sdk/resource"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
    oteltrace "go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("fiber-server")

func main() {
    res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("my-service"))

    traceExporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
    if err != nil {
        log.Fatal(err)
    }
    tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExporter), sdktrace.WithResource(res))

    metricExporter, err := stdoutmetric.New()
    if err != nil {
        log.Fatal(err)
    }
    mp := sdkmetric.NewMeterProvider(
        sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
        sdkmetric.WithResource(res),
    )

    otel.SetTracerProvider(tp)
    otel.SetMeterProvider(mp)
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
    defer func() {
        _ = tp.Shutdown(context.Background())
        _ = mp.Shutdown(context.Background())
    }()

    app := fiber.New()
    app.Use(recover.New())
    app.Use(fiberotel.New(fiberotel.WithTraceResponseHeader("X-Trace-Id")))

    app.Get("/error", func(fiber.Ctx) error {
        return errors.New("abc")
    })

    app.Get("/users/:id", func(c fiber.Ctx) error {
        id := c.Params("id")
        name := getUser(c.Context(), id)
        return c.JSON(fiber.Map{"id": id, "name": name})
    })

    if err := app.Listen(":3000"); err != nil {
        log.Print(err)
    }
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
