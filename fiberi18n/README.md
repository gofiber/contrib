---
id: fiberi18n
---

# Fiberi18n

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=fiberi18n*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

[go-i18n](https://github.com/nicksnyder/go-i18n) support for Fiber.

**Note: Requires Go 1.18 and above**

## Install

This middleware supports Fiber v2.

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/fiberi18n
```

## Signature

```
fiberi18n.New(config ...*fiberi18n.Config) fiber.Handler
```

## Config

| Property         | Type                                              | Description                                                  | Default                                                      |
| ---------------- | ------------------------------------------------- | ------------------------------------------------------------ | ------------------------------------------------------------ |
| Next             | `func(c *fiber.Ctx) bool`                         | A function to skip this middleware when returned `true`.     | `nil`                                                        |
| RootPath         | `string`                                          | The i18n template folder path.                               | `"./example/localize"`                                       |
| AcceptLanguages  | `[]language.Tag`                                  | A collection of languages that can be processed.             | `[]language.Tag{language.Chinese, language.English}`         |
| FormatBundleFile | `string`                                          | The type of the template file.                               | `"yaml"`                                                     |
| DefaultLanguage  | `language.Tag`                                    | The default returned language type.                          | `language.English`                                           |
| Loader           | `Loader`                                          | The implementation of the Loader interface, which defines how to read the file. We provide both os.ReadFile and embed.FS.ReadFile. | `LoaderFunc(os.ReadFile)`                                    |
| UnmarshalFunc    | `i18n.UnmarshalFunc`                              | The function used for decoding template files.               | `yaml.Unmarshal`                                             |
| LangHandler      | `func(ctx *fiber.Ctx, defaultLang string) string` | Used to get the kind of language handled by *fiber.Ctx and defaultLang. | Retrieved from the request header `Accept-Language` or query parameter `lang`. |

## Example

```go
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
			RootPath:        "./example/localize",
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
```

