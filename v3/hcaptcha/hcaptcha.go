// Package hcaptcha is a simple middleware that checks for an HCaptcha UUID
// and then validates it. It returns an error if the UUID is not valid (the request may have been sent by a robot).
package hcaptcha

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
	"net/url"
)

// HCaptcha is a middleware handler that checks for an HCaptcha UUID and then validates it.
type HCaptcha struct {
	Config
}

// New creates a new HCaptcha middleware handler.
func New(config Config) fiber.Handler {
	if config.SiteVerifyURL == "" {
		config.SiteVerifyURL = DefaultSiteVerifyURL
	}

	if config.ResponseKeyFunc == nil {
		config.ResponseKeyFunc = DefaultResponseKeyFunc
	}

	h := &HCaptcha{
		config,
	}
	return h.Validate
}

// Validate checks for an HCaptcha UUID and then validates it.
func (h *HCaptcha) Validate(c fiber.Ctx) error {
	token, err := h.ResponseKeyFunc(c)
	if err != nil {
		c.Status(fiber.StatusBadRequest)
		return fmt.Errorf("error retrieving HCaptcha token: %w", err)
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetBody([]byte(url.Values{
		"secret":   {h.SecretKey},
		"response": {token},
	}.Encode()))
	req.Header.SetMethod("POST")
	req.Header.SetContentType("application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	req.SetRequestURI(h.SiteVerifyURL)
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(res)

	// Send the request to the HCaptcha API
	if err = fasthttp.Do(req, res); err != nil {
		c.Status(fiber.StatusBadRequest)
		return fmt.Errorf("error sending request to HCaptcha API: %w", err)
	}

	o := struct {
		Success bool `json:"success"`
	}{}

	if err = json.NewDecoder(bytes.NewReader(res.Body())).Decode(&o); err != nil {
		c.Status(fiber.StatusInternalServerError)
		return fmt.Errorf("error decoding HCaptcha API response: %w", err)
	}

	// Execute custom validation if ValidateFunc is defined.
	// ValidateFunc receives the siteverify result and should return an error on validation failure.
	// If ValidateFunc is nil, default behavior rejects unsuccessful verification.
	var validationErr error
	if h.ValidateFunc != nil {
		validationErr = h.ValidateFunc(o.Success, c)
	} else if !o.Success {
		validationErr = errors.New("unable to check that you are not a robot")
	}

	if validationErr != nil {
		if c.Response().StatusCode() < fiber.StatusBadRequest {
			c.Status(fiber.StatusForbidden)
		}
		if len(c.Response().Body()) == 0 {
			return c.SendString(validationErr.Error())
		}
		return nil
	}

	return c.Next()
}
