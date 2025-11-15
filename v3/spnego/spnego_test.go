package spnego

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestNewSpnegoKrb5AuthenticateMiddleware(t *testing.T) {
	t.Run("test for keytab lookup function not set", func(t *testing.T) {
		_, err := New(Config{})
		require.ErrorIs(t, err, ErrConfigInvalidOfKeytabLookupFunctionRequired)
	})
	t.Run("test for keytab lookup failed", func(t *testing.T) {
		middleware, err := New(Config{
			KeytabLookup: func() (*keytab.Keytab, error) {
				return nil, errors.New("mock keytab lookup error")
			},
		})
		require.NoError(t, err)
		app := fiber.New()
		app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
			return c.SendString("authenticated")
		})
		handler := app.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fiber.MethodGet)
		ctx.Request.SetRequestURI("/authenticate")
		handler(ctx)
		require.Equal(t, http.StatusInternalServerError, ctx.Response.StatusCode())
		require.Equal(t, fmt.Sprintf("%s: mock keytab lookup error", ErrLookupKeytabFailed), string(ctx.Response.Body()))
	})
	t.Run("test for keytab lookup function is set", func(t *testing.T) {
		tm := time.Now()
		filename1 := path.Join(t.TempDir(), "temp-sso1.keytab")
		filename2 := path.Join(t.TempDir(), "temp-sso2.keytab")
		_, clean1, err1 := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso1.example.com"),
			utils.WithRealm("EXAMPLE.LOCAL"),
			utils.WithFilename(filename1),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
		)
		require.NoError(t, err1)
		t.Cleanup(clean1)
		_, clean2, err2 := utils.NewMockKeytab(
			utils.WithPrincipal("HTTP/sso2.example.com"),
			utils.WithRealm("EXAMPLE.LOCAL"),
			utils.WithFilename(filename2),
			utils.WithPairs(utils.EncryptTypePair{
				Version:     2,
				EncryptType: 18,
				CreateTime:  tm,
			}),
		)
		require.NoError(t, err2)
		t.Cleanup(clean2)
		lookupFunc, err := NewKeytabFileLookupFunc(filename1, filename2)
		require.NoError(t, err)
		middleware, err := New(Config{
			KeytabLookup: lookupFunc,
		})
		require.NoError(t, err)
		app := fiber.New()
		app.Get("/authenticate", middleware, func(c fiber.Ctx) error {
			user, ok := GetAuthenticatedIdentityFromContext(c)
			if ok {
				t.Logf("username: %s\ndomain: %s\n", user.UserName(), user.Domain())
			}
			return c.SendString("authenticated")
		})
		handler := app.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fiber.MethodGet)
		ctx.Request.SetRequestURI("/authenticate")
		handler(ctx)
		require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	})
}
