package spnego

import (
	"fmt"
	"time"

	"github.com/gofiber/contrib/spnego/config"
	v3 "github.com/gofiber/contrib/spnego/v3"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
)

func ExampleNewSpnegoKrb5AuthenticateMiddleware() {
	app := fiber.New()
	keytabLookup, err := config.NewKeytabFileLookupFunc("/keytabFile/one.keytab", "/keytabFile/two.keyta")
	if err != nil {
		panic(fmt.Errorf("create keytab lookup function failed: %w", err))
	}
	authMiddleware, err := v3.NewSpnegoKrb5AuthenticateMiddleware(&config.Config{
		KeytabLookup: keytabLookup,
	})
	if err != nil {
		panic(fmt.Errorf("create spnego middleware failed: %w", err))
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
	log.Info("Server is running on :3000")
	go func() {
		<-time.After(time.Second * 1)
		fmt.Println("use curl -kv --negotiate http://sso.example.local:3000/protected/resource")
		fmt.Println("if response is 401, execute `klist` to check use kerberos session")
		<-time.After(time.Second * 2)
		fmt.Println("close server")
		if err = app.Shutdown(); err != nil {
			panic(fmt.Errorf("shutdown server failed: %w", err))
		}
	}()
	if err := app.Listen("sso.example.local:3000"); err != nil {
		panic(fmt.Errorf("start server failed: %w", err))
	}

	// Output: Server is running on :3000
}
