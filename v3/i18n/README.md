---
id: i18n
---

# I18n

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=i18n*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20i18n/badge.svg)

[go-i18n](https://github.com/nicksnyder/go-i18n) support for Fiber.


**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/i18n
```

## API

| Name                 | Signature                                                                | Description                                                                 |
|----------------------|--------------------------------------------------------------------------|-----------------------------------------------------------------------------|
| New                  | `New(config ...*i18n.Config) *i18n.I18n`                                 | Create a reusable, thread-safe localization container.                     |
| (*I18n).Localize     | `Localize(ctx fiber.Ctx, params interface{}) (string, error)`            | Returns a localized message. `params` may be a message ID or `*i18n.LocalizeConfig`. |
| (*I18n).MustLocalize | `MustLocalize(ctx fiber.Ctx, params interface{}) string`                 | Like `Localize` but panics when localization fails.                         |

## Config

| Property         | Type                                              | Description                                                                                                                        | Default                                                                        |
|------------------|---------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------|
| RootPath         | `string`                                          | The i18n template folder path.                                                                                                     | `"./example/localize"`                                                         |
| AcceptLanguages  | `[]language.Tag`                                  | A collection of languages that can be processed.                                                                                   | `[]language.Tag{language.Chinese, language.English}`                           |
| FormatBundleFile | `string`                                          | The type of the template file.                                                                                                     | `"yaml"`                                                                       |
| DefaultLanguage  | `language.Tag`                                    | The default returned language type.                                                                                                | `language.English`                                                             |
| Loader           | `Loader`                                          | The implementation of the Loader interface, which defines how to read the file. We provide both os.ReadFile and embed.FS.ReadFile. | `LoaderFunc(os.ReadFile)`                                                      |
| UnmarshalFunc    | `i18n.UnmarshalFunc`                              | The function used for decoding template files.                                                                                     | `yaml.Unmarshal`                                                               |
| LangHandler      | `func(ctx fiber.Ctx, defaultLang string) string` | Used to get the kind of language handled by fiber.Ctx and defaultLang.                                                            | Retrieved from the request header `Accept-Language` or query parameter `lang`. |

## Example

```go
package main

import (
    "log"

    contribi18n "github.com/gofiber/contrib/v3/i18n"
    "github.com/gofiber/fiber/v3"
    goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
    "golang.org/x/text/language"
)

func main() {
    translator := contribi18n.New(&contribi18n.Config{
        RootPath:        "./example/localize",
        AcceptLanguages: []language.Tag{language.Chinese, language.English},
        DefaultLanguage: language.Chinese,
    })

    app := fiber.New()
    app.Get("/", func(c fiber.Ctx) error {
        localize, err := translator.Localize(c, "welcome")
        if err != nil {
            return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
        }
        return c.SendString(localize)
    })
    app.Get("/:name", func(ctx fiber.Ctx) error {
        return ctx.SendString(translator.MustLocalize(ctx, &goi18n.LocalizeConfig{
            MessageID: "welcomeWithName",
            TemplateData: map[string]string{
                "name": ctx.Params("name"),
            },
        }))
    })
    log.Fatal(app.Listen(":3000"))
}
```

## Migration from middleware usage

The package now exposes a global, thread-safe container instead of middleware. To migrate existing code:

1. Remove any `app.Use(i18n.New(...))` calls—the translator no longer registers middleware.
2. Instantiate a shared translator during application startup with `translator := i18n.New(...)`.
3. Replace package-level calls such as `i18n.Localize`/`i18n.MustLocalize` with the respective methods on your translator (`translator.Localize`, `translator.MustLocalize`).
4. Drop any manual interaction with `ctx.Locals("i18n")`; all state is managed inside the translator instance.

The translator instance is safe for concurrent use across handlers and reduces per-request allocations by reusing the same bundle and localizer map.
