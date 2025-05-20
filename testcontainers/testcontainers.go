package testcontainers

import (
	"context"
	"errors"
	"fmt"

	"github.com/gofiber/fiber/v3"
	"github.com/testcontainers/testcontainers-go"
)

// serviceSuffix is the suffix added to the service key to identify it as a Testcontainers service.
const serviceSuffix = " (using testcontainers-go)"

var (
	// ErrNilConfig is returned when the config is nil.
	ErrNilConfig = errors.New("config is nil")

	// ErrContainerNotRunning is returned when the container is not running.
	ErrContainerNotRunning = errors.New("container is not running")
)

// buildKey builds a key for a container service.
// This key is used to identify the service in the Fiber app's state.
func buildKey(key string) string {
	return key + serviceSuffix
}

// ContainerService represents a container that implements the [fiber.Service] interface.
// It manages the lifecycle of a [testcontainers.Container] instance, and it can be
// retrieved from the Fiber app's state calling the [fiber.MustGetService] function with
// the key returned by the [ContainerService.Key] method.
type ContainerService[T testcontainers.Container] struct {
	// The container instance, using the generic type T.
	ctr T

	// initialized tracks whether the container has been started
	initialized bool

	// The key used to identify the service in the Fiber app's state.
	key string

	// The image to use for the container.
	// It's used to run the container with a specific image.
	img string

	// The functional options to pass to the [runFn] function.
	// It's used to customize the container.
	opts []testcontainers.ContainerCustomizer

	// The function to use to run the container.
	// It's usually the Run function from a testcontainers-go module, like redis.Run or postgres.Run,
	// or the Run function from the testcontainers-go package.
	// It returns a container instance of type T, which embeds [testcontainers.Container],
	// like [redis.RedisContainer] or [postgres.PostgresContainer].
	runFn func(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (T, error)
}

// Key returns the key used to identify the service in the Fiber app's state.
func (c *ContainerService[T]) Key() string {
	return c.key
}

// Start creates and starts the container, calling the [runFn] function with the [img] and [opts] arguments.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) Start(ctx context.Context) error {
	ctr, err := c.runFn(ctx, c.img, c.opts...)
	if err != nil {
		return fmt.Errorf("run container: %w", err)
	}

	c.ctr = ctr
	c.initialized = true

	return nil
}

// String returns a human-readable representation of the container's state.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) String() string {
	return c.key
}

// State returns the status of the container.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) State(ctx context.Context) (string, error) {
	if !c.initialized {
		return "", ErrContainerNotRunning
	}

	st, err := c.ctr.State(ctx)
	if err != nil {
		return "", fmt.Errorf("get container state: %w", err)
	}

	return st.Status, nil
}

// Terminate stops and removes the container. It implements the [fiber.Service] interface.
func (c *ContainerService[T]) Terminate(ctx context.Context) error {
	return c.ctr.Terminate(ctx)
}

// AddModule adds a Testcontainers module container as a [fiber.Service] for the Fiber app,
// returning the key used to identify the service in the Fiber app's state, and an error if the config is nil.
// The module should be a function like redis.Run or postgres.Run that returns a container type
// which embeds [testcontainers.Container].
// - The cfg is the Fiber app's configuration.
// - The serviceKey is the key used to identify the service in the Fiber app's state.
// - The moduleRunFn is the function to use to run the container. It's usually the Run function from the module, like redis.Run or postgres.Run.
// - The img is the image to use for the container.
// - The opts are the functional options to pass to the [testcontainers.Run] function. This argument is optional.
func AddModule[T testcontainers.Container](
	ctx context.Context,
	cfg *fiber.Config,
	serviceKey string,
	moduleRunFn func(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (T, error),
	img string,
	opts ...testcontainers.ContainerCustomizer,
) (*ContainerService[T], error) {
	if cfg == nil {
		return nil, ErrNilConfig
	}

	k := buildKey(serviceKey)

	c := &ContainerService[T]{
		key:   k,
		img:   img,
		opts:  opts,
		runFn: moduleRunFn,
	}

	cfg.Services = append(cfg.Services, c)

	return c, nil
}

// Add adds a Testcontainers container as a [fiber.Service] to the Fiber app,
// using the [testcontainers.Run] function. It returns the key used to identify the service in the Fiber app's state,
// and an error if the config is nil.
// It's equivalent to calling [AddModule] with the [testcontainers.Run] function.
// - The cfg is the Fiber app's configuration.
// - The serviceKey is the key used to identify the service in the Fiber app's state.
// - The img is the image name to use for the container.
// - The opts are the functional options to pass to the [testcontainers.Run] function. This argument is optional.
func Add(
	ctx context.Context,
	cfg *fiber.Config,
	serviceKey string,
	img string,
	opts ...testcontainers.ContainerCustomizer,
) (*ContainerService[*testcontainers.DockerContainer], error) {
	return AddModule(ctx, cfg, serviceKey, testcontainers.Run, img, opts...)
}
