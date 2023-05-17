package fiberi18n

import (
	"os"

	"github.com/gofiber/fiber/v2"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v2"
)

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

	ctx          *fiber.Ctx
	bundle       *i18n.Bundle
	localizerMap map[string]*i18n.Localizer
}

type Loader interface {
	LoadMessage(path string) ([]byte, error)
}

type LoaderFunc func(path string) ([]byte, error)

func (f LoaderFunc) LoadMessage(path string) ([]byte, error) {
	return f(path)
}

var ConfigDefault = &Config{
	RootPath:         "./example/localize",
	DefaultLanguage:  language.English,
	AcceptLanguages:  []language.Tag{language.Chinese, language.English},
	FormatBundleFile: "yaml",
	UnmarshalFunc:    yaml.Unmarshal,
	Loader:           LoaderFunc(os.ReadFile),
	LangHandler:      defaultLangHandler,
}

func defaultLangHandler(c *fiber.Ctx, defaultLang string) string {
	var lang string
	lang = c.Query("lang")
	if lang != "" {
		return lang
	}

	lang = c.Get("Accept-Language")
	if lang != "" {
		return lang
	}

	return defaultLang

}

func configDefault(config ...*Config) *Config {
	// Return default config if nothing provided
	if len(config) == 0 {
		return ConfigDefault
	}

	// Override default config
	cfg := config[0]

	if cfg.Next == nil {
		cfg.Next = ConfigDefault.Next
	}

	if cfg.RootPath == "" {
		cfg.RootPath = ConfigDefault.RootPath
	}

	if cfg.DefaultLanguage == language.Und {
		cfg.DefaultLanguage = ConfigDefault.DefaultLanguage
	}

	if cfg.UnmarshalFunc == nil {
		cfg.UnmarshalFunc = ConfigDefault.UnmarshalFunc
	}

	if cfg.FormatBundleFile == "" {
		cfg.FormatBundleFile = ConfigDefault.FormatBundleFile
	}

	if cfg.AcceptLanguages == nil {
		cfg.AcceptLanguages = ConfigDefault.AcceptLanguages
	}

	if cfg.Loader == nil {
		cfg.Loader = ConfigDefault.Loader
	}

	if cfg.UnmarshalFunc == nil {
		cfg.UnmarshalFunc = ConfigDefault.UnmarshalFunc
	}

	if cfg.LangHandler == nil {
		cfg.LangHandler = ConfigDefault.LangHandler
	}
	return cfg
}
