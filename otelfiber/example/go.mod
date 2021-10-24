module github.com/gofiber/contrib/otelfiber/example

go 1.15

replace github.com/gofiber/contrib/otelfiber => ../

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/gofiber/contrib/otelfiber v0.23.0
	github.com/gofiber/fiber/v2 v2.14.0
	github.com/klauspost/compress v1.13.1 // indirect
	go.opentelemetry.io/otel v1.0.0-RC3
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.0.0-RC3
	go.opentelemetry.io/otel/sdk v1.0.0-RC3
	go.opentelemetry.io/otel/trace v1.0.0-RC3

)
