package jwtware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	jwtware "github.com/gofiber/contrib/v3/jwt"
)

// signWith builds a token signed with the shared HMAC test key, applying the
// given mutations to its header first.
func signWith(tb testing.TB, claims jwt.Claims, header map[string]any) string {
	tb.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	for name, value := range header {
		token.Header[name] = value
	}
	signed, err := token.SignedString([]byte(defaultSigningKey))
	require.NoError(tb, err)
	return signed
}

// protectedApp guards a single route with the middleware and echoes the token's
// "name" claim, so a test can tell whose token was accepted.
func protectedApp(cfg jwtware.Config) *fiber.App {
	app := fiber.New()
	app.Use(jwtware.New(cfg))
	app.Get("/ok", func(c fiber.Ctx) error {
		token := jwtware.FromContext(c)
		if token == nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		name, _ := token.Claims.(jwt.MapClaims)["name"].(string)
		return c.SendString("OK " + name)
	})
	return app
}

func doGet(t *testing.T, app *fiber.App, authorization string) *http.Response {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	if authorization != "" {
		req.Header.Set(fiber.HeaderAuthorization, authorization)
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	return resp
}

// TestCriticalHeaders covers RFC 7515 Section 4.1.11: a JWS that marks header
// parameters critical may only be accepted when every one of them is understood.
func TestCriticalHeaders(t *testing.T) {
	t.Parallel()

	claims := jwt.MapClaims{"name": "John Doe"}

	tests := []struct {
		header map[string]any
		name   string
		known  []string
		status int
	}{
		{
			name:   "unknown extension is rejected",
			header: map[string]any{"crit": []string{"exp_ext"}, "exp_ext": "value"},
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "declared extension is accepted",
			header: map[string]any{"crit": []string{"exp_ext"}, "exp_ext": "value"},
			known:  []string{"exp_ext"},
			status: fiber.StatusOK,
		},
		{
			name:   "declaring one extension does not cover another",
			header: map[string]any{"crit": []string{"exp_ext", "other"}, "exp_ext": "v", "other": "v"},
			known:  []string{"exp_ext"},
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "registered header parameter may not be critical",
			header: map[string]any{"crit": []string{"alg"}},
			known:  []string{"alg"},
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "name absent from the header is rejected",
			header: map[string]any{"crit": []string{"exp_ext"}},
			known:  []string{"exp_ext"},
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "duplicate name is rejected",
			header: map[string]any{"crit": []string{"exp_ext", "exp_ext"}, "exp_ext": "v"},
			known:  []string{"exp_ext"},
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "empty list is rejected",
			header: map[string]any{"crit": []string{}},
			known:  []string{"exp_ext"},
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "non-array value is rejected",
			header: map[string]any{"crit": "exp_ext", "exp_ext": "v"},
			known:  []string{"exp_ext"},
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "non-string entry is rejected",
			header: map[string]any{"crit": []any{1}, "1": "v"},
			known:  []string{"exp_ext"},
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "token without crit is untouched",
			header: nil,
			status: fiber.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			app := protectedApp(jwtware.Config{
				SigningKey:           jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
				KnownCriticalHeaders: test.known,
			})

			resp := doGet(t, app, "Bearer "+signWith(t, claims, test.header))
			require.Equal(t, test.status, resp.StatusCode)
		})
	}
}

