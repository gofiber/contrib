---
id: testcontainers
---

# Testcontainers

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=testcontainers*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20testcontainers/badge.svg)

A [Testcontainers](https://golang.testcontainers.org/) Service Implementation for Fiber.

:::note

Requires Go **1.23** and above

:::

## Install

:::caution

This Service Implementation only supports Fiber **v3**.

:::

```shell
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/testcontainers
```

## Signature

### Adding a Generic Container

```go
testcontainers.Add(ctx context.Context, cfg *fiber.Config, serviceKey string, img string, opts ...testcontainers.ContainerCustomizer) (*ContainerService[T], error)
```

### Adding a Module

```go
testcontainers.AddModule(ctx context.Context, cfg *fiber.Config, serviceKey string, moduleRunFn func(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (T, error), img string, opts ...testcontainers.ContainerCustomizer) (*ContainerService[T], error)
```

## Types

### ContainerService

The `ContainerService` type is a generic type that embeds a [testcontainers.Container](https://pkg.go.dev/github.com/testcontainers/testcontainers-go#Container) interface, and implements the [fiber.Service] interface, thanks to the
Start, String, State and Terminate methods.

```go
// ContainerService represents a container that implements the [fiber.Service] interface.
// It manages the lifecycle of a [testcontainers.Container] instance, and it can be
// retrieved from the Fiber app's state calling the [fiber.MustGetService] function with
// the key returned by the [ContainerService.Key] method.
type ContainerService[T testcontainers.Container] struct
```

#### Signature

#####  Key

```go
// Key returns the key used to identify the service in the Fiber app's state.
func (c *ContainerService[T]) Key() string
```

##### Container

```go
// Container returns the Testcontainers container instance, giving full access to the T type methods.
// It's useful to access the container's methods, like [testcontainers.Container.MappedPort]
// or [testcontainers.Container.Inspect].
func (c *ContainerService[T]) Container() T
```

##### Start

```go
// Start creates and starts the container, calling the [runFn] function with the [img] and [opts] arguments.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) Start(ctx context.Context) error
```

##### String

```go
// String returns a human-readable representation of the container's state.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) String() string
```

##### State

```go
// State returns the status of the container.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) State(ctx context.Context) (string, error)
```

##### Terminate

```go
// Terminate stops and removes the container. It implements the [fiber.Service] interface.
func (c *ContainerService[T]) Terminate(ctx context.Context) error
```

## Examples

See the [testable examples](./examples_test.go) for more details.
