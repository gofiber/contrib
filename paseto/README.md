---
id: paseto
---

# Paseto

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=paseto*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20paseto/badge.svg)

PASETO returns a Web Token (PASETO) auth middleware.

- For valid token, it sets the payload data in Ctx.Locals and calls next handler.
- For invalid token, it returns "401 - Unauthorized" error.
- For missing token, it returns "400 - BadRequest" error.

**Note: Requires Go 1.18 and above**

## Install

This middleware supports Fiber v2.

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/paseto
go get -u github.com/o1egl/paseto
```

## Signature

```go
pasetoware.New(config ...pasetoware.Config) func(*fiber.Ctx) error
```

## Config

| Property       | Type                            | Description                                                                                                                                                                                             | Default                         |
|:---------------|:--------------------------------|:--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:--------------------------------|
| Next           | `func(*Ctx) bool`               | Defines a function to skip middleware                                                                                                                                                                   | `nil`                           |
| SuccessHandler | `func(*fiber.Ctx) error`        | SuccessHandler defines a function which is executed for a valid token.                                                                                                                                  | `c.Next()`                      |
| ErrorHandler   | `func(*fiber.Ctx, error) error` | ErrorHandler defines a function which is executed for an invalid token.                                                                                                                                 | `401 Invalid or expired PASETO` |
| Validate       | `PayloadValidator`              | Defines a function to validate if payload is valid. Optional. In case payload used is created using `CreateToken` function. If token is created using another function, this function must be provided. | `nil`                           |
| SymmetricKey   | `[]byte`                        | Secret key to encrypt token. If present the middleware will generate local tokens.                                                                                                                      | `nil`                           |
| PrivateKey     | `ed25519.PrivateKey`            | Secret key to sign the tokens. If present (along with its `PublicKey`) the middleware will generate public tokens.                                                                                      | `nil`                           
| PublicKey      | `crypto.PublicKey`              | Public key to verify the tokens. If present (along with `PrivateKey`) the middleware will generate public tokens.                                                                                       | `nil`                           
| ContextKey     | `string`                        | Context key to store user information from the token into context.                                                                                                                                      | `"auth-token"`                  |
| TokenLookup    | `[2]string`                     | TokenLookup is a string slice with size 2, that is used to extract token from the request                                                                                                               | `["header","Authorization"]`    |

## Instructions

When using this middleware, and creating a token for authentication, you can use the function pasetoware.CreateToken,
that will create a token, encrypt or sign it and returns the PASETO token.

Passing a `SymmetricKey` in the Config results in a local (encrypted) token, while passing a `PublicKey`
and `PrivateKey` results in a public (signed) token.

In case you want to use your own data structure, is needed to provide the `Validate` function in `paseware.Config`, that
will return the data stored in the token, and a error.

## Examples

Below have a list of some examples that can help you start to use this middleware. In case of any additional example
that doesn't show here, please take a look at the test file.

### SymmetricKey

```go
package main

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/o1egl/paseto"

	pasetoware "github.com/gofiber/contrib/paseto"
)

const secretSymmetricKey = "symmetric-secret-key (size = 32)"

func main() {

	app := fiber.New()

	// Login route
	app.Post("/login", login)

	// Unauthenticated route
	app.Get("/", accessible)

	// Paseto Middleware with local (encrypted) token
	apiGroup := app.Group("api", pasetoware.New(pasetoware.Config{
		SymmetricKey: []byte(secretSymmetricKey),
		TokenPrefix:  "Bearer",
	}))

	// Restricted Routes
	apiGroup.Get("/restricted", restricted)

	err := app.Listen(":8088")
	if err != nil {
		return
	}
}

