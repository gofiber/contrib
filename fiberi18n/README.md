# Fiberi18n

![Release](https://img.shields.io/github/release/gofiber/contrib.svg)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

[go-i18n](https://github.com/nicksnyder/go-i18n) support for Fiber.

### Install

This middleware supports Fiber v2.

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/fiberi18n
```

### Signature

```
fiberi18n.New(config ...*Config) fiber.Handler
```

### Config
```go
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(c *fiber.Ctx) bool
	
	// RootPath is i18n template folder path
	//
	// Default: ./example/localize
	RootPath string
	
	// AcceptLanguages is a collection of languages that can be processed
	//
	// Optional. Default: []language.Tag{language.Chinese, language.English}
	AcceptLanguages []language.Tag
	
	// FormatBundleFile is type of template file.
	//
	// Optional. Default: "yaml"
	FormatBundleFile string
	
	// DefaultLanguage is the default returned language type
	//
	// Optional. Default: language.English
	DefaultLanguage language.Tag
	
	// Loader implements the Loader interface, which defines how to read the file.
	// We provide both os.ReadFile and embed.FS.ReadFile
	// Optional. Default: LoaderFunc(os.ReadFile)
	Loader Loader
	
	// UnmarshalFunc for decoding template files
	//
	// Optional. Default: yaml.Unmarshal
	UnmarshalFunc i18n.UnmarshalFunc
	
	// LangHandler is used to get the kind of language handled by *fiber.Ctx and defaultLang
	//
	// Optional. Default: The language type is retrieved from the request header: `Accept-Language` or query param : `lang`
	LangHandler func(ctx *fiber.Ctx, defaultLang string) string
}
```

### Example

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