// TestCriticalHeaderWrapsCustomKeyfunc makes sure the "crit" check is not
// something a caller loses by supplying a key function of their own.
func TestCriticalHeaderWrapsCustomKeyfunc(t *testing.T) {
	t.Parallel()

	app := protectedApp(jwtware.Config{KeyFunc: customKeyfunc()})

	header := map[string]any{"crit": []string{"exp_ext"}, "exp_ext": "value"}
	resp := doGet(t, app, "Bearer "+signWith(t, jwt.MapClaims{"name": "John Doe"}, header))
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

// TestChallengeOnRejection covers RFC 9110 Section 15.5.2 and RFC 6750
// Section 3: a rejected request has to be told how to authenticate, and why the
// credentials it sent were refused.
func TestChallengeOnRejection(t *testing.T) {
	t.Parallel()

	expired := signWith(t, jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
	}, nil)

	tests := []struct {
		name      string
		authValue string
		challenge string
		status    int
	}{
		{
			name:      "missing credentials",
			authValue: "",
			status:    fiber.StatusBadRequest,
			challenge: `Bearer realm="Restricted", error="invalid_request", error_description="The access token is missing or malformed"`,
		},
		{
			name:      "malformed credentials",
			authValue: "Bearer not-a-jwt",
			status:    fiber.StatusUnauthorized,
			challenge: `Bearer realm="Restricted", error="invalid_token", error_description="The access token is malformed"`,
		},
		{
			name:      "expired token",
			authValue: "Bearer " + expired,
			status:    fiber.StatusUnauthorized,
			challenge: `Bearer realm="Restricted", error="invalid_token", error_description="The access token expired"`,
		},
		{
			name:      "signature from another key",
			authValue: "Bearer " + hamac[1].Token,
			status:    fiber.StatusUnauthorized,
			challenge: `Bearer realm="Restricted", error="invalid_token", error_description="The access token signature is invalid"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			app := protectedApp(jwtware.Config{
				SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
			})

			resp := doGet(t, app, test.authValue)
			require.Equal(t, test.status, resp.StatusCode)
			require.Equal(t, test.challenge, resp.Header.Get(fiber.HeaderWWWAuthenticate))
		})
	}
}

func TestChallengeIsAbsentOnSuccess(t *testing.T) {
	t.Parallel()

	app := protectedApp(jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
	})

	resp := doGet(t, app, "Bearer "+hamac[0].Token)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Empty(t, resp.Header.Get(fiber.HeaderWWWAuthenticate))
}

func TestChallengeUsesConfiguredRealmAndScheme(t *testing.T) {
	t.Parallel()

	app := protectedApp(jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		Realm:      "api",
		Extractor:  extractors.FromAuthHeader("JWT"),
	})

	resp := doGet(t, app, "JWT not-a-jwt")
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	// The RFC 6750 error parameters are defined for the bearer scheme only.
	require.Equal(t, `JWT realm="api"`, resp.Header.Get(fiber.HeaderWWWAuthenticate))
}

// TestChallengeForNonHeaderExtractor checks that a token read from somewhere
// other than the Authorization header still produces the challenge RFC 9110
// Section 15.5.2 requires.
func TestChallengeForNonHeaderExtractor(t *testing.T) {
	t.Parallel()

	app := protectedApp(jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		Extractor:  extractors.FromCookie("token"),
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.AddCookie(&http.Cookie{Name: "token", Value: "not-a-jwt"})
	resp, err := app.Test(req)
	require.NoError(t, err)

	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	require.Equal(t,
		`Bearer realm="Restricted", error="invalid_token", error_description="The access token is malformed"`,
		resp.Header.Get(fiber.HeaderWWWAuthenticate))
}

func TestChallengeFromCustomErrorHandlerIsKept(t *testing.T) {
	t.Parallel()

	app := protectedApp(jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			c.Set(fiber.HeaderWWWAuthenticate, `Bearer realm="custom"`)
			return c.SendStatus(fiber.StatusUnauthorized)
		},
	})

	resp := doGet(t, app, "Bearer not-a-jwt")
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, `Bearer realm="custom"`, resp.Header.Get(fiber.HeaderWWWAuthenticate))
}

// TestChallengeForDeferredStatus covers an error handler that leaves the status
// to Fiber by returning a *fiber.Error instead of writing a response.
func TestChallengeForDeferredStatus(t *testing.T) {
	t.Parallel()

	app := protectedApp(jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		ErrorHandler: func(_ fiber.Ctx, _ error) error {
			return fiber.ErrUnauthorized
		},
	})

	resp := doGet(t, app, "Bearer not-a-jwt")
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	require.Equal(t,
		`Bearer realm="Restricted", error="invalid_token", error_description="The access token is malformed"`,
		resp.Header.Get(fiber.HeaderWWWAuthenticate))
}

func TestNoChallengeOnUnrelatedStatus(t *testing.T) {
	t.Parallel()

	app := protectedApp(jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			return c.SendStatus(fiber.StatusTeapot)
		},
	})

	resp := doGet(t, app, "Bearer not-a-jwt")
	require.Equal(t, fiber.StatusTeapot, resp.StatusCode)
	require.Empty(t, resp.Header.Get(fiber.HeaderWWWAuthenticate))
}

// TestAlgorithmIsPinnedToConfiguration covers RFC 8725 Section 3.1: the
// algorithm comes from the configuration, never from the token.
func TestAlgorithmIsPinnedToConfiguration(t *testing.T) {
	t.Parallel()

	t.Run("single key", func(t *testing.T) {
		t.Parallel()

		app := protectedApp(jwtware.Config{
			SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS512, Key: []byte(defaultSigningKey)},
		})

		// hamac[0] is signed with the same key, but with HS256.
		resp := doGet(t, app, "Bearer "+hamac[0].Token)
		require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("key set", func(t *testing.T) {
		t.Parallel()

		app := protectedApp(jwtware.Config{
			SigningKeys: map[string]jwtware.SigningKey{
				"one": {JWTAlg: jwtware.HS512, Key: []byte(defaultSigningKey)},
				"two": {JWTAlg: jwtware.HS384, Key: []byte(defaultSigningKey)},
			},
		})

		accepted := signWith(t, jwt.MapClaims{"name": "John Doe"}, nil)
		resp := doGet(t, app, "Bearer "+accepted)
		require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, "HS256 is not one of the configured algorithms")
	})

	t.Run("caller options win", func(t *testing.T) {
		t.Parallel()

		app := protectedApp(jwtware.Config{
			SigningKey:    jwtware.SigningKey{Key: []byte(defaultSigningKey)},
			ParserOptions: []jwt.ParserOption{jwt.WithValidMethods([]string{jwtware.HS512})},
		})

		resp := doGet(t, app, "Bearer "+hamac[0].Token)
		require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	})
}

func TestUnexpectedAlgorithmIsReported(t *testing.T) {
	t.Parallel()

	var handled error
	app := protectedApp(jwtware.Config{
		// Without a pinned algorithm the parser accepts the token and the key
		// function is the one that has to refuse it.
		KeyFunc: func(_ *jwt.Token) (any, error) {
			return nil, jwtware.ErrJWTAlg
		},
		ErrorHandler: func(c fiber.Ctx, err error) error {
			handled = err
			return c.SendStatus(fiber.StatusUnauthorized)
		},
	})

	resp := doGet(t, app, "Bearer "+hamac[0].Token)
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	require.ErrorIs(t, handled, jwtware.ErrJWTAlg)
}

// TestClaimsAreNotSharedBetweenRequests makes sure the configured Claims value
// only ever supplies the type. Sharing the value itself would leak one request's
// claims into the next.
func TestClaimsAreNotSharedBetweenRequests(t *testing.T) {
	t.Parallel()

	configured := &customClaims{}

	app := fiber.New()
	app.Use(jwtware.New(jwtware.Config{
		SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		Claims:     configured,
	}))
	app.Get("/ok", func(c fiber.Ctx) error {
		claims, ok := jwtware.FromContext(c).Claims.(*customClaims)
		require.True(t, ok)
		return c.SendString(claims.Name)
	})

	for _, name := range []string{"first", "second"} {
		token := signWith(t, jwt.MapClaims{"name": name}, nil)

		req := httptest.NewRequest(http.MethodGet, "/ok", nil)
		req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, name, string(body))
	}

	require.Empty(t, configured.Name, "the configured claims value must not be written to")
}

// TestQueryTokenResponseIsPrivate covers RFC 6750 Section 2.3: a token in the
// query string travels in the URL, so a shared cache must not keep the response.
func TestQueryTokenResponseIsPrivate(t *testing.T) {
	t.Parallel()

	newApp := func(handler fiber.Handler) *fiber.App {
		app := fiber.New()
		app.Use(jwtware.New(jwtware.Config{
			SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
			Extractor:  extractors.FromQuery("token"),
		}))
		app.Get("/ok", handler)
		return app
	}

	get := func(t *testing.T, app *fiber.App, target string) *http.Response {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, target, nil))
		require.NoError(t, err)
		return resp
	}

	okHandler := func(c fiber.Ctx) error { return c.SendString("OK") }

	t.Run("success is marked private", func(t *testing.T) {
		t.Parallel()

		resp := get(t, newApp(okHandler), "/ok?token="+hamac[0].Token)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		require.Equal(t, "private", resp.Header.Get(fiber.HeaderCacheControl))
	})

	t.Run("handler keeps its own policy", func(t *testing.T) {
		t.Parallel()

		app := newApp(func(c fiber.Ctx) error {
			c.Set(fiber.HeaderCacheControl, "no-store")
			return c.SendString("OK")
		})

		resp := get(t, app, "/ok?token="+hamac[0].Token)
		require.Equal(t, "no-store", resp.Header.Get(fiber.HeaderCacheControl))
	})

	t.Run("rejection is not marked", func(t *testing.T) {
		t.Parallel()

		resp := get(t, newApp(okHandler), "/ok?token=not-a-jwt")
		require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
		require.Empty(t, resp.Header.Get(fiber.HeaderCacheControl))
	})

	t.Run("header tokens are left alone", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()
		app.Use(jwtware.New(jwtware.Config{
			SigningKey: jwtware.SigningKey{JWTAlg: jwtware.HS256, Key: []byte(defaultSigningKey)},
		}))
		app.Get("/ok", okHandler)

		resp := doGet(t, app, "Bearer "+hamac[0].Token)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		require.Empty(t, resp.Header.Get(fiber.HeaderCacheControl))
	})
}
