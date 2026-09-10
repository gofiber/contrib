package jwtware_test

import (
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"

	jwtware "github.com/gofiber/contrib/v3/jwt"
)

// benchApp builds a Fiber app guarded by the JWT middleware and returns its
// fasthttp handler together with a request context carrying the given header.
//
// Reset the response between iterations, the way a served request starts from a
// clean one: a handler that sets a response header would otherwise only set it
// on the first iteration.
func benchApp(b *testing.B, cfg jwtware.Config, header, value string) (fasthttp.RequestHandler, *fasthttp.RequestCtx) {
	b.Helper()

	app := fiber.New()
	app.Use(jwtware.New(cfg))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusTeapot)
	})

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI("/")
	if header != "" {
		fctx.Request.Header.Set(header, value)
	}

	return app.Handler(), fctx
}

// Benchmark_Middleware_JWT_Baseline drives the same Fiber app without the JWT
// middleware attached. Subtract it from the numbers below to read the
// middleware's own cost.
func Benchmark_Middleware_JWT_Baseline(b *testing.B) {
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusTeapot)
	})

	h := app.Handler()
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI("/")

	b.ReportAllocs()

	for b.Loop() {
		fctx.Response.Reset()
		h(fctx)
	}

	require.Equal(b, fiber.StatusTeapot, fctx.Response.Header.StatusCode())
}

// go test -run=^$ -bench=Benchmark_Middleware_JWT -benchmem -count=6
func Benchmark_Middleware_JWT_MapClaims(b *testing.B) {
	h, fctx := benchApp(b, jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
	}, fiber.HeaderAuthorization, "Bearer "+hamac[0].Token)

	b.ReportAllocs()

	for b.Loop() {
		fctx.Response.Reset()
		h(fctx)
	}

	require.Equal(b, fiber.StatusTeapot, fctx.Response.Header.StatusCode())
}

func Benchmark_Middleware_JWT_CustomClaims(b *testing.B) {
	h, fctx := benchApp(b, jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		Claims:     &customClaims{},
	}, fiber.HeaderAuthorization, "Bearer "+hamac[0].Token)

	b.ReportAllocs()

	for b.Loop() {
		fctx.Response.Reset()
		h(fctx)
	}

	require.Equal(b, fiber.StatusTeapot, fctx.Response.Header.StatusCode())
}

func Benchmark_Middleware_JWT_ParserOptions(b *testing.B) {
	h, fctx := benchApp(b, jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		ParserOptions: []jwt.ParserOption{
			jwt.WithIssuedAt(),
			jwt.WithLeeway(0),
			jwt.WithValidMethods([]string{jwtware.HS256}),
		},
	}, fiber.HeaderAuthorization, "Bearer "+hamac[0].Token)

	b.ReportAllocs()

	for b.Loop() {
		fctx.Response.Reset()
		h(fctx)
	}

	require.Equal(b, fiber.StatusTeapot, fctx.Response.Header.StatusCode())
}

func Benchmark_Middleware_JWT_MissingToken(b *testing.B) {
	h, fctx := benchApp(b, jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
	}, "", "")

	b.ReportAllocs()

	for b.Loop() {
		fctx.Response.Reset()
		h(fctx)
	}

	require.Equal(b, fiber.StatusBadRequest, fctx.Response.Header.StatusCode())
}
