package main

import (
	"github.com/gofiber/contrib/fiberi18n"
	"github.com/gofiber/fiber/v2"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

func main() {
	app := fiber.New()
	app.Use(
		fiberi18n.New(&fiberi18n.Config{
			RootPath:        "./localize",
			AcceptLanguages: []language.Tag{language.Chinese, language.English},
			DefaultLanguage: language.Chinese,
		}),
	)
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(fiberi18n.MustGetMessage("welcome"))
	})
	app.Get("/:name", func(ctx *fiber.Ctx) error {
		return ctx.SendString(fiberi18n.MustGetMessage(&i18n.LocalizeConfig{
			MessageID: "welcomeWithName",
			TemplateData: map[string]string{
				"name": ctx.Params("name"),
			},
		}))
	})
	app.Listen("127.0.0.1:3000")
}
