package testcontainers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/testcontainers/testcontainers-go"
)

const (
	// serviceSuffix is the suffix added to the service key to identify it as a Testcontainers service.
	serviceSuffix = " (using testcontainers-go)"

	// fiberContainerLabel is the label added to the container to identify it as a Fiber app.
	fiberContainerLabel = "org.testcontainers.golang.framework"

	// fiberContainerLabelValue is the value of the label added to the container to identify it as a Fiber app.
	fiberContainerLabelValue = "Go Fiber"
)

var (
	// ErrNilConfig is returned when the config is nil.
	ErrNilConfig = errors.New("config is nil")

	// ErrContainerNotRunning is returned when the container is not running.
	ErrContainerNotRunning = errors.New("container is not running")

	// ErrEmptyServiceKey is returned when the service key is empty.
	ErrEmptyServiceKey = errors.New("service key is empty")

	// ErrImageEmpty is returned when the image is empty.
	ErrImageEmpty = errors.New("image is empty")

	// ErrRunFnNil is returned when the runFn is nil.
	ErrRunFnNil = errors.New("runFn is nil")
)

// buildKey builds a key for a container service.
// This key is used to identify the service in the Fiber app's state.
func buildKey(key string) string {
	if strings.HasSuffix(key, serviceSuffix) {
		return key
	}

	return key + serviceSuffix
}

// ContainerService represents a container that implements the [fiber.Service] interface.
// It manages the lifecycle of a [testcontainers.Container] instance, and it can be
// retrieved from the Fiber app's state calling the [fiber.MustGetService] function with
// the key returned by the [ContainerService.Key] method.
//
// The type parameter T must implement the [testcontainers.Container] interface.
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
// Consumers should use string constants for service keys to ensure consistency
// when retrieving services from the Fiber app's state.
func (c *ContainerService[T]) Key() string {
	return c.key
}

// Container returns the Testcontainers container instance, giving full access to the T type methods.
// It's useful to access the container's methods, like [testcontainers.Container.MappedPort]
// or [testcontainers.Container.Inspect].
func (c *ContainerService[T]) Container() T {
	if !c.initialized {
		var zero T
		return zero
	}

	return c.ctr
}

// Start creates and starts the container, calling the [runFn] function with the [img] and [opts] arguments.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) Start(ctx context.Context) error {
	c.opts = append(c.opts, testcontainers.WithLabels(map[string]string{
		fiberContainerLabel: fiberContainerLabelValue,
	}))

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
		return "", fmt.Errorf("get container state for %s: %w", c.key, err)
	}

	return st.Status, nil
}

// Terminate stops and removes the container. It implements the [fiber.Service] interface.
func (c *ContainerService[T]) Terminate(ctx context.Context) error {
	if !c.initialized {
		return ErrContainerNotRunning
	}

	if err := c.ctr.Terminate(ctx); err != nil {
		return fmt.Errorf("terminate container: %w", err)
	}

	c.initialized = false
	// Reset container reference to avoid potential use after free
	var zero T
	c.ctr = zero

	return nil
}

// AddService adds a Testcontainers container as a [fiber.Service] for the Fiber app.
// It returns a pointer to a [ContainerService[T]] object, which contains the key used to identify
// the service in the Fiber app's state, and an error if the config is nil.
// The container should be a function like redis.Run or postgres.Run that returns a container type
// which embeds [testcontainers.Container].
// - The cfg is the Fiber app's configuration, needed to add the service to the Fiber app's state.
// - The containerConfig is the configuration for the container, where:
//   - The containerConfig.ServiceKey is the key used to identify the service in the Fiber app's state.
//   - The containerConfig.RunFn is the function to use to run the container. It's usually the Run function from the module, like redis.Run or postgres.Run.
//   - The containerConfig.Image is the image to use for the container.
//   - The containerConfig.Options are the functional options to pass to the [testcontainers.Run] function. This argument is optional.
//
// Use [NewModuleConfig] or [NewContainerConfig] helper functions to create valid containerConfig objects.
func AddService[T testcontainers.Container](cfg *fiber.Config, containerConfig Config[T]) (*ContainerService[T], error) {
	if cfg == nil {
		return nil, ErrNilConfig
	}

	if containerConfig.ServiceKey == "" {
		return nil, ErrEmptyServiceKey
	}

	if containerConfig.Image == "" {
		return nil, ErrImageEmpty
	}

	if containerConfig.RunFn == nil {
		return nil, ErrRunFnNil
	}

	k := buildKey(containerConfig.ServiceKey)

	c := &ContainerService[T]{
		key:   k,
		img:   containerConfig.Image,
		opts:  containerConfig.Options,
		runFn: containerConfig.RunFn,
	}

	cfg.Services = append(cfg.Services, c)

	return c, nil
}
