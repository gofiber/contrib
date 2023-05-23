# Minify
Minify middleware for [Fiber](https://github.com/gofiber/fiber). The middleware handles minifying HTML, CSS and JavaScript responses.

### Table of Contents
- [Signatures](#signatures)
- [Examples](#examples)
- [Config](#config)
- [Minify HTML Options](#minifyhtmloptions)
- [Minify CSS Options](#minifycssoptions)
- [Minify JS Options](#minifyjsoptions)


### Signatures
```go
func New(config ...Config) fiber.Handler
```

### Examples
Import the middleware package that is part of the Fiber web framework
```go
import (
  "github.com/gofiber/fiber/v2"
  "github.com/gofiber/contrib/minify"
)
```

Then create a Fiber app with app := fiber.New().

After you initiate your Fiber app, you can use the following possibilities:

### Default Config

```go
app.Use(minify.New())
```

### Custom Config

```go
cfg := minify.Config{
    MinifyHTML:        true,
    MinifyHTMLOptions: MinifyHTMLOptions{
      MinifyScripts: true,
      MinifyStyles:  true
    },
    MinifyCSS:         true,
    MinifyJS:          true,
    Method:            GET,
}

app.Use(minify.New(cfg))
```
### Config
| Property | Type | Description | Optional | Default |
| :--- | :--- | :--- | :--- | :--- |
| MinifyHTML | `bool` | Enable / Disable Html minfy | `yes` | `true` |
| MinifyHTMLOptions | `MinifyHTMLOptions` | [Options for the MinifyHTML](#minifyhTMLoptions) | `yes` | `MinifyHTMLOptions` |
| MinifyCSS | `bool` | Enable / Disable CSS minfy | `yes` | `false` |
| MinifyCSSOptions | `MinifyCSSOptions` | [Options for the MinifyCSS](#MinifyCSSOptions) | `yes` | `MinifyCSSOptions` |
| MinifyJS | `bool` | Enable / Disable JS minfy | `yes` | `false` |
| MinifyJSOptions | `MinifyJSOptions` | [Options for the MinifyJS](#MinifyJSOptions) | `yes` | `MinifyJSOptions` |
| Method | `Method` | Representation of minify route method | `yes` | `GET` |
| Next | `func(c *fiber.Ctx) bool` | Skip this middleware when returned true | `yes` | `nil` |

### MinifyHTMLOptions
| Property | Type | Description | Default |
| :--- | :--- | :--- | :--- |
| MinifyScripts | `bool` | Whether scripts inside the HTML should be minified or not | `false` |
| MinifyStyles | `bool` | Whether styles inside the HTML should be minified or not | `false` |
| ExcludeURLs | `[]string` | URLs Exclud from minification | `nil` |

### MinifyCSSOptions
| Property | Type | Description | Default |
| :--- | :--- | :--- | :--- |
| ExcludeStyles | `[]string` | Styles exclud from minification | `[]string{"*.min.css", "*.bundle.css"}` |

### MinifyJSOptions
| Property | Type | Description | Default |
| :--- | :--- | :--- | :--- |
| ExcludeScripts | `[]string` | Styles exclud from minification | `[]string{"*.min.js", "*.bundle.js"}` |