func login(c *fiber.Ctx) error {
	user := c.FormValue("user")
	pass := c.FormValue("pass")

	// Throws Unauthorized error
	if user != "john" || pass != "doe" {
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	// Create token and encrypt it
	encryptedToken, err := pasetoware.CreateToken([]byte(secretSymmetricKey), user, 12*time.Hour, pasetoware.PurposeLocal)
	if err != nil {
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	return c.JSON(fiber.Map{"token": encryptedToken})
}

func accessible(c *fiber.Ctx) error {
	return c.SendString("Accessible")
}

func restricted(c *fiber.Ctx) error {
	payload := c.Locals(pasetoware.DefaultContextKey).(string)
	return c.SendString("Welcome " + payload)
}

```

#### Test it

_Login using username and password to retrieve a token._

```
curl --data "user=john&pass=doe" http://localhost:8088/login
```

_Response_

```json
{
  "token": "v2.local.eY7o9YAJ7Uqyo0JdyfHXKVARj3HgBhqIHckPgNIJOU6u489CXYL6bpOXbEtTB_nNM7nTFpcRVi7YAtJToxbxkkraHmE39pqjnBgkca-URgE-jhZGuhGu7ablmK-8tVoe5iY8mQqWFuJHAznTASUHh4AG55AMUcIALi6pEG28lAgVfw2azvnvbg4JOVZnjutcOVswd-ErsAuGtuEZkTmX7BfaLaO9ZvEX9cHahYPajuRjwU2TQrcpqITg-eYMNA1NuO8OVdnGf0mkUk6ElJUTZqhx4CSSylNXr7IlOwzTbUotEDAQTcNP7IRZI3VfpnRgnmtnZ5s.bnVsbAY"
}
```

_Request a restricted resource using the token in Authorization request header._

```
curl localhost:8088/api/restricted -H "Authorization: Bearer v2.local.eY7o9YAJ7Uqyo0JdyfHXKVARj3HgBhqIHckPgNIJOU6u489CXYL6bpOXbEtTB_nNM7nTFpcRVi7YAtJToxbxkkraHmE39pqjnBgkca-URgE-jhZGuhGu7ablmK-8tVoe5iY8mQqWFuJHAznTASUHh4AG55AMUcIALi6pEG28lAgVfw2azvnvbg4JOVZnjutcOVswd-ErsAuGtuEZkTmX7BfaLaO9ZvEX9cHahYPajuRjwU2TQrcpqITg-eYMNA1NuO8OVdnGf0mkUk6ElJUTZqhx4CSSylNXr7IlOwzTbUotEDAQTcNP7IRZI3VfpnRgnmtnZ5s.bnVsbA"
```

_Response_

```
Welcome john
```

### SymmetricKey + Custom Validator callback

```go
package main

import (
	"encoding/json"
	"time"

	"github.com/o1egl/paseto"

	pasetoware "github.com/gofiber/contrib/paseto"
)

const secretSymmetricKey = "symmetric-secret-key (size = 32)"

type customPayloadStruct struct {
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func main() {

	app := fiber.New()

	// Login route
	app.Post("/login", login)

	// Unauthenticated route
	app.Get("/", accessible)

	// Paseto Middleware with local (encrypted) token
	apiGroup := app.Group("api", pasetoware.New(pasetoware.Config{
		SymmetricKey: []byte(secretSymmetricKey),
		TokenPrefix:  "Bearer",
		Validate: func(decrypted []byte) (any, error) {
			var payload customPayloadStruct
			err := json.Unmarshal(decrypted, &payload)
			return payload, err
		},
	}))

	// Restricted Routes
	apiGroup.Get("/restricted", restricted)

	err := app.Listen(":8088")
	if err != nil {
		return
	}
}

func login(c *fiber.Ctx) error {
	user := c.FormValue("user")
	pass := c.FormValue("pass")

	// Throws Unauthorized error
	if user != "john" || pass != "doe" {
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	// Create the payload
	payload := customPayloadStruct{
		Name:      "John Doe",
		ExpiresAt: time.Now().Add(12 * time.Hour),
	}

	// Create token and encrypt it
	encryptedToken, err := paseto.NewV2().Encrypt([]byte(secretSymmetricKey), payload, nil)
	if err != nil {
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	return c.JSON(fiber.Map{"token": encryptedToken})
}

func accessible(c *fiber.Ctx) error {
	return c.SendString("Accessible")
}

func restricted(c *fiber.Ctx) error {
	payload := c.Locals(pasetoware.DefaultContextKey).(customPayloadStruct)
	return c.SendString("Welcome " + payload.Name)
}

```

#### Test it

_Login using username and password to retrieve a token._

```
curl --data "user=john&pass=doe" http://localhost:8088/login
```

_Response_

```json
{
  "token": "v2.local.OSnDEMUndq8JpRdCD8yX-mr-Z0-Mi85Jw0ftxseiNLCbRc44Mxl5dnn-SV9Qew1n9Y44wXZwm_FG279cILJk7lYc_B_IoMCRBudJE7qMgctkD9UBM-ZRZgCX9ekJh3S1Oo6Erp7bO-omPra5.bnVsbA"
}
```

_Request a restricted resource using the token in Authorization request header._

```
curl localhost:8088/api/restricted -H "Authorization: Bearer v2.local.OSnDEMUndq8JpRdCD8yX-mr-Z0-Mi85Jw0ftxseiNLCbRc44Mxl5dnn-SV9Qew1n9Y44wXZwm_FG279cILJk7lYc_B_IoMCRBudJE7qMgctkD9UBM-ZRZgCX9ekJh3S1Oo6Erp7bO-omPra5.bnVsbA"
```

_Response_

```
Welcome John Doe
```

### PublicPrivate Key

```go
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"time"

	"github.com/gofiber/fiber/v2"

	pasetoware "github.com/gofiber/contrib/paseto"
)

const privateKeySeed = "e9c67fe2433aa4110caf029eba70df2c822cad226b6300ead3dcae443ac3810f"

var seed, _ = hex.DecodeString(privateKeySeed)
var privateKey = ed25519.NewKeyFromSeed(seed)

type customPayloadStruct struct {
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func main() {

	app := fiber.New()

	// Login route
	app.Post("/login", login)

	// Unauthenticated route
	app.Get("/", accessible)

	// Paseto Middleware with local (encrypted) token
	apiGroup := app.Group("api", pasetoware.New(pasetoware.Config{
		TokenPrefix: "Bearer",
		PrivateKey:  privateKey,
		PublicKey:   privateKey.Public(),
	}))

	// Restricted Routes
	apiGroup.Get("/restricted", restricted)

	err := app.Listen(":8088")
	if err != nil {
		return
	}
}

func login(c *fiber.Ctx) error {
	user := c.FormValue("user")
	pass := c.FormValue("pass")

	// Throws Unauthorized error
	if user != "john" || pass != "doe" {
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	// Create token and encrypt it
	encryptedToken, err := pasetoware.CreateToken(privateKey, user, 12*time.Hour, pasetoware.PurposePublic)
	if err != nil {
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	return c.JSON(fiber.Map{"token": encryptedToken})
}

func accessible(c *fiber.Ctx) error {
	return c.SendString("Accessible")
}

func restricted(c *fiber.Ctx) error {
	payload := c.Locals(pasetoware.DefaultContextKey).(string)
	return c.SendString("Welcome " + payload)
}

```

#### Test it

_Login using username and password to retrieve a token._

```
curl --data "user=john&pass=doe" http://localhost:8088/login
```

_Response_

```json
{
  "token": "v2.public.eyJhdWQiOiJnb2ZpYmVyLmdvcGhlcnMiLCJkYXRhIjoiam9obiIsImV4cCI6IjIwMjMtMDctMTNUMDg6NDk6MzctMDM6MDAiLCJpYXQiOiIyMDIzLTA3LTEyVDIwOjQ5OjM3LTAzOjAwIiwianRpIjoiMjIzYjM0MjQtNWNkZS00NDFhLWJiZWEtZjBjYWFhYTdiYWFlIiwibmJmIjoiMjAyMy0wNy0xMlQyMDo0OTozNy0wMzowMCIsInN1YiI6InVzZXItdG9rZW4ifWiqK_yg0eJbIs2hnup4NuBYg7v4lxh33zEhEljsH7QUaZXAdtbCPK7cN-NSfSxrw68owwgo-dOlPrD7lc5M_AU.bnVsbA"
}
```

_Request a restricted resource using the token in Authorization request header._

```
curl localhost:8088/api/restricted -H "Authorization: Bearer v2.public.eyJhdWQiOiJnb2ZpYmVyLmdvcGhlcnMiLCJkYXRhIjoiam9obiIsImV4cCI6IjIwMjMtMDctMTNUMDg6NDk6MzctMDM6MDAiLCJpYXQiOiIyMDIzLTA3LTEyVDIwOjQ5OjM3LTAzOjAwIiwianRpIjoiMjIzYjM0MjQtNWNkZS00NDFhLWJiZWEtZjBjYWFhYTdiYWFlIiwibmJmIjoiMjAyMy0wNy0xMlQyMDo0OTozNy0wMzowMCIsInN1YiI6InVzZXItdG9rZW4ifWiqK_yg0eJbIs2hnup4NuBYg7v4lxh33zEhEljsH7QUaZXAdtbCPK7cN-NSfSxrw68owwgo-dOlPrD7lc5M_AU.bnVsbA"
```

_Response_

```
Welcome John Doe
```
