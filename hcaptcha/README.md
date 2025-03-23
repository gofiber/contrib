---
id: hcaptcha
---

# HCaptcha

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=hcaptcha*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20hcaptcha/badge.svg)

A simple [HCaptcha](https://hcaptcha.com) middleware to prevent bot attacks.

:::note

Requires Go **1.21** and above

:::

## Install

:::caution

This middleware only supports Fiber **v3**.

:::

```shell
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/hcaptcha
```

## Signature

```go
hcaptcha.New(config hcaptcha.Config) fiber.Handler
```

## Config

| Property         | Type                              | Description                                                                                                                                                                                          | Default                               |
|:-----------------|:----------------------------------|:-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:--------------------------------------|
| SecretKey        | `string`                          | The secret key you obtained from the HCaptcha admin panel. This field must not be empty.                                                                                                             | `""`                                  |
| ResponseKeyFunc  | `func(fiber.Ctx) (string, error)`| ResponseKeyFunc should return the token that captcha provides upon successful solving. By default, it gets the token from the body by parsing a JSON request and returns the `hcaptcha_token` field. | `hcaptcha.DefaultResponseKeyFunc`    |
| SiteVerifyURL    | `string`                          | This property specifies the API resource used for token authentication.                                                                                                                              | `https://api.hcaptcha.com/siteverify` |
| ValidateFunc     | `func(bool, fiber.Ctx) error`    | A custom validation function that allows you to define the behavior upon validation success or failure. If set, it will be called with the validation result and the context.                       | `nil`                                 |

## Example

```go
package main

import (
    "errors"
    "github.com/gofiber/contrib/hcaptcha"
    "github.com/gofiber/fiber/v3"
    "log"
)

const (
    TestSecretKey = "0x0000000000000000000000000000000000000000"
    TestSiteKey   = "20000000-ffff-ffff-ffff-000000000002"
)

func main() {
    app := fiber.New()
    
    // Create HCaptcha middleware
    captcha := hcaptcha.New(hcaptcha.Config{
        // Must set the secret key
        SecretKey: TestSecretKey,
        // Custom validation function (optional)
        ValidateFunc: func(success bool, c fiber.Ctx) error {
            if !success {
                c.Status(fiber.StatusForbidden).JSON(fiber.Map{
                    "error":   "Custom error: validation failed, please try again",
                    "details": "The HCaptcha validation was unsuccessful.",
                })
                return errors.New("custom error: validation failed")
            }
            return nil
        },
    })
	
    app.Get("/api/", func(c fiber.Ctx) error {
        return c.JSON(fiber.Map{
            "hcaptcha_site_key": TestSiteKey,
        })
    })
	
    app.Post("/api/submit", captcha, func(c fiber.Ctx) error {
        return c.SendString("You are not a robot")
    })
	
    log.Fatal(app.Listen(":3000"))
}
```
