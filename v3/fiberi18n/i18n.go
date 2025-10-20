package fiberi18n

import (
	"fmt"
	"path"
	"sync"

	"github.com/gofiber/fiber/v3/log"

	"github.com/gofiber/fiber/v3"
	"github.com/nicksnyder/go-i18n/v2/i18n"
)

const localsKey = "fiberi18n"

// New creates a new middleware handler
func New(config ...*Config) fiber.Handler {
	cfg := configDefault(config...)
	// init bundle
	bundle := i18n.NewBundle(cfg.DefaultLanguage)
	bundle.RegisterUnmarshalFunc(cfg.FormatBundleFile, cfg.UnmarshalFunc)
	cfg.bundle = bundle

	cfg.loadMessages()
	cfg.initLocalizerMap()

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}
		c.Locals(localsKey, cfg)
		return c.Next()
	}
}

func (c *Config) loadMessage(filepath string) {
	buf, err := c.Loader.LoadMessage(filepath)
	if err != nil {
		panic(err)
	}
	if _, err := c.bundle.ParseMessageFileBytes(buf, filepath); err != nil {
		panic(err)
	}
}

func (c *Config) loadMessages() *Config {
	for _, lang := range c.AcceptLanguages {
		bundleFilePath := fmt.Sprintf("%s.%s", lang.String(), c.FormatBundleFile)
		filepath := path.Join(c.RootPath, bundleFilePath)
		c.loadMessage(filepath)
	}
	return c
}

func (c *Config) initLocalizerMap() {
	localizerMap := &sync.Map{}

	for _, lang := range c.AcceptLanguages {
		s := lang.String()
		localizerMap.Store(s, i18n.NewLocalizer(c.bundle, s))
	}

	lang := c.DefaultLanguage.String()
	if _, ok := localizerMap.Load(lang); !ok {
		localizerMap.Store(lang, i18n.NewLocalizer(c.bundle, lang))
	}
	c.mu.Lock()
	c.localizerMap = localizerMap
	c.mu.Unlock()
}

/*
MustLocalize get the i18n message without error handling

	  param is one of these type: messageID, *i18n.LocalizeConfig
	  Example:
		MustLocalize(ctx, "hello") // messageID is hello
		MustLocalize(ctx, &i18n.LocalizeConfig{
				MessageID: "welcomeWithName",
				TemplateData: map[string]string{
					"name": context.Param("name"),
				},
		})
*/
func MustLocalize(ctx fiber.Ctx, params interface{}) string {
	message, err := Localize(ctx, params)
	if err != nil {
		panic(err)
	}
	return message
}

/*
Localize get the i18n message

	 param is one of these type: messageID, *i18n.LocalizeConfig
	 Example:
		Localize(ctx, "hello") // messageID is hello
		Localize(ctx, &i18n.LocalizeConfig{
				MessageID: "welcomeWithName",
				TemplateData: map[string]string{
					"name": context.Param("name"),
				},
		})
*/
func Localize(ctx fiber.Ctx, params interface{}) (string, error) {
	local := ctx.Locals(localsKey)
	if local == nil {
		return "", fmt.Errorf("i18n.Localize error: %v", "Config is nil")
	}

	appCfg, ok := local.(*Config)
	if !ok {
		return "", fmt.Errorf("i18n.Localize error: %v", "Config is not *Config type")
	}

	lang := appCfg.LangHandler(ctx, appCfg.DefaultLanguage.String())
	localizer, _ := appCfg.localizerMap.Load(lang)

	if localizer == nil {
		defaultLang := appCfg.DefaultLanguage.String()
		localizer, _ = appCfg.localizerMap.Load(defaultLang)
	}

	var localizeConfig *i18n.LocalizeConfig
	switch paramValue := params.(type) {
	case string:
		localizeConfig = &i18n.LocalizeConfig{MessageID: paramValue}
	case *i18n.LocalizeConfig:
		localizeConfig = paramValue
	}

	message, err := localizer.(*i18n.Localizer).Localize(localizeConfig)
	if err != nil {
		log.Errorf("i18n.Localize error: %v", err)
		return "", fmt.Errorf("i18n.Localize error: %v", err)
	}
	return message, nil
}
