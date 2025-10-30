---
id: jwt
---

# JWT

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=jwt*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20jwt/badge.svg)

JWT returns a JSON Web Token (JWT) auth middleware.
For valid token, it sets the token in Ctx.Locals and calls next handler.
For invalid token, it returns "401 - Unauthorized" error.
For missing token, it returns "400 - Bad Request" error.

Special thanks and credits to [Echo](https://echo.labstack.com/middleware/jwt)


**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```bash
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/jwt
go get -u github.com/golang-jwt/jwt/v5
```

## Signature

```go
jwtware.New(config ...jwtware.Config) func(fiber.Ctx) error
jwtware.FromContext(c fiber.Ctx) *jwt.Token
```

## Config

| Property           | Type                                 | Description                                                                                                  | Default                      |
|:-------------------|:-------------------------------------|:-------------------------------------------------------------------------------------------------------------|:-----------------------------|
| Next               | `func(fiber.Ctx) bool`               | Defines a function to skip this middleware when it returns true                                              | `nil`                        |
| SuccessHandler     | `func(fiber.Ctx) error`              | Executed when a token is valid.                                                                               | `c.Next()`                   |
| ErrorHandler       | `func(fiber.Ctx, error) error`       | ErrorHandler defines a function which is executed for an invalid token.                                      | `401 Invalid or expired JWT` |
| SigningKey         | `SigningKey`                         | Signing key used to validate the token. Used as a fallback if `SigningKeys` is empty.                        | `nil`                        |
| SigningKeys        | `map[string]SigningKey`              | Map of signing keys used to validate tokens via the `kid` header.                                            | `nil`                        |
| Claims             | `jwt.Claims`                         | Claims are extendable claims data defining token content.                                                    | `jwt.MapClaims{}`            |
| Extractor          | `Extractor`                          | Function used to extract the token from the request.                                                         | `FromAuthHeader("Bearer")`   |
| TokenProcessorFunc | `func(token string) (string, error)` | TokenProcessorFunc processes the token extracted using the Extractor.                                        | `nil`                        |
| KeyFunc            | `jwt.Keyfunc`                        | User-defined function that supplies the public key for token validation.                                     | `nil` (uses internal default)|
| JWKSetURLs         | `[]string`                           | List of JSON Web Key (JWK) Set URLs used to obtain signing keys for parsing JWTs.                            | `nil`                        |

## Available Extractors

JWT middleware uses the shared Fiber extractors (github.com/gofiber/fiber/v3/extractors) and provides several helpers for different token sources. Import them with:

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

## HS256 Example

```go
package main

import (
 "time"

 "github.com/gofiber/fiber/v3"
 "github.com/gofiber/fiber/v3/extractors"

 jwtware "github.com/gofiber/contrib/v3/jwt"
 "github.com/golang-jwt/jwt/v5"
)

func main() {
 app := fiber.New()

 // Login route
 app.Post("/login", login)

 // Unauthenticated route
 app.Get("/", accessible)

 // JWT Middleware
 app.Use(jwtware.New(jwtware.Config{
  SigningKey: jwtware.SigningKey{Key: []byte("secret")},
  Extractor:  extractors.FromAuthHeader("Bearer"),
 }))

 // Restricted Routes
 app.Get("/restricted", restricted)

 app.Listen(":3000")
}

func login(c fiber.Ctx) error {
 user := c.FormValue("user")
 pass := c.FormValue("pass")

 // Throws Unauthorized error
 if user != "john" || pass != "doe" {
  return c.SendStatus(fiber.StatusUnauthorized)
 }

 // Create the Claims
 claims := jwt.MapClaims{
  "name":  "John Doe",
  "admin": true,
  "exp":   time.Now().Add(time.Hour * 72).Unix(),
 }

 // Create token
 token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

 // Generate encoded token and send it as response.
 t, err := token.SignedString([]byte("secret"))
 if err != nil {
  return c.SendStatus(fiber.StatusInternalServerError)
 }

 return c.JSON(fiber.Map{"token": t})
}

func accessible(c fiber.Ctx) error {
 return c.SendString("Accessible")
}

func restricted(c fiber.Ctx) error {
    user := jwtware.FromContext(c)
    claims := user.Claims.(jwt.MapClaims)
    name := claims["name"].(string)
    return c.SendString("Welcome " + name)
}

```

## Cookie Extractor Example

```go
package main

import (
 "github.com/gofiber/fiber/v3"

 jwtware "github.com/gofiber/contrib/v3/jwt"
)

func main() {
 app := fiber.New()

 // JWT Middleware with cookie extractor
 app.Use(jwtware.New(jwtware.Config{
  SigningKey: jwtware.SigningKey{Key: []byte("secret")},
  Extractor:  extractors.FromCookie("token"),
 }))

 app.Get("/protected", func(c fiber.Ctx) error {
  return c.SendString("Protected route")
 })

 app.Listen(":3000")
}
```

## HS256 Test

_Login using username and password to retrieve a token._

```bash
curl --data "user=john&pass=doe" http://localhost:3000/login
```

_Response_

```json
{
  "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE0NjE5NTcxMzZ9.RB3arc4-OyzASAaUhC2W3ReWaXAt_z2Fd3BN4aWTgEY"
}
```

_Request a restricted resource using the token in Authorization request header._

```bash
curl localhost:3000/restricted -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE0NjE5NTcxMzZ9.RB3arc4-OyzASAaUhC2W3ReWaXAt_z2Fd3BN4aWTgEY"
```

_Response_

```text
Welcome John Doe
```

## RS256 Example

```go
package main

import (
 "crypto/rand"
 "crypto/rsa"
 "log"
 "time"

 "github.com/gofiber/fiber/v3"

 "github.com/golang-jwt/jwt/v5"

 jwtware "github.com/gofiber/contrib/v3/jwt"
)

var (
 // Obviously, this is just a test example. Do not do this in production.
 // In production, you would have the private key and public key pair generated
 // in advance. NEVER add a private key to any GitHub repo.
 privateKey *rsa.PrivateKey
)

func main() {
 app := fiber.New()

 // Just as a demo, generate a new private/public key pair on each run. See note above.
 rng := rand.Reader
 var err error
 privateKey, err = rsa.GenerateKey(rng, 2048)
 if err != nil {
  log.Fatalf("rsa.GenerateKey: %v", err)
 }

 // Login route
 app.Post("/login", login)

 // Unauthenticated route
 app.Get("/", accessible)

 // JWT Middleware
 app.Use(jwtware.New(jwtware.Config{
  SigningKey: jwtware.SigningKey{
   JWTAlg: jwtware.RS256,
   Key:    privateKey.Public(),
  },
  Extractor: extractors.FromAuthHeader("Bearer"),
 }))

 // Restricted Routes
 app.Get("/restricted", restricted)

 app.Listen(":3000")
}

func login(c fiber.Ctx) error {
 user := c.FormValue("user")
 pass := c.FormValue("pass")

 // Throws Unauthorized error
 if user != "john" || pass != "doe" {
  return c.SendStatus(fiber.StatusUnauthorized)
 }

 // Create the Claims
 claims := jwt.MapClaims{
  "name":  "John Doe",
  "admin": true,
  "exp":   time.Now().Add(time.Hour * 72).Unix(),
 }

 // Create token
 token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

 // Generate encoded token and send it as response.
 t, err := token.SignedString(privateKey)
 if err != nil {
  log.Printf("token.SignedString: %v", err)
  return c.SendStatus(fiber.StatusInternalServerError)
 }

 return c.JSON(fiber.Map{"token": t})
}

func accessible(c fiber.Ctx) error {
 return c.SendString("Accessible")
}

func restricted(c fiber.Ctx) error {
    user := jwtware.FromContext(c)
    claims := user.Claims.(jwt.MapClaims)
    name := claims["name"].(string)
    return c.SendString("Welcome " + name)
}
```

## RS256 Test

The RS256 is actually identical to the HS256 test above.

## JWK Set Test

The tests are identical to basic `JWT` tests above, with exception that `JWKSetURLs` to valid public keys collection in JSON Web Key (JWK) Set format should be supplied. See [RFC 7517](https://www.rfc-editor.org/rfc/rfc7517).

## Custom KeyFunc example

KeyFunc defines a user-defined function that supplies the public key for a token validation.
The function shall take care of verifying the signing algorithm and selecting the proper key.
A user-defined KeyFunc can be useful if tokens are issued by an external party.

When a user-defined KeyFunc is provided, SigningKey, SigningKeys, and SigningMethod are ignored.
This is one of the three options to provide a token validation key.
The order of precedence is a user-defined KeyFunc, SigningKeys and SigningKey.
Required if neither SigningKeys nor SigningKey is provided.
Default to an internal implementation verifying the signing algorithm and selecting the proper key.

```go
package main

import (
 "fmt"
  "github.com/gofiber/fiber/v3"

  jwtware "github.com/gofiber/contrib/v3/jwt"
  "github.com/golang-jwt/jwt/v5"
)

func main() {
 app := fiber.New()

 app.Use(jwtware.New(jwtware.Config{
  KeyFunc:   customKeyFunc(),
  Extractor: extractors.FromAuthHeader("Bearer"),
 }))

 app.Get("/ok", func(c fiber.Ctx) error {
  return c.SendString("OK")
 })
}

func customKeyFunc() jwt.Keyfunc {
 return func(t *jwt.Token) (interface{}, error) {
  // Always check the signing method
  if t.Method.Alg() != jwtware.HS256 {
   return nil, fmt.Errorf("Unexpected jwt signing method=%v", t.Header["alg"])
  }

  // TODO custom implementation of loading signing key like from a database
    signingKey := "secret"

  return []byte(signingKey), nil
 }
}
```
