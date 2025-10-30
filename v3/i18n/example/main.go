package main

import (
	"log"

	contribi18n "github.com/gofiber/contrib/v3/i18n"
	"github.com/gofiber/fiber/v3"
	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

func main() {
	app := fiber.New()
	app.Use(
		contribi18n.New(&contribi18n.Config{
			RootPath:        "./localize",
			AcceptLanguages: []language.Tag{language.Chinese, language.English},
			DefaultLanguage: language.Chinese,
		}),
	)
	app.Get("/", func(c fiber.Ctx) error {
		localize, err := contribi18n.Localize(c, "welcome")
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
		}
		return c.SendString(localize)
	})
	app.Get("/:name", func(ctx fiber.Ctx) error {
		return ctx.SendString(contribi18n.MustLocalize(ctx, &goi18n.LocalizeConfig{
			MessageID: "welcomeWithName",
			TemplateData: map[string]string{
				"name": ctx.Params("name"),
			},
		}))
	})
	log.Fatal(app.Listen(":3000"))
}
