module github.com/gofiber/contrib/otelfiber/example

go 1.16

replace github.com/gofiber/contrib/otelfiber => ../

require (
	github.com/gofiber/contrib/otelfiber v0.23.0
	github.com/gofiber/fiber/v2 v2.28.0
	go.opentelemetry.io/otel v1.4.1
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.4.0
	go.opentelemetry.io/otel/sdk v1.4.0
	go.opentelemetry.io/otel/trace v1.4.1

)
