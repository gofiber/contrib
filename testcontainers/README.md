---
id: testcontainers
---

# Testcontainers

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=testcontainers*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Testcontainers%20Services/badge.svg)

A [Testcontainers](https://golang.testcontainers.org/) Service Implementation for Fiber.

:::note

Requires Go **1.23** and above

:::

## Common Use Cases

- Local development
- Integration testing
- Isolated service testing
- End-to-end testing

## Install

:::caution

This Service Implementation only supports Fiber **v3**.

:::

```shell
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/testcontainers
```

## Signature

### NewModuleConfig

```go
// NewModuleConfig creates a new container service config for a module.
//
// - The serviceKey is the key used to identify the service in the Fiber app's state.
// - The img is the image name to use for the container.
// - The run is the function to use to run the container. It's usually the Run function from the module, like [redis.Run] or [postgres.Run].
// - The opts are the functional options to pass to the run function. This argument is optional.
func NewModuleConfig[T testcontainers.Container](
 serviceKey string,
 img string,
 run func(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (T, error),
 opts ...testcontainers.ContainerCustomizer,
) Config[T] {
```

### NewContainerConfig

```go
// NewContainerConfig creates a new container service config for a generic container type,
// not created by a Testcontainers module. So this function best used in combination with
// the [AddService] function to add a custom container to the Fiber app's state.
//
// - The serviceKey is the key used to identify the service in the Fiber app's state.
// - The img is the image name to use for the container.
// - The opts are the functional options to pass to the [testcontainers.Run] function. This argument is optional.
//
// This function uses the [testcontainers.Run] function as the run function.
func NewContainerConfig[T *testcontainers.DockerContainer](serviceKey string, img string, opts ...testcontainers.ContainerCustomizer) Config[*testcontainers.DockerContainer]
```

### AddService

```go
// AddService adds a Testcontainers container as a [fiber.Service] for the Fiber app.
// It returns a pointer to a [ContainerService[T]] object, which contains the key used to identify
// the service in the Fiber app's state, and an error if the config is nil.
// The container should be a function like redis.Run or postgres.Run that returns a container type
// which embeds [testcontainers.Container].
// - The cfg is the Fiber app's configuration, needed to add the service to the Fiber app's state.
// - The containerConfig is the configuration for the container, where:
//   - The containerConfig.ServiceKey is the key used to identify the service in the Fiber app's state.
//   - The containerConfig.Run is the function to use to run the container. It's usually the Run function from the module, like redis.Run or postgres.Run.
//   - The containerConfig.Image is the image to use for the container.
//   - The containerConfig.Options are the functional options to pass to the [testcontainers.Run] function. This argument is optional.
//
// Use [NewModuleConfig] or [NewContainerConfig] helper functions to create valid containerConfig objects.
func AddService[T testcontainers.Container](cfg *fiber.Config, containerConfig Config[T]) (*ContainerService[T], error) {
```

## Types

### Config

The `Config` type is a generic type that is used to configure the container.

| Property    | Type | Description | Default |
|-------------|------|-------------|---------|
| ServiceKey  | string | The key used to identify the service in the Fiber app's state. | - |
| Image      | string | The image name to use for the container. | - |
| Run     | func(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (T, error) | The function to use to run the container. It's usually the Run function from the testcontainers-go module, like redis.Run or postgres.Run | - |
| Options    | []testcontainers.ContainerCustomizer | The functional options to pass to the [testcontainers.Run] function. This argument is optional. | - |

```go
// Config contains the configuration for a container service.
type Config[T testcontainers.Container] struct {
 // ServiceKey is the key used to identify the service in the Fiber app's state.
 ServiceKey string

 // Image is the image name to use for the container.
 Image string

 // Run is the function to use to run the container.
 // It's usually the Run function from the testcontainers-go module, like redis.Run or postgres.Run,
 // although it could be the generic [testcontainers.Run] function from the testcontainers-go package.
 Run func(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (T, error)

 // Options are the functional options to pass to the [testcontainers.Run] function. This argument is optional.
 // You can find the available options in the [testcontainers website].
 //
 // [testcontainers website]: https://golang.testcontainers.org/features/creating_container/#customizing-the-container
 Options []testcontainers.ContainerCustomizer
}
```

### ContainerService

The `ContainerService` type is a generic type that embeds a [testcontainers.Container](https://pkg.go.dev/github.com/testcontainers/testcontainers-go#Container) interface,
and implements the [fiber.Service] interface, thanks to the Start, String, State and Terminate methods. It manages the lifecycle of a `testcontainers.Container` instance,
and it can be retrieved from the Fiber app's state calling the `fiber.MustGetService` function with the key returned by the `ContainerService.Key` method.

The type parameter `T` must implement the [testcontainers.Container](https://pkg.go.dev/github.com/testcontainers/testcontainers-go#Container) interface,
as in the Testcontainers Go modules (e.g. [redis.RedisContainer](https://pkg.go.dev/github.com/testcontainers/testcontainers-go/modules/redis#RedisContainer),
[postgres.PostgresContainer](https://pkg.go.dev/github.com/testcontainers/testcontainers-go/modules/postgres#PostgresContainer), etc.), or in the generic
[testcontainers.DockerContainer](https://pkg.go.dev/github.com/testcontainers/testcontainers-go#GenericContainer) type, used for custom containers.

:::note

Since `ContainerService` implements the `fiber.Service` interface, container cleanup is handled automatically by the Fiber framework when the application shuts down. There's no need for manual cleanup code.

:::

```go
type ContainerService[T testcontainers.Container] struct
```

#### Signature

#####  Key

```go
// Key returns the key used to identify the service in the Fiber app's state.
// Consumers should use string constants for service keys to ensure consistency
// when retrieving services from the Fiber app's state.
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
// Start creates and starts the container, calling the [run] function with the [img] and [opts] arguments.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) Start(ctx context.Context) error
```

##### String

```go
// String returns the service key, which uniquely identifies the container service.
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

### Common Errors

| Error | Description | Resolution |
|-------|-------------|------------|
| ErrNilConfig | Returned when the config is nil | Ensure config is properly initialized |
| ErrContainerNotRunning | Returned when the container is not running | Check container state before operations |
| ErrEmptyServiceKey | Returned when the service key is empty | Provide a non-empty service key |
| ErrImageEmpty | Returned when the image is empty | Provide a valid image name |
| ErrRunNil | Returned when the run is nil | Provide a valid run function |

## Examples

You can find more examples in the [testable examples](https://github.com/gofiber/contrib/blob/main/testcontainers/examples_test.go).

### Adding a module container using the Testcontainers Go's Redis module

```go
package main

import (
 "fmt"
 "log"

 "github.com/gofiber/fiber/v3"

 "github.com/gofiber/contrib/testcontainers"
 tc "github.com/testcontainers/testcontainers-go"
 "github.com/testcontainers/testcontainers-go/modules/redis"
)

func main() {
 cfg := &fiber.Config{}

 // Define the base key for the module service.
 // The service returned by the [testcontainers.AddService] function,
 // using the [ContainerService.Key] method,
 // concatenates the base key with the "using testcontainers-go" suffix.
 const (
  redisKey    = "redis-module"
 )

 // Adding containers coming from the testcontainers-go modules,
 // in this case, a Redis and a Postgres container.

 redisModuleConfig := testcontainers.NewModuleConfig(redisKey, "redis:latest", redis.Run)
 redisSrv, err := testcontainers.AddService(cfg, redisModuleConfig)
 if err != nil {
  log.Println("error adding redis module:", err)
  return
 }

 // Create a new Fiber app, using the provided configuration.
 app := fiber.New(*cfg)

 // Retrieve all services from the app's state.
 // This returns a slice of all the services registered in the app's state.
 srvs := app.State().Services()

 // Retrieve the Redis container from the app's state using the key returned by the [ContainerService.Key] method.
 redisCtr := fiber.MustGetService[*testcontainers.ContainerService[*redis.RedisContainer]](app.State(), redisSrv.Key())

 // Start the Fiber app.
 app.Listen(":3000")
}
```

### Adding a custom container using the Testcontainers Go package

```go
package main

import (
 "fmt"
 "log"

 "github.com/gofiber/fiber/v3"

 "github.com/gofiber/contrib/testcontainers"
 tc "github.com/testcontainers/testcontainers-go"
)

func main() {
 cfg := &fiber.Config{}

 // Define the base key for the generic service.
 // The service returned by the [testcontainers.AddService] function,
 // using the [ContainerService.Key] method,
 // concatenates the base key with the "using testcontainers-go" suffix.
 const (
  nginxKey = "nginx-generic"
 )

 // Adding a generic container, directly from the testcontainers-go package.
 containerConfig := testcontainers.NewContainerConfig(nginxKey, "nginx:latest", tc.WithExposedPorts("80/tcp"))

 nginxSrv, err := testcontainers.AddService(cfg, containerConfig)
 if err != nil {
  log.Println("error adding nginx generic:", err)
  return
 }

 app := fiber.New(*cfg)

 nginxCtr := fiber.MustGetService[*testcontainers.ContainerService[*tc.DockerContainer]](app.State(), nginxSrv.Key())

 // Start the Fiber app.
 app.Listen(":3000")
}
```
