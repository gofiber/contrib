package pasetoware

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
)

const (
	testMessage    = "fiber with PASETO middleware!!"
	invalidToken   = "We are gophers!"
	durationTest   = 10 * time.Minute
	symmetricKey   = "go+fiber=love;FiberWithPASETO<3!"
	privateKeySeed = "e9c67fe2433aa4110caf029eba70df2c822cad226b6300ead3dcae443ac3810f"
)

type customPayload struct {
	Data           string        `json:"data"`
	ExpirationTime time.Duration `json:"expiration_time"`
	CreatedAt      time.Time     `json:"created_at"`
}

func createCustomToken(key []byte, dataInfo string, duration time.Duration, purpose TokenPurpose) (string, error) {
	if purpose == PurposeLocal {
		return pasetoObject.Encrypt(key, customPayload{
			Data:           dataInfo,
			ExpirationTime: duration,
			CreatedAt:      time.Now(),
		}, nil)
	}

	return pasetoObject.Sign(ed25519.PrivateKey(key), customPayload{
		Data:           dataInfo,
		ExpirationTime: duration,
		CreatedAt:      time.Now(),
	}, nil)
}

func generateTokenRequest(
	targetRoute string, tokenGenerator PayloadCreator, duration time.Duration, purpose TokenPurpose,
	schemes ...string,
) (*http.Request, error) {
	var token string
	var err error
	if purpose == PurposeLocal {
		token, err = tokenGenerator([]byte(symmetricKey), testMessage, duration, purpose)
	} else {
		seed, _ := hex.DecodeString(privateKeySeed)
		privateKey := ed25519.NewKeyFromSeed(seed)

		token, err = tokenGenerator(privateKey, testMessage, duration, purpose)
	}

	if err != nil {
		return nil, err
	}
	request := httptest.NewRequest("GET", targetRoute, nil)
	scheme := "Bearer"
	if len(schemes) > 0 && schemes[0] != "" {
		scheme = schemes[0]
	}
	request.Header.Set(fiber.HeaderAuthorization, scheme+" "+token)
	return request, nil
}

func getPrivateKey() ed25519.PrivateKey {
	seed, _ := hex.DecodeString(privateKeySeed)

	return ed25519.NewKeyFromSeed(seed)
}

func assertErrorHandler(t *testing.T, toAssert error) fiber.ErrorHandler {
	t.Helper()
	return func(ctx fiber.Ctx, err error) error {
		assert.Equal(t, toAssert, err)
		assert.Equal(t, true, errors.Is(err, toAssert))
		return defaultErrorHandler(ctx, err)
	}
}

func Test_PASETO_LocalToken_MissingToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ErrorHandler: assertErrorHandler(t, ErrMissingToken),
	}))
	request := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(request)
	if err == nil {
		assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
	}
}

func Test_PASETO_PublicToken_MissingToken(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey:   privateKey,
		PublicKey:    privateKey.Public(),
		ErrorHandler: assertErrorHandler(t, ErrMissingToken),
	}))

	request := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(request)

	assert.Equal(t, nil, err)

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func Test_PASETO_LocalToken_ErrDataUnmarshal(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ErrorHandler: assertErrorHandler(t, ErrDataUnmarshal),
	}))
	request, err := generateTokenRequest("/", createCustomToken, durationTest, PurposeLocal)
	if err == nil {
		var resp *http.Response
		resp, err = app.Test(request)
		assert.Equal(t, nil, err)
		assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	}
}

func Test_PASETO_PublicToken_ErrDataUnmarshal(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		PublicKey:  privateKey.Public(),
	}))

	request, err := generateTokenRequest("/", createCustomToken, durationTest, PurposePublic)

	assert.Equal(t, nil, err)

	var resp *http.Response
	resp, err = app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func Test_PASETO_LocalToken_ErrTokenExpired(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ErrorHandler: assertErrorHandler(t, ErrExpiredToken),
	}))
	request, err := generateTokenRequest("/", CreateToken, time.Nanosecond*-10, PurposeLocal)
	if err == nil {
		var resp *http.Response
		resp, err = app.Test(request)
		assert.Equal(t, nil, err)
		assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	}
}

func Test_PASETO_PublicToken_ErrTokenExpired(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey:   privateKey,
		PublicKey:    privateKey.Public(),
		ErrorHandler: assertErrorHandler(t, ErrExpiredToken),
	}))

	request, err := generateTokenRequest("/", CreateToken, time.Nanosecond*-10, PurposePublic)

	assert.Equal(t, nil, err)

	var resp *http.Response
	resp, err = app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func Test_PASETO_LocalToken_Next(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+invalidToken)
	resp, err := app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func Test_PASETO_PublicToken_Next(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		PublicKey:  privateKey.Public(),
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+invalidToken)
	resp, err := app.Test(request)

	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func Test_PASETO_LocalTokenDecrypt(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
	}))
	app.Get("/", func(ctx fiber.Ctx) error {
		assert.Equal(t, testMessage, FromContext(ctx))
		return nil
	})
	request, err := generateTokenRequest("/", CreateToken, durationTest, PurposeLocal)
	if err == nil {
		var resp *http.Response
		resp, err = app.Test(request)
		assert.Equal(t, nil, err)
		assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	}
}

