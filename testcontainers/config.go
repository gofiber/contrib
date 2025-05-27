package testcontainers

import (
	"context"

	"github.com/testcontainers/testcontainers-go"
)

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

	return Config[T]{
		ServiceKey: serviceKey,
		Image:      img,
		Run:        run,
		Options:    opts,
	}
}

// NewContainerConfig creates a new container service config for a generic container type,
// not created by a Testcontainers module. So this function best used in combination with
// the [AddService] function to add a custom container to the Fiber app's state.
//
// - The serviceKey is the key used to identify the service in the Fiber app's state.
// - The img is the image name to use for the container.
// - The opts are the functional options to pass to the [testcontainers.Run] function. This argument is optional.
//
// This function uses the [testcontainers.Run] function as the run function.
func NewContainerConfig(serviceKey string, img string, opts ...testcontainers.ContainerCustomizer) Config[*testcontainers.DockerContainer] {
	return NewModuleConfig(serviceKey, img, testcontainers.Run, opts...)
}
