---
id: prometheus
---

# Prometheus

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*prometheus*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Prometheus/badge.svg)

Prometheus middleware for [Fiber](https://github.com/gofiber/fiber) that instruments incoming requests and serves the metrics endpoint, based on [ansrivas/fiberprometheus](https://github.com/ansrivas/fiberprometheus).

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/prometheus
```

## Signature

```go
prometheus.New(config ...prometheus.Config) fiber.Handler
```

## Config

| Property | Type | Description | Default |
|:---------|:-----|:------------|:--------|
| ServiceName | `string` | Added as the `service` const label on every metric. Omitted when empty. Trimmed; must be valid UTF-8. | `""` |
| Namespace | `string` | Prefixes every metric name. Trimmed; must form a valid metric name. | `"http"` |
| Subsystem | `string` | Prefixes every metric name after `Namespace`. Trimmed and validated the same way. | `""` |
| MetricsPath | `string` | Path served with the Prometheus exposition format. Unless `Next` returns true, requests to it are answered by the middleware and are not instrumented. Compared case-sensitively against the full request path, ignoring trailing slashes. | `"/metrics"` |
| Labels | `prometheus.Labels` | Extra const labels attached to every metric. A key that is empty, starts with `__`, or collides with a reserved label (`status_code`, `status_class`, `method`, `path`, `le`) panics. | `nil` |
| Registerer | `prometheus.Registerer` | Registry used to register the metrics. Reusing one across two `New` calls panics on duplicate registration. | private registry |
| Gatherer | `prometheus.Gatherer` | Source the metrics endpoint gathers from. | private registry |
| DisableGoCollector | `bool` | Skips registration of the Go runtime metrics collector. A registry that refuses the collector is reported to `MetricsErrorLog`, not fatal. | `false` |
| DisableProcessCollector | `bool` | Skips registration of the process metrics collector. A refusal is handled as for `DisableGoCollector`. | `false` |
| RequestDurationBuckets | `[]float64` | Histogram buckets for request latency, in seconds. `nil` selects the defaults; an empty non-nil slice drops the classic buckets, but only alongside `NativeHistogramBucketFactor`. Bounds must be strictly increasing, `+Inf` last only, or `New` panics. | see [Default Config](#default-config) |
| RequestSizeBuckets | `[]float64` | Histogram buckets for request payload size, in bytes. | see [Default Config](#default-config) |
| ResponseSizeBuckets | `[]float64` | Histogram buckets for response payload size, in bytes. | see [Default Config](#default-config) |
| NativeHistogramBucketFactor | `float64` | Enables native histograms when greater than 1, capping the growth factor between buckets. Any other non-zero value panics, including a NaN or an infinity. | `0` |
| NativeHistogramMaxBucketNumber | `uint32` | Bounds the native histogram buckets kept per series. | `0` (unlimited) |
| NativeHistogramMinResetDuration | `time.Duration` | Minimum time before a native histogram may be reset to control its bucket count. | `0` |
| TrackUnmatchedRequests | `bool` | Records metrics for requests that do not resolve to a registered route. | `false` |
| UnmatchedRouteLabel | `string` | Path label used for unmatched requests when `TrackUnmatchedRequests` is enabled. Must be valid UTF-8. | `"/__unmatched__"` |
| EnableOpenMetrics | `bool` | Negotiates the experimental OpenMetrics encoding. Not required for exemplars — protobuf carries them too. | `false` |
| EnableOpenMetricsTextCreatedSamples | `bool` | Adds synthetic `_created` samples to OpenMetrics responses. Requires `EnableOpenMetrics`. | `false` |
| DisableExemplars | `bool` | Skips trace exemplar collection, and with it the request-context read every instrumented request otherwise pays. | `false` |
| DisableCompression | `bool` | Serves metrics uncompressed even when the client requests gzip or zstd. | `false` |
| MetricsMaxRequestsInFlight | `int` | Caps concurrent scrapes; the excess is answered with 503. A negative panics — `promhttp` would read it as unlimited. | `0` (unlimited) |
| MetricsTimeout | `time.Duration` | Bounds a single scrape before it is answered with 503. A negative panics, as for `MetricsMaxRequestsInFlight`. | `0` (no timeout) |
| MetricsErrorLog | `promhttp.Logger` | Receives errors raised while gathering or writing metrics, plus the faults the middleware absorbs (a panicking `DynamicLabels` or `Next`, a refused runtime collector). A typed nil panics. | `nil` |
| MetricsErrorHandling | `promhttp.HandlerErrorHandling` | How gathering errors are reported to the scraper. An unknown value panics; avoid `PanicOnError` — it takes the process down. | `promhttp.HTTPErrorOnError` |
| DisabledMetrics | `[]Metric` | Metric families to skip registering and recording. Entries are trimmed; an unknown name panics. | `nil` |
| SkipURIs | `[]string` | Route patterns excluded from instrumentation, e.g. `/user/:id`. A trailing `*` matches by prefix; a leading `/` is added when missing. A skipped route's error propagates normally. | `nil` |
| SkipStatusCodes | `[]int` | Response status codes excluded from metrics. Codes are three digits; anything else panics. | `nil` |
| SkipStatusClasses | `[]string` | Status classes excluded from metrics, `"1xx"` through `"5xx"` or `"unknown"`. Anything else panics. | `nil` |
| DynamicLabels | `map[string]func(fiber.Ctx) string` | Extra labels computed per request. Names follow the same rules as `Labels`. A panicking function drops the sample. | `nil` |
| Next | `func(fiber.Ctx) bool` | Skips the middleware when it returns true, including for `MetricsPath`. A panicking function is read as true. | `nil` |

## Default Config

```go
var ConfigDefault = Config{
    Namespace:              "http",
    MetricsPath:            "/metrics",
    UnmatchedRouteLabel:    "/__unmatched__",
    RequestDurationBuckets: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 10, 15, 30, 60},
    RequestSizeBuckets:     []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880},
    ResponseSizeBuckets:    []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880},
}
```

## Example

```go
package main

