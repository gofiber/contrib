package i18n

import (
	"os"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v2"
)

type Config struct {
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

	// LangHandler is used to get the kind of language handled by fiber.Ctx and defaultLang
	//
	// Optional. Default: The language type is retrieved from the request header: `Accept-Language` or query param : `lang`
	LangHandler func(ctx fiber.Ctx, defaultLang string) string

	bundle       *i18n.Bundle
	localizerMap *sync.Map
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

func defaultLangHandler(c fiber.Ctx, defaultLang string) string {
	if c == nil || c.Request() == nil {
		return defaultLang
	}
	if lang := c.Query("lang"); lang != "" {
		return utils.CopyString(lang)
	}
	if lang := c.Get("Accept-Language"); lang != "" {
		return utils.CopyString(lang)
	}

	return defaultLang
}

func configDefault(config ...*Config) *Config {
	var cfg *Config

	switch {
	case len(config) == 0 || config[0] == nil:
		copyCfg := *ConfigDefault
		// ensure mutable fields are not shared with defaults
		if copyCfg.AcceptLanguages != nil {
			copyCfg.AcceptLanguages = append([]language.Tag(nil), copyCfg.AcceptLanguages...)
		}
		cfg = &copyCfg
	default:
		cfg = config[0]
	}

	if cfg.RootPath == "" {
		cfg.RootPath = ConfigDefault.RootPath
	}

	if cfg.DefaultLanguage == language.Und {
		cfg.DefaultLanguage = ConfigDefault.DefaultLanguage
	}

	if cfg.FormatBundleFile == "" {
		cfg.FormatBundleFile = ConfigDefault.FormatBundleFile
	}

	if cfg.UnmarshalFunc == nil {
		cfg.UnmarshalFunc = ConfigDefault.UnmarshalFunc
	}

	if cfg.AcceptLanguages == nil {
		cfg.AcceptLanguages = append([]language.Tag(nil), ConfigDefault.AcceptLanguages...)
	}

	if cfg.Loader == nil {
		cfg.Loader = ConfigDefault.Loader
	}

	if cfg.LangHandler == nil {
		cfg.LangHandler = ConfigDefault.LangHandler
	}

	return cfg
}
