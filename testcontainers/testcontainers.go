package testcontainers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	tc "github.com/testcontainers/testcontainers-go"
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

	// ErrRunNil is returned when the run is nil.
	ErrRunNil = errors.New("run is nil")
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
// It manages the lifecycle of a [tc.Container] instance, and it can be
// retrieved from the Fiber app's state calling the [fiber.MustGetService] function with
// the key returned by the [ContainerService.Key] method.
//
// The type parameter T must implement the [tc.Container] interface.
type ContainerService[T tc.Container] struct {
	// The container instance, using the generic type T.
	ctr T

	// initialized tracks whether the container has been started
	initialized bool

	// The key used to identify the service in the Fiber app's state.
	key string

	// The image to use for the container.
	// It's used to run the container with a specific image.
	img string

	// The functional options to pass to the [run] function.
	// It's used to customize the container.
	opts []tc.ContainerCustomizer

	// The function to use to run the container.
	// It's usually the Run function from a testcontainers-go module, like redis.Run or postgres.Run,
	// or the Run function from the testcontainers-go package.
	// It returns a container instance of type T, which embeds [tc.Container],
	// like [redis.RedisContainer] or [postgres.PostgresContainer].
	run func(ctx context.Context, img string, opts ...tc.ContainerCustomizer) (T, error)
}

// Key returns the key used to identify the service in the Fiber app's state.
// Consumers should use string constants for service keys to ensure consistency
// when retrieving services from the Fiber app's state.
func (c *ContainerService[T]) Key() string {
	return c.key
}

// Container returns the Testcontainers container instance, giving full access to the T type methods.
// It's useful to access the container's methods, like [tc.Container.MappedPort]
// or [tc.Container.Inspect].
func (c *ContainerService[T]) Container() T {
	if !c.initialized {
		var zero T
		return zero
	}

	return c.ctr
}

// Start creates and starts the container, calling the [run] function with the [img] and [opts] arguments.
// It implements the [fiber.Service] interface.
func (c *ContainerService[T]) Start(ctx context.Context) error {
	if c.initialized {
		return fmt.Errorf("container %s already initialized", c.key)
	}

	opts := append([]tc.ContainerCustomizer{}, c.opts...)
	opts = append(opts, tc.WithLabels(map[string]string{
		fiberContainerLabel: fiberContainerLabelValue,
	}))

	ctr, err := c.run(ctx, c.img, opts...)
	if err != nil {
		return fmt.Errorf("run container: %w", err)
	}

	c.ctr = ctr
	c.initialized = true

	return nil
}

// String returns the service key, which uniquely identifies the container service.
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

	if st == nil {
		return "", fmt.Errorf("container state is nil for %s", c.key)
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
// which embeds [tc.Container].
// - The cfg is the Fiber app's configuration, needed to add the service to the Fiber app's state.
// - The containerConfig is the configuration for the container, where:
//   - The containerConfig.ServiceKey is the key used to identify the service in the Fiber app's state.
//   - The containerConfig.Run is the function to use to run the container. It's usually the Run function from the module, like redis.Run or postgres.Run.
//   - The containerConfig.Image is the image to use for the container.
//   - The containerConfig.Options are the functional options to pass to the [tc.Run] function. This argument is optional.
//
// Use [NewModuleConfig] or [NewContainerConfig] helper functions to create valid containerConfig objects.
func AddService[T tc.Container](cfg *fiber.Config, containerConfig Config[T]) (*ContainerService[T], error) {
	if cfg == nil {
		return nil, ErrNilConfig
	}

	if containerConfig.ServiceKey == "" {
		return nil, ErrEmptyServiceKey
	}

	if containerConfig.Image == "" {
		return nil, ErrImageEmpty
	}

	if containerConfig.Run == nil {
		return nil, ErrRunNil
	}

	k := buildKey(containerConfig.ServiceKey)

	c := &ContainerService[T]{
		key:  k,
		img:  containerConfig.Image,
		opts: containerConfig.Options,
		run:  containerConfig.Run,
	}

	cfg.Services = append(cfg.Services, c)

	return c, nil
}
