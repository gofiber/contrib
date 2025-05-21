package testcontainers_test

import (
	"fmt"
	"log"

	"github.com/gofiber/fiber/v3"

	"github.com/gofiber/contrib/testcontainers"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

func ExampleAddService_fromContainer() {
	cfg := &fiber.Config{}

	// Define the base key for the generic service.
	// The service returned by the [testcontainers.Add] function,
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
	fmt.Println(app.State().ServicesLen())

	srvs := app.State().Services()
	fmt.Println(len(srvs))

	nginxCtr := fiber.MustGetService[*testcontainers.ContainerService[*tc.DockerContainer]](app.State(), nginxSrv.Key())

	fmt.Println(nginxCtr.String())

	// Output:
	// 1
	// 1
	// nginx-generic (using testcontainers-go)
}

func ExampleAddService_fromModule() {
	cfg := &fiber.Config{}

	// Define the base keys for the module services.
	// The service returned by the [testcontainers.AddModule] function,
	// using the [ContainerService.Key] method,
	// concatenates the base key with the "using testcontainers-go" suffix.
	const (
		redisKey    = "redis-module"
		postgresKey = "postgres-module"
	)

	// Adding containers coming from the testcontainers-go modules,
	// in this case, a Redis and a Postgres container.

	redisModuleConfig := testcontainers.Config[*redis.RedisContainer]{
		ServiceKey: redisKey,
		Image:      "redis:latest",
		RunFn:      redis.Run,
	}
	redisSrv, err := testcontainers.AddService(cfg, redisModuleConfig)
	if err != nil {
		log.Println("error adding redis module:", err)
		return
	}

	postgresModuleConfig := testcontainers.Config[*postgres.PostgresContainer]{
		ServiceKey: postgresKey,
		Image:      "postgres:latest",
		RunFn:      postgres.Run,
	}
	postgresSrv, err := testcontainers.AddService(cfg, postgresModuleConfig)
	if err != nil {
		log.Println("error adding postgres module:", err)
		return
	}

	// Create a new Fiber app, using the provided configuration.
	app := fiber.New(*cfg)

	// Verify the number of services in the app's state.
	fmt.Println(app.State().ServicesLen())

	// Retrieve all services from the app's state.
	// This returns a slice of all the services registered in the app's state.
	srvs := app.State().Services()
	fmt.Println(len(srvs))

	// Retrieve the Redis container from the app's state using the key returned by the [ContainerService.Key] method.
	redisCtr := fiber.MustGetService[*testcontainers.ContainerService[*redis.RedisContainer]](app.State(), redisSrv.Key())

	// Retrieve the Postgres container from the app's state using the key returned by the [ContainerService.Key] method.
	postgresCtr := fiber.MustGetService[*testcontainers.ContainerService[*postgres.PostgresContainer]](app.State(), postgresSrv.Key())

	// Verify the string representation of the Redis and Postgres containers.
	fmt.Println(redisCtr.String())
	fmt.Println(postgresCtr.String())

	// Output:
	// 2
	// 2
	// redis-module (using testcontainers-go)
	// postgres-module (using testcontainers-go)
}
