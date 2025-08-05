# SPNEGO Kerberos Authentication Middleware for Fiber

[中文版本](README.zh-CN.md)

This middleware provides SPNEGO (Simple and Protected GSSAPI Negotiation Mechanism) authentication for Fiber applications, enabling Kerberos authentication for HTTP requests.

## Features

- Kerberos authentication via SPNEGO mechanism
- Flexible keytab lookup system
- Support for dynamic keytab retrieval from various sources
- Integration with Fiber context for authenticated identity storage
- Configurable logging

## Version Compatibility

This middleware is available in two versions to support different Fiber releases:

- **v2**: Compatible with Fiber v2
- **v3**: Compatible with Fiber v3

## Installation

```bash
# For Fiber v3
go get github.com/gofiber/contrib/spnego/v3

# For Fiber v2
go get github.com/gofiber/contrib/spnego/v2
```

## Usage

### For Fiber v3

```go
package main

import (
    flog "github.com/gofiber/fiber/v3/log"
    "fmt"

    "github.com/jcmturner/gokrb5/v8/keytab"
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/contrib/spnego/v3"
)

func main() {
    app := fiber.New()

    // Create a configuration with a keytab lookup function
    cfg := &spnego.Config{
        // Use a function to look up keytab from files
        KeytabLookup: func() (*keytab.Keytab, error) {
            // Implement your keytab lookup logic here
            // This could be from files, database, or other sources
            kt, err := spnego.NewKeytabFileLookupFunc("/path/to/keytab/file.keytab")
            if err != nil {
                return nil, err
            }
            return kt()
        },
        // Optional: Set a custom logger
        Log: flog.DefaultLogger().Logger().(*log.Logger),
    }

    // Create the middleware
    authMiddleware, err := v3.NewSpnegoKrb5AuthenticateMiddleware(cfg)
    if err != nil {
        flog.Fatalf("Failed to create middleware: %v", err)
    }

    // Apply the middleware to protected routes
    app.Use("/protected", authMiddleware)

    // Access authenticated identity
    app.Get("/protected/resource", func(c fiber.Ctx) error {
        identity, ok := v3.GetAuthenticatedIdentityFromContext(c)
        if !ok {
            return c.Status(fiber.StatusUnauthorized).SendString("Unauthorized")
        }
        return c.SendString(fmt.Sprintf("Hello, %s!", identity.UserName()))
    })

    app.Listen(":3000")
}
```

### For Fiber v2

```go
package main

import (
    "fmt"
    "log"
    "os"

    "github.com/jcmturner/gokrb5/v8/keytab"
    "github.com/gofiber/fiber/v2"
    "github.com/gofiber/contrib/spnego/v2"
)

func main() {
    app := fiber.New()

    // Create a configuration with a keytab lookup function
    cfg := &spnego.Config{
        // Use a function to look up keytab from files
        KeytabLookup: func() (*keytab.Keytab, error) {
            // Implement your keytab lookup logic here
            // This could be from files, database, or other sources
            kt, err := spnego.NewKeytabFileLookupFunc("/path/to/keytab/file.keytab")
            if err != nil {
                return nil, err
            }
            return kt()
        },
        // Optional: Set a custom logger
        Log: log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds),
    }

    // Create the middleware
    authMiddleware, err := v2.NewSpnegoKrb5AuthenticateMiddleware(cfg)
    if err != nil {
        log.Fatalf("Failed to create middleware: %v", err)
    }

    // Apply the middleware to protected routes
    app.Use("/protected", authMiddleware)

    // Access authenticated identity
    app.Get("/protected/resource", func(c *fiber.Ctx) error {
        identity, ok := v2.GetAuthenticatedIdentityFromContext(c)
        if !ok {
            return c.Status(fiber.StatusUnauthorized).SendString("Unauthorized")
        }
        return c.SendString(fmt.Sprintf("Hello, %s!", identity.UserName()))
    })

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

### `NewSpnegoKrb5AuthenticateMiddleware(cfg *Config) (fiber.Handler, error)`

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

- Go 1.21 or higher
- For v3: Fiber v3
- For v2: Fiber v2
- Kerberos infrastructure

## Notes

- Ensure your Kerberos infrastructure is properly configured
- The middleware handles the SPNEGO negotiation process
- Authenticated identities are stored in the Fiber context using `config.ContextKeyOfIdentity`
