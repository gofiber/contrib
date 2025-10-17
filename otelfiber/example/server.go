package main

import (
	"context"
	"errors"
	"log"

	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/gofiber/fiber/v3"

	"github.com/gofiber/contrib/otelfiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	stdout "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"

	//"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("fiber-server")

func main() {
	tp := initTracer()
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down tracer provider: %v", err)
		}
	}()

	app := fiber.New()

	// customise span name
	//app.Use(otelfiber.Middleware(otelfiber.WithSpanNameFormatter(func(ctx fiber.Ctx) string {
	//	return fmt.Sprintf("%s - %s", ctx.Method(), ctx.Route().Path)
	//})))

	app.Use(otelfiber.Middleware())

	app.Get("/error", func(ctx fiber.Ctx) error {
		return errors.New("abc")
	})

	app.Get("/users/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		name := getUser(c, id)
		return c.JSON(fiber.Map{"id": id, name: name})
	})

	log.Fatal(app.Listen(":3000"))
}

func initTracer() *sdktrace.TracerProvider {
	exporter, err := stdout.New(stdout.WithPrettyPrint())
	//exporter, err := jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint("http://localhost:14268/api/traces")))
	if err != nil {
		log.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(
			resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceNameKey.String("my-service"),
			)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp
}

func getUser(ctx context.Context, id string) string {
	_, span := tracer.Start(ctx, "getUser", oteltrace.WithAttributes(attribute.String("id", id)))
	defer span.End()
	if id == "123" {
		return "otelfiber tester"
	}
	return "unknown"
}
