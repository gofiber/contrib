package pasetoware

import (
	"context"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// go test -run Test_Extractors_Missing
func Test_Extractors_Missing(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	// Add a route to test the missing param
	app.Get("/test", func(c fiber.Ctx) error {
		token, err := FromParam("token").Extract(c)
		require.Empty(t, token)
		require.Equal(t, ErrMissingToken, err)
		return nil
	})
	_, err := app.Test(newRequest(fiber.MethodGet, "/test"))
	require.NoError(t, err)

	ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)

	// Missing form
	token, err := FromForm("token").Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)

	// Missing query
	token, err = FromQuery("token").Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)

	// Missing header
	token, err = FromHeader("X-Token").Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)

	// Missing Auth header
	token, err = FromAuthHeader("Bearer").Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)

	// Missing cookie
	token, err = FromCookie("token").Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)
}

// newRequest creates a new *http.Request for Fiber's app.Test
func newRequest(method, target string) *http.Request {
	req, err := http.NewRequestWithContext(context.Background(), method, target, nil)
	if err != nil {
		panic(err)
	}
	return req
}

// go test -run Test_Extractors
func Test_Extractors(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	// FromParam
	app.Get("/test/:token", func(c fiber.Ctx) error {
		token, err := FromParam("token").Extract(c)
		require.NoError(t, err)
		require.Equal(t, "token_from_param", token)
		return nil
	})
	_, err := app.Test(newRequest(fiber.MethodGet, "/test/token_from_param"))
	require.NoError(t, err)

	// FromForm
	ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().Header.SetContentType(fiber.MIMEApplicationForm)
	ctx.Request().SetBodyString("token=token_from_form")
	token, err := FromForm("token").Extract(ctx)
	require.NoError(t, err)
	require.Equal(t, "token_from_form", token)

	// FromQuery
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().SetRequestURI("/?token=token_from_query")
	token, err = FromQuery("token").Extract(ctx)
	require.NoError(t, err)
	require.Equal(t, "token_from_query", token)

	// FromHeader
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().Header.Set("X-Token", "token_from_header")
	token, err = FromHeader("X-Token").Extract(ctx)
	require.NoError(t, err)
	require.Equal(t, "token_from_header", token)

	// FromAuthHeader
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().Header.Set(fiber.HeaderAuthorization, "Bearer token_from_auth_header")
	token, err = FromAuthHeader("Bearer").Extract(ctx)
	require.NoError(t, err)
	require.Equal(t, "token_from_auth_header", token)

	// FromCookie
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().Header.SetCookie("token", "token_from_cookie")
	token, err = FromCookie("token").Extract(ctx)
	require.NoError(t, err)
	require.Equal(t, "token_from_cookie", token)
}

// go test -run Test_Extractor_Chain
func Test_Extractor_Chain(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	// No extractors
	ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	token, err := Chain().Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)

	// First extractor succeeds
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().Header.Set("X-Token", "token_from_header")
	ctx.Request().SetRequestURI("/?token=token_from_query")
	token, err = Chain(FromHeader("X-Token"), FromQuery("token")).Extract(ctx)
	require.NoError(t, err)
	require.Equal(t, "token_from_header", token)

	// Second extractor succeeds
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().SetRequestURI("/?token=token_from_query")
	token, err = Chain(FromHeader("X-Token"), FromQuery("token")).Extract(ctx)
	require.NoError(t, err)
	require.Equal(t, "token_from_query", token)

	// All extractors fail, should return the last error
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	token, err = Chain(FromHeader("X-Token"), FromQuery("token")).Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)

	// All extractors find nothing (return empty string and nil error), should return ErrMissingToken
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	// This extractor will return "", nil
	dummyExtractor := Extractor{
		Extract: func(_ fiber.Ctx) (string, error) {
			return "", nil
		},
		Source: SourceCustom,
		Key:    "token",
	}
	token, err = Chain(dummyExtractor).Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)
}

// go test -run Test_Extractor_FromAuthHeader_EdgeCases
func Test_Extractor_FromAuthHeader_EdgeCases(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	// Test case: Authorization header exists but doesn't match the expected scheme
	ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().Header.Set(fiber.HeaderAuthorization, "Basic dXNlcjpwYXNz") // Basic auth instead of Bearer
	token, err := FromAuthHeader("Bearer").Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)

	// Test case: Authorization header exists but has wrong format
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().Header.Set(fiber.HeaderAuthorization, "Bearertoken") // Missing space after Bearer
	token, err = FromAuthHeader("Bearer").Extract(ctx)
	require.Empty(t, token)
	require.Equal(t, ErrMissingToken, err)

	// Test case: Authorization header exists but scheme doesn't match case-insensitively
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)
	ctx.Request().Header.Set(fiber.HeaderAuthorization, "bearer token") // lowercase bearer
	token, err = FromAuthHeader("Bearer").Extract(ctx)
	require.NoError(t, err)
	require.Equal(t, "token", token)
}
