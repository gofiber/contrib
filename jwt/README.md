---
id: jwt
---

# JWT

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=jwt*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20jwt/badge.svg)

JWT returns a JSON Web Token (JWT) auth middleware.
For valid token, it sets the user in Ctx.Locals and calls next handler.
For invalid token, it returns "401 - Unauthorized" error.
For missing token, it returns "400 - Bad Request" error.

Special thanks and credits to [Echo](https://echo.labstack.com/middleware/jwt)

**Note: Requires Go 1.19 and above**

## Install

This middleware supports Fiber v1 & v2, install accordingly.

```bash
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/jwt
go get -u github.com/golang-jwt/jwt/v5
```

## Signature

```go
jwtware.New(config ...jwtware.Config) func(*fiber.Ctx) error
```

## Config

| Property           | Type                                 | Description                                                                                                                                                                                                                                                            | Default                      |
|--------------------|--------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------|
| Filter             | `func(*fiber.Ctx) bool`              | Function to skip middleware execution for specific requests. Optional.                                                                                                                                                                                                 | `nil`                        |
| SuccessHandler     | `fiber.Handler`                      | Handler executed when a token is successfully validated. Optional.                                                                                                                                                                                                     | `nil`                        |
| ErrorHandler       | `fiber.ErrorHandler`                 | Handler executed when token validation fails. Allows customization of JWT error responses. Optional.                                                                                                                                                                   | `401 Invalid or expired JWT` |
| SigningKey         | `SigningKey`                         | Primary key used to validate tokens. Used as a fallback if SigningKeys is empty. At least one of KeyFunc, JWKSetURLs, SigningKeys, or SigningKey is required.                                                                                                          | `nil`                        |
| SigningKeys        | `map[string]SigningKey`              | Map of keys used to validate tokens with the "kid" field. At least one of KeyFunc, JWKSetURLs, SigningKeys, or SigningKey is required.                                                                                                                                 | `nil`                        |
| ContextKey         | `string`                             | Key used to store user information in the context. Optional.                                                                                                                                                                                                           | `"user"`                     |
| Claims             | `jwt.Claims`                         | Defines the structure of token claims. Extendable for custom claims data. Optional.                                                                                                                                                                                    | `jwt.MapClaims{}`            |
| TokenLookup        | `string`                             | Specifies how to extract the token from the request. Format: `"<source>:<name>"` (e.g., `"header:Authorization"`, `"query:token"`, `"param:token"`, `"cookie:token"`). Optional.                                                                                                 | `"header:Authorization"`     |
| TokenProcessorFunc | `func(token string) (string, error)` | Processes the token extracted using `TokenLookup`. Optional.                                                                                                                                                                                                           | `nil`                        |
| AuthScheme         | `string`                             | Scheme used in the Authorization header. Only used with the default TokenLookup. Optional.                                                                                                                                                                             | `"Bearer"`                   |
| KeyFunc            | `func() jwt.Keyfunc`                 | Provides the public key for JWT verification, handling algorithm verification and key selection. At least one of KeyFunc, JWKSetURLs, SigningKeys, or SigningKey is required.                                                                                          | `jwtKeyFunc`                 |
| JWKSetURLs         | `[]string`                           | List of URLs containing JSON Web Key Sets (JWKS) for signature verification. HTTPS is recommended. The "kid" field is mandatory in both the JWT header and the JWKS. Default behavior includes hourly refresh, auto-refresh on new "kid", rate limiting, and timeouts. | `nil`                        |

## HS256 Example

```go
package main

import (
 "time"

 "github.com/gofiber/fiber/v2"

 jwtware "github.com/gofiber/contrib/jwt"
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
 }))

 // Restricted Routes
 app.Get("/restricted", restricted)

 app.Listen(":3000")
}

func login(c *fiber.Ctx) error {
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

func accessible(c *fiber.Ctx) error {
 return c.SendString("Accessible")
}

func restricted(c *fiber.Ctx) error {
 user := c.Locals("user").(*jwt.Token)
 claims := user.Claims.(jwt.MapClaims)
 name := claims["name"].(string)
 return c.SendString("Welcome " + name)
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

 "github.com/gofiber/fiber/v2"

 "github.com/golang-jwt/jwt/v5"

 jwtware "github.com/gofiber/contrib/jwt"
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
 }))

 // Restricted Routes
 app.Get("/restricted", restricted)

 app.Listen(":3000")
}

func login(c *fiber.Ctx) error {
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

func accessible(c *fiber.Ctx) error {
 return c.SendString("Accessible")
}

func restricted(c *fiber.Ctx) error {
 user := c.Locals("user").(*jwt.Token)
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
  "github.com/gofiber/fiber/v2"

  jwtware "github.com/gofiber/contrib/jwt"
  "github.com/golang-jwt/jwt/v5"
)

func main() {
 app := fiber.New()

 app.Use(jwtware.New(jwtware.Config{
  KeyFunc: customKeyFunc(),
 }))

 app.Get("/ok", func(c *fiber.Ctx) error {
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
