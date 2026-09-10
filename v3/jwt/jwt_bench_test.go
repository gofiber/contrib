package jwtware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MicahParks/keyfunc/v2"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"

	jwtware "github.com/gofiber/contrib/v3/jwt"
)

// The benchmarks below all drive a one-route Fiber app through its fasthttp
// handler, so a result is the cost of a whole request: routing, the middleware,
// and a handler that does nothing but set a status. Benchmark_Middleware_JWT_Baseline
// is that app without the middleware, and subtracting it leaves the
// middleware's own cost.
//
// go test -run=^$ -bench=Benchmark_Middleware_JWT -benchmem -count=6

// benchHandler builds the guarded app and returns its fasthttp handler.
func benchHandler(b *testing.B, cfg jwtware.Config) fasthttp.RequestHandler {
	b.Helper()

	return benchHandlerFunc(b, cfg, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusTeapot)
	})
}

// benchHandlerFunc is benchHandler with a route handler of the caller's choice.
func benchHandlerFunc(b *testing.B, cfg jwtware.Config, handler fiber.Handler) fasthttp.RequestHandler {
	b.Helper()

	app := fiber.New()
	app.Use(jwtware.New(cfg))
	app.Get("/", handler)
	return app.Handler()
}

// benchCtx builds the request the benchmark replays.
func benchCtx(setup ...func(*fasthttp.RequestCtx)) *fasthttp.RequestCtx {
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI("/")
	for _, apply := range setup {
		apply(fctx)
	}
	return fctx
}

func withBearer(token string) func(*fasthttp.RequestCtx) {
	return func(fctx *fasthttp.RequestCtx) {
		fctx.Request.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	}
}

func withCookie(name, token string) func(*fasthttp.RequestCtx) {
	return func(fctx *fasthttp.RequestCtx) {
		fctx.Request.Header.SetCookie(name, token)
	}
}

func withQuery(param, token string) func(*fasthttp.RequestCtx) {
	return func(fctx *fasthttp.RequestCtx) {
		fctx.Request.SetRequestURI("/?" + param + "=" + token)
	}
}

// runBench replays the request and checks the middleware answered as the
// benchmark expects, so a benchmark cannot quietly measure the wrong path.
//
// The response is reset between iterations, the way a served request starts
// from a clean one: a rejection that sets a challenge header would otherwise
// only set it on the first iteration.
func runBench(b *testing.B, h fasthttp.RequestHandler, fctx *fasthttp.RequestCtx, want int) {
	b.Helper()
	b.ReportAllocs()

	for b.Loop() {
		fctx.Response.Reset()
		h(fctx)
	}

	require.Equal(b, want, fctx.Response.Header.StatusCode())
}

// benchClaims mirrors the claim set of the shared test tokens, so a benchmark
// that has to sign its own is still decoding the same amount of JSON as the
// ones that do not.
var benchClaims = jwt.MapClaims{"sub": "1234567890", "name": "John Doe", "iat": 1516239022}

// hmacKey is the configuration the symmetric benchmarks share.
func hmacKey(alg string) jwtware.Config {
	return jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: alg, Key: []byte(defaultSigningKey)},
	}
}

// publicKeys parses the test JWK Set once so the asymmetric algorithms can be
// measured without a key set server in the loop.
func publicKeys(b *testing.B) map[string]any {
	b.Helper()

	set, err := keyfunc.NewJSON([]byte(defaultKeySet))
	require.NoError(b, err)
	return set.ReadOnlyKeys()
}

// tamper invalidates a token's signature without disturbing its encoding.
func tamper(token string) string {
	replacement := byte('A')
	if token[len(token)-1] == replacement {
		replacement = 'B'
	}
	return token[:len(token)-1] + string(replacement)
}

// Benchmark_Middleware_JWT_Baseline drives the same Fiber app without the JWT
// middleware attached. Subtract it from the numbers below to read the
// middleware's own cost.
func Benchmark_Middleware_JWT_Baseline(b *testing.B) {
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusTeapot)
	})

	runBench(b, app.Handler(), benchCtx(), fiber.StatusTeapot)
}

func Benchmark_Middleware_JWT_MapClaims(b *testing.B) {
	h := benchHandler(b, hmacKey(jwtware.HS256))
	runBench(b, h, benchCtx(withBearer(hamac[0].Token)), fiber.StatusTeapot)
}

