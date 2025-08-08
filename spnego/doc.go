// Package spnego provides SPNEGO (Simple and Protected GSSAPI Negotiation Mechanism)
// Package spnego provides SPNEGO (Simple and Protected GSSAPI Negotiation Mechanism)
// authentication middleware for Fiber applications. It enables Kerberos authentication
// for HTTP requests, allowing seamless integration with Active Directory and other
// Kerberos-based authentication systems.
//
// Version Compatibility:
// - v2 package: Compatible with Fiber v2
// - v3 package: Compatible with Fiber v3
//
// Example Usage:
//
//  import (
//      "fmt"
//      "github.com/gofiber/contrib/spnego/config"
//      v3 "github.com/gofiber/contrib/spnego/v3"
//      "github.com/gofiber/fiber/v3"
//  )
//
//  func main() {
//      app := fiber.New()
//
//      // Create keytab lookup function
//      keytabLookup, err := config.NewKeytabFileLookupFunc("/path/to/keytab.keytab")
//      if err != nil {
//          panic(err)
//      }
//
//      // Create SPNEGO middleware
//      authMiddleware, err := v3.NewSpnegoKrb5AuthenticateMiddleware(&config.Config{
//          KeytabLookup: keytabLookup,
//      })
//      if err != nil {
//          panic(err)
//      }
//
//      // Apply middleware to protected routes
//      app.Use("/protected", authMiddleware)
//
//      // Access authenticated identity
//      app.Get("/protected/resource", func(c fiber.Ctx) error {
//          identity, ok := v3.GetAuthenticatedIdentityFromContext(c)
//          if !ok {
//              return c.Status(fiber.StatusUnauthorized).SendString("Unauthorized")
//          }
//          return c.SendString(fmt.Sprintf("Hello, %s!", identity.UserName()))
//      })
//
//      app.Listen(":3000")
//  }
package spnego
