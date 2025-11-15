package example

import (
	"fmt"
	"time"

	"github.com/gofiber/contrib/v3/spnego"
	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
)

func ExampleNew() {
	app := fiber.New()
	// create mock keytab file
	// you must use a real keytab file
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
		log.Fatalf("create mock keytab error: %v", err)
	}
	defer clean()
	keytabLookup, err := spnego.NewKeytabFileLookupFunc("./temp-sso1.keytab")
	if err != nil {
		panic(fmt.Errorf("create keytab lookup function failed: %w", err))
	}
	authMiddleware, err := spnego.New(spnego.Config{
		KeytabLookup: keytabLookup,
	})
	if err != nil {
		panic(fmt.Errorf("create spnego middleware failed: %w", err))
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
	log.Info("Server is running on sso.example.local:3000")
	go func() {
		<-time.After(time.Second * 1)
		fmt.Println("use curl -kv --negotiate http://sso.example.local:3000/protected/resource")
		fmt.Println("Note: In /etc/hosts, sso.example.local must be bound to a LAN address; 127.0.0.1 won't work.")
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
}
