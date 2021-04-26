package pasetoware

import (
	"encoding/json"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	testMessage  = "fiber with PASETO middleware!!"
	symmetricKey = "go+fiber=love;FiberWithPASETO<3!"
)

func assertRecoveryPanic(t *testing.T) {
	err := recover()
	utils.AssertEqual(t, true, err != nil)
}

func generateTokenRequest(targetRoute string) (*http.Request, error) {
	token, err := CreateToken([]byte(symmetricKey), testMessage, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	request := httptest.NewRequest("GET", targetRoute, nil)
	request.Header.Set(fiber.HeaderAuthorization, token)
	return request, nil
}

func Test_PASETO_No_SymmetricKey(t *testing.T) {
	defer assertRecoveryPanic(t)
	app := fiber.New()
	app.Use(New())

	_, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, false, err == nil)
}

func Test_Paseto_Next(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		Next: func(_ *fiber.Ctx) bool {
			return true
		},
	}))

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusNotFound, resp.StatusCode)
}

func Test_Paseto_TokenDecrypt(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ContextKey:   DefaultContextKey,
	}))
	app.Get("/", func(ctx *fiber.Ctx) error {
		utils.AssertEqual(t, testMessage, ctx.Locals(DefaultContextKey))
		return nil
	})
	request, err := generateTokenRequest("/")
	if err == nil {
		resp, err := app.Test(request)
		utils.AssertEqual(t, nil, err)
		utils.AssertEqual(t, fiber.StatusOK, resp.StatusCode)
	}
}

func Test_Paseto_InvalidToken(t *testing.T) {
	app := fiber.New()
	app.Use(New(Config{
		SymmetricKey: []byte(symmetricKey),
		ContextKey:   DefaultContextKey,
	}))
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set(fiber.HeaderAuthorization, "We are gophers!")
	resp, err := app.Test(request)
	utils.AssertEqual(t, nil, err)
	utils.AssertEqual(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func Test_CustomValidate(t *testing.T) {
	type customPayload struct {
		Data           string        `json:"data"`
		ExpirationTime time.Duration `json:"expiration_time"`
		CreatedAt      time.Time     `json:"created_at"`
	}

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