import (
    fiberprometheus "github.com/gofiber/contrib/v3/prometheus"
    "github.com/gofiber/fiber/v3"
)

func main() {
    app := fiber.New()

    // Mount the middleware globally so it observes every request. It answers
    // scrapes on Config.MetricsPath ("/metrics" by default) itself and passes
    // everything else through to your handlers.
    app.Use(fiberprometheus.New(fiberprometheus.Config{
        ServiceName:     "my-service-name",
        SkipURIs:        []string{"/ping"},
        SkipStatusCodes: []int{401, 403, 404},
    }))

    app.Get("/", func(c fiber.Ctx) error {
        return c.SendString("Hello World")
    })

    app.Get("/ping", func(c fiber.Ctx) error {
        return c.SendString("pong")
    })

    app.Post("/some", func(c fiber.Ctx) error {
        return c.SendString("Welcome!")
    })

    app.Listen(":3000")
}
```

Register the middleware before the routes you want instrumented, and mount it
only once — mounting the same handler twice double-counts every request that
reaches both instances. Scrapes are the exception: the first invocation answers
them without calling `ctx.Next()`, so they never reach the second.

## Metrics

The following metrics are exposed, prefixed with `Namespace` (`http` by
default) and `Subsystem`:

```text
http_requests_total
http_requests_status_class_total
http_request_duration_seconds
http_requests_in_progress
http_request_size_bytes
http_response_size_bytes
```

Every metric except `http_requests_in_progress` is labeled with the *registered
route pattern* (for example `/user/:id`), not the request path, so label
cardinality stays bounded no matter what clients request. Trailing slashes are
trimmed from the pattern, which is what lets a `SkipURIs` entry match however it
was spelled — so under `fiber.Config{StrictRouting: true}`, `/foo` and `/foo/`
are two endpoints sharing one series.

`http_requests_in_progress` is labeled by HTTP method only. It has to be
incremented before the router picks a handler, at which point the route pattern
is not known yet.

`http_request_size_bytes` and `http_response_size_bytes` record a payload only
when its size is known — either `Content-Length` is set, or the body is buffered
and can be measured. A stream of unannounced length, such as `c.SendStream`
without a size or an SSE response, is left out of those histograms rather than
recorded as zero bytes, which would drag the reported percentiles towards
nothing. On the request side what actually arrived is measured, so a client
cannot bill the histogram for a body it never sent. The exception is a pre-parsed
multipart form: fasthttp keeps the parsed parts — the large ones spilled to temp
files — so reading the body back would re-marshal every uploaded file into memory
just to size it, and the announced `Content-Length` is used instead — unless it
exceeds `BodyLimit`, which means the body was never received in full and no
honest size exists to record.

Under `fiber.Config{StreamRequestBody: true}` request payloads are left out of
`http_request_size_bytes` entirely. fasthttp prefetches a bounded prefix and
hands the rest to the handler, and a handler that returns without reading it —
an auth rejection, a 404, any route that ignores its body — leaves the remainder
undrained. The announced `Content-Length` is then a client's claim rather than a
measurement, and draining the stream to check would defeat the point of
streaming.

A response that carries no body on the wire records zero however much the
handler wrote — a `HEAD`, any status RFC 9110 forbids a body on (`1xx`, `204`,
`304`), or a handler that sets `c.Response().SkipBody` itself — because fasthttp
drops the body and no payload bytes reach the client.

`http_requests_status_class_total` is convenience, not new information:
`status_class` is a function of `status_code`, so
`rate(http_requests_status_class_total{status_class="5xx"}[5m])` is
`rate(http_requests_total{status_code=~"5.."}[5m])`. It costs a second series
per route, method and class — drop it through `DisabledMetrics` if that trade is
not worth it to you.

Requests that miss every registered route are not recorded unless
`TrackUnmatchedRequests` is enabled, in which case they are labeled with
`UnmatchedRouteLabel`. One caveat comes with that flag: a request fasthttp
rejects before routing — a body over `BodyLimit`, oversized headers, a read
timeout — is counted as a `200`. Fiber answers those through its server error
handler, which replays the `Use` chain with non-`Use` routes skipped and writes
the real status only afterwards, and Fiber v3.4.0 offers no way to tell that
replay apart from an ordinary request answered by `Use` handlers.

A request answered entirely by `app.Use` handlers counts as unmatched too:
`static.New`, or a `Use`-mounted guard returning 401, never matches a non-`Use`
route, so nothing is recorded for it by default.

One case is attributed to the wrong pattern. If the matched handler delegates
onwards with `c.Next()` and a trailing `app.Use` middleware runs last, Fiber has
already replaced the route on the context with that middleware's mount path by
the time this middleware regains control, so the request is recorded under that
mount path — usually `/`. The endpoint pattern is unrecoverable at that point,
and Fiber exposes no way to tell a `use` route from any other, so the case
cannot be detected either. Avoid falling through from a route handler into a
trailing `app.Use` if you need exact route labels.

### Dropping metrics you do not need

Every family costs cardinality. `DisabledMetrics` skips registering and
recording the ones you will not query — most often the two size histograms:

```go
app.Use(fiberprometheus.New(fiberprometheus.Config{
    DisabledMetrics: []fiberprometheus.Metric{
        fiberprometheus.MetricRequestSize,
        fiberprometheus.MetricResponseSize,
    },
}))
```

### Extra labels per request

`DynamicLabels` adds labels whose values are computed once per recorded
request, after the handler chain has returned.

Every distinct value creates a new series, so a value taken straight from the
request is a denial-of-service vector: a client that varies a header freely
grows the registry without bound. Map untrusted input onto a fixed set of
values before returning it:

```go
var knownTenants = map[string]bool{"acme": true, "globex": true}

