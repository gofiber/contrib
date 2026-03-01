---
id: paseto
---

# Paseto

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*paseto*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20paseto/badge.svg)

PASETO returns a Web Token (PASETO) auth middleware.

- For valid token, it sets the payload data in Ctx.Locals and calls next handler.
- For invalid token, it returns "401 - Unauthorized" error.
- For missing token, it returns "400 - BadRequest" error.


**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/paseto
go get -u github.com/o1egl/paseto
```

## Signature

```go
pasetoware.New(config ...pasetoware.Config) func(fiber.Ctx) error
pasetoware.FromContext(c fiber.Ctx) interface{}
```

## Config

| Property       | Type                            | Description                                                                                                                                                                                             | Default                         |
|:---------------|:--------------------------------|:--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:--------------------------------|
| Next           | `func(fiber.Ctx) bool`          | Defines a function to skip this middleware when it returns true.                                                                                                                                        | `nil`                           |
| SuccessHandler | `func(fiber.Ctx) error`         | SuccessHandler defines a function which is executed for a valid token.                                                                                                                                  | `c.Next()`                      |
| ErrorHandler   | `func(fiber.Ctx, error) error`  | ErrorHandler defines a function which is executed for an invalid token.                                                                                                                                 | `401 Invalid or expired PASETO` |
| Validate       | `PayloadValidator`              | Defines a function to validate if payload is valid. Optional. In case payload used is created using `CreateToken` function. If token is created using another function, this function must be provided. | `nil`                           |
| SymmetricKey   | `[]byte`                        | Secret key to encrypt token. If present the middleware will generate local tokens.                                                                                                                      | `nil`                           |
| PrivateKey     | `ed25519.PrivateKey`            | Secret key to sign the tokens. If present (along with its `PublicKey`) the middleware will generate public tokens.                                                                                      | `nil`                           |  
| PublicKey      | `crypto.PublicKey`              | Public key to verify the tokens. If present (along with `PrivateKey`) the middleware will generate public tokens.                                                                                       | `nil`                           |  
| Extractor      | `Extractor`                     | Extractor defines a function to extract the token from the request.                                                                                                                                     | `FromAuthHeader("Bearer")`      |

## Available Extractors

PASETO middleware uses the shared Fiber extractors (github.com/gofiber/fiber/v3/extractors) and provides several helpers for different token sources:

Import them like this:

```go
import "github.com/gofiber/fiber/v3/extractors"
```

For an overview and additional examples, see the Fiber Extractors guide:

- https://docs.gofiber.io/guide/extractors

- `extractors.FromAuthHeader(prefix string)` - Extracts token from the Authorization header using the given scheme prefix (e.g., "Bearer"). **This is the recommended and most secure method.**
- `extractors.FromHeader(header string)` - Extracts token from the specified HTTP header
- `extractors.FromQuery(param string)` - Extracts token from URL query parameters
- `extractors.FromParam(param string)` - Extracts token from URL path parameters
- `extractors.FromCookie(key string)` - Extracts token from cookies
- `extractors.FromForm(param string)` - Extracts token from form data
- `extractors.Chain(extrs ...extractors.Extractor)` - Tries multiple extractors in order until one succeeds

### Security Considerations

⚠️ **Security Warning**: When choosing an extractor, consider the security implications:

- **URL-based extractors** (`FromQuery`, `FromParam`): Tokens can leak through server logs, browser referrer headers, proxy logs, and browser history. Use only for development or when security is not a primary concern.
- **Form-based extractors** (`FromForm`): Similar risks to URL extractors, especially if forms are submitted via GET requests.
- **Header-based extractors** (`FromAuthHeader`, `FromHeader`): Most secure as headers are not typically logged or exposed in referrers.
- **Cookie-based extractors** (`FromCookie`): Secure for web applications but requires proper cookie security settings (HttpOnly, Secure, SameSite).

**Recommendation**: Use `FromAuthHeader("Bearer")` (the default) for production applications unless you have specific requirements that necessitate alternative extractors.

## Migration from TokenPrefix

If you were previously using `TokenPrefix`, you can now use `extractors.FromAuthHeader` with the prefix:

```go
// Old way
pasetoware.New(pasetoware.Config{
    SymmetricKey: []byte("secret"),
    TokenPrefix:  "Bearer",
})

// New way
pasetoware.New(pasetoware.Config{
    SymmetricKey: []byte("secret"),
    Extractor:    extractors.FromAuthHeader("Bearer"),
})
```

## Examples

Below have a list of some examples that can help you start to use this middleware. In case of any additional example
that doesn't show here, please take a look at the test file.

### SymmetricKey

```go
package main

import (
    "time"

    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/extractors"

    pasetoware "github.com/gofiber/contrib/v3/paseto"
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
        Extractor:    extractors.FromAuthHeader("Bearer"),
    }))

    // Restricted Routes
    apiGroup.Get("/restricted", restricted)

    err := app.Listen(":8088")
    if err != nil {
        return
    }
}

