package v2

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gofiber/contrib/spnego/config"
	"github.com/gofiber/fiber/v2"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestNewSpnegoKrb5AuthenticateMiddleware(t *testing.T) {
	t.Run("test for keytab lookup function not set", func(t *testing.T) {
		_, err := NewSpnegoKrb5AuthenticateMiddleware(nil)
		require.ErrorIs(t, err, config.ErrConfigInvalidOfKeytabLookupFunctionRequired)
	})
	t.Run("test for keytab lookup failed", func(t *testing.T) {
		middleware, err := NewSpnegoKrb5AuthenticateMiddleware(&config.Config{
			KeytabLookup: func() (*keytab.Keytab, error) {
				return nil, errors.New("mock keytab lookup error")
			},
		})
		require.NoError(t, err)
		app := fiber.New()
		app.Get("/authenticate", middleware, func(c *fiber.Ctx) error {
			return c.SendString("authenticated")
		})
		handler := app.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fiber.MethodGet)
		ctx.Request.SetRequestURI("/authenticate")
		handler(ctx)
		require.Equal(t, http.StatusInternalServerError, ctx.Response.StatusCode())
		require.Equal(t, fmt.Sprintf("%s: mock keytab lookup error", config.ErrLookupKeytabFailed), string(ctx.Response.Body()))
	})
	t.Run("test for keytab lookup function is set", func(t *testing.T) {
		var keytabFiles []string
		for i := 0; i < 5; i++ {
			kt, clean, err := newKeytabTempFile(fmt.Sprintf("HTTP/sso%d.example.com", i), "KRB5.TEST", 18, 19)
			require.NoError(t, err)
			t.Cleanup(clean)
			keytabFiles = append(keytabFiles, kt)
		}
		lookupFunc, err := config.NewKeytabFileLookupFunc(keytabFiles...)
		require.NoError(t, err)
		middleware, err := NewSpnegoKrb5AuthenticateMiddleware(&config.Config{
			KeytabLookup: lookupFunc,
		})
		require.NoError(t, err)
		app := fiber.New()
		app.Get("/authenticate", middleware, func(c *fiber.Ctx) error {
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

func TestNewKeytabFileLookupFunc(t *testing.T) {
	t.Run("test for empty keytab files", func(t *testing.T) {
		_, err := config.NewKeytabFileLookupFunc()
		require.ErrorIs(t, err, config.ErrConfigInvalidOfAtLeastOneKeytabFileRequired)
	})
	t.Run("test for has invalid keytab file", func(t *testing.T) {
		kt1, clean, err := newKeytabTempFile("HTTP/sso.example.com", "KRB5.TEST", 18, 19)
		require.NoError(t, err)
		t.Cleanup(clean)
		kt2, clean, err := newBadKeytabTempFile("HTTP/sso1.example.com", "KRB5.TEST", 18, 19)
		require.NoError(t, err)
		t.Cleanup(clean)
		_, err = config.NewKeytabFileLookupFunc(kt1, kt2)
		require.ErrorIs(t, err, config.ErrLoadKeytabFileFailed)
	})
	t.Run("test for some keytab files", func(t *testing.T) {
		var keytabFiles []string
		for i := 0; i < 5; i++ {
			kt, clean, err := newKeytabTempFile(fmt.Sprintf("HTTP/sso%d.example.com", i), "KRB5.TEST", 18, 19)
			require.NoError(t, err)
			t.Cleanup(clean)
			keytabFiles = append(keytabFiles, kt)
		}
		lookupFunc, err := config.NewKeytabFileLookupFunc(keytabFiles...)
		require.NoError(t, err)
		_, err = lookupFunc()
		require.NoError(t, err)
	})
}

func newBadKeytabTempFile(principal string, realm string, et ...int32) (filename string, clean func(), err error) {
	filename = fmt.Sprintf("./tmp_%d.keytab", time.Now().Unix())
	clean = func() {
		os.Remove(filename)
	}
	var kt keytab.Keytab
	for _, e := range et {
		kt.AddEntry(principal, realm, "abcdefg", time.Now(), 2, e)
	}
	file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return filename, clean, fmt.Errorf("open file failed: %w", err)
	}
	if _, err = kt.Write(file); err != nil {
		return filename, clean, fmt.Errorf("write file failed: %w", err)
	}
	file.Close()
	return filename, clean, nil
}

func newKeytabTempFile(principal string, realm string, et ...int32) (filename string, clean func(), err error) {
	filename = fmt.Sprintf("./tmp_%d.keytab", time.Now().Unix())
	clean = func() {
		os.Remove(filename)
	}
	kt := keytab.New()
	for _, e := range et {
		kt.AddEntry(principal, realm, "abcdefg", time.Now(), 2, e)
	}
	file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return filename, clean, fmt.Errorf("open file failed: %w", err)
	}
	if _, err = kt.Write(file); err != nil {
		return filename, clean, fmt.Errorf("write file failed: %w", err)
	}
	file.Close()
	return filename, clean, nil
}

func TestGetAuthenticatedIdentityFromContext(t *testing.T) {
	app := fiber.New()
	app.Use("/testContext", func(ctx *fiber.Ctx) error {
		user := goidentity.NewUser("test-user")
		user.SetDomain("example.com")
		_, ok := GetAuthenticatedIdentityFromContext(ctx)
		require.False(t, ok)
		setAuthenticatedIdentityToContext(ctx, &user)
		user1, ok := GetAuthenticatedIdentityFromContext(ctx)
		require.True(t, ok)
		require.Equal(t, user.UserName(), user1.UserName())
		require.Equal(t, user.Domain(), user1.Domain())
		return ctx.SendStatus(200)
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/testContext", nil))
	require.NoError(t, err)
	require.Equal(t, resp.StatusCode, 200)
}