func Benchmark_Middleware_JWT_CustomClaims(b *testing.B) {
	cfg := hmacKey(jwtware.HS256)
	cfg.Claims = &customClaims{}

	h := benchHandler(b, cfg)
	runBench(b, h, benchCtx(withBearer(hamac[0].Token)), fiber.StatusTeapot)
}

func Benchmark_Middleware_JWT_ParserOptions(b *testing.B) {
	cfg := hmacKey(jwtware.HS256)
	cfg.ParserOptions = []jwt.ParserOption{
		jwt.WithIssuedAt(),
		jwt.WithLeeway(0),
		jwt.WithValidMethods([]string{jwtware.HS256}),
	}

	h := benchHandler(b, cfg)
	runBench(b, h, benchCtx(withBearer(hamac[0].Token)), fiber.StatusTeapot)
}

func Benchmark_Middleware_JWT_MissingToken(b *testing.B) {
	h := benchHandler(b, hmacKey(jwtware.HS256))
	runBench(b, h, benchCtx(), fiber.StatusBadRequest)
}

// Benchmark_Middleware_JWT_Algorithms measures each signing algorithm on the
// same path. The spread between them is the verification cost, which dominates
// a request.
func Benchmark_Middleware_JWT_Algorithms(b *testing.B) {
	keys := publicKeys(b)

	tests := []struct {
		key   any
		name  string
		alg   string
		token string
	}{
		{name: "HS256", alg: jwtware.HS256, key: []byte(defaultSigningKey), token: hamac[0].Token},
		{name: "HS384", alg: jwtware.HS384, key: []byte(defaultSigningKey), token: hamac[1].Token},
		{name: "HS512", alg: jwtware.HS512, key: []byte(defaultSigningKey), token: hamac[2].Token},
		{name: "RS256", alg: jwtware.RS256, key: keys["gofiber-rsa"], token: rsa[0].Token},
		{name: "ES256", alg: jwtware.ES256, key: keys["gofiber-p-256"], token: ecdsa[0].Token},
	}

	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			h := benchHandler(b, jwtware.Config{
				SigningKey: jwtware.SigningKey{JWTAlg: test.alg, Key: test.key},
			})
			runBench(b, h, benchCtx(withBearer(test.token)), fiber.StatusTeapot)
		})
	}
}

// Benchmark_Middleware_JWT_Extractors measures where the token is read from.
// The query case also carries the RFC 6750 Section 2.3 cache directive the
// middleware adds to those responses.
func Benchmark_Middleware_JWT_Extractors(b *testing.B) {
	token := hamac[0].Token

	tests := []struct {
		setup     func(*fasthttp.RequestCtx)
		extractor extractors.Extractor
		name      string
	}{
		{name: "AuthHeader", extractor: extractors.FromAuthHeader("Bearer"), setup: withBearer(token)},
		{name: "Cookie", extractor: extractors.FromCookie("token"), setup: withCookie("token", token)},
		{name: "Query", extractor: extractors.FromQuery("token"), setup: withQuery("token", token)},
	}

	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			cfg := hmacKey(jwtware.HS256)
			cfg.Extractor = test.extractor

			h := benchHandler(b, cfg)
			runBench(b, h, benchCtx(test.setup), fiber.StatusTeapot)
		})
	}
}

// Benchmark_Middleware_JWT_KeySource measures how the verification key is
// found: straight from the configuration, by "kid" out of a key set, from a
// caller's key function, or from a cached JWK Set.
func Benchmark_Middleware_JWT_KeySource(b *testing.B) {
	b.Run("SigningKey", func(b *testing.B) {
		h := benchHandler(b, hmacKey(jwtware.HS256))
		runBench(b, h, benchCtx(withBearer(hamac[0].Token)), fiber.StatusTeapot)
	})

	b.Run("SigningKeys", func(b *testing.B) {
		h := benchHandler(b, jwtware.Config{
			SigningKeys: map[string]jwtware.SigningKey{
				"one": {JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
				"two": {JWTAlg: jwtware.HS256, Key: []byte("another-secret")},
			},
		})

		token := signWith(b, benchClaims, map[string]any{"kid": "one"})
		runBench(b, h, benchCtx(withBearer(token)), fiber.StatusTeapot)
	})

	b.Run("KeyFunc", func(b *testing.B) {
		h := benchHandler(b, jwtware.Config{KeyFunc: customKeyfunc()})
		runBench(b, h, benchCtx(withBearer(hamac[0].Token)), fiber.StatusTeapot)
	})

	b.Run("JWKSet", func(b *testing.B) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(defaultKeySet))
		}))
		b.Cleanup(server.Close)

		// The set is fetched when the middleware is built, so the loop measures
		// the cached lookup rather than the request to the server.
		h := benchHandler(b, jwtware.Config{JWKSetURLs: []string{server.URL}})
		runBench(b, h, benchCtx(withBearer(rsa[0].Token)), fiber.StatusTeapot)
	})
}

