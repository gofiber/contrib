---
id: spnego
---

# SPNEGO Kerberos Authentication Middleware for Fiber

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=spnego*)
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

	log.Info("Server is running on :3000")
	app.Listen(":3000")
}
```

## Dynamic Keytab Lookup

The middleware is designed with extensibility in mind, allowing keytab retrieval from various sources beyond static files:

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

### `GetAuthenticatedIdentityFromContext(ctx fiber.Ctx) (goidentity.Identity, bool)`

Retrieves the authenticated identity from the Fiber context.

### `NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error)`

Creates a new KeytabLookupFunc that loads keytab files.

## Configuration

The `Config` struct supports the following fields:

- `KeytabLookup`: A function that retrieves the keytab (required)
- `Log`: The logger used for middleware logging (optional, defaults to Fiber's default logger)

## Requirements

- Fiber v3
- Kerberos infrastructure

## Notes

- Ensure your Kerberos infrastructure is properly configured
- The middleware handles the SPNEGO negotiation process
- Authenticated identities are stored in the Fiber context using `contextKeyOfIdentity`
