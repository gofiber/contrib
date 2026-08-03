---
id: spnego
---

# SPNEGO Kerberos Authentication Middleware for Fiber

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*spnego*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20spnego/badge.svg)

This middleware provides SPNEGO (Simple and Protected GSSAPI Negotiation Mechanism) authentication for [Fiber](https://github.com/gofiber/fiber) applications, enabling Kerberos authentication for HTTP requests and inspired by [gokrb5](https://github.com/jcmturner/gokrb5)

## Features

- Kerberos authentication via SPNEGO mechanism
- Flexible keytab lookup system
- Support for dynamic keytab retrieval from various sources
- Integration with Fiber context for authenticated identity storage
- Configurable logging

## Version Compatibility

This middleware is compatible with:

- **Fiber v3**

## Installation

```bash
# For Fiber v3
go get github.com/gofiber/contrib/v3/spnego
```

## Usage

```go
package main

import (
	"fmt"
	"time"

	"github.com/gofiber/contrib/v3/spnego"
	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
)

func main() {
	app := fiber.New()
	// Create a configuration with a keytab lookup function
	// For testing, you can create a mock keytab file using utils.NewMockKeytab
	// In production, use a real keytab file
	_, clean, err := utils.NewMockKeytab(
		utils.WithPrincipal("HTTP/sso1.example.com"),
		utils.WithRealm("EXAMPLE.LOCAL"),
		utils.WithFilename("./temp-sso1.keytab"),
		utils.WithPairs(utils.EncryptTypePair{
			Version:     2,
			EncryptType: 18,
			CreateTime:  time.Now(),
		}),
	)
	if err != nil {
		log.Fatalf("Failed to create mock keytab: %v", err)
	}
	defer clean()
	keytabLookup, err := spnego.NewKeytabFileLookupFunc("./temp-sso1.keytab")
	if err != nil {
		log.Fatalf("Failed to create keytab lookup function: %v", err)
	}
	
	// Create the middleware
	authMiddleware, err := spnego.New(spnego.Config{
		KeytabLookup: keytabLookup,
	})
	if err != nil {
		log.Fatalf("Failed to create middleware: %v", err)
	}

	// Apply the middleware to protected routes
	app.Use("/protected", authMiddleware)

	// Access authenticated identity
	app.Get("/protected/resource", func(c fiber.Ctx) error {
		identity, ok := spnego.GetAuthenticatedIdentityFromContext(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).SendString("Unauthorized")
		}
		return c.SendString(fmt.Sprintf("Hello, %s!", identity.UserName()))
	})

	log.Info("Starting server on :3000")
	if err := app.Listen(":3000"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
```

## Dynamic Keytab Lookup

The middleware is designed with extensibility in mind, allowing keytab retrieval from various sources beyond static files:

> **`KeytabLookup` is called on every authenticated request.** It sits in the request hot path, so a lookup that contacts an external system will add its latency — and its failure modes — to each authentication. If you fetch from a database, a secrets manager, or a remote service, cache the result and refresh it out of band (on a TTL or a rotation event), and apply a bounded timeout.
>
> `NewKeytabFileLookupFunc` already does this: it caches the merged keytab and re-reads the files only when one of them changes size or modification time, so rotating a keytab on disk still takes effect without paying for a parse per request. If a reload fails — a keytab rotation is not atomic, so a read can catch a half-written file — the last keytab that loaded cleanly keeps being served and the next request retries, rather than failing authentication outright.

```go
// Example: Retrieve keytab from a database
func dbKeytabLookup() (*keytab.Keytab, error) {
    // Your database lookup logic here
    // ...
    return keytabFromDatabase, nil
}

// Example: Retrieve keytab from a remote service
func remoteKeytabLookup() (*keytab.Keytab, error) {
    // Your remote service call logic here
    // ...
    return keytabFromRemote, nil
}
```

## API Reference

### `New(cfg Config) (fiber.Handler, error)`

Creates a new SPNEGO authentication middleware.

### `GetAuthenticatedIdentityFromContext(ctx FiberContext) (goidentity.Identity, bool)`

Retrieves the authenticated identity from the Fiber context. `FiberContext` is any type exposing `Locals(key any, value ...any) any`, which `fiber.Ctx` satisfies, so handlers call this as `GetAuthenticatedIdentityFromContext(c)`.

### `NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error)`

Creates a new KeytabLookupFunc that loads keytab files, caching the merged result until a file changes on disk.

### `NewSystemKeytabLookupFunc() (KeytabLookupFunc, error)`

Creates a KeytabLookupFunc for the host's system keytab: `$KRB5_KTNAME` when set, otherwise `/etc/krb5.keytab`.

MIT Kerberos writes `KRB5_KTNAME` as an optional `TYPE:residual` pair. The file-backed types (`FILE:`, `WRFILE:`) are accepted and stripped; anything else — `KEYRING:`, `MEMORY:`, `ANY:` — is rejected at construction with `ErrUnsupportedKeytabResidualType` rather than being treated as a literal path that fails on every request.

Note that this resolves a *keytab*, not a credential cache. Accepting a SPNEGO ticket requires the service's long-term keys, so a client credential cache (`KRB5CCNAME`) cannot be used to authenticate incoming requests.

## Configuration

The `Config` struct supports the following fields:

- `KeytabLookup`: A function that retrieves the keytab (required, unless `FallbackToSystemKeytab` is set)
- `Log`: A `*log.Logger` receiving gokrb5's diagnostics (optional, defaults to no logging)
- `UseFiberLogger`: Send gokrb5's diagnostics to Fiber's default logger when `Log` is nil (optional, defaults to `false`)
- `Unauthorized`: A `func(fiber.Ctx) error` invoked when authentication does not succeed, in place of the default challenge response (optional)
- `FallbackToSystemKeytab`: Load the system keytab when `KeytabLookup` is nil, instead of rejecting the configuration (optional, defaults to `false`)

### Logging

gokrb5 logs once per unauthenticated request, with no level of its own. Since the first leg of every Negotiate handshake is unauthenticated, leaving both `Log` and `UseFiberLogger` unset keeps an unauthenticated client from driving log volume. Setting either one opts in, and note that neither honours Fiber's log level.

### Customizing the unauthorized response

```go
authMiddleware, err := spnego.New(spnego.Config{
	KeytabLookup: keytabLookup,
	Unauthorized: func(c fiber.Ctx) error {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "kerberos authentication required",
		})
	},
})
```

The `WWW-Authenticate` challenge SPNEGO produced is already on the response when this runs, so a handler that only sets a status and body leaves it in place. Removing that header will break Kerberos negotiation with the client.

This handler is **not** called for the "continue needed" leg of a handshake — the 401 that carries a continuation token telling the client to retry with KRB5. That response is a step in a *successful* negotiation, not a failure, and clients renegotiate only if it reaches them untouched, so it is always passed straight through. Your handler sees the initial challenge and outright rejections.

## Requirements

- Fiber v3
- Kerberos infrastructure

## Notes

- Ensure your Kerberos infrastructure is properly configured
- The middleware handles the SPNEGO negotiation process
- Authenticated identities are stored in the Fiber context using `contextKeyOfIdentity`
