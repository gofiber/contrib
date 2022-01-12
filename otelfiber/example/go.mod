module github.com/gofiber/contrib/otelfiber/example

go 1.16

replace github.com/gofiber/contrib/otelfiber => ../

require (
	github.com/andybalholm/brotli v1.0.4 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/gofiber/contrib/otelfiber v0.23.0
	github.com/gofiber/fiber/v2 v2.24.0
	github.com/klauspost/compress v1.14.1 // indirect
	github.com/valyala/fasthttp v1.32.0 // indirect
	go.opentelemetry.io/contrib v1.3.0 // indirect
	go.opentelemetry.io/otel v1.3.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.3.0
	go.opentelemetry.io/otel/sdk v1.3.0
	go.opentelemetry.io/otel/trace v1.3.0
	golang.org/x/sys v0.0.0-20220111092808-5a964db01320 // indirect

)
