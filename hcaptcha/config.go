package hcaptcha

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gofiber/fiber/v3"
)

// DefaultSiteVerifyURL is the default URL for the HCaptcha API
const DefaultSiteVerifyURL = "https://api.hcaptcha.com/siteverify"

// Config defines the config for HCaptcha middleware.
type Config struct {
	// SecretKey is the secret key you get from HCaptcha when you create a new application
	SecretKey string
	// ResponseKeyFunc should return the generated pass UUID from the ctx, which will be validated
	ResponseKeyFunc func(fiber.Ctx) (string, error)
	// SiteVerifyURL is the endpoint URL where the program should verify the given token
	// default value is: "https://api.hcaptcha.com/siteverify"
	SiteVerifyURL string
	// ValidateFunc allows custom validation logic based on the HCaptcha validation result and the context
	ValidateFunc func(success bool, c fiber.Ctx) error
}

// DefaultResponseKeyFunc is the default function to get the HCaptcha token from the request body
func DefaultResponseKeyFunc(c fiber.Ctx) (string, error) {
	data := struct {
		HCaptchaToken string `json:"hcaptcha_token"`
	}{}

	err := json.NewDecoder(bytes.NewReader(c.Body())).Decode(&data)

	if err != nil {
		return "", fmt.Errorf("failed to decode HCaptcha token: %w", err)
	}

	return data.HCaptchaToken, nil
}