// Benchmark_Middleware_JWT_Rejected measures the paths a request that fails
// authentication takes, each of which also writes the challenge header.
func Benchmark_Middleware_JWT_Rejected(b *testing.B) {
	tests := []struct {
		name   string
		token  string
		status int
	}{
		{name: "Malformed", token: "not-a-jwt", status: fiber.StatusUnauthorized},
		{name: "Expired", token: expiredToken.Token, status: fiber.StatusUnauthorized},
		{name: "BadSignature", token: tamper(hamac[0].Token), status: fiber.StatusUnauthorized},
		// Refused by the parser before a key is looked up: hamac[1] is HS384.
		{name: "UnexpectedAlgorithm", token: hamac[1].Token, status: fiber.StatusUnauthorized},
	}

	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			h := benchHandler(b, hmacKey(jwtware.HS256))
			runBench(b, h, benchCtx(withBearer(test.token)), test.status)
		})
	}
}

// Benchmark_Middleware_JWT_CriticalHeader measures the RFC 7515 Section 4.1.11
// check on the tokens that reach it. Every other benchmark here carries no
// "crit" header, which is the case that only costs a map lookup.
func Benchmark_Middleware_JWT_CriticalHeader(b *testing.B) {
	header := map[string]any{"crit": []string{"exp_ext"}, "exp_ext": "value"}
	token := signWith(b, benchClaims, header)

	b.Run("Understood", func(b *testing.B) {
		cfg := hmacKey(jwtware.HS256)
		cfg.KnownCriticalHeaders = []string{"exp_ext"}

		h := benchHandler(b, cfg)
		runBench(b, h, benchCtx(withBearer(token)), fiber.StatusTeapot)
	})

	b.Run("Rejected", func(b *testing.B) {
		h := benchHandler(b, hmacKey(jwtware.HS256))
		runBench(b, h, benchCtx(withBearer(token)), fiber.StatusUnauthorized)
	})
}

// Benchmark_Middleware_JWT_Skipped measures a request the Next filter waves
// through, which is the whole cost of having the middleware on a route it does
// not guard.
func Benchmark_Middleware_JWT_Skipped(b *testing.B) {
	cfg := hmacKey(jwtware.HS256)
	cfg.Next = func(fiber.Ctx) bool { return true }

	h := benchHandler(b, cfg)
	runBench(b, h, benchCtx(), fiber.StatusTeapot)
}

// Benchmark_Middleware_JWT_TokenProcessor measures the hook that rewrites the
// extracted token, here with the cheapest possible processor so the result is
// the cost of the hook itself.
func Benchmark_Middleware_JWT_TokenProcessor(b *testing.B) {
	cfg := hmacKey(jwtware.HS256)
	cfg.TokenProcessorFunc = func(token string) (string, error) { return token, nil }

	h := benchHandler(b, cfg)
	runBench(b, h, benchCtx(withBearer(hamac[0].Token)), fiber.StatusTeapot)
}

// Benchmark_Middleware_JWT_FromContext measures a handler that reads the token
// back out of the context, which is what a real one does.
func Benchmark_Middleware_JWT_FromContext(b *testing.B) {
	h := benchHandlerFunc(b, hmacKey(jwtware.HS256), func(c fiber.Ctx) error {
		if jwtware.FromContext(c) == nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		return c.SendStatus(fiber.StatusTeapot)
	})

	runBench(b, h, benchCtx(withBearer(hamac[0].Token)), fiber.StatusTeapot)
}
