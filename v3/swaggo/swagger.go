package swaggo

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	swaggerFiles "github.com/swaggo/files/v2"
	"github.com/swaggo/swag"
)

const (
	defaultDocURL         = "doc.json"
	defaultIndex          = "index.html"
	headerForwardedPrefix = "X-Forwarded-Prefix"
)

var HandlerDefault = New()

// New returns custom handler
func New(config ...Config) fiber.Handler {
	cfg := configDefault(config...)

	index, err := template.New("swagger_index.html").Parse(indexTmpl)
	if err != nil {
		panic(fmt.Errorf("fiber: swagger middleware error -> %w", err))
	}

	var (
		basePrefix string
		once       sync.Once
	)

	return func(c fiber.Ctx) error {
		once.Do(func() {
			basePrefix = strings.ReplaceAll(c.Route().Path, "*", "")
		})

		prefix := basePrefix
		forwardedPrefix := getForwardedPrefix(c)
		if forwardedPrefix != "" {
			prefix = forwardedPrefix + prefix
		}

		cfgCopy := cfg
		if len(cfgCopy.URL) == 0 {
			cfgCopy.URL = path.Join(prefix, defaultDocURL)
		}

		p := utils.CopyString(c.Params("*"))

		switch p {
		case defaultIndex:
			setForwardedPrefixCaching(c, forwardedPrefix)
			c.Type("html")
			return index.Execute(c, cfgCopy)
		case defaultDocURL:
			var doc string
			if doc, err = swag.ReadDoc(cfgCopy.InstanceName); err != nil {
				return err
			}
			return c.Type("json").SendString(doc)
		case "", "/":
			setForwardedPrefixCaching(c, forwardedPrefix)
			return c.Redirect().Status(fiber.StatusMovedPermanently).To(path.Join(prefix, defaultIndex))
		default:
			filePath := path.Clean("/" + p)
			filePath = strings.TrimPrefix(filePath, "/")
			if filePath == "" {
				return fiber.ErrNotFound
			}

			file, err := swaggerFiles.FS.Open(filePath)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fiber.ErrNotFound
				}
				return err
			}
			defer func() {
				cerr := file.Close()
				if cerr != nil && err == nil {
					err = cerr
				}
			}()

			info, err := file.Stat()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fiber.ErrNotFound
				}
				return err
			}

			if info.IsDir() {
				return fiber.ErrNotFound
			}

			data, err := io.ReadAll(file)
			if err != nil {
				return err
			}

			if ext := strings.TrimPrefix(path.Ext(filePath), "."); ext != "" {
				c.Type(ext)
			}

			return c.Send(data)
		}
	}
}

// getForwardedPrefix honors the header only from a trusted proxy, like Fiber's
// other X-Forwarded-* handling.
func getForwardedPrefix(c fiber.Ctx) string {
	if !c.IsProxyTrusted() {
		return ""
	}

	header := c.GetReqHeaders()[headerForwardedPrefix]

	if len(header) == 0 {
		return ""
	}

	var prefix strings.Builder

	for _, rawPrefix := range header {
		endIndex := len(rawPrefix)
		for endIndex > 1 && rawPrefix[endIndex-1] == '/' {
			endIndex--
		}

		prefix.WriteString(rawPrefix[:endIndex])
	}

	return sanitizeForwardedPrefix(prefix.String())
}

// sanitizeForwardedPrefix keeps the prefix a same-origin path, since browsers
// resolve "\\host", "/\host" and "//host" to another origin.
func sanitizeForwardedPrefix(prefix string) string {
	if len(prefix) < 2 || prefix[0] != '/' || prefix[1] == '/' {
		return ""
	}
	for i := 0; i < len(prefix); i++ {
		if ch := prefix[i]; ch == '\\' || ch == '?' || ch == '#' || ch <= ' ' || ch == 0x7f {
			return ""
		}
	}
	return path.Clean(prefix)
}

// setForwardedPrefixCaching marks the responses that embed the prefix: caches
// key on the header, and a shared cache never stores a prefixed copy.
func setForwardedPrefixCaching(c fiber.Ctx, forwardedPrefix string) {
	c.Vary(headerForwardedPrefix)
	if forwardedPrefix != "" {
		c.Set(fiber.HeaderCacheControl, "private")
	}
}
