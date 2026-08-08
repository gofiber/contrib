package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofiber/contrib/v3/spnego"
	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
)

func main() {
	app := fiber.New()
	// A temp dir keeps key material out of the working directory. Use a real
	// keytab in production.
	tempDir, err := os.MkdirTemp("", "spnego-example")
	if err != nil {
		panic(fmt.Errorf("create temp dir failed: %w", err))
	}
	// panic, not log.Fatalf: Fatalf calls os.Exit and would skip this cleanup.
	defer func() { _ = os.RemoveAll(tempDir) }()
	keytabPath := filepath.Join(tempDir, "temp-sso.keytab")
	_, clean, err := utils.NewMockKeytab(
		// The SPN must match the host clients contact below, or the client's
		// service ticket will not match any entry in the keytab.
		utils.WithPrincipal("HTTP/sso.example.local"),
		utils.WithRealm("EXAMPLE.LOCAL"),
		utils.WithFilename(keytabPath),
		utils.WithPairs(utils.EncryptTypePair{
			Version:     2,
			EncryptType: 18,
			CreateTime:  time.Now(),
		}),
	)
	if err != nil {
		panic(fmt.Errorf("create mock keytab failed: %w", err))
	}
	defer clean()
	keytabLookup, err := spnego.NewKeytabFileLookupFunc(keytabPath)
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
		// -u : is not optional: curl enables Negotiate only with a credential
		// option, and an empty user:password means "use the ticket cache".
		fmt.Println("use curl -kv --negotiate -u : http://sso.example.local:3000/protected/resource")
		fmt.Println("Note: In /etc/hosts, sso.example.local must be bound to a LAN address; 127.0.0.1 won't work.")
		fmt.Println("if response is 401, execute `klist` to check your Kerberos session")
		<-time.After(time.Second * 2)
		fmt.Println("close server")
		if shutdownErr := app.Shutdown(); shutdownErr != nil {
			// Not a panic: a crash here would bypass main's cleanup.
			log.Errorf("shutdown server failed: %v", shutdownErr)
		}
	}()
	if err := app.Listen("sso.example.local:3000"); err != nil {
		panic(fmt.Errorf("start server failed: %w", err))
	}
}
