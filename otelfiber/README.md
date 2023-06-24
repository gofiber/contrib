# Otelfiber

![Release](https://img.shields.io/github/release/gofiber/contrib.svg)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

[OpenTelemetry](https://opentelemetry.io/) support for Fiber.

Can be found on [OpenTelemetry Registry](https://opentelemetry.io/registry/instrumentation-go-fiber/).

### Install

This middleware supports Fiber v2.

```
go get -u github.com/gofiber/contrib/otelfiber
```

### Signature

```
otelfiber.Middleware(opts ...Option) fiber.Handler
```

### Config


| Property          | Type                            | Description                                                                      | Default                                                             |
| :------------------ | :-------------------------------- | :--------------------------------------------------------------------------------- | :-------------------------------------------------------------------- |
| Next              | `func(*fiber.Ctx) bool`         | Define a function to skip this middleware when returned trueRequired - Rego quer | nil                                                                 |
| TracerProvider    | `oteltrace.TracerProvider`      | Specifies a tracer provider to use for creating a tracer                         | nil - the global tracer provider is used                                   |
| MeterProvider     | `otelmetric.MeterProvider`      | Specifies a meter provider to use for reporting                                     | nil - the global meter provider is used                                                             |
| Port              | `*int`                          | Specifies the value to use when setting the `net.host.port` attribute on metrics/spans                            | Required: If not default (`80` for `http`, `443` for `https`)                                                               |
| Propagators       | `propagation.TextMapPropagator` | Specifies propagators to use for extracting information from the HTTP requests                     | If none are specified, global ones will be used                                                               |
| ServerName        | `*string`                       | specifies the value to use when setting the `http.server_name` attribute on metrics/spans                                          | -                                                                   |
| SpanNameFormatter | `func(*fiber.Ctx) string`       | Takes a function that will be called on every request and the returned string will become the Span Name                                   | default formatter returns the route pathRaw |

### Usage

Please refer to [example](./example)
