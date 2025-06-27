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

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
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
	request.Header.Set(fiber.HeaderAuthorization, token)
	return request, nil
}

func getPrivateKey() ed25519.PrivateKey {
	seed, _ := hex.DecodeString(privateKeySeed)

	return ed25519.NewKeyFromSeed(seed)
}

func assertErrorHandler(t *testing.T, toAssert error) fiber.ErrorHandler {
	return func(ctx *fiber.Ctx, err error) error {
		utils.AssertEqual(t, toAssert, err)
		utils.AssertEqual(t, true, errors.Is(err, toAssert))
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
		utils.AssertEqual(t, fiber.StatusBadRequest, resp.StatusCode)
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

	utils.AssertEqual(t, nil, err)

	utils.AssertEqual(t, fiber.StatusBadRequest, resp.StatusCode)
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
		utils.AssertEqual(t, nil, err)
		utils.AssertEqual(t, fiber.StatusUnauthorized, resp.StatusCode)
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

	utils.AssertEqual(t, nil, err)

	var resp *http.Response
	resp, err = app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusUnauthorized, resp.StatusCode)
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
		utils.AssertEqual(t, nil, err)
		utils.AssertEqual(t, fiber.StatusUnauthorized, resp.StatusCode)
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

	utils.AssertEqual(t, nil, err)

	var resp *http.Response
	resp, err = app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func Test_PASETO_LocalToken_Next(t *testing.T) {
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

func Test_PASETO_PublicToken_Next(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		PublicKey:  privateKey.Public(),
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

func Test_PASETO_LocalTokenDecrypt(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
	}))
	app.Get("/", func(ctx *fiber.Ctx) error {
		utils.AssertEqual(t, testMessage, FromContext(ctx))
		return nil
	})
	request, err := generateTokenRequest("/", CreateToken, durationTest, PurposeLocal)
	if err == nil {
		var resp *http.Response
		resp, err = app.Test(request)
		utils.AssertEqual(t, nil, err)
		utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)
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
	app.Get("/", func(ctx *fiber.Ctx) error {
		utils.AssertEqual(t, testMessage, FromContext(ctx))
		return nil
	})
	request, err := generateTokenRequest("/", CreateToken, durationTest, PurposePublic)
	utils.AssertEqual(t, nil, err)

	var resp *http.Response
	resp, err = app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)
}

func Test_PASETO_LocalToken_IncorrectBearerToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
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

func Test_PASETO_PublicToken_IncorrectBearerToken(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey:  privateKey,
		PublicKey:   privateKey.Public(),
		TokenPrefix: "Gopher",
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

func Test_PASETO_LocalToken_InvalidToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
	}))
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, invalidToken)
	resp, err := app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusBadRequest, resp.StatusCode)
}

func Test_PASETO_PublicToken_InvalidToken(t *testing.T) {
	privateKey := getPrivateKey()

	app := fiber.New()
	app.Use(New(Config{
		PrivateKey: privateKey,
		PublicKey:  privateKey.Public(),
	}))

	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, invalidToken)
	resp, err := app.Test(request)

	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusBadRequest, resp.StatusCode)
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

	app.Get("/", func(ctx *fiber.Ctx) error {
		utils.AssertEqual(t, testMessage, FromContext(ctx))
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

	app.Get("/", func(ctx *fiber.Ctx) error {
		utils.AssertEqual(t, testMessage, FromContext(ctx))
		return nil
	})

	token, _ := pasetoObject.Sign(privateKey, customPayload{
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