func Test_PASETO_PublicTokenVerify(t *testing.T) {
	seed, _ := hex.DecodeString(privateKeySeed)
	privateKey := ed25519.NewKeyFromSeed(seed)

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		PublicKey:  privateKey.Public(),
	}))
	app.Get("/", func(ctx fiber.Ctx) error {
		assert.Equal(t, testMessage, FromContext(ctx))
		return nil
	})
	request, err := generateTokenRequest("/", CreateToken, durationTest, PurposePublic)
	assert.Equal(t, nil, err)

	var resp *http.Response
	resp, err = app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func Test_PASETO_LocalToken_IncorrectBearerToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		Extractor:    extractors.FromAuthHeader("Gopher"),
	}))
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+invalidToken)
	resp, err := app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func Test_PASETO_PublicToken_IncorrectBearerToken(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		PublicKey:  privateKey.Public(),
		Extractor:  extractors.FromAuthHeader("Gopher"),
	}))

	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+invalidToken)
	resp, err := app.Test(request)

	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func Test_PASETO_LocalToken_InvalidToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
	}))
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+invalidToken)
	resp, err := app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func Test_PASETO_PublicToken_InvalidToken(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		PublicKey:  privateKey.Public(),
	}))

	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+invalidToken)
	resp, err := app.Test(request)

	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func Test_PASETO_LocalToken_CustomValidate(t *testing.T) {
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

	app.Get("/", func(ctx fiber.Ctx) error {
		assert.Equal(t, testMessage, FromContext(ctx))
		return nil
	})

	token, _ := pasetoObject.Encrypt([]byte(symmetricKey), customPayload{
		Data:           testMessage,
		ExpirationTime: 10 * time.Minute,
		CreatedAt:      time.Now(),
	}, nil)
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	resp, err := app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func Test_PASETO_PublicToken_CustomValidate(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		PublicKey:  privateKey.Public(),
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

	app.Get("/", func(ctx fiber.Ctx) error {
		assert.Equal(t, testMessage, FromContext(ctx))
		return nil
	})

	token, _ := pasetoObject.Sign(privateKey, customPayload{
		Data:           testMessage,
		ExpirationTime: 10 * time.Minute,
		CreatedAt:      time.Now(),
	}, nil)

	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	resp, err := app.Test(request)

	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func Test_PASETO_CustomErrorHandler(t *testing.T) {
	app := fiber.New()

	customErrorCalled := false
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ErrorHandler: func(ctx fiber.Ctx, err error) error {
			customErrorCalled = true
			return ctx.Status(fiber.StatusTeapot).SendString("Custom PASETO Error: " + err.Error())
		},
	}))

	app.Get("/protected", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})

	request := httptest.NewRequest("GET", "/protected", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+invalidToken)
	resp, err := app.Test(request)

	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusTeapot, resp.StatusCode)
	assert.True(t, customErrorCalled)
}

func Test_PASETO_CustomSuccessHandler(t *testing.T) {
	app := fiber.New()

	customSuccessCalled := false
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		SuccessHandler: func(ctx fiber.Ctx) error {
			customSuccessCalled = true
			ctx.Locals("custom", "paseto-success")
			return ctx.Next()
		},
	}))

	app.Get("/protected", func(ctx fiber.Ctx) error {
		if ctx.Locals("custom") == "paseto-success" {
			return ctx.SendString("Custom Success Handler Worked")
		}
		return ctx.SendString("OK")
	})

	request, err := generateTokenRequest("/protected", CreateToken, durationTest, PurposeLocal)
	assert.Equal(t, nil, err)

	resp, err := app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.True(t, customSuccessCalled)
}

func Test_PASETO_InvalidSymmetricKey(t *testing.T) {
	defer func() {
		err := recover()
		assert.NotNil(t, err)
	}()

	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte("invalid-key-length"), // Wrong length
	}))

	app.Get("/protected", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})
}

func Test_PASETO_MissingPublicKey(t *testing.T) {
	defer func() {
		err := recover()
		assert.NotNil(t, err)
	}()

	privateKey := getPrivateKey()
	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		// Missing PublicKey
	}))

	app.Get("/protected", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})
}

func Test_PASETO_MissingPrivateKey(t *testing.T) {
	defer func() {
		err := recover()
		assert.NotNil(t, err)
	}()

	app := fiber.New()
	app.Use(New(Config{
		PublicKey: getPrivateKey().Public(),
		// Missing PrivateKey
	}))

	app.Get("/protected", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})
}

func Test_PASETO_BothKeysProvided(t *testing.T) {
	defer func() {
		err := recover()
		assert.NotNil(t, err)
	}()

	privateKey := getPrivateKey()
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		PrivateKey:   privateKey,
		PublicKey:    privateKey.Public(),
	}))

	app.Get("/protected", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})
}

func Test_PASETO_FromContextWithoutToken(t *testing.T) {
	app := fiber.New()

	app.Get("/no-token", func(ctx fiber.Ctx) error {
		payload := FromContext(ctx)
		if payload == nil {
			return ctx.SendString("No payload as expected")
		}
		return ctx.SendString("Unexpected payload")
	})

	request := httptest.NewRequest("GET", "/no-token", nil)
	resp, err := app.Test(request)

	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func Test_PASETO_CustomValidateError(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		Validate: func(data []byte) (interface{}, error) {
			return nil, fiber.NewError(fiber.StatusForbidden, "Custom validation failed")
		},
		ErrorHandler: func(ctx fiber.Ctx, err error) error {
			return ctx.Status(fiber.StatusForbidden).SendString("Validation failed")
		},
	}))

	app.Get("/protected", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})

	request, err := generateTokenRequest("/protected", CreateToken, durationTest, PurposeLocal)
	assert.Equal(t, nil, err)

	resp, err := app.Test(request)
	assert.Equal(t, nil, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}
