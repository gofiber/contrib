---
id: paseto
title: Paseto
---

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=paseto*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

PASETO returns a Web Token (PASETO) auth middleware.

- For valid token, it sets the payload data in Ctx.Locals and calls next handler.
- For invalid token, it returns "401 - Unauthorized" error.
- For missing token, it returns "400 - BadRequest" error.

### Install

This middleware supports Fiber v2.

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/paseto
go get -u github.com/o1egl/paseto
```

### Signature

```go
pasetoware.New(config ...pasetoware.Config) func(*fiber.Ctx) error
```

### Config

| Property       | Type                            | Description                                                                                                                                                                                             | Default                         |
| :------------- | :------------------------------ | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | :------------------------------ |
| Next           | `func(*Ctx) bool`               | Defines a function to skip middleware                                                                                                                                                                   | `nil`                           |
| SuccessHandler | `func(*fiber.Ctx) error`        | SuccessHandler defines a function which is executed for a valid token.                                                                                                                                  | `c.Next()`                      |
| ErrorHandler   | `func(*fiber.Ctx, error) error` | ErrorHandler defines a function which is executed for an invalid token.                                                                                                                                 | `401 Invalid or expired PASETO` |
| Validate       | `PayloadValidator`              | Defines a function to validate if payload is valid. Optional. In case payload used is created using `CreateToken` function. If token is created using another function, this function must be provided. | `nil`                           |
| SymmetricKey   | `[]byte`                        | Secret key to encrypt token. If present the middleware will generate local tokens.                                                                                                                                                                           | `nil`                           |
| PrivateKey   | `ed25519.PrivateKey`                        | Secret key to sign the tokens. If present (along with its `PublicKey`) the middleware will generate public tokens.                                                                                                                                                                           | `nil`
| PublicKey   | `crypto.PublicKey`                        | Public key to verify the tokens. If present (along with `PrivateKey`) the middleware will generate public tokens.                                                                                                                                                                           | `nil`
| ContextKey     | `string`                        | Context key to store user information from the token into context.                                                                                                                                      | `"auth-token"`                  |
| TokenLookup    | `[2]string`                     | TokenLookup is a string slice with size 2, that is used to extract token from the request                                                                                                               | `["header","Authorization"]`    |

### Instructions

When using this middleware, and creating a token for authentication, you can use the function pasetoware.CreateToken, that will create a token, encrypt or sign it and returns the PASETO token.

Passing a `SymmetricKey` in the Config results in a local (encrypted) token, while passing a `PublicKey` and `PrivateKey` results in a public (signed) token.

In case you want to use your own data structure, is needed to provide the `Validate` function in `paseware.Config`, that will return the data stored in the token, and a error.
