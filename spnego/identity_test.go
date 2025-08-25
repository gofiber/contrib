package spnego

import (
	"net/http/httptest"
	"testing"

	fiberV2 "github.com/gofiber/fiber/v2"
	fiberV3 "github.com/gofiber/fiber/v3"
	"github.com/jcmturner/goidentity/v6"
	"github.com/stretchr/testify/require"
)

func TestGetAndSetAuthenticatedIdentityFromContextForFiberV2(t *testing.T) {
	app := fiberV2.New()
	id := goidentity.NewUser("test@TEST.LOCAL")
	app.Use("/identity", func(ctx *fiberV2.Ctx) error {
		SetAuthenticatedIdentityToContext(ctx, &id)
		return ctx.Next()
	})
	app.Get("/test", func(ctx *fiberV2.Ctx) error {
		_, ok := GetAuthenticatedIdentityFromContext(ctx)
		require.False(t, ok)
		return ctx.SendStatus(fiberV2.StatusOK)
	})
	app.Get("/identity/test", func(ctx *fiberV2.Ctx) error {
		user, ok := GetAuthenticatedIdentityFromContext(ctx)
		require.True(t, ok)
		require.Equal(t, id.UserName(), user.UserName())
		require.Equal(t, id.Domain(), user.Domain())
		return ctx.SendStatus(fiberV2.StatusOK)
	})
	app.Test(httptest.NewRequest("GET", "/test", nil))
	app.Test(httptest.NewRequest("GET", "/identity/test", nil))
}

func TestGetAndSetAuthenticatedIdentityFromContextForFiberV3(t *testing.T) {
	app := fiberV3.New()
	id := goidentity.NewUser("test@TEST.LOCAL")
	app.Use("/identity", func(ctx fiberV3.Ctx) error {
		SetAuthenticatedIdentityToContext(ctx, &id)
		return ctx.Next()
	})
	app.Get("/test", func(ctx fiberV3.Ctx) error {
		_, ok := GetAuthenticatedIdentityFromContext(ctx)
		require.False(t, ok)
		return ctx.SendStatus(fiberV3.StatusOK)
	})
	app.Get("/identity/test", func(ctx fiberV3.Ctx) error {
		user, ok := GetAuthenticatedIdentityFromContext(ctx)
		require.True(t, ok)
		require.Equal(t, id.UserName(), user.UserName())
		require.Equal(t, id.Domain(), user.Domain())
		return ctx.SendStatus(fiberV3.StatusOK)
	})
	app.Test(httptest.NewRequest("GET", "/test", nil))
	app.Test(httptest.NewRequest("GET", "/identity/test", nil))
}
