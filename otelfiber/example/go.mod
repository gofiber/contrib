module github.com/gofiber/contrib/otelfiber/example

go 1.18

replace github.com/gofiber/contrib/otelfiber => ../

require (
	github.com/gofiber/contrib/otelfiber v0.23.0
	github.com/gofiber/fiber/v2 v2.42.0
	go.opentelemetry.io/otel v1.12.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.12.0
	go.opentelemetry.io/otel/sdk v1.12.0
	go.opentelemetry.io/otel/trace v1.12.0

)

require (
	github.com/andybalholm/brotli v1.0.4 // indirect
	github.com/go-logr/logr v1.2.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/klauspost/compress v1.15.12 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.17 // indirect
	github.com/mattn/go-runewidth v0.0.14 // indirect
	github.com/philhofer/fwd v1.1.1 // indirect
	github.com/rivo/uniseg v0.4.3 // indirect
	github.com/savsgio/dictpool v0.0.0-20221023140959-7bf2e61cea94 // indirect
	github.com/savsgio/gotils v0.0.0-20220530130905-52f3993e8d6d // indirect
	github.com/tinylib/msgp v1.1.6 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.44.0 // indirect
	github.com/valyala/tcplisten v1.0.0 // indirect
	go.opentelemetry.io/contrib v1.13.0 // indirect
	go.opentelemetry.io/otel/metric v0.34.0 // indirect
	golang.org/x/sys v0.3.0 // indirect
)
