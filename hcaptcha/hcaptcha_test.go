package hcaptcha

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
)

const (
	TestSecretKey     = "0x0000000000000000000000000000000000000000"
	TestResponseToken = "20000000-aaaa-bbbb-cccc-000000000002" // Got by using this site key: 20000000-ffff-ffff-ffff-000000000002
)

// TestHCaptcha tests the hcaptcha middleware
func TestHCaptcha(t *testing.T) {
	app := fiber.New()

	m := New(Config{
		SecretKey: TestSecretKey,
		ResponseKeyFunc: func(c fiber.Ctx) (string, error) {
			return c.Query("token"), nil
		},
	})

	app.Get("/hcaptcha", func(c fiber.Ctx) error {
		return c.Status(200).SendString("ok")
	}, m)

	req := httptest.NewRequest("GET", "/hcaptcha?token="+TestResponseToken, nil)
	req.Header.Set("Content-Type", "application/json")

	res, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
	defer res.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, res.StatusCode, fiber.StatusOK, "Response status code")

	body, err := io.ReadAll(res.Body)

	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, "ok", string(body))
}
