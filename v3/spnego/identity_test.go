package spnego

import (
	"net/http/httptest"
	"testing"

	fiberV3 "github.com/gofiber/fiber/v3"
	"github.com/jcmturner/goidentity/v6"
	"github.com/stretchr/testify/require"
)

func TestGetAndSetAuthenticatedIdentityFromContextForFiberV3(t *testing.T) {
	app := fiberV3.New()
	// The domain is set explicitly: NewUser stores its whole argument as the
	// username and leaves the domain empty, so asserting against id.Domain()
	// would compare "" to "" and hold however Locals behaved.
	id := goidentity.NewUser("test")
	id.SetDomain("TEST.LOCAL")
	app.Use("/identity", func(ctx fiberV3.Ctx) error {
		SetAuthenticatedIdentityToContext(ctx, &id)
		return ctx.Next()
	})
	// app.Test serves on its own goroutine, and require calls runtime.Goexit,
	// which is invalid off the test goroutine — it would kill the handler
	// mid-request and surface as a transport error instead of the assertion
	// that failed. The handlers record what they saw; the assertions run below.
	var (
		plainFound bool
		withID     goidentity.Identity
		withIDOK   bool
	)
	app.Get("/test", func(ctx fiberV3.Ctx) error {
		_, plainFound = GetAuthenticatedIdentityFromContext(ctx)
		return ctx.SendStatus(fiberV3.StatusOK)
	})
	app.Get("/identity/test", func(ctx fiberV3.Ctx) error {
		withID, withIDOK = GetAuthenticatedIdentityFromContext(ctx)
		return ctx.SendStatus(fiberV3.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/test", nil))
	require.NoError(t, err)
	require.Equal(t, fiberV3.StatusOK, resp.StatusCode)
	require.False(t, plainFound, "no identity is set on a route outside the middleware")

	resp, err = app.Test(httptest.NewRequest("GET", "/identity/test", nil))
	require.NoError(t, err)
	require.Equal(t, fiberV3.StatusOK, resp.StatusCode)
	require.True(t, withIDOK)
	require.Equal(t, "test", withID.UserName())
	require.Equal(t, "TEST.LOCAL", withID.Domain())
}
