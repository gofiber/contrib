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
	defaultDocURL = "doc.json"
	defaultIndex  = "index.html"
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
		if forwardedPrefix := getForwardedPrefix(c); forwardedPrefix != "" {
			prefix = forwardedPrefix + prefix
		}

		cfgCopy := cfg
		if len(cfgCopy.URL) == 0 {
			cfgCopy.URL = path.Join(prefix, defaultDocURL)
		}

		p := utils.CopyString(c.Params("*"))

		switch p {
		case defaultIndex:
			c.Type("html")
			return index.Execute(c, cfgCopy)
		case defaultDocURL:
			var doc string
			if doc, err = swag.ReadDoc(cfgCopy.InstanceName); err != nil {
				return err
			}
			return c.Type("json").SendString(doc)
		case "", "/":
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
			defer file.Close()

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

func getForwardedPrefix(c fiber.Ctx) string {
	header := c.GetReqHeaders()["X-Forwarded-Prefix"]

	if len(header) == 0 {
		return ""
	}

	prefix := ""

	for _, rawPrefix := range header {
		endIndex := len(rawPrefix)
		for endIndex > 1 && rawPrefix[endIndex-1] == '/' {
			endIndex--
		}

		prefix += rawPrefix[:endIndex]
	}

	return prefix
}
