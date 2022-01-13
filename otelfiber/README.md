# Fibersentry

![Release](https://img.shields.io/github/release/gofiber/contrib.svg)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

[OpenTelemetry](https://opentelemetry.io/) support for Fiber.

### Install

This middleware supports Fiber v2.

```
go get -u github.com/gofiber/contrib/otelfiber
```

### Signature

```
otelfiber.Middleware(service string, opts ...Option) fiber.Handler
```

### Usage

Please refer to [example](./example)