func login(c fiber.Ctx) error {
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

func accessible(c fiber.Ctx) error {
    return c.SendString("Accessible")
}

func restricted(c fiber.Ctx) error {
    payload := pasetoware.FromContext(c).(string)
    return c.SendString("Welcome " + payload)
}

```

#### Test it

_Login using username and password to retrieve a token._

```sh
curl --data "user=john&pass=doe" http://localhost:8088/login
```

_Response_

```json
{
  "token": "<local-token>"
}
```

_Request a restricted resource using the token in Authorization request header._

```sh
curl localhost:8088/api/restricted -H "Authorization: Bearer <local-token>"
```

_Response_

```text
Welcome john
```

### SymmetricKey + Custom Validator callback

```go
package main

import (
    "encoding/json"
    "time"

    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/extractors"
    "github.com/o1egl/paseto"

    pasetoware "github.com/gofiber/contrib/v3/paseto"
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
        Extractor:    extractors.FromAuthHeader("Bearer"),
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

func login(c fiber.Ctx) error {
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

func accessible(c fiber.Ctx) error {
    return c.SendString("Accessible")
}

func restricted(c fiber.Ctx) error {
    payload := pasetoware.FromContext(c).(customPayloadStruct)
    return c.SendString("Welcome " + payload.Name)
}

```

### Cookie Extractor Example

```go
package main

import (
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/extractors"

    pasetoware "github.com/gofiber/contrib/v3/paseto"
)

const secretSymmetricKey = "symmetric-secret-key (size = 32)"

func main() {
    app := fiber.New()

    // Paseto Middleware with cookie extractor
    app.Use(pasetoware.New(pasetoware.Config{
        SymmetricKey: []byte(secretSymmetricKey),
        Extractor:    extractors.FromCookie("token"),
    }))

    app.Get("/protected", func(c fiber.Ctx) error {
        return c.SendString("Protected route")
    })

    app.Listen(":8080")
}
```

### Query Extractor Example

```go
package main

import (
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/extractors"

    pasetoware "github.com/gofiber/contrib/v3/paseto"
)

const secretSymmetricKey = "symmetric-secret-key (size = 32)"

func main() {
    app := fiber.New()

    // Paseto Middleware with query extractor
    app.Use(pasetoware.New(pasetoware.Config{
        SymmetricKey: []byte(secretSymmetricKey),
        Extractor:    extractors.FromQuery("token"),
    }))

    app.Get("/protected", func(c fiber.Ctx) error {
        return c.SendString("Protected route")
    })

    app.Listen(":8080")
}
```

### PublicPrivate Key

```go
package main

import (
    "crypto/ed25519"
    "encoding/hex"
    "time"

    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/extractors"

    pasetoware "github.com/gofiber/contrib/v3/paseto"
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

    // Paseto Middleware with public (signed) token
    apiGroup := app.Group("api", pasetoware.New(pasetoware.Config{
        Extractor:  extractors.FromAuthHeader("Bearer"),
        PrivateKey: privateKey,
        PublicKey:  privateKey.Public(),
    }))

    // Restricted Routes
    apiGroup.Get("/restricted", restricted)

    err := app.Listen(":8088")
    if err != nil {
        return
    }
}

func login(c fiber.Ctx) error {
    user := c.FormValue("user")
    pass := c.FormValue("pass")

    // Throws Unauthorized error
    if user != "john" || pass != "doe" {
        return c.SendStatus(fiber.StatusUnauthorized)
    }

    // Create token and sign it
    signedToken, err := pasetoware.CreateToken(privateKey, user, 12*time.Hour, pasetoware.PurposePublic)
    if err != nil {
        return c.SendStatus(fiber.StatusInternalServerError)
    }

    return c.JSON(fiber.Map{"token": signedToken})
}

func accessible(c fiber.Ctx) error {
    return c.SendString("Accessible")
}

func restricted(c fiber.Ctx) error {
    payload := pasetoware.FromContext(c).(string)
    return c.SendString("Welcome " + payload)
}

```

#### Get the payload from the context

```go
payloadFromCtx := pasetoware.FromContext(c)  
if payloadFromCtx == nil {  
    // Handle case where token is not in context, e.g. by returning an error  
    return  
}  
payload := payloadFromCtx.(string)  
```

#### Test it

_Login using username and password to retrieve a token._

```sh
curl --data "user=john&pass=doe" http://localhost:8088/login
```

_Response_

```json
{
  "token": "<public-token>"
}
```

_Request a restricted resource using the token in Authorization request header._

```sh
curl localhost:8088/api/restricted -H "Authorization: Bearer <public-token>"
```

_Response_

```text
Welcome John Doe
```
