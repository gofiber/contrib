---
id: jwt
---

# JWT

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*jwt*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20jwt/badge.svg)

JWT returns a JSON Web Token (JWT) auth middleware.
For valid token, it sets the token in Ctx.Locals (and in the underlying `context.Context` when `PassLocalsToContext` is enabled) and calls next handler.
For invalid token, it returns "401 - Unauthorized" error.
For missing token, it returns "400 - Bad Request" error.
Either way the response carries the `WWW-Authenticate` challenge required by [RFC 9110](https://www.rfc-editor.org/rfc/rfc9110#section-15.5.2) and [RFC 6750](https://www.rfc-editor.org/rfc/rfc6750#section-3). See [Standards compliance](#standards-compliance).

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
jwtware.FromContext(ctx any) *jwt.Token    // jwt "github.com/golang-jwt/jwt/v5"
```

`FromContext` accepts a `fiber.Ctx`, `fiber.CustomCtx`, `*fasthttp.RequestCtx`, or a standard `context.Context` (e.g. the value returned by `c.Context()` when `PassLocalsToContext` is enabled). It returns a `*jwt.Token` from `github.com/golang-jwt/jwt/v5`.

## Config

| Property           | Type                                 | Description                                                                           | Default                      |
|:-------------------|:-------------------------------------|:--------------------------------------------------------------------------------------|:-----------------------------|
| Next               | `func(fiber.Ctx) bool`               | Defines a function to skip this middleware when it returns true                       | `nil`                        |
| SuccessHandler     | `func(fiber.Ctx) error`              | Executed when a token is valid.                                                       | `c.Next()`                   |
| ErrorHandler       | `func(fiber.Ctx, error) error`       | ErrorHandler defines a function which is executed for an invalid token.               | `401 Invalid or expired JWT` |
| Realm              | `string`                             | Protected area named in the `WWW-Authenticate` challenge sent with a rejection.       | `"Restricted"`               |
| SigningKey         | `SigningKey`                         | Signing key used to validate the token. Used as a fallback if `SigningKeys` is empty. | `nil`                        |
| SigningKeys        | `map[string]SigningKey`              | Map of signing keys used to validate tokens via the `kid` header.                     | `nil`                        |
| Claims             | `jwt.Claims`                         | Claims are extendable claims data defining token content.                             | `jwt.MapClaims{}`            |
| Extractor          | `Extractor`                          | Function used to extract the token from the request.                                  | `FromAuthHeader("Bearer")`   |
| TokenProcessorFunc | `func(token string) (string, error)` | TokenProcessorFunc processes the token extracted using the Extractor.                 | `nil`                        |
| KeyFunc            | `jwt.Keyfunc`                        | User-defined function that supplies the public key for token validation.              | `nil` (uses internal default)|
| JWKSetURLs         | `[]string`                           | List of JSON Web Key (JWK) Set URLs used to obtain signing keys for parsing JWTs.     | `nil`                        |
| ParserOptions      | `[]jwt.ParserOption`                 | List of [`jwt.ParserOption`](https://pkg.go.dev/github.com/golang-jwt/jwt/v5#ParserOption), provides additional options for JWT parsing.                | `nil`                        |
| KnownCriticalHeaders | `[]string`                         | JWS `crit` header parameters this application understands and processes itself.       | `nil`                        |

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

## Standards compliance

### What the middleware enforces

- **The signing algorithm comes from your configuration, not from the token.** When
  `SigningKey.JWTAlg` is set - or when every entry of `SigningKeys` sets it - the
  parser rejects any other `alg` before a key is looked up, as
  [RFC 8725 Section 3.1](https://www.rfc-editor.org/rfc/rfc8725#section-3.1) asks.
  Pass `jwt.WithValidMethods` in `ParserOptions` to pin the algorithms yourself; a
  value you pass wins over the derived one. Configurations that leave the
  algorithm open (`KeyFunc`, `JWKSetURLs`, or a `SigningKey` without `JWTAlg`)
  are only as strict as the key material and the JWK `alg` allow, so pinning is
  worth it.
- **`alg: none` is refused**, and keys are never read out of the token: the `jwk`,
  `jku`, `x5u` and `x5c` header parameters are ignored, per
  [RFC 8725 Sections 3.4 and 3.5](https://www.rfc-editor.org/rfc/rfc8725#section-3.4).
- **Critical header parameters** (`crit`) are checked as
  [RFC 7515 Section 4.1.11](https://www.rfc-editor.org/rfc/rfc7515#section-4.1.11)
  requires: a token that marks a header parameter critical is rejected unless the
  parameter is listed in `KnownCriticalHeaders`. A `crit` value that is not a
  non-empty array of names, repeats a name, names a registered JOSE parameter
  such as `alg`, or names a parameter the header does not contain, is rejected as
  well. Nothing else uses `crit`, so leaving `KnownCriticalHeaders` unset is the
  safe default.
- **`exp` and `nbf`** are validated by default on every request by
  `github.com/golang-jwt/jwt/v5` ([RFC 7519 Sections 4.1.4 and
  4.1.5](https://www.rfc-editor.org/rfc/rfc7519#section-4.1.4)). Use
  `jwt.WithLeeway`, `jwt.WithExpirationRequired` or `jwt.WithNotBeforeRequired` in
  `ParserOptions` to tighten this - and note that `jwt.WithoutClaimsValidation()`
  in `ParserOptions` turns it off entirely, so an expired token is accepted.
- **Base64url segments must be unpadded** ([RFC 7515 Section
  2](https://www.rfc-editor.org/rfc/rfc7515#section-2)), and an `Authorization`
  header has to be well-formed `token68` ([RFC 9110 Section
  11.6.2](https://www.rfc-editor.org/rfc/rfc9110#section-11.6.2)).
- **Rejections carry a challenge.** Every 400, 401 and 407 response the
  middleware produces gets a `WWW-Authenticate` (or `Proxy-Authenticate`) header,
  which [RFC 9110 Section
  15.5.2](https://www.rfc-editor.org/rfc/rfc9110#section-15.5.2) requires of a 401
  and [RFC 6750 Section 3](https://www.rfc-editor.org/rfc/rfc6750#section-3)
  requires of a resource server refusing a bearer token:

  ```text
  WWW-Authenticate: Bearer realm="Restricted", error="invalid_token", error_description="The access token expired"
  ```

  The scheme is taken from the extractor (`Bearer` unless the token comes from an
  `Authorization` header with another scheme), the realm from `Realm`, and the
  `error_description` from the reason the token failed. Error parameters are only
  added for the bearer scheme, as RFC 6750 defines them, and a request that
  presented no usable credential is answered with a bare `Bearer realm="..."`
  instead, which [RFC 6750 Section
  3.1](https://www.rfc-editor.org/rfc/rfc6750#section-3.1) asks for. A challenge
  already on the response is never replaced, whether an `ErrorHandler` of yours
  or an earlier authentication middleware put it there.
- **Tokens in the query string are not stored in shared caches.** When the token
  arrived in the query, the successful response is marked `Cache-Control: private`,
  as [RFC 6750 Section
  2.3](https://www.rfc-editor.org/rfc/rfc6750#section-2.3) asks, since the URL a
  shared cache keys on contains the token. A policy the handler set is kept,
  except that `public` is dropped and `private` added unless the policy already
  keeps the response out of shared caches. Responses to tokens that arrived in a
  header or a cookie are untouched, including from an extractor chain that could
  have read the query but did not.

### What your application has to configure

Some rules cannot be applied by a middleware, because only the application knows
the values involved:

- **`aud`** - [RFC 7519 Section
  4.1.3](https://www.rfc-editor.org/rfc/rfc7519#section-4.1.3) requires a token
  whose audience is not this application to be rejected. The audience is only
  checked when you name it:

  ```go
  app.Use(jwtware.New(jwtware.Config{
      SigningKey: jwtware.SigningKey{JWTAlg: jwtware.RS256, Key: publicKey},
      ParserOptions: []jwt.ParserOption{
          jwt.WithAudience("https://api.example.com"),
          jwt.WithIssuer("https://issuer.example.com"),
          jwt.WithExpirationRequired(),
      },
  }))
  ```

- **`typ`** - if your deployment issues several kinds of JWT, check the media type
  as [RFC 8725 Section
  3.11](https://www.rfc-editor.org/rfc/rfc8725#section-3.11) recommends. For
  example, an OAuth 2.0 access token per [RFC
  9068](https://www.rfc-editor.org/rfc/rfc9068#section-2.1) carries
  `"typ": "at+jwt"`:

  ```go
  SuccessHandler: func(c fiber.Ctx) error {
      if typ, _ := jwtware.FromContext(c).Header["typ"].(string); !strings.EqualFold(typ, "at+jwt") {
          // A rejection from here does not pass through the middleware's own
          // rejection path, so it has to carry its own challenge.
          c.Set(fiber.HeaderWWWAuthenticate,
              `Bearer realm="Restricted", error="invalid_token", error_description="The access token is of an unexpected type"`)
          return c.Status(fiber.StatusUnauthorized).SendString("unexpected token type")
      }
      return c.Next()
  },
  ```

- **`jti`** - replay detection needs state the middleware does not keep.

### Status code for a request without credentials

A request that carries no credentials at all is answered with **400** and
`missing or malformed JWT`, which is what this middleware has always done and
what the extractor can tell us: it reports a missing and a malformed credential
as the same error, which is also why its challenge names no error code. [RFC 6750 Section
3.1](https://www.rfc-editor.org/rfc/rfc6750#section-3.1) reserves 400 for a
malformed request and answers a request that "lacks any authentication
information" with 401 instead. If you need those OAuth 2.0 semantics exactly,
say so in an `ErrorHandler`:

```go
ErrorHandler: func(c fiber.Ctx, err error) error {
    if errors.Is(err, extractors.ErrNotFound) {
        if c.Get(fiber.HeaderAuthorization) == "" {
            // No credentials at all: challenge without naming an error.
            c.Set(fiber.HeaderWWWAuthenticate, `Bearer realm="Restricted"`)
            return c.SendStatus(fiber.StatusUnauthorized)
        }
        return c.Status(fiber.StatusBadRequest).SendString(jwtware.ErrMissingToken.Error())
    }
    return c.Status(fiber.StatusUnauthorized).SendString("Invalid or expired JWT")
},
```

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

## Retrieving the token with PassLocalsToContext

When `fiber.Config{PassLocalsToContext: true}` is set, the JWT token stored by the middleware is also available in the underlying `context.Context`. Use `jwtware.FromContext` with any of the supported context types:

```go
// From a fiber.Ctx (most common usage)
token := jwtware.FromContext(c)

// From the underlying context.Context (useful in service layers or when PassLocalsToContext is enabled)
token := jwtware.FromContext(c.Context())
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