app.Use(fiberprometheus.New(fiberprometheus.Config{
    DynamicLabels: map[string]func(fiber.Ctx) string{
        "tenant": func(c fiber.Ctx) string {
            if tenant := c.Get("X-Tenant"); knownTenants[tenant] {
                return tenant
            }
            return "other"
        },
    },
}))
```

They apply to every family except `http_requests_in_progress`, which is
incremented before routing and so cannot see them. Names must not collide with
the reserved `status_code`, `status_class`, `method`, `path` and `le` labels or with
`Labels`; the middleware panics at startup if they do.

The middleware copies each returned value, so it is safe to return one of
Fiber's zero-copy strings such as `c.Get(...)` or `c.Params(...)` directly.

A function that panics costs its request every metric, not the request itself:
the sample is dropped, the response is unaffected, and the drop is reported to
`MetricsErrorLog` — once, since a bad type assertion panics on every request, and
a line per request would funnel every connection through the log sink. The
middleware cannot do better than drop it, because these run after the handler
chain has unwound, past any `recover` the application mounted. `Config.Next` is
guarded the same way, but treated as having returned true: the middleware stands
aside, leaving the request — `MetricsPath` included — to the rest of the chain,
rather than serving an endpoint the panicking filter may have meant to withhold.
Guard your type assertions rather than relying on either.

### Filtering

An entry may also name `UnmatchedRouteLabel` to exclude the traffic
`TrackUnmatchedRequests` records. That is a separate namespace — this list holds
route patterns, the label is a value — so a real route spelled the same as the
label still obeys the pattern rules, and a rule written for real routes never
takes 404 monitoring with it. The separator distinguishes the two shapes: with
the label set to `/api`, `/api`, `/api*` and `/api*/` name the label, while
`/api/*` is a prefix rule for the routes below it.

`SkipURIs` matches the registered route pattern — note that fiberzap's option
of the same name matches the request path instead. A trailing `*` matches by
prefix and stops at a path segment boundary, so `/admin/*` excludes `/admin` and
`/admin/users` but not `/administration`. Trailing stars are stripped as a
group, so the glob spelling `/admin/**` means the same thing. `/*` excludes
everything, and then no metric family is registered at all — not even the
in-flight gauge, which is incremented before routing and so beyond the reach of
any per-route filter.
Trailing slashes are ignored, and a leading `/` is added when missing — route
patterns always carry one, so `admin` without it would otherwise match nothing.
The match is case-sensitive against the pattern as registered, while Fiber routes
case-insensitively by default, so spell the entry the way the route was
registered: `/Admin`, not `/admin`.

Because Fiber route patterns can themselves end in `*`, such an entry also
matches the pattern named exactly that: `/static*` excludes both the route
registered as `/static*` and anything under `/static`.

Blank entries are ignored in `SkipURIs` and `SkipStatusClasses` alike, so
splitting an unset environment variable on `,` neither excludes the root route
nor stops the process from booting; ask for `/` explicitly to skip the root.
Every other unmatchable entry — an unknown metric name, a status code that is not
three digits, a status class outside `"1xx"`–`"5xx"` and `"unknown"` — panics at
startup rather than filtering nothing in silence.

`SkipStatusCodes` takes exact codes; `SkipStatusClasses` takes whole classes
so you do not have to enumerate them:

```go
app.Use(fiberprometheus.New(fiberprometheus.Config{
    SkipURIs:          []string{"/health", "/internal/*"},
    SkipStatusClasses: []string{"4xx"},
}))
```

## Error handling

Because Fiber runs the application error handler only after the whole handler
chain has unwound, the middleware invokes it itself when a downstream handler
returns an error. This is what Fiber's own logger middleware does, and it is
what allows the recorded status code and response size to match what the client
actually received. The error is therefore consumed by this middleware and does
not propagate to handlers mounted *before* it.

That applies only to requests it records. When the middleware stands aside —
`Next` returns true, every metric family is disabled, or the route matched a
`SkipURIs` pattern — there is no status code to be gained, so the error is
returned unchanged and surfaces where it would without the middleware mounted.

A request answered by Fiber's `timeout` middleware is recorded with the `408` the
client receives. That middleware answers through fasthttp's
`RequestCtx.TimeoutErrorWithCode`, which parks the response and installs it only
after the whole chain has unwound, so the live response still reads `200` at the
point metrics are taken; the middleware reads the parked one instead.

Mount `recover.New()` **after** this middleware, not before it:

```go
app.Use(prometheus.New(prometheus.Config{}))
app.Use(recover.New())
```

A panic unwinds straight past the recording step, so with `recover` mounted
first the middleware never sees the request complete and the resulting 500
appears in no metric family at all — only the in-flight gauge, which is
deferred, stays balanced. Recovering downstream turns the panic into an ordinary
error return, which this middleware records as the 500 the client received.

## Registry and collectors

By default the middleware creates a private `Registerer`/`Gatherer` pair and
uses it for both registration and scraping, and registers the Go runtime and
process collectors into it.

When customizing the registry, ensure that `Registerer` and `Gatherer` refer to
the same metrics source (for example, a `*prometheus.Registry`). Supplying only
one that does not implement the other interface panics during initialization so
metrics are not silently dropped.

Supplying both also supports pairing a wrapper such as
`prometheus.WrapRegistererWithPrefix` with the registry it wraps. Such a wrapper
is not itself a `Gatherer`, so it is unwrapped — through its nested
client_golang wrappers — down to the `Registerer` it hands registrations to, and
that is what the `Gatherer` is compared against. A wrapper the walk resolves to
an unrelated registry panics during initialization rather than silently omitting
the middleware's metrics from scrapes.

The comparison is deliberately one of provable inequality, so a pair the walk
cannot resolve is accepted, and three shapes are undecidable. A `Registerer`
that is neither a `Gatherer` nor a client_golang wrapper keeps its destination
to itself — reading any type that merely holds another `Registerer` as a
delegation chain would reject valid pairings. A `Gatherer` that hides its source
behind a wrapper, `prometheus.GathererFunc(other.Gather)` for instance, is
opaque in the same way even when the `Registerer` is a plain registry. And the
walk itself is depth-bounded, so a chain of wrappers longer than the bound ends
undecided rather than resolved — a bound no real configuration reaches, since
wrappers are nested by hand. None of the three is checked, so pairing one with
an unrelated source still scrapes nothing.

```go
registry := prometheus.NewRegistry()

app.Use(fiberprometheus.New(fiberprometheus.Config{
    ServiceName:    "my-service-name",
    Registerer: registry,
    Gatherer:   registry,
}))
```

### Where the endpoint is served

`MetricsPath` is compared case-sensitively against the full request path, with
trailing slashes ignored on both sides, which has two consequences worth knowing
before you mount the middleware anywhere but the root.

Mounting on a group leaves the default endpoint unreachable — `/api/metrics`
never equals `/metrics`, and `/metrics` never reaches the group, so both 404.
Set the full path instead:

```go
api := app.Group("/api")
api.Use(fiberprometheus.New(fiberprometheus.Config{MetricsPath: "/api/metrics"}))
```

And because Fiber routes case-insensitively by default while this comparison
does not, `GET /METRICS` is instrumented as an ordinary request rather than
answered with the exposition page.

### Exposure of the metrics endpoint

The endpoint is unauthenticated. Because the middleware answers `MetricsPath`
itself before routing, a route registered at the same path with auth in front of
it is never reached — so restrict access at the network layer, bind the metrics
listener to an internal interface, or use `Config.Next` to reject scrapes that
do not come from your monitoring system.

A scrape discloses more than request counts: the default collectors report the
exact Go version, process memory, open file descriptors and start time, and the
`path` label enumerates every registered route pattern in the application. Treat
it as internal.

### Protecting the scrape endpoint

A slow or stuck gather can otherwise pile up. `MetricsMaxRequestsInFlight` caps
concurrent scrapes and `MetricsTimeout` bounds each one; both answer the excess
with 503. Gather errors are silent by default — pass a `MetricsErrorLog` to see
them:

```go
app.Use(fiberprometheus.New(fiberprometheus.Config{
    MetricsMaxRequestsInFlight: 4,
    MetricsTimeout:             10 * time.Second,
    MetricsErrorLog:            log.New(os.Stderr, "prometheus: ", log.LstdFlags),
}))
```

`MetricsTimeout` bounds the *answer*, not the gather: `promhttp` replies 503 and
returns while the gathering goroutine runs on to completion, still holding its
`MetricsMaxRequestsInFlight` slot. A gatherer that regularly outruns the timeout
will therefore still exhaust the cap — fix the slow collector rather than raising
the limit.

## Native histograms

Setting `NativeHistogramBucketFactor` above 1 enables native histograms on the
duration and size histograms. They resolve latency without hand-tuned buckets:
the factor caps the growth between consecutive buckets, so `1.1` gives roughly
10% resolution. This requires a Prometheus server with native histograms
enabled, and the values are only carried by the protobuf exposition format.

Classic and native buckets are emitted together by default. To go native-only,
pass an empty non-nil bucket slice — unlike `nil`, which selects the defaults.
This only takes effect alongside `NativeHistogramBucketFactor`: client_golang
substitutes its own defaults rather than leave a histogram with no buckets, so
an empty slice on its own would give the size histograms latency-shaped buckets
(`le="0.005"` … `le="10"`) measured in bytes.

```go
app.Use(fiberprometheus.New(fiberprometheus.Config{
    NativeHistogramBucketFactor:     1.1,
    NativeHistogramMaxBucketNumber:  160,
    NativeHistogramMinResetDuration: time.Hour,

    RequestDurationBuckets: []float64{},
    RequestSizeBuckets:     []float64{},
    ResponseSizeBuckets:    []float64{},
}))
```

## Exemplars

When the request context carries a **sampled** OpenTelemetry span, the duration
and size histograms record the trace ID as an exemplar under the `traceID`
label. Unsampled spans are skipped: Prometheus keeps one exemplar per bucket and
overwrites it on each observation, so recording traces that were never exported
would evict the links that lead somewhere.
Reaching a scraper takes an encoding that carries exemplars. Protobuf does, and
promhttp negotiates it without any configuration — which is what a Prometheus
server asks for once native histograms are enabled. OpenMetrics text does too,
but only when you set `EnableOpenMetrics: true`; the plain text exposition
carries no exemplars in either case.

Collecting one costs a request-context read on every instrumented request: Fiber
installs a background context when the application never set one, which the
request then has to clear again on release. Set `DisableExemplars: true` when
nothing in your stack starts spans, and that work goes away.

## 📊 Result

- Hit the default url at [http://localhost:3000](http://localhost:3000)
- Navigate to [http://localhost:3000/metrics](http://localhost:3000/metrics)
