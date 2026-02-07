---
id: hcaptcha
---

# HCaptcha

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=hcaptcha*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20hcaptcha/badge.svg)

A simple [HCaptcha](https://hcaptcha.com) middleware to prevent bot attacks.

:::note

Requires Go **1.25** and above

:::

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

:::caution

This middleware only supports Fiber **v3**.

:::

```shell
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/hcaptcha
```

## Signature

```go
hcaptcha.New(config hcaptcha.Config) fiber.Handler
```

## Config

| Property        | Type                               | Description                                                                                                                                                                                                                                                                                  | Default                               |
|:----------------|:-----------------------------------|:---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:--------------------------------------|
| SecretKey       | `string`                           | The secret key you obtained from the HCaptcha admin panel. This field must not be empty.                                                                                                                                                                                                    | `""`                                  |
| ResponseKeyFunc | `func(fiber.Ctx) (string, error)`  | ResponseKeyFunc should return the token that the captcha provides upon successful solving. By default, it gets the token from the body by parsing a JSON request and returns the `hcaptcha_token` field.                                                                                 | `hcaptcha.DefaultResponseKeyFunc`     |
| SiteVerifyURL   | `string`                           | This property specifies the API resource used for token authentication.                                                                                                                                                                                                                      | `https://api.hcaptcha.com/siteverify` |
| ValidateFunc    | `func(success bool, c fiber.Ctx) error` | Optional custom validation hook called after siteverify completes. Parameters: `success` (hCaptcha verification result), `c` (Fiber context). Return `nil` to continue, or return an `error` to stop request processing. If unset, middleware defaults to blocking unsuccessful verification. For secure bot protection, reject when `success == false`. | `nil`                                 |

## Example

```go
package main

import (
	"errors"
	"log"

	"github.com/gofiber/contrib/v3/hcaptcha"
	"github.com/gofiber/fiber/v3"
)

const (
    TestSecretKey = "0x0000000000000000000000000000000000000000"
    TestSiteKey   = "20000000-ffff-ffff-ffff-000000000002"
)

func main() {
	app := fiber.New()
	captcha := hcaptcha.New(hcaptcha.Config{
		// Must set the secret key.
		SecretKey: TestSecretKey,
		// Optional custom validation handling.
		ValidateFunc: func(success bool, c fiber.Ctx) error {
			if !success {
				if err := c.Status(fiber.StatusForbidden).JSON(fiber.Map{
					"error":   "HCaptcha validation failed",
					"details": "Please complete the captcha challenge and try again",
				}); err != nil {
					return err
				}
				return errors.New("custom validation failed")
			}
			return nil
		},
	})

	app.Get("/api/", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"hcaptcha_site_key": TestSiteKey,
		})
	})

	// Middleware order matters: place hcaptcha middleware before the final handler.
	app.Post("/api/submit", captcha, func(c fiber.Ctx) error {
		return c.SendString("You are not a robot")
	})

	log.Fatal(app.Listen(":3000"))
}
```
