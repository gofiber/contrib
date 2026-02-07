package hcaptcha

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

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
	// ValidateFunc allows custom validation handling based on the HCaptcha validation result.
	// If set, it is called with the API success status and the current context after siteverify.
	// For secure bot protection, reject requests when success is false.
	// Return nil to continue to the next handler, or return an error to stop the middleware chain.
	// If ValidateFunc is nil, default behavior is used and unsuccessful verification returns 403.
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

	if strings.TrimSpace(data.HCaptchaToken) == "" {
		return "", fmt.Errorf("hcaptcha token is empty")
	}

	return data.HCaptchaToken, nil
}
