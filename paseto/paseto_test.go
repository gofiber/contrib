package pasetoware

import (
	"encoding/json"
	"errors"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	testMessage  = "fiber with PASETO middleware!!"
	invalidToken = "We are gophers!"
	durationTest = 10 * time.Minute
	symmetricKey = "go+fiber=love;FiberWithPASETO<3!"
)

type customPayload struct {
	Data           string        `json:"data"`
	ExpirationTime time.Duration `json:"expiration_time"`
	CreatedAt      time.Time     `json:"created_at"`
}

func createCustomToken(key []byte, dataInfo string, duration time.Duration) (string, error) {
	return pasetoObject.Encrypt(key, customPayload{
		Data:           dataInfo,
		ExpirationTime: duration,
		CreatedAt:      time.Now(),
	}, nil)
}

func generateTokenRequest(
	targetRoute string, tokenGenerator PayloadCreator, duration time.Duration,
) (*http.Request, error) {
	token, err := tokenGenerator([]byte(symmetricKey), testMessage, duration)
	if err != nil {
		return nil, err
	}
	request := httptest.NewRequest("GET", targetRoute, nil)
	request.Header.Set(fiber.HeaderAuthorization, token)
	return request, nil
}

func assertErrorHandler(t *testing.T, toAssert error) fiber.ErrorHandler {
	return func(ctx *fiber.Ctx, err error) error {
		utils.AssertEqual(t, toAssert, err)
		utils.AssertEqual(t, true, errors.Is(err, toAssert))
		return defaultErrorHandler(ctx, err)
	}
}

func Test_PASETO_MissingToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ContextKey:   DefaultContextKey,
		ErrorHandler: assertErrorHandler(t, ErrMissingToken),
	}))
	request := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(request)
	if err == nil {
		utils.AssertEqual(t, fiber.StatusBadRequest, resp.StatusCode)
	}
}

func Test_PASETO_ErrDataUnmarshal(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ContextKey:   DefaultContextKey,
		ErrorHandler: assertErrorHandler(t, ErrDataUnmarshal),
	}))
	request, err := generateTokenRequest("/", createCustomToken, durationTest)
	if err == nil {
		var resp *http.Response
		resp, err = app.Test(request)
		utils.AssertEqual(t, nil, err)
		utils.AssertEqual(t, fiber.StatusUnauthorized, resp.StatusCode)
	}
}

func Test_PASETO_ErrTokenExpired(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ContextKey:   DefaultContextKey,
		ErrorHandler: assertErrorHandler(t, ErrExpiredToken),
	}))
	request, err := generateTokenRequest("/", CreateToken, time.Nanosecond*-10)
	if err == nil {
		var resp *http.Response
		resp, err = app.Test(request)
		utils.AssertEqual(t, nil, err)
		utils.AssertEqual(t, fiber.StatusUnauthorized, resp.StatusCode)
	}
}

func Test_PASETO_Next(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		Next: func(_ *fiber.Ctx) bool {
			return true
		},
	}))

	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, invalidToken)
	resp, err := app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)
}

func Test_PASETO_TokenDecrypt(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ContextKey:   DefaultContextKey,
	}))
	app.Get("/", func(ctx *fiber.Ctx) error {
		utils.AssertEqual(t, testMessage, ctx.Locals(DefaultContextKey))
		return nil
	})
	request, err := generateTokenRequest("/", CreateToken, durationTest)
	if err == nil {
		var resp *http.Response
		resp, err = app.Test(request)
		utils.AssertEqual(t, nil, err)
		utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)
	}
}

func Test_PASETO_IncorrectBearerToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ContextKey:   DefaultContextKey,
		TokenPrefix:  "Gopher",
		ErrorHandler: func(ctx *fiber.Ctx, err error) error {
			if errors.Is(err, ErrIncorrectTokenPrefix) {
				return ctx.SendStatus(fiber.StatusUpgradeRequired)
			}
			return ctx.SendStatus(fiber.StatusBadRequest)
		},
	}))
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+invalidToken)
	resp, err := app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusUpgradeRequired, resp.StatusCode)
}

func Test_PASETO_InvalidToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ContextKey:   DefaultContextKey,
	}))
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, invalidToken)
	resp, err := app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusBadRequest, resp.StatusCode)
}

func Test_PASETO_CustomValidate(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		Validate: func(data []byte) (interface{}, error) {
			var payload customPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				return nil, ErrDataUnmarshal
			}

			if time.Now().After(payload.CreatedAt.Add(payload.ExpirationTime)) {
				return nil, ErrExpiredToken
			}
			return payload.Data, nil
		},
	}))

	app.Get("/", func(ctx *fiber.Ctx) error {
		utils.AssertEqual(t, testMessage, ctx.Locals(DefaultContextKey))
		return nil
	})

	token, _ := pasetoObject.Encrypt([]byte(symmetricKey), customPayload{
		Data:           testMessage,
		ExpirationTime: 10 * time.Minute,
		CreatedAt:      time.Now(),
	}, nil)
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, token)
	resp, err := app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)
}